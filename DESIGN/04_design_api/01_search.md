# 04 — API & UI: Search

See [Overview](../04_design_api.md) for the full route list.

## `/api/search`

Query params:

| Param | Required | Default | Notes |
| :--- | :--- | :--- | :--- |
| `q` | no | `""` (browse mode) | FTS5 match expression, sanitized per-term (see `sanitizeMatchQuery` in [DESIGN/03](../03_design_storage.md)). Empty/whitespace-only means "browse," not "search for nothing" — see "Browse mode" below. |
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
[DESIGN/02](../02_design_tailer.md) — `snippet`, `rank`). Each page is
capped at 500 results server-side regardless of what the caller asks for;
`next_cursor` is `""` once there's nothing left to page through.

## Browse mode (v0.5.2)

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
[DESIGN/03](../03_design_storage.md#store-daily-sharded-sqlite--fts5) for
the query construction and why browse mode's cursor sorts by recency
(shard, then per-shard rowid, both descending) instead of by rank.

## Snippet escaping (v0.5.2)

`snippet` is always HTML-escaped server-side before any `<mark>`/`</mark>`
highlighting is reintroduced (`storage.escapeSnippet`) — log content is
not trusted input, and the UI injects `snippet` via `innerHTML` to render
the highlighting, so returning it unescaped would let a log line
containing real markup (e.g. a raw request path logged verbatim,
containing `<img src=x onerror=...>`) execute as HTML in the browser. The
only real markup ever present in a response's `snippet` is the
highlighting grepod itself added.

## Level filtering

`level` needs a severity ordering, not an equality filter, since `level`
is stored as free text: `FATAL > ERROR > WARN > INFO > DEBUG > TRACE`
(`storage.levelOrder`). `storage.levelsAtOrAbove(minLevel)` turns a tab
selection into the set of recognized levels that qualify, which
`Store.Search` then filters on with `level IN (...)` per attached shard.
An empty or unrecognized line's `level` (`""`) is its own bucket rather
than being sorted into `TRACE` or silently dropped: it shows up under
"ALL" but never matches a specific level tab, since there's no way to
know where it'd actually rank.

## Pagination

Cursor-based, not `OFFSET` — `OFFSET` would have to walk and discard every
prior row on every attached shard on every page, which gets slow as
shards accumulate (see [DESIGN/03](../03_design_storage.md#search-cross-shard-attach)'s
`UNION ALL` over per-query `ATTACH`ed shards). The cursor is a keyset over
`Search`'s actual sort order — `bm25()` rank, then shard, then the
per-shard FTS5 `rowid` as a tiebreaker — base64-encoded and opaque to the
caller; treat it as a token to echo back via `?cursor=`, not something to
construct or parse. Paging through with the returned cursor is guaranteed
gap-free and duplicate-free against a stable result set. Also caps at
[DESIGN/03](../03_design_storage.md#store-daily-sharded-sqlite--fts5)'s
`maxAttachedShards` — a date range needing more shards than a single
SQLite connection can attach keeps only the most recent ones (v0.6.0).

Errors are always JSON (`{"error": "..."}`) with an appropriate 4xx/5xx
status — see `writeJSONError`. There's no auth: the handler assumes the
Service is only reachable inside the cluster (or behind whatever Ingress
auth you layer on — see `k8s/README.md`).
