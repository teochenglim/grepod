# 04 — API & UI

`internal/api` (package `api`, type `Handler`) is the entire HTTP surface.
It wraps a `*storage.Store` and the `web` package's two embedded
`embed.FS`s (`web.TemplatesFS`, `web.StaticFS`) behind a plain
`http.ServeMux`.

## Routes

| Route | Method | Purpose |
| :--- | :--- | :--- |
| `/api/search` | GET | Full-text search over one or more days of logs. |
| `/api/tail` | GET | Live-streams newly-ingested lines (SSE) — see below. |
| `/healthz` | GET | Liveness — see below. |
| `/readyz` | GET | Readiness — see below. |
| `/static/*` | GET | File server over the embedded `web/static/` assets (CSS/JS/favicon). |
| `/` | GET | Renders the `web/templates/index.html` page shell. |

## `/api/search`

Query params:

| Param | Required | Default | Notes |
| :--- | :--- | :--- | :--- |
| `q` | yes | — | FTS5 match expression, sanitized per-term (see `sanitizeMatchQuery` in [DESIGN/03](03_design_storage.md)). `400` if missing. |
| `start` | no | `end` minus `DEFAULT_SEARCH_DAYS - 1` days (`YYYY-MM-DD`) | Inclusive. `400` if unparseable. |
| `end` | no | today | Inclusive. `400` if before `start`. |

The default window (7 days unless `DEFAULT_SEARCH_DAYS` overrides it — a
`Handler` field set from `cmd/server`, not hardcoded) is inclusive of
today: `DEFAULT_SEARCH_DAYS=7` means today plus the 6 days before it.

Response is JSON: `{query, start, end, count, results}`, where each result
is a `storage.SearchResult` (`pod`, `namespace`, `container`, `timestamp`,
`level` — best-effort detected, may be `""`, see
[DESIGN/02](02_design_tailer.md) — `snippet` pre-highlighted with `<mark>`
tags by SQLite's `snippet()`, `rank`). Results are capped at 500
server-side regardless of what the caller asks for.

Errors are always JSON (`{"error": "..."}`) with an appropriate 4xx/5xx
status — see `writeJSONError`. There's no auth: the handler assumes the
Service is only reachable inside the cluster (or behind whatever Ingress
auth you layer on — see `k8s/README.md`).

## `/api/tail` (v0.4.0)

Server-Sent Events, not WebSocket — chosen because it needs no dependency
(stdlib `net/http`'s `http.Flusher` covers it) and grepod's tail is
inherently one-directional: the server pushes lines, the client's only
input is the query params it connected with. No UI yet ([v0.5.0](../RELEASE/v0.5.0.md)
wires it into the page); this is the backend surface.

Query params (all optional, ANDed together): `pod` (exact match),
`container` (exact match), `q` (case-insensitive substring match against
the line content). Filtering happens per-connection in the handler, not
in the fan-out itself — see [DESIGN/03](03_design_storage.md#broadcaster-live-tail-fan-out).

Each event is `data: <json>\n\n` where the JSON is `{pod, namespace,
container, timestamp, level, content}` — deliberately not
`storage.SearchResult`'s shape (no `snippet`/`rank`; those are
search-specific, meaningless for a live line that hasn't been ranked
against anything).

No replay: a client only sees lines published *after* it subscribes.
There's no buffering-before-connect the way `/api/search` can look
backward — that's what search is for.

A slow or disconnected client never blocks ingestion (see
[DESIGN/03](03_design_storage.md#broadcaster-live-tail-fan-out)); the
handler's own loop exits via `r.Context().Done()` on client disconnect,
unsubscribing from the broadcaster.

**Interacts with [v0.8.0](../RELEASE/v0.8.0.md)'s planned `WriteTimeout`**:
a blanket `http.Server.WriteTimeout` would kill every `/api/tail`
connection after that duration, since it's long-lived by design. That
release needs to either exempt streaming routes or use per-request
`http.ResponseController.SetWriteDeadline` instead of a server-wide
timeout — flagged here so it isn't rediscovered cold.

## `/healthz` + `/readyz`

- **`/healthz`** — pure liveness. Always `200` if the handler is running
  at all; no dependency checks. Kubernetes' `livenessProbe` points here.
- **`/readyz`** — `200` once `tailer.Manager.Ready()` reports the pod
  informer's initial cache sync has completed, `503` before that.
  `storage.Store`'s own readiness isn't separately checked here: `main.go`
  calls `storage.NewStore` and exits before the HTTP server ever starts
  listening if that fails, so a running `Handler` always has an opened
  store — there's no partial-storage-readiness state to report.
  Kubernetes' `readinessProbe` points here.

`Handler` takes `ready func() bool` from `New(...)` rather than importing
`tailer` directly — `cmd/server` wires `mgr.Ready` in. Keeps `api`'s only
dependency on `storage`, per [ARCHITECTURE.md](../ARCHITECTURE.md)'s
layering (`tailer` and `api` are siblings, not import each other).

## UI

`web/templates/index.html` is the page shell, parsed with `html/template`
and executed with no data (it's static markup today — the template exists
so server-rendered data can be threaded in later without restructuring).
It links `/static/style.css`, `/static/app.js`, and `/static/favicon.svg`,
which `Handler` serves straight from `web.StaticFS` via `http.FileServer`.

`web/static/app.js` (vanilla JS, no dependencies, no build step):

1. Defaults the date pickers to a 7-day window (6 days ago through today),
   matching the server's own default so the UI's initial state isn't
   narrower than a bare `/api/search?q=` call.
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
