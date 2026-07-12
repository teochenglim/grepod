# 04 — API & UI: Health & UI

See [Overview](../04_design_api.md) for the full route list.

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
dependency on `storage`, per [ARCHITECTURE.md](../../ARCHITECTURE.md)'s
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
   with an empty `q` on page load — browse mode (v0.5.2): there's
   something to look at immediately, not an empty "type a keyword" prompt.
   The query box is an optional filter on top of that view, not a gate on
   it.
2. On search, calls `/api/search` with `q`/`start`/`end`/`level` and
   renders each result as one line: `[pod/container] timestamp snippet`,
   injecting the snippet's HTML directly since the `<mark>` tags are meant
   to render — safe because the server always HTML-escapes `snippet`
   before reintroducing the highlighting markup, see
   [Search](01_search.md#snippet-escaping-v052).
3. **Level tabs** (`ALL`/`FATAL`/`ERROR`/`WARN`/`INFO`/`DEBUG`) re-run the
   same search with `level` set, resetting pagination. See
   [Search](01_search.md#level-filtering) for the "this level and
   anything more severe" semantics.
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
filter, all passed straight through as `/api/tail`'s query params — see
[Tail & known](02_tail_and_known.md). "Start" opens an `EventSource`;
each event appends one line and, if auto-scroll is active, scrolls the
container to the bottom. Auto-scroll turns off the moment the user
scrolls away from the bottom (detected via `scrollHeight - scrollTop -
clientHeight`) — lines keep arriving and appending underneath, they just
don't yank the viewport, and a "Resume" button appears to jump back down
and re-enable it. The line buffer is capped at 2000 DOM nodes (oldest
evicted first) so a long-running tail session doesn't grow the page
unboundedly; this is separate from — and much larger than —
`Broadcaster`'s own 256-line per-subscriber channel buffer (see
[DESIGN/03](../03_design_storage.md#broadcaster-live-tail-fan-out)),
which exists to bound memory on the *server* side, not the browser.
Switching back to Search mode closes the `EventSource`.

Because there's no bundler, `web.TemplatesFS`/`web.StaticFS` embed the
files verbatim — no separate frontend build/toolchain to run before
`go build`.
