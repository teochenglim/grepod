# Releases

One row per version, newest first. Each links to its detail file in
`RELEASE/`.

| Version | Theme | Status | Notes |
| :--- | :--- | :--- | :--- |
| [v1.0.0](RELEASE/v1.0.0.md) | Stabilize & commit to compatibility | Not started | |
| [v0.8.0](RELEASE/v0.8.0.md) | Read/write timeouts + SQLite write consolidation | Not started | |
| [v0.7.0](RELEASE/v0.7.0.md) | Restart-safe tailing + RED metrics | Not started | `/metrics` moved here from v0.3.0. |
| [v0.6.0](RELEASE/v0.6.0.md) | Hardening, perf pass, docs catch-up | Not started | |
| [v0.5.0](RELEASE/v0.5.0.md) | Live tail UI + search UX + level filtering | Not started | |
| [v0.4.0](RELEASE/v0.4.0.md) | Live tail backend (`/api/tail`) | Ready to tag | Implemented, verified locally, uncommitted. Also fixed a missing-SIGTERM and shutdown-ordering bug — see below. |
| [v0.3.0](RELEASE/v0.3.0.md) | Observability foundation: health, structured logging, log level | Ready to tag | Implemented, verified locally, uncommitted. |
| [v0.2.1](RELEASE/v0.2.1.md) | Security workflow fixes | Ready to tag | Committed (`f0579d4`), fixes all 3 jobs that failed on v0.2.0's tag push. |
| [v0.2.0](RELEASE/v0.2.0.md) | Architecture-driven test suite | Shipped | Tagged. Also fixed 3 real bugs the tests surfaced — see below. |
| [v0.1.0](RELEASE/v0.1.0.md) | Core tail-index-search loop | Shipped | Tagged and pushed. |

## Roadmap to 1.0.0

A sequence of small releases, each landed and tagged on its own rather
than batched into one big jump. Scope was picked deliberately:

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
