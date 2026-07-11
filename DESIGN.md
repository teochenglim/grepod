# Design

grepod is a single static Go binary that tails every pod's logs in one
Kubernetes namespace, indexes them into local SQLite FTS5 databases, and
serves a small embedded search UI. No Loki, no Alloy, no sidecars.

This file is an index. The actual design is split by subsystem in `DESIGN/`,
numbered in reading order:

1. [Overview](DESIGN/01_design_overview.md) — goals, non-goals, and how the
   pieces fit together.
2. [Tailer](DESIGN/02_design_tailer.md) — pod discovery and log streaming via
   client-go.
3. [Storage](DESIGN/03_design_storage.md) — the batch queue and daily-sharded
   SQLite FTS5 store.
4. [API & UI](DESIGN/04_design_api.md) — the HTTP surface and embedded search
   frontend.

See also: [README.md](README.md) for usage, [ARCHITECTURE.md](ARCHITECTURE.md)
for the code layout, [RELEASE.md](RELEASE.md) for what shipped when, and
[CLAUDE.md](CLAUDE.md) for repo orientation.
