# CLAUDE.md

grepod: single Go binary that tails every pod's logs in one Kubernetes
namespace via `client-go`, indexes them into daily-sharded SQLite FTS5
databases, and serves an embedded search UI. Namespace-scoped by design —
never `DaemonSet`, never more than one replica. Module:
`github.com/teochenglim/grepod`.

## Read these, in order

1. [ARCHITECTURE.md](ARCHITECTURE.md) — layering, where-things-go table,
   data-flow walkthrough, "adding a new X" checklists.
2. [DESIGN.md](DESIGN.md) → `DESIGN/01`–`04` — why each subsystem works
   the way it does, including non-goals and the no-replicas constraint.
3. [RELEASE.md](RELEASE.md) → `RELEASE/vX.Y.Z.md` — what shipped, what's
   planned, and every bug a version's own work surfaced. This is where
   version-specific history and rationale live — not here.
4. [k8s/README.md](k8s/README.md) / [helm/README.md](helm/README.md) —
   deployment, kept in sync as two interfaces to the same decisions.

## Where things go

| Concern | Location |
| :--- | :--- |
| Pod discovery / log streaming | `internal/tailer/manager.go` |
| Write batching, SQLite/FTS5, retention | `internal/storage/` |
| HTTP API (`/api/search`) | `internal/api/handler.go` |
| Search UI | `web/templates/index.html` + `web/static/{style.css,app.js,favicon.svg}` |
| Config (env vars) | `cmd/server/main.go` (`env*` helpers) |
| CI/CD | `.github/workflows/*.yml` (tag-driven; SHA-pinned actions) |

`web` is its own package (not under `cmd/server`) because `go:embed`
patterns can't reach outside the directory of the file that declares them.

## Commands

`make` with no args prints every target. `make release VERSION=x.y.z`
triggers the tag-driven release CI — see [RELEASE.md](RELEASE.md) before using it.

## Conventions / deviations from the golden template

Came out of scaffolding via `.claude/skills/spawn-golang.md` — don't
"fix" these back:

- `scripts/`, not `hack/`.
- Helm chart is flat (`helm/Chart.yaml`, `helm/templates/`), not nested
  under `helm/grepod/`.
- No `docker-compose.yaml` — Kubernetes/Helm are the only deployment path.
- GitHub Actions follow the pinned-SHA style of
  `~/code/servicedesk/.github/workflows`, not ad-hoc `@v4` tags.

## Working conventions

- Run tests before writing or updating docs — verify behavior, then document it, not the reverse.

### Release workflow: who does what

Implementation and the actual release are a hard split, always in this
order:

1. **Claude**: implements the release's scope, runs tests/vet/build,
   writes/updates the matching `RELEASE/vX.Y.Z.md` (status line included —
   see [RELEASE.md](RELEASE.md) for the two states a doc moves through),
   updates any other docs the change touches, and ends the turn with a
   one-line suggested commit message. Proposing only — never runs
   `git add`/`commit`/`tag`/`push`/`make release` itself, and never
   without an explicit, standalone instruction for that exact action in
   the moment. Release-flavored phrasing ("cut vX.Y.Z," "ship it") is not
   authorization to commit — verify locally, leave changes uncommitted,
   say what's ready.
2. **User**: reviews, then does `git add` / `git commit` /
   `make release VERSION=x.y.z` manually, immediately after Claude
   finishes step 1 — not a separate later session.
