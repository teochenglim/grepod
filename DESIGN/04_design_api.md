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
| `/api/known` | GET | Distinct pod/container names seen recently — feeds the tail filter dropdowns. |
| `/healthz` | GET | Liveness — see below. |
| `/readyz` | GET | Readiness — see below. |
| `/static/*` | GET | File server over the embedded `web/static/` assets (CSS/JS/favicon). |
| `/` | GET | Renders the `web/templates/index.html` page shell. |

## `/api/search`

Query params:

| Param | Required | Default | Notes |
| :--- | :--- | :--- | :--- |
| `q` | no | `""` (browse mode) | FTS5 match expression, sanitized per-term (see `sanitizeMatchQuery` in [DESIGN/03](03_design_storage.md)). Empty/whitespace-only means "browse," not "search for nothing" — see "Browse mode" below. |
| `start` | no | `end` minus `DEFAULT_SEARCH_DAYS - 1` days (`YYYY-MM-DD`) | Inclusive. `400` if unparseable. |
| `end` | no | today | Inclusive. `400` if before `start`. |
| `level` | no | `""` (no filtering — the UI's "ALL" tab) | One of `FATAL`/`ERROR`/`WARN`/`INFO`/`DEBUG`/`TRACE`. Matches that level *and anything more severe* (`level=WARN` returns `WARN`, `ERROR`, and `FATAL`), not an exact match — see "Level filtering" below. Unrecognized values are treated the same as `""`. |
| `cursor` | no | `""` (first page) | Opaque, from a previous response's `next_cursor`. See "Pagination" below. |

The default window (7 days unless `DEFAULT_SEARCH_DAYS` overrides it — a
`Handler` field set from `cmd/server`, not hardcoded) is inclusive of
today: `DEFAULT_SEARCH_DAYS=7` means today plus the 6 days before it.

Response is JSON: `{query, start, end, level, count, results, next_cursor}`,
where each result is a `storage.SearchResult` (`pod`, `namespace`,
`container`, `timestamp`, `level` — best-effort detected, may be `""`, see
[DESIGN/02](02_design_tailer.md) — `snippet`, `rank`). Each page is capped
at 500 results server-side regardless of what the caller asks for;
`next_cursor` is `""` once there's nothing left to page through.

### Browse mode (v0.5.3)

`q=""` (the default — omitting it entirely, or a value that's only
whitespace) doesn't 400 and doesn't search for an empty string: it browses
every line in `[start, end]` (still narrowable by `level`), most-recent
first, no keyword needed just to see what's there. Internally this skips
FTS5's `MATCH` entirely — `bm25()`/`snippet()` are only meaningful in the
context of an active `MATCH`, so a browse-mode result's `snippet` is the
raw line (HTML-escaped, see "Snippet escaping" below, but with no
`<mark>` highlighting since nothing was searched for) and `rank` is
always `0`. `q` still works as an optional filter on top of browse mode —
type a keyword and it narrows the same view to a bm25-ranked match. See
[DESIGN/03](03_design_storage.md#store-daily-sharded-sqlite--fts5) for
the query construction and why browse mode's cursor sorts by recency
(shard, then per-shard rowid, both descending) instead of by rank.

### Snippet escaping (v0.5.3)

`snippet` is always HTML-escaped server-side before any `<mark>`/`</mark>`
highlighting is reintroduced (`storage.escapeSnippet`) — log content is
not trusted input, and the UI injects `snippet` via `innerHTML` to render
the highlighting, so returning it unescaped would let a log line
containing real markup (e.g. a raw request path logged verbatim,
containing `<img src=x onerror=...>`) execute as HTML in the browser. The
only real markup ever present in a response's `snippet` is the
highlighting grepod itself added.

### Level filtering

`level` needs a severity ordering, not an equality filter, since `level`
is stored as free text: `FATAL > ERROR > WARN > INFO > DEBUG > TRACE`
(`storage.levelOrder`). `storage.levelsAtOrAbove(minLevel)` turns a tab
selection into the set of recognized levels that qualify, which
`Store.Search` then filters on with `level IN (...)` per attached shard.
An empty or unrecognized line's `level` (`""`) is its own bucket rather
than being sorted into `TRACE` or silently dropped: it shows up under
"ALL" but never matches a specific level tab, since there's no way to
know where it'd actually rank.

### Pagination

Cursor-based, not `OFFSET` — `OFFSET` would have to walk and discard every
prior row on every attached shard on every page, which gets slow as
shards accumulate (see [DESIGN/03](03_design_storage.md#search-cross-shard-attach)'s
`UNION ALL` over per-query `ATTACH`ed shards). The cursor is a keyset over
`Search`'s actual sort order — `bm25()` rank, then shard, then the
per-shard FTS5 `rowid` as a tiebreaker — base64-encoded and opaque to the
caller; treat it as a token to echo back via `?cursor=`, not something to
construct or parse. Paging through with the returned cursor is guaranteed
gap-free and duplicate-free against a stable result set.

Errors are always JSON (`{"error": "..."}`) with an appropriate 4xx/5xx
status — see `writeJSONError`. There's no auth: the handler assumes the
Service is only reachable inside the cluster (or behind whatever Ingress
auth you layer on — see `k8s/README.md`).

## `/api/tail` (v0.4.0)

Server-Sent Events, not WebSocket — chosen because it needs no dependency
(stdlib `net/http`'s `http.Flusher` covers it) and grepod's tail is
inherently one-directional: the server pushes lines, the client's only
input is the query params it connected with. Wired into the page as the
"Tail" mode since [v0.5.0](../RELEASE/v0.5.0.md) — see "UI" below.

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

## `/api/known` (v0.5.0)

Feeds the Tail view's pod/container filter dropdowns (see "UI" below) so
a user picks from what's actually present in recent logs instead of
typing an exact pod/container name blind into `/api/tail`'s exact-match
`pod`/`container` params.

Query params: `days` (optional, default `1` — just today; `400` on a
non-positive or unparseable value). Backed by `Store.KnownPods(since)`,
which scans every shard in `[today - (days-1), today]` for distinct
`pod`/`container` values — see
[DESIGN/03](03_design_storage.md#search-cross-shard-attach) for why that
scan is cheap (same per-query `ATTACH` pattern as `Search`, just without
the FTS `MATCH`).

Response is JSON: `storage.KnownFilters` — `{pods: [...], containers:
[...]}`, both sorted and deduplicated. Independent lists, not paired
tuples: a pod's containers aren't cross-referenced against which pod they
ran in, matching `/api/tail`'s own independent `pod`/`container` filters.

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
Two top-level `<section>`s, `#searchView` and `#tailView`, sit side by
side in the markup; `app.js` toggles their `hidden` attribute rather than
swapping templates, so switching modes is instant and stateless on the
server side — there's still exactly one page shell.

`web/static/app.js` (vanilla JS, no dependencies, no build step):

**Search mode** (default):

1. Defaults the date pickers to a 7-day window (6 days ago through today),
   matching the server's own default so the UI's initial state isn't
   narrower than a bare `/api/search?q=` call, and runs an initial search
   with an empty `q` on page load — browse mode (v0.5.3): there's
   something to look at immediately, not an empty "type a keyword" prompt.
   The query box is an optional filter on top of that view, not a gate on
   it.
2. On search, calls `/api/search` with `q`/`start`/`end`/`level` and
   renders each result as one line: `[pod/container] timestamp snippet`,
   injecting the snippet's HTML directly since the `<mark>` tags are meant
   to render — safe because the server always HTML-escapes `snippet`
   before reintroducing the highlighting markup, see "Snippet escaping"
   above.
3. **Level tabs** (`ALL`/`INFO`/`WARN`/`DEBUG`/`FATAL`) re-run the same
   search with `level` set, resetting pagination. See "Level filtering"
   above for the "this level and anything more severe" semantics.
4. **Pagination**: a "Load more" button appears whenever a response comes
   back with a non-empty `next_cursor`; clicking it re-queries with
   `cursor` set and appends the new page's results to what's already
   accumulated client-side (`allResults`), rather than replacing them.
5. **Grouping** (`#groupToggle`): a client-side aggregation over
   `allResults` — not a new SQL aggregate query, since it operates on an
   already date-ranged/leveled/paginated result set. Rows are grouped by
   `pod + container + normalizeForGrouping(snippet)`, where
   `normalizeForGrouping` strips `<mark>` tags and replaces obvious
   variable tokens (UUIDs, ISO timestamps, bare numbers) with a
   placeholder so near-identical repeated lines (e.g. the same error with
   a different request ID each time) collapse into one entry. Groups
   render most-recent-first with a `×N` occurrence count.

**Tail mode**: pod/container `<select>`s populated from `/api/known` (a
free-text fallback isn't offered — the dropdown *is* the fix for "no way
to narrow without knowing the exact name to type") plus a substring `q`
filter, all passed straight through as `/api/tail`'s query params.
"Start" opens an `EventSource`; each event appends one line and, if
auto-scroll is active, scrolls the container to the bottom. Auto-scroll
turns off the moment the user scrolls away from the bottom (detected via
`scrollHeight - scrollTop - clientHeight`) — lines keep arriving and
appending underneath, they just don't yank the viewport, and a "Resume"
button appears to jump back down and re-enable it. The line buffer is
capped at 2000 DOM nodes (oldest evicted first) so a long-running tail
session doesn't grow the page unboundedly; this is separate from — and
much larger than — `Broadcaster`'s own 256-line per-subscriber channel
buffer (see [DESIGN/03](03_design_storage.md#broadcaster-live-tail-fan-out)),
which exists to bound memory on the *server* side, not the browser.
Switching back to Search mode closes the `EventSource`.

Because there's no bundler, `web.TemplatesFS`/`web.StaticFS` embed the
files verbatim — no separate frontend build/toolchain to run before
`go build`.

## Adding a new HTTP endpoint

1. Add a handler method on `Handler` in `internal/api/handler.go`.
2. Register it in `New()` via `h.mux.HandleFunc(...)`.
3. If it needs new UI, edit `web/templates/index.html` (markup) or
   `web/static/{style.css,app.js}` (styling/behavior) directly — no
   bundler, no `npm install`.
