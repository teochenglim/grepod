# CLAUDE.md

grepod: single Go binary that tails every pod's logs in one Kubernetes
namespace via `client-go`, indexes them into daily-sharded SQLite FTS5
databases, and serves an embedded search UI. No Loki, no sidecars.
Module: `github.com/teochenglim/grepod`. Deployed as a `Deployment`
(never `DaemonSet` — it's namespace-scoped, not node-local) and **cannot
run more than one replica** — see
[DESIGN/03](DESIGN/03_design_storage.md#why-not-horizontal-scale-out)
before touching `replicas`/`autoscaling` anywhere.

## Where things go

Read [ARCHITECTURE.md](ARCHITECTURE.md) first — it has the full layering
diagram, a where-things-go table, and a data-flow walkthrough. Short version:

| Concern | Location |
| :--- | :--- |
| Pod discovery / log streaming | `internal/tailer/manager.go` |
| Write batching, SQLite/FTS5, retention | `internal/storage/` |
| HTTP API (`/api/search`) | `internal/api/handler.go` |
| Search UI markup | `web/templates/index.html` (Go `html/template`) |
| Search UI styling/behavior | `web/static/{style.css,app.js,favicon.svg}` |
| Config (env vars) | `cmd/server/main.go` (`env*` helpers) |
| Design rationale | [DESIGN.md](DESIGN.md) → `DESIGN/01`–`04` |
| Release history | [RELEASE.md](RELEASE.md) → `RELEASE/vX.Y.Z.md` |

`web` is its own package (not under `cmd/server`) because `go:embed`
patterns can't reach outside the directory of the file that declares
them — `web/web.go` owns both `TemplatesFS` and `StaticFS`.

## Build / test / run

`make` with no args prints every target. Highlights:

```sh
make build          # binary -> bin/grepod
make run            # go run ./cmd/server (needs a kubeconfig context)
make test           # go test ./... -race -cover
make docker-build   # local image
make helm-template  # render the chart without installing
make k8s-apply      # kubectl apply -f k8s/
make release VERSION=x.y.z   # bump + commit + tag + push -> triggers release CI
```

## CI/CD

Tag-driven (`v[0-9]*`), not push-to-main:

- `ci.yml` — vet/build/test, on every PR + tag push.
- `release.yml` — test gate → 6-way OS/arch binary matrix → GitHub Release
  → (parallel) GHCR image push.
- `security.yml` — Semgrep + Trivy (fs + image), on tag push + weekly.

All third-party Actions are pinned to commit SHAs (see
`.github/workflows/*.yml`); `make github-action-bump` re-pins via `pinact`.

## Deployment: two interfaces, kept in sync

`k8s/` (plain, numbered by apply order) and `helm/` (flat — chart files
live directly under `helm/`, not `helm/grepod/`) describe the *same*
resources. Changing RBAC, config, or storage in one means changing it in
the other — see their READMEs
([k8s/README.md](k8s/README.md), [helm/README.md](helm/README.md)) and
[ARCHITECTURE.md](ARCHITECTURE.md#adding-a-new-kubernetes-resource).
No `docker-compose.yaml` — Kubernetes/Helm are the only supported
deployment path (grepod's only job is talking to the Kubernetes API, so a
standalone Compose stack isn't a meaningful target).

## Conventions / deviations worth knowing

These came out of scaffolding this repo with `.claude/skills/spawn-golang.md`
and diverge from that skill's generic golden-standard template — don't
"fix" them back:

- Scripts live in `scripts/`, not `hack/`.
- The Helm chart is flat (`helm/Chart.yaml`, `helm/templates/`), not
  nested under `helm/grepod/`.
- `images/` is kept (with a `.gitkeep`) for future screenshots, even
  though nothing references it yet.
- GitHub Actions workflows follow the pinned-SHA / job-naming style of
  `~/code/servicedesk/.github/workflows`, not ad-hoc `@v4`-style tags.

## Known constraints

- One grepod instance = one namespace. Multi-namespace means multiple
  releases/installs, not a config flag.
- No built-in auth on the HTTP API or UI — see
  [k8s/README.md#exposing-it-safely](k8s/README.md#exposing-it-safely)
  before adding an Ingress.
