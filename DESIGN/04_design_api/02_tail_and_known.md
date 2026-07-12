# 04 — API & UI: Tail & known

See [Overview](../04_design_api.md) for the full route list.

## `/api/tail` (v0.4.0)

Server-Sent Events, not WebSocket — chosen because it needs no dependency
(stdlib `net/http`'s `http.Flusher` covers it) and grepod's tail is
inherently one-directional: the server pushes lines, the client's only
input is the query params it connected with. Wired into the page as the
"Tail" mode since [v0.5.0](../../RELEASE/v0.5.0.md) — see
[Health & UI](03_health_and_ui.md) for the frontend.

Query params (all optional, ANDed together): `pod` (exact match),
`container` (exact match), `q` (case-insensitive substring match against
the line content). Filtering happens per-connection in the handler, not
in the fan-out itself — see [DESIGN/03](../03_design_storage.md#broadcaster-live-tail-fan-out).

Each event is `data: <json>\n\n` where the JSON is `{pod, namespace,
container, timestamp, level, content}` — deliberately not
`storage.SearchResult`'s shape (no `snippet`/`rank`; those are
search-specific, meaningless for a live line that hasn't been ranked
against anything).

No replay: a client only sees lines published *after* it subscribes.
There's no buffering-before-connect the way `/api/search` can look
backward — that's what search is for.

A slow or disconnected client never blocks ingestion (see
[DESIGN/03](../03_design_storage.md#broadcaster-live-tail-fan-out)); the
handler's own loop exits via `r.Context().Done()` on client disconnect,
unsubscribing from the broadcaster.

**Interacts with [v0.8.0](../../RELEASE/v0.8.0.md)'s planned
`WriteTimeout`**: a blanket `http.Server.WriteTimeout` would kill every
`/api/tail` connection after that duration, since it's long-lived by
design. That release needs to either exempt streaming routes or use
per-request `http.ResponseController.SetWriteDeadline` instead of a
server-wide timeout — flagged here so it isn't rediscovered cold.

## `/api/known` (v0.5.0)

Feeds the Tail view's pod/container filter dropdowns (see
[Health & UI](03_health_and_ui.md)) so a user picks from what's actually
present in recent logs instead of typing an exact pod/container name
blind into `/api/tail`'s exact-match `pod`/`container` params.

Query params: `days` (optional, default `1` — just today; `400` on a
non-positive or unparseable value). Backed by `Store.KnownPods(since)`,
which scans every shard in `[today - (days-1), today]` for distinct
`pod`/`container` values — see
[DESIGN/03](../03_design_storage.md#search-cross-shard-attach) for why
that scan is cheap (same per-query `ATTACH` pattern as `Search`, just
without the FTS `MATCH`) and for the same `maxAttachedShards` cap
`Search` has (v0.6.0).

Response is JSON: `storage.KnownFilters` — `{pods: [...], containers:
[...]}`, both sorted and deduplicated. Independent lists, not paired
tuples: a pod's containers aren't cross-referenced against which pod they
ran in, matching `/api/tail`'s own independent `pod`/`container` filters.
