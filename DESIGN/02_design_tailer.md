# 02 — Tailer

`internal/tailer` (package `tailer`, type `Manager`) owns pod discovery and
log streaming.

## Pod discovery

`Manager.Run` starts a `client-go` `SharedInformerFactory` scoped to a single
namespace (`informers.WithNamespace(namespace)`). Add/Update events call
`reconcilePod`; Delete events call `stopPod`. Once the informer's initial
cache sync completes, `Manager.Ready()` flips to `true` — the signal
`internal/api`'s `/readyz` reports (see
[DESIGN/04](04_design_api/03_health_and_ui.md#healthz--readyz)).

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
2. `streamLogs` — `GetLogs(Follow: true, SinceTime: <marker, if any>)`, read
   until the stream drops, then retry with exponential backoff (`250ms` →
   `5s` cap) until `ctx` is cancelled. See "Restart-safe reconnects
   (v0.7.0)" below for what the marker is and why it's there.

Each container is exactly one goroutine, keyed by `containerKey{pod,
container}`. `Manager.mu` guards the two maps (`cancels`, `restartCounts`);
`Manager.markersMu` separately guards the marker map below — kept distinct
because ingest touches it on every line, and sharing a lock with the
(infrequent) reconciliation path would add contention for no reason.

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
[DESIGN/04](04_design_api/01_search.md#level-filtering) for how it's surfaced, and
[v0.5.0](../RELEASE/v0.5.0.md) for the UI built on top of it.

## Restart-safe reconnects (v0.7.0)

Before v0.7.0, `streamLogs` called `GetLogs(Follow: true)` with no
`SinceTime`. Per the Kubernetes API, that combination returns the
container's **entire currently-buffered log content** first, then follows
live — the same behavior as `kubectl logs -f` with no `--tail`. So every
reconnect to an already-running container (a dropped stream retrying inside
`tailContainer`'s backoff loop, or grepod's own process restarting and
rediscovering pods it was already tailing) re-ingested everything still
sitting in that container's buffer: not data loss, but duplicate rows and
confusing duplicate search hits. (A container *restarting* — `RestartCount`
bumping — was never actually affected: that gets a fresh log buffer from
Kubernetes, so there was nothing to duplicate there in the first place.)

Each `Manager` now tracks an in-memory `marker` per `containerKey` — the
timestamp of the most recently ingested *live* line for that container —
and passes it as `PodLogOptions.SinceTime` on every `streamLogs` call for
that container, including the very first one. Two things make this cheap
and simple rather than a full persisted-state subsystem:

- **`setMarker`** advances the marker as `ingest` reads each line, but only
  when called from `streamLogs`'s read (`ingest(..., trackMarker: true)`).
  `fetchPreviousLogs`'s read is explicitly excluded
  (`trackMarker: false`) — it's reading a *different*, already-crashed
  container instance, and letting it advance the marker could skip
  legitimate lines from the just-started current instance.
- **`resolveMarker`** seeds the marker for a container this `Manager` has
  never tailed before by querying `storage.Store.LastSeen(pod, container)`
  (see [Storage](03_design_storage.md#lastseen-v070)) — but only once per
  container per process lifetime, cached in the marker map from then on.
  This is what actually closes the gap for a full grepod restart: a fresh
  `Manager` has no in-memory history, but the store does. Within a single
  process's lifetime, every subsequent reconnect (dropped stream, informer
  resync) reuses the in-memory value — no repeated store query per
  reconnect, which matters for a namespace with many pods.
- A container the store has never indexed (a genuinely new pod, or
  `MarkerStore` is `nil`) resolves to the zero-value marker, which
  `streamLogs` treats as "no `SinceTime`" — falling through to the
  pre-v0.7.0 behavior of ingesting whatever's currently buffered. This is
  the correct behavior for a first-ever connection, not a bug: there is
  nothing to dedupe against yet.
- Markers are cleared on `stopPod` (an actual pod deletion) but
  deliberately *not* on `stopContainer` alone (a restart-count-triggered
  stop) — `reconcilePod`'s restart path stops the old goroutine and starts
  a fresh one for the same `containerKey` right after, and that fresh
  goroutine should still benefit from what the crashed instance already
  ingested.

**Known imprecision:** `SinceTime` is a caveat, not a bug — see `ingest`'s
own timestamp caveat above. Grepod's marker is its own ingestion-time
stamp, not the container runtime's per-line timestamp that `SinceTime`
actually filters on server-side. In practice ingestion follows emission
closely enough that this only matters at the exact boundary line, which
the K8s API includes inclusively — worth stating precisely rather than
promising exact dedup this mechanism doesn't provide. See
[RELEASE/v0.7.0](../RELEASE/v0.7.0.md).

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
