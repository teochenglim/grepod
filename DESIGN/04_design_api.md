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
| `/static/*` | GET | File server over the embedded `web/static/` assets (CSS/JS/favicon). |
| `/` | GET | Renders the `web/templates/index.html` page shell. |

## Adding a new HTTP endpoint

1. Add a handler method on `Handler` in `internal/api/handler.go`.
2. Register it in `New()` via `h.mux.HandleFunc(...)`.
3. If it needs new UI, edit `web/templates/index.html` (markup) or
   `web/static/{style.css,app.js}` (styling/behavior) directly — no
   bundler, no `npm install`.
