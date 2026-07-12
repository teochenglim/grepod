# 03 — Storage

`internal/storage` has three collaborators: `BatchQueue` (write buffering),
`Store` (the SQLite FTS5 persistence layer), and `Broadcaster` (live tail
fan-out, since v0.4.0).

## BatchQueue

Tailer goroutines call `Enqueue` from many concurrent goroutines (one per
container). `Enqueue` is non-blocking: it pushes onto a buffered channel
(`size*4` capacity) and drops the line with a log warning if full, rather
than ever back-pressuring a tailer goroutine — losing a line under extreme
load is preferable to stalling log collection.

A single `run()` goroutine owns the actual buffer and flushes it to `Store`
either when it hits `BATCH_SIZE` (default 200) lines or every
`BATCH_INTERVAL` (default 15s, was 500ms before v1.0.0 — see "Context-bounded
queries" below) tick, whichever comes first. This bounds both memory use and
worst-case indexing latency.

Each `flush()` call records the Insert RED metrics (v0.7.0, see
[DESIGN/04](04_design_api.md#metrics-v070)): `grepod_insert_requests_total`,
`grepod_insert_duration_seconds` (wall time around `Store.InsertBatch`), and
`grepod_insert_errors_total` when it returns an error.
`recordDrop` — below — additionally increments `grepod_lines_dropped_total`
per dropped line, independent of the rate-limited log warning. A `nil`
`*metrics.Metrics` (every `BatchQueue` built directly via struct literal in
tests, bypassing `NewBatchQueue`) disables all of this rather than panicking
— every call site checks first.

### Never flooding on a full queue (v0.5.1)

`Enqueue`'s full-channel case originally logged one `slog.Warn` per
dropped line — fine in isolation, but under a sustained overload (hundreds
of drops/second) that's hundreds of warning lines/second written to
grepod's own stdout, which [DESIGN/02](02_design_tailer.md#never-tailing-itself-v051)
explains is itself tailed back in absent the `selfPod` exclusion added in
the same release. `recordDrop` collapses this to at most one summarizing
warning (`count`, `last_pod`, `last_container`) per `dropWarnInterval`
(5s), tracked with two atomics (`dropped`, `lastWarnedAt`) rather than a
mutex, keeping `Enqueue`'s existing never-block guarantee intact on this
path too.

## Broadcaster: live tail fan-out

`tailer.Manager.ingest` doesn't call `BatchQueue.Enqueue` directly —
`cmd/server` wires a `fanoutSink` in front of it (`main.go`, not a
`storage` type) that calls both `BatchQueue.Enqueue` *and*
`Broadcaster.Publish` for every line. Neither `BatchQueue` nor
`Broadcaster` know about each other or about `tailer`; they're composed
at the top, not coupled to each other. This matters because the two have
different latency requirements: `BatchQueue` batches for SQLite
throughput (`BATCH_INTERVAL`, 15s as of v1.0.0 — see "Context-bounded
queries" below), while
`/api/tail` (see [DESIGN/04](04_design_api/02_tail_and_known.md#apitail-v040)) needs lines
the instant they arrive, not after a flush.

`Broadcaster.Subscribe()` gives each subscriber (one per `/api/tail`
connection) its own buffered channel (256 lines) and an unsubscribe func;
`Publish` fans a line out to every current subscriber, holding its mutex
only for the short non-blocking iteration. Two things distinguish it from
`BatchQueue.Enqueue`'s already-established "never block, never
back-pressure the tailer" philosophy:

- **Drop-oldest, not drop-newest.** A full `BatchQueue` channel drops the
  incoming line (it can't reorder what's already committed to the batch).
  A full `Broadcaster` subscriber instead evicts its *oldest* buffered
  line to make room for the new one — for a live viewer, the most recent
  activity matters more than preserving a gap they've already scrolled
  past.
- **No replay.** A subscriber only receives lines published after it
  subscribed. There's no historical buffer to catch up on — that's what
  `/api/search` is for.

`SubscriberCount()` backs [v0.7.0](../RELEASE/v0.7.0.md)'s
`grepod_tail_subscribers` gauge (see
[DESIGN/04](04_design_api.md#metrics-v070)) — a `promauto.NewGaugeFunc` in
`cmd/server/main.go` reads it on every `/metrics` scrape rather than
`Broadcaster` incrementing/decrementing a counter at subscribe/unsubscribe
time, keeping `Broadcaster` itself with zero Prometheus dependency.

## Store: daily-sharded SQLite + FTS5

One SQLite database file per calendar day: `logs_YYYY-MM-DD.db`, each
containing a single `fts` virtual table (`FTS5`) with columns `pod`,
`namespace`, `container`, `timestamp`, `level`, `line` — all `UNINDEXED`
except `line` itself, which FTS5 tokenizes and indexes.

**Breaking, pre-1.0:** `level` was added in v0.3.0. `CREATE VIRTUAL TABLE
IF NOT EXISTS` is a no-op against a shard file that already has an `fts`
table, so a shard predating that release would otherwise keep its
original 5-column schema forever — and FTS5 tables reject `ALTER TABLE`
outright (`"virtual tables may not be altered"`, confirmed against
`modernc.org/sqlite`; there's no in-place `ADD COLUMN` for FTS5). As of
v0.5.2, `getOrOpenDB` detects this (`pragma_table_info('fts')`) and
rebuilds the table — `DROP TABLE` + recreate, the only option available —
the first time that shard is opened for a write. This loses that shard's
pre-migration rows (delete-and-re-ingest, same stance as any other
pre-1.0 schema change — grepod has no compatibility guarantee before
v1.0.0, see [RELEASE/v1.0.0](../RELEASE/v1.0.0.md)), but is a strict
improvement over the alternative: without it, *every* insert into that
shard fails, not just the pre-migration rows' searchability — see
[RELEASE/v0.5.2](../RELEASE/v0.5.2.md) for how that surfaced. One gap
remains: `Search`'s per-query `ATTACH` (below) bypasses `getOrOpenDB`
entirely, so a legacy shard that's only ever searched, never written to
again, still fails its `SELECT` with `"no such column: level"` — manual
delete-and-re-ingest is still the answer for that case.

Sharding by day rather than one giant table makes two operations cheap
that would otherwise be expensive at scale:

- **Retention** — deleting a day's logs is `os.Remove` on that day's file
  (plus its `-wal`/`-shm` siblings), not a `DELETE ... WHERE timestamp <
  cutoff` scan across all history.
- **Vacuuming** — `PRAGMA vacuum` runs per-shard on the (small) shards that
  remain after retention, not on one ever-growing file.

Each shard is opened with `SetMaxOpenConns(1)` — `modernc.org/sqlite` is
pure Go with no CGO, and serializing writes per shard avoids
`SQLITE_BUSY` under concurrent flushes. WAL mode + a 5s busy timeout absorb
the rest.

`InsertBatch` groups an incoming batch by the line's ingestion date (so a
flush that straddles midnight lands correctly split), then opens one
transaction per affected shard for throughput.

## `LastSeen` (v0.7.0)

`Store.LastSeen(pod, container string) (time.Time, bool, error)` returns the
most recent timestamp already indexed for a pod/container — used by
`tailer.Manager` to seed a restart-safe `SinceTime` on reconnect, see
[DESIGN/02](02_design_tailer.md#restart-safe-reconnects-v070).

It scans shard files **most-recent-day-first**, opening each as its own
short-lived connection (separate from the long-lived write handles in
`Store.dbs` — `LastSeen` routinely runs against a shard this process has
never written to, e.g. right after a restart) and querying `SELECT
MAX(timestamp) FROM fts WHERE pod = ? AND container = ?`. It **stops at the
first day with a matching row** rather than scanning every shard and taking
an overall max: shards are disjoint by construction (`InsertBatch` groups by
date), so a later calendar day's timestamps are always greater than an
earlier day's — the first hit scanning backward from today is already the
answer. The scan is bounded to `markerLookbackDays` (30) days back; a
container that hasn't logged within that window is treated as having no
marker at all (`ok == false`) rather than scanned for indefinitely — most of
that history will be gone via retention anyway (default `RETENTION_DAYS=7`),
and the tailer's fallback for "no marker" (ingest the container's current
full buffer, same as pre-v0.7.0) is a fine degradation for a container that
stale.

## Context-bounded queries (v1.0.0)

Originally planned as a separate v0.8.0 release, folded into v1.0.0 — see
[RELEASE/v1.0.0](../RELEASE/v1.0.0.md). Before this, `Store.Search` took
no `context.Context` at all: it called `sql.DB.Query`/`Exec`, not the
`...Context` variants, so a request's timeout or an early client
disconnect never actually reached SQLite — an expensive cross-shard query
ran to completion regardless, with nothing bounding it but the HTTP
server's own `WriteTimeout` from the outside.

- **`Store.Search(ctx context.Context, opts SearchOptions)`** and
  **`Store.InsertBatch(ctx context.Context, lines []LogLine)`** both check
  `ctx.Err()` up front (an already-cancelled context returns promptly,
  without ever touching SQLite) and use `QueryContext`/`ExecContext`/
  `BeginTx`/`PrepareContext` throughout, so a context that expires or is
  cancelled mid-query actually cancels the underlying SQLite operation.
  `internal/api.Handler.handleSearch` passes `r.Context()` — a client that
  disconnects mid-search no longer leaves that query running to
  completion for nothing.
- **`BatchQueue.flush` passes a bounded-but-generous context**
  (`insertTimeout`, default 30s — see `defaultInsertTimeout`) rather than
  `context.Background()`, even though ingestion isn't request-driven the
  way search is: a self-imposed backstop against one pathological shard
  write (a stuck disk, WAL corruption) stalling the whole ingestion
  pipeline behind it forever. 30s is deliberately generous — DESIGN/05's
  benchmarks put a normal 200-line flush at ~1.5ms, four orders of
  magnitude under this; this is a hang detector, not a latency target.
- **`Enqueue`'s channel capacity (`size*4`) was re-checked against
  `BATCH_INTERVAL`'s 500ms→15s change and left unchanged.** The channel
  only needs to absorb lines arriving while `run()`'s loop is blocked
  inside a synchronous `flush()` call — bounded by `BATCH_SIZE` (how big
  a batch gets before `flush()` is even called) and that flush's actual
  duration (~1.5ms at the default size, per DESIGN/05), neither of which
  `BATCH_INTERVAL` affects. `BATCH_INTERVAL` only matters for namespaces
  quiet enough to never hit `BATCH_SIZE` within the interval — and by
  definition, those never approach `size*4` lines in flight either. The
  real effect of the 500ms→15s change is entirely in per-container
  search-latency, not channel pressure: a line enqueued right after a
  flush on a quiet container is now searchable up to ~15s later instead
  of ~500ms later. Expected, not a bug — trades a small latency window on
  quiet containers for meaningfully fewer, larger write transactions.

## Search: cross-shard ATTACH

`Store.Search(ctx context.Context, opts SearchOptions)`:

1. Lists which shard files exist for `[opts.Start, opts.End]` (`os.Stat` —
   a missing day is skipped, not an error).
2. Opens a fresh in-memory SQLite connection scoped to this one query, and
   `ATTACH DATABASE`s every matching shard onto it.
3. Runs a single `UNION ALL` query across all attached shards' `fts`
   tables, wrapped in an outer query that applies the keyset pagination
   cursor and `LIMIT` (see [DESIGN/04](04_design_api/01_search.md#pagination)). With
   `opts.Query` set, each shard's `SELECT` uses FTS5 `MATCH`, ranked by
   `bm25()`, with `snippet()` producing the highlight — see
   [DESIGN/04](04_design_api/01_search.md#browse-mode-v052) for the `opts.Query ==
   ""` browse-mode alternative, which skips `MATCH`/`bm25()`/`snippet()`
   entirely and orders by recency instead. Either way, every row's
   `snippet`/line content is HTML-escaped before `<mark>` highlighting is
   reintroduced — see
   [DESIGN/04](04_design_api/01_search.md#snippet-escaping-v052) — since log content
   is not trusted input and the UI renders it via `innerHTML`.

Attaching per-query (rather than keeping every shard attached to one
long-lived connection) keeps shard lifecycle simple: a shard file being
deleted by the retention cron can't corrupt an in-flight query on a
different connection.

**Filtering (`opts.MinLevel`, `opts.Pod` — the latter since v1.1.0)** is
built per-shard as a `conditions []string`/`condArgs []any` pair, shared
between browse and keyword mode rather than each hand-rolling its own
`WHERE`-vs-`AND` string concatenation: browse mode joins `conditions`
into a `WHERE` clause (there's no `MATCH` to anchor an `AND` to), keyword
mode appends each as `AND <condition>` after `WHERE fts MATCH ?`. Both
filters combine with `AND`, not `OR` — `pod=web-1&level=ERROR` means
"web-1's ERROR-and-above lines," not "web-1's lines, or anything
ERROR-and-above."

## Retention

`StartRetentionCron` runs `CleanupOldDBs` once daily at 03:00 local time.
Shards older than `RETENTION_DAYS` are closed (if open) and deleted;
remaining shards are vacuumed to reclaim FTS5 fragmentation.

## Why not horizontal scale-out

Each grepod replica would run its own `tailer.Manager` against the same
namespace, duplicating every log line, and the `ReadWriteOnce` PVC backing
`/data` can only be mounted read-write by one pod at a time. `replicas` is
pinned to 1 in both the plain manifests and the Helm chart — see
[ARCHITECTURE.md](../ARCHITECTURE.md) and `k8s/README.md` for how that
constraint shows up in the Kubernetes manifests.
