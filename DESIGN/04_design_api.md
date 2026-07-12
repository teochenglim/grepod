# 04 — API & UI: Overview

`internal/api` (package `api`, type `Handler`) is the entire HTTP surface.
It wraps a `*storage.Store` and the `web` package's two embedded
`embed.FS`s (`web.TemplatesFS`, `web.StaticFS`) behind a plain
`http.ServeMux`.

This subsystem doc is split by endpoint/concern, in reading order:

1. Overview (this file) — routes, adding a new endpoint.
2. [Search](04_design_api/01_search.md) — `/api/search`: browse mode,
   snippet escaping, level filtering, pagination.
3. [Tail & known](04_design_api/02_tail_and_known.md) — `/api/tail`
   (SSE) and `/api/known` (filter dropdowns).
4. [Health & UI](04_design_api/03_health_and_ui.md) — `/healthz`/
   `/readyz` and the embedded search/tail frontend.

## Routes

| Route | Method | Purpose |
| :--- | :--- | :--- |
| `/api/search` | GET | Full-text search over one or more days of logs. |
| `/api/tail` | GET | Live-streams newly-ingested lines (SSE) — see [Tail & known](04_design_api/02_tail_and_known.md). |
| `/api/known` | GET | Distinct pod/container names seen recently — feeds the tail filter dropdowns. |
| `/healthz` | GET | Liveness — see [Health & UI](04_design_api/03_health_and_ui.md). |
| `/readyz` | GET | Readiness — see [Health & UI](04_design_api/03_health_and_ui.md). |
| `/metrics` | GET | Prometheus text format — see "Metrics (v0.7.0)" below. |
| `/static/*` | GET | File server over the embedded `web/static/` assets (CSS/JS/favicon). |
| `/` | GET | Renders the `web/templates/index.html` page shell. |

## Adding a new HTTP endpoint

1. Add a handler method on `Handler` in `internal/api/handler.go`.
2. Register it in `New()` via `h.mux.HandleFunc(...)`.
3. If it needs new UI, edit `web/templates/index.html` (markup) or
   `web/static/{style.css,app.js}` (styling/behavior) directly — no
   bundler, no `npm install`.

## Metrics (v0.7.0)

`/metrics` is `promhttp.Handler()` (from `client_golang`), registered in
`api.New` alongside every other route — grepod is one HTTP server, so
Prometheus scrapes the same port as everything else (see
[k8s/README.md](../k8s/README.md#metrics) /
[helm/README.md](../helm/README.md#metrics) for how that's wired into the
Deployment). Every collector is defined once, in `internal/metrics` — a leaf
package with no dependency on `storage`, `tailer`, or `api` (they depend on
it, not the reverse, so this doesn't violate
[ARCHITECTURE.md](../ARCHITECTURE.md)'s "dependencies only point
downward/inward"). Everything registers against the default Prometheus
registry (`promauto`'s default), which also means `/metrics` gets the
`client_golang`-provided process/Go runtime metrics for free — no reason to
isolate a private registry in a single-binary app with one `/metrics`
handler.

Each RED-metric triplet is recorded at that operation's own natural
boundary, not centrally:

| Metric | Type | Recorded where |
| :--- | :--- | :--- |
| `grepod_insert_requests_total` | Counter | `storage.BatchQueue.flush`, once per `Store.InsertBatch` call. |
| `grepod_insert_errors_total` | Counter | Same, when `InsertBatch` returns an error. |
| `grepod_insert_duration_seconds` | Histogram | Same, wall time around `InsertBatch`. |
| `grepod_lines_dropped_total` | Counter | `storage.BatchQueue.recordDrop`, once per dropped line (independent of the rate-limited log warning — see [DESIGN/03](03_design_storage.md#never-flooding-on-a-full-queue-v051)). |
| `grepod_tail_streams_total` | Counter | `tailer.Manager.tailContainer`, once per `streamLogs` call (initial connect and every reconnect). |
| `grepod_tail_stream_errors_total` | Counter | Same, when `streamLogs` returns an error (the `nextBackoff` retry path). |
| `grepod_tail_subscribers` | Gauge | `cmd/server/main.go`, a `GaugeFunc` reading `storage.Broadcaster.SubscriberCount()` on every scrape — not incremented/decremented inline, so `Broadcaster` itself stays free of any Prometheus dependency. |
| `grepod_search_requests_total` | Counter | `api.Handler.handleSearch`, once per request. |
| `grepod_search_errors_total` | Counter | Same, when `Store.Search` returns an error. |
| `grepod_search_duration_seconds` | Histogram | Same, wall time around `Store.Search`. |

Every collector-holding struct (`storage.BatchQueue`, `tailer.Manager`,
`api.Handler`) accepts a `*metrics.Metrics` at construction and nil-checks
it before every use — tests that build these types directly (bypassing the
constructor, e.g. `storage`'s `&BatchQueue{in: ...}` pattern) never wire one
up, and must not panic for it.
