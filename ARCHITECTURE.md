# Architecture

This describes the as-built code layout. For *why* each piece exists, see
[DESIGN.md](DESIGN.md) and the `DESIGN/` subsystem docs it indexes.

## Layering

```
cmd/server         entrypoint: wiring, config from env, HTTP server lifecycle
   │
   ├── internal/tailer    watches Pods, streams container logs (client-go)
   │        │
   │        ▼
   ├── internal/storage   BatchQueue (write buffering) + Store (SQLite FTS5)
   │        ▲
   │        │
   └── internal/api       HTTP handler: /api/search + static file server
            │
            ▼
         web/            embedded search UI (web/templates + web/static)
```

Dependencies only point downward/inward: `tailer` and `api` both depend on
`storage`, but `storage` knows nothing about either of them (`tailer` talks
to it through the narrow `Sink` interface). `cmd/server` is the only package
that imports all three.

## Where things go

| If you're... | Put it in |
| :--- | :--- |
| Changing how pods/containers are discovered or logs are streamed | `internal/tailer/manager.go` |
| Changing write batching, SQLite schema, sharding, retention, or search ranking | `internal/storage/` |
| Adding/changing an HTTP endpoint | `internal/api/handler.go` |
| Changing the search UI | `web/templates/index.html` (markup) or `web/static/{style.css,app.js}` (styling/behavior) |
| Changing env-driven config (new flag, new default) | `cmd/server/main.go` (`envOr`/`envInt`/`envDuration`/`envBool` helpers) |
| Changing what RBAC the pod needs | `k8s/13-role.yaml` **and** `helm/templates/role.yaml` (keep both in sync) |
| Changing the container image build | `Dockerfile` |
| Changing how it's deployed | `k8s/` (plain manifests) **and** `helm/` (chart) — see their READMEs |

## Data flow walkthrough: one log line, end to end

1. A container in the watched namespace writes a line to stdout/stderr.
2. `tailer.Manager` already has a goroutine following that container
   (`GetLogs(Follow: true)`, established when the pod was first seen or last
   restarted — see [DESIGN/02](DESIGN/02_design_tailer.md)). The goroutine's
   `bufio.Scanner` reads the line.
3. `ingest()` wraps it in a `storage.LogLine{Pod, Namespace, Container,
   Timestamp: time.Now(), Content}` and calls `Sink.Enqueue`.
4. `storage.BatchQueue.Enqueue` pushes it onto an internal channel
   (non-blocking; dropped with a warning if the channel is full).
5. The queue's single `run()` goroutine buffers it. When the buffer hits
   `BATCH_SIZE` (default 200) or `BATCH_INTERVAL` (default 500ms) elapses,
   `flush()` calls `Store.InsertBatch`.
6. `InsertBatch` groups the batch by the line's date, opens (or reuses) that
   day's `logs_YYYY-MM-DD.db`, and inserts into its `fts` FTS5 table inside
   one transaction.
7. A browser hits `GET /api/search?q=...`. `api.Handler.handleSearch` parses
   `q`/`start`/`end` and calls `Store.Search`.
8. `Search` attaches every shard file in `[start, end]` to a fresh in-memory
   connection and runs one `UNION ALL … ORDER BY bm25() … LIMIT` query
   across them.
9. Results (with `<mark>`-highlighted snippets from SQLite's `snippet()`)
   come back as JSON; `web/static/app.js`'s `render()` turns each into a
   `[pod/container] timestamp snippet` line.

## Adding a new HTTP endpoint

See [DESIGN/04](DESIGN/04_design_api.md#adding-a-new-http-endpoint).

## Adding a new config knob

1. Read it in `cmd/server/main.go` with the matching `env*` helper.
2. Pass it down to whichever package needs it (avoid reading `os.Getenv`
   outside `main.go` — it's the only place config is resolved).
3. Add it to `helm/values.yaml` + `helm/templates/configmap.yaml` and to
   `k8s/10-configmap.yaml`, and document the default in `README.md`'s
   configuration table.

## Adding a new Kubernetes resource

1. Add the plain manifest under `k8s/`, numbered by apply order (see
   `k8s/README.md`).
2. Add the equivalent Helm template under `helm/templates/`, wired through
   `helm/values.yaml` and using the `_helpers.tpl` label/name helpers.
3. Update the RBAC `Role` in both places if the new resource needs the pod
   to reach a new API.
