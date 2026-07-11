# 04 — API & UI

`internal/api` (package `api`, type `Handler`) is the entire HTTP surface.
It wraps a `*storage.Store` and the `web` package's two embedded
`embed.FS`s (`web.TemplatesFS`, `web.StaticFS`) behind a plain
`http.ServeMux`.

## Routes

| Route | Method | Purpose |
| :--- | :--- | :--- |
| `/api/search` | GET | Full-text search over one or more days of logs. |
| `/static/*` | GET | File server over the embedded `web/static/` assets (CSS/JS/favicon). |
| `/` | GET | Renders the `web/templates/index.html` page shell. |

## `/api/search`

Query params:

| Param | Required | Default | Notes |
| :--- | :--- | :--- | :--- |
| `q` | yes | — | FTS5 match expression. `400` if missing. |
| `start` | no | today (`YYYY-MM-DD`) | Inclusive. `400` if unparseable. |
| `end` | no | today | Inclusive. `400` if before `start`. |

Response is JSON: `{query, start, end, count, results}`, where each result
is a `storage.SearchResult` (`pod`, `namespace`, `container`, `timestamp`,
`snippet` — pre-highlighted with `<mark>` tags by SQLite's `snippet()`,
`rank`). Results are capped at 500 server-side regardless of what the
caller asks for.

Errors are always JSON (`{"error": "..."}`) with an appropriate 4xx/5xx
status — see `writeJSONError`. There's no auth: the handler assumes the
Service is only reachable inside the cluster (or behind whatever Ingress
auth you layer on — see `k8s/README.md`).

## UI

`web/templates/index.html` is the page shell, parsed with `html/template`
and executed with no data (it's static markup today — the template exists
so server-rendered data can be threaded in later without restructuring).
It links `/static/style.css`, `/static/app.js`, and `/static/favicon.svg`,
which `Handler` serves straight from `web.StaticFS` via `http.FileServer`.

`web/static/app.js` (vanilla JS, no dependencies, no build step):

1. Defaults both date pickers to today.
2. On search, calls `/api/search` with `q`/`start`/`end` and renders each
   result as one line: `[pod/container] timestamp snippet`, injecting the
   snippet's HTML directly since the `<mark>` tags are meant to render.

Because there's no bundler, `web.TemplatesFS`/`web.StaticFS` embed the
files verbatim — no separate frontend build/toolchain to run before
`go build`.

## Adding a new HTTP endpoint

1. Add a handler method on `Handler` in `internal/api/handler.go`.
2. Register it in `New()` via `h.mux.HandleFunc(...)`.
3. If it needs new UI, edit `web/templates/index.html` (markup) or
   `web/static/{style.css,app.js}` (styling/behavior) directly — no
   bundler, no `npm install`.
