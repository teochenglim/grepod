# 03 — Storage

`internal/storage` has two collaborators: `BatchQueue` (write buffering) and
`Store` (the SQLite FTS5 persistence layer).

## BatchQueue

Tailer goroutines call `Enqueue` from many concurrent goroutines (one per
container). `Enqueue` is non-blocking: it pushes onto a buffered channel
(`size*4` capacity) and drops the line with a log warning if full, rather
than ever back-pressuring a tailer goroutine — losing a line under extreme
load is preferable to stalling log collection.

A single `run()` goroutine owns the actual buffer and flushes it to `Store`
either when it hits `BATCH_SIZE` (default 200) lines or every
`BATCH_INTERVAL` (default 500ms) tick, whichever comes first. This bounds
both memory use and worst-case indexing latency.

## Store: daily-sharded SQLite + FTS5

One SQLite database file per calendar day: `logs_YYYY-MM-DD.db`, each
containing a single `fts` virtual table (`FTS5`) with columns `pod`,
`namespace`, `container`, `timestamp`, `level`, `line` — all `UNINDEXED`
except `line` itself, which FTS5 tokenizes and indexes.

**Breaking, pre-1.0:** `level` was added in v0.3.0. Shard files from
before that release don't have the column; no migration is attempted —
delete and re-ingest, same as any pre-1.0 schema change (grepod has no
compatibility guarantee before v1.0.0 — see
[RELEASE/v1.0.0](../RELEASE/v1.0.0.md)).

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

## Search: cross-shard ATTACH

`Store.Search(query, start, end, limit)`:

1. Lists which shard files exist for `[start, end]` (`os.Stat` — a missing
   day is skipped, not an error).
2. Opens a fresh in-memory SQLite connection scoped to this one query, and
   `ATTACH DATABASE`s every matching shard onto it.
3. Runs a single `UNION ALL` query across all attached shards' `fts` tables,
   ranked by FTS5's `bm25()`, with `snippet()` producing the `<mark>`-wrapped
   highlight the UI renders directly.

Attaching per-query (rather than keeping every shard attached to one
long-lived connection) keeps shard lifecycle simple: a shard file being
deleted by the retention cron can't corrupt an in-flight query on a
different connection.

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
