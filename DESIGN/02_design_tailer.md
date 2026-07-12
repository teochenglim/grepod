# 02 — Tailer

`internal/tailer` (package `tailer`, type `Manager`) owns pod discovery and
log streaming.

## Pod discovery

`Manager.Run` starts a `client-go` `SharedInformerFactory` scoped to a single
namespace (`informers.WithNamespace(namespace)`). Add/Update events call
`reconcilePod`; Delete events call `stopPod`. Once the informer's initial
cache sync completes, `Manager.Ready()` flips to `true` — the signal
`internal/api`'s `/readyz` reports (see
[DESIGN/04](04_design_api.md#healthz--readyz)).

## Reconciliation

`reconcilePod` walks every container status on the pod (plus init containers
if `INCLUDE_INIT_CONTAINERS=true`) and compares each container's current
`RestartCount` against the last value the Manager saw
(`m.restartCounts[containerKey]`). Three cases:

- **New container** (not in `m.cancels`) → start a tailer goroutine.
- **Restart count changed** → the old goroutine is stopped and a fresh one
  started, so the tailer re-fetches the crashed container's previous logs.
- **No change** → skip; already tailing.

This makes the Manager idempotent under repeated informer events, which
`client-go` delivers routinely (periodic resync, not just real changes).

`reconcilePod` returns immediately, before touching any container, if
`pod.Name == selfPod` — grepod's own pod (`selfPod` is `POD_NAME` from the
Downward API, threaded through `tailer.NewManager`'s `selfPod` parameter)
is never tailed. See "Never tailing itself" below for why this is a
correctness fix, not just a self-observability preference.

## Streaming a container

`tailContainer` runs two phases per container, forever, until its `ctx` is
cancelled:

1. `fetchPreviousLogs` — one-shot `GetLogs(Previous: true, TailLines: 100)`.
   Errors here are swallowed (expected on a container's first run — there is
   no previous instance yet). This guarantees a crash's last lines are
   captured even though the crash itself raced the informer event.
2. `streamLogs` — `GetLogs(Follow: true)`, read until the stream drops, then
   retry with exponential backoff (`250ms` → `5s` cap) until `ctx` is
   cancelled.

Each container is exactly one goroutine, keyed by `containerKey{pod,
container}`. `Manager.mu` guards the two maps (`cancels`, `restartCounts`);
nothing else is shared mutable state.

## Ingestion

`ingest` scans the log stream line-by-line (`bufio.Scanner`, 1MB max line)
and calls `Sink.Enqueue` per line, stamping `time.Now()` — Kubernetes does
not attach a timestamp to raw (non-`--timestamps`) log output, so ingestion
time is what grepod indexes and displays. `Sink` is satisfied by
`storage.BatchQueue` (see [Storage](03_design_storage.md)); the tailer never
talks to SQLite directly.

Each line also gets a best-effort `Level` via `detectLevel` (`level.go`):
a single regex, `\b(FATAL|ERROR|WARNING|WARN|INFO|DEBUG|TRACE)\b`
(case-insensitive, `WARNING` normalized to `WARN`), matched against the
raw line. No log-format parsing, no per-runtime/framework special-casing —
if a recognizable token isn't present as a whole word, `Level` is `""`
rather than guessed. This is a heuristic, not a contract: it will miss
levels embedded in structured formats it doesn't specifically look for
(e.g. a bare JSON `{"lvl":"w",...}`) and false-positive on the word
appearing in a message that isn't actually indicating severity. Good
enough for "mostly right, never silently wrong" — see
[DESIGN/04](04_design_api.md) for how it's surfaced, and
[v0.5.0](../RELEASE/v0.5.0.md) for the UI built on top of it.

## Never tailing itself (v0.5.1)

Early versions had no special case for grepod's own pod, and hit a
self-sustaining feedback loop under any sustained overload:
`storage.BatchQueue.Enqueue` logs a warning when its internal channel is
full (`"batch queue full, dropping line"`, written to grepod's own
stdout) — and since grepod tails every container in its namespace with no
exclusion, it would tail that warning back in and try to enqueue it too.
If the queue was still full, *that* attempt logged another warning, which
got tailed back in, indefinitely — independent of whatever originally
filled the queue, and it never recovered on its own. `reconcilePod`'s
`pod.Name == selfPod` early return closes the loop at its root: grepod's
own log lines never re-enter the pipeline in the first place. Paired with
[DESIGN/03](03_design_storage.md#never-flooding-on-a-full-queue)'s
rate-limited warning as defense in depth for any other pod producing a
genuine sustained burst.

An empty `selfPod` (e.g. `POD_NAME` unset, running outside Kubernetes)
disables the exclusion rather than matching a pod literally named `""`.

## Adding a new event source

Tailer only speaks to the Kubernetes Pods API today. To add another log
source (e.g. tailing a file, or a second namespace), implement something
that produces `storage.LogLine` and calls `Sink.Enqueue` — no changes to
`storage` or `api` are needed, since both are decoupled from *how* lines
arrive.
