# Releases

One row per version, newest first. Each links to its detail file in
`RELEASE/`.

| Version | Theme | Status | Notes |
| :--- | :--- | :--- | :--- |
| [v1.0.0](RELEASE/v1.0.0.md) | Stabilize & commit to compatibility | Not started | |
| [v0.8.0](RELEASE/v0.8.0.md) | Read/write timeouts + SQLite write consolidation | Not started | |
| [v0.7.0](RELEASE/v0.7.0.md) | Restart-safe tailing + RED metrics | Not started | `/metrics` moved here from v0.3.0. |
| [v0.6.0](RELEASE/v0.6.0.md) | Hardening, perf pass, docs catch-up | Not started | |
| [v0.5.2](RELEASE/v0.5.2.md) | Fix permanent ingestion failure; browse logs without a keyword; fix unescaped log content in the UI | Ready to tag | Implemented, verified locally, uncommitted. |
| [v0.5.1](RELEASE/v0.5.1.md) | Fix self-tail feedback loop; pin image tags; release tooling fix | Shipped | Tagged. |
| [v0.5.0](RELEASE/v0.5.0.md) | Live tail UI + search UX + level filtering | Shipped | Tagged. Tail mode, level tabs, cursor pagination, pod/container filters, group-occurrences — see its file. |
| [v0.4.2](RELEASE/v0.4.2.md) | Release tooling fix: push HEAD before tagging | Shipped | Tagged. |
| [v0.4.1](RELEASE/v0.4.1.md) | Release tooling fix: no more auto-generated bump commits | Shipped | Tagged. Surfaced the exact gap v0.4.2 fixes — see its file. |
| [v0.4.0](RELEASE/v0.4.0.md) | Live tail backend (`/api/tail`) | Shipped | Tagged. Also fixed a missing-SIGTERM and shutdown-ordering bug — see its file. |
| [v0.3.0](RELEASE/v0.3.0.md) | Observability foundation: health, structured logging, log level | Shipped | Tagged. |
| [v0.2.1](RELEASE/v0.2.1.md) | Security workflow fixes | Shipped | Tagged. Fixed all 3 jobs that failed on v0.2.0's tag push. |
| [v0.2.0](RELEASE/v0.2.0.md) | Architecture-driven test suite | Shipped | Tagged. Also fixed 3 real bugs the tests surfaced — see its file. |
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

**Commit and push your own work first** — with a real commit message,
since that's what ends up on the tag:

```sh
git add -A && git commit -m "..." && git push origin main
```

Then:

```sh
make release VERSION=x.y.z
```

This bumps `VERSION`, `helm/Chart.yaml`'s `appVersion`, and
`k8s/20-deployment.yaml`'s image tag (all rewritten by `make bump` via
`sed`, not committed — the release pipeline itself doesn't read any of
them; `release.yml` derives everything from `github.ref_name`, the git
tag, so this is purely local convenience for `make version`/
`make docker-build`/the help banner/reproducible manifests), pushes
`HEAD`, tags whatever's now at `HEAD`, and pushes the tag. It never
creates a commit itself — pushing `HEAD` only pushes what you already
committed (the bump above ends up uncommitted; fold it into your next
commit, or its own, whenever convenient). Since `release` doesn't create
a commit, the tag lands directly on the commit you actually wrote, so
GitHub Actions' run list shows *your* message, not a generic bump
commit's.

Triggers `.github/workflows/release.yml` (cross-platform binaries +
GitHub Release + GHCR image, multi-arch since v0.4.0's CI fix). See each
version's file in `RELEASE/` for what's actually in scope. See
[RELEASE/v0.4.1](RELEASE/v0.4.1.md) and [RELEASE/v0.4.2](RELEASE/v0.4.2.md)
for how this flow settled here (four iterations to remove the auto-commit,
then one more to add `git push origin HEAD` back deliberately, in the
right order, after skipping it caused a real incident) — and
[RELEASE/v0.5.1](RELEASE/v0.5.1.md) for why it briefly regressed back to
an auto-`--amend`-and-force-push flow, and for the `helm`/`k8s` image-tag
bump added in the same fix.

### `RELEASE/vX.Y.Z.md` status line

Exactly two states, not three: `ready to tag — implemented and verified
locally, uncommitted.` while work is still in progress, `shipped, tagged
vX.Y.Z.` once the release doc itself is finished — not gated on the tag
already existing in `git tag`, since finishing the doc is the cue for the
user to run the commit/tag/push above (see
[CLAUDE.md](CLAUDE.md#release-workflow-who-does-what)). Never write an
intermediate "committed, not yet tagged/pushed" state: `make release`
bumps `VERSION`, tags whatever's at `HEAD`, and pushes the tag all in one
step, so there's no real gap between committed and tagged/pushed to
describe.
