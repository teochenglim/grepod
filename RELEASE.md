# Releases

One row per version, newest first. Each links to its detail file in
`RELEASE/`.

| Version | Theme | Status | Notes |
| :--- | :--- | :--- | :--- |
| [v1.0.0](RELEASE/v1.0.0.md) | Stabilize & commit to compatibility | Not started | |
| [v0.8.0](RELEASE/v0.8.0.md) | Read/write timeouts (HTTP + DB context) | Not started | |
| [v0.7.0](RELEASE/v0.7.0.md) | Restart-safe tailing (no re-ingest on restart) | Not started | |
| [v0.6.0](RELEASE/v0.6.0.md) | Hardening, perf pass, docs catch-up | Not started | |
| [v0.5.0](RELEASE/v0.5.0.md) | Live tail UI + search UX | Not started | |
| [v0.4.0](RELEASE/v0.4.0.md) | Live tail backend (`/api/tail`) | Not started | |
| [v0.3.0](RELEASE/v0.3.0.md) | Observability (health, metrics, logging) | Not started | |
| [v0.2.0](RELEASE/v0.2.0.md) | Architecture-driven test suite | Shipped | Tagged. Also fixed 3 real bugs the tests surfaced — see below. |
| [v0.1.0](RELEASE/v0.1.0.md) | Core tail-index-search loop | Shipped | Tagged and pushed. |

## Roadmap to 1.0.0

Eight releases, each small enough to land and tag on its own. Scope was
picked deliberately:

- **In scope for 1.0:** tests, observability, live tail/streaming.
- **Explicitly deferred past 1.0:** built-in auth (grepod stays
  "put a proxy in front" — see
  [k8s/README.md#exposing-it-safely](../k8s/README.md#exposing-it-safely))
  and multi-namespace support (grepod stays one release per namespace by
  design — see
  [DESIGN/01](DESIGN/01_design_overview.md#non-goals)). Both were
  considered and consciously left out, not forgotten.
- Tests are scoped to validate the behaviors each `DESIGN/0X` doc already
  documents (reconciliation, sharding/retention, search ranking, etc.),
  not to hit a coverage percentage.

## Cutting a release

```sh
make release VERSION=x.y.z
```

Bumps `VERSION`, commits, tags `vx.y.z`, and pushes — which triggers
`.github/workflows/release.yml` (cross-platform binaries + GitHub Release +
GHCR image). See each version's file in `RELEASE/` for what's actually in
scope.
