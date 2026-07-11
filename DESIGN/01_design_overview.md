# 01 — Overview

## Goal

Give a namespace-scoped, keyword-searchable view over every pod's logs
without running a log aggregation stack. `kubectl logs -f` per pod, or Loki +
Promtail/Alloy + object storage, are both overkill for a single namespace on
a small cluster.

## Non-goals

- Multi-namespace or cluster-wide aggregation (one grepod instance watches
  one namespace — run one per namespace if you need more).
- Long-term log retention or compliance archival (`RETENTION_DAYS` defaults
  to 7; shards older than that are deleted).
- Structured log parsing/enrichment. Lines are stored and searched as raw
  text via SQLite FTS5.
- Horizontal scale-out. See [Storage](03_design_storage.md) for why a second
  replica would duplicate ingestion and corrupt the single `ReadWriteOnce`
  volume.

## How it fits together

```
┌─────────────┐   watch pods    ┌──────────────┐   enqueue    ┌────────────┐
│  Kubernetes  │ ──────────────▶│    tailer    │─────────────▶│ batchqueue │
│  API server  │  stream logs   │   (Manager)  │  LogLine{}   │            │
└─────────────┘◀────────────────└──────────────┘              └─────┬──────┘
                                                                       │ flush
                                                                       ▼
┌─────────────┐   HTTP GET      ┌──────────────┐   FTS5 MATCH  ┌────────────┐
│   browser    │◀───────────────│  api.Handler │◀──────────────│   Store    │
│  (embedded   │  /api/search   │              │               │ (SQLite,   │
│   UI)        │───────────────▶│              │──────────────▶│ daily      │
└─────────────┘                 └──────────────┘   results     │ shards)    │
                                                                └────────────┘
```

One process, one namespace, one data directory. `cmd/server/main.go` wires
all four pieces together and owns the HTTP server lifecycle and graceful
shutdown.

## Key decisions

- **client-go informer, not `kubectl logs` subprocesses.** A `SharedInformerFactory`
  scoped to one namespace drives pod add/update/delete events; each
  container gets its own long-lived log-streaming goroutine. See
  [Tailer](02_design_tailer.md).
- **SQLite FTS5, daily-sharded, pure Go (`modernc.org/sqlite`).** No CGO, no
  external search service. Sharding by day makes retention a matter of
  deleting old files rather than running `DELETE` + `VACUUM` on one huge
  table. See [Storage](03_design_storage.md).
- **Batched writes.** Tailer goroutines never touch SQLite directly — they
  enqueue into an in-memory `BatchQueue` that flushes on a size or time
  threshold, so log ingestion never blocks on disk I/O.
- **Everything embedded.** The search UI (`web/templates/index.html` plus
  its `web/static/` CSS/JS/favicon) is compiled into the binary via
  `go:embed`, so the container image and the Kubernetes deployment are both
  a single artifact plus a PVC.
