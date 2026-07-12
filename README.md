# grepod

Full-text search across every pod's logs in a Kubernetes namespace — no
Loki, no Alloy, no sidecars. One static Go binary tails pods directly via
the Kubernetes API, indexes lines into local SQLite FTS5 databases, and
serves a small embedded search UI.

```
kubectl logs -f my-pod         # one pod, no search, scroll forever
vs.
grepod                          # every pod in the namespace, searchable
```

## Quickstart

### Bare `go run` (needs a working kubeconfig context)

```sh
export NAMESPACE=default
go run ./cmd/server
# → http://localhost:8080
```

### Docker

```sh
make docker-build
docker run --rm -p 8080:8080 \
  -e NAMESPACE=default \
  -v ~/.kube:/kube:ro -e KUBECONFIG=/kube/config \
  -v grepod-data:/data \
  grepod:local
```

(In-cluster deployments use the pod's ServiceAccount instead of a mounted
kubeconfig — see below.)

### Kubernetes

Plain manifests, via Kustomize (deploys into the `default` namespace;
`kustomize edit set namespace <ns>` from `k8s/` to change it — grepod then
watches whichever namespace it's deployed into automatically):

```sh
kubectl apply -k k8s/
```

Helm:

```sh
helm upgrade --install grepod ./helm \
  --namespace default --create-namespace --set namespace=default
```

See [k8s/README.md](k8s/README.md) and [helm/README.md](helm/README.md)
for the full story, including why grepod is one `Deployment` per namespace
(not a `DaemonSet`, not horizontally scaled).

## Configuration

| Env var | Default | Notes |
| :--- | :--- | :--- |
| `NAMESPACE` | `default` | The namespace grepod watches. In k8s/Helm this is sourced from the pod's own metadata via the Downward API — set it directly only for bare `go run`/`docker run`. |
| `POD_NAME` | *(empty)* | Tagged onto grepod's own structured logs. Downward API-sourced in k8s/Helm; harmless if unset locally. |
| `DATA_DIR` | `/data` | Where daily SQLite shards are written. |
| `LISTEN_ADDR` | `:8080` | HTTP listen address. |
| `LOG_LEVEL` | `info` | grepod's own log verbosity: `debug`/`info`/`warn`/`error`. |
| `RETENTION_DAYS` | `7` | Shards older than this are deleted nightly at 03:00 local time. |
| `BATCH_SIZE` | `200` | Lines buffered before a write flush. |
| `BATCH_INTERVAL` | `500ms` | Max time buffered lines wait before a flush. |
| `INCLUDE_INIT_CONTAINERS` | `false` | Also tail init containers. |
| `DEFAULT_SEARCH_DAYS` | `7` | How many days back `/api/search` looks when the caller omits `start`. |

## Features

- **One namespace, every pod.** A `client-go` informer discovers pods and
  tails every container, including init containers if enabled — except
  grepod's own pod, which is never tailed (avoids a feedback loop with its
  own operational logs).
- **Crash-safe.** Fetches a crashed container's previous logs before
  resuming the live stream, so a panic's last lines are never lost.
- **Browse or search** — the UI shows every line in range by default, most
  recent first, no keyword required; typing one narrows that same view to
  a SQLite FTS5 `bm25()`-ranked, highlighted match. Either way, results
  page past the first 500 via cursor-based pagination ("Load more"), and
  can be scoped to a log level and anything more severe (`WARN` also
  surfaces `ERROR`/`FATAL`) via the level tabs.
- **Live tail.** A second UI mode streams newly-ingested lines as they
  arrive (Server-Sent Events), filterable by pod/container (picked from a
  dropdown of what's actually present, not typed blind) and a substring —
  independent of the SQLite-backed search/browse path, so a slow or
  disconnected tail client never affects ingestion.
- **Best-effort log level detection.** Each ingested line is scanned for
  a recognizable level token (`FATAL`/`ERROR`/`WARN`/`INFO`/`DEBUG`/
  `TRACE`) and stored alongside it — empty when nothing matches, never
  guessed.
- **Zero external dependencies.** No Loki, no message queue, no separate
  database — one binary, one PVC.
- **Automatic retention.** Old shards are deleted (and remaining ones
  vacuumed) on a nightly cron.
- **`/healthz` + `/readyz`**, structured JSON logs (`log/slog`) for
  grepod's own operation — see [DESIGN/04](DESIGN/04_design_api.md).

See [DESIGN.md](DESIGN.md) for how it's built and
[ARCHITECTURE.md](ARCHITECTURE.md) for where things live in the code.

## Security

- RBAC is namespace-scoped: `get/watch/list` on `pods`, `get` on
  `pods/log`. Nothing cluster-wide.
- Runs as a non-root user (uid `65532`) on a distroless base image with
  no shell.
- **grepod itself has no authentication.** Keep the Service `ClusterIP`
  and use `kubectl port-forward`, or put an auth layer (basic auth,
  OAuth2 proxy) in front of any Ingress you add — see
  [k8s/README.md](k8s/README.md#exposing-it-safely).
- Tagged releases run Semgrep SAST and Trivy filesystem + container scans
  (`.github/workflows/security.yml`), failing on CRITICAL/HIGH findings.

## Releasing

```sh
git add -A && git commit -m "..." && git push origin main   # your work first
make release VERSION=x.y.z
```

`make release` bumps `VERSION` and the `helm`/`k8s` manifests' image tags
(rewritten, uncommitted — commit them yourself whenever convenient), pushes
`HEAD`, tags `vx.y.z`, and pushes the tag — it never creates a commit
itself, so the tag always lands on your own commit message, not a generic
bump. Pushing the tag triggers the tag-driven release pipeline: tests →
cross-platform binaries (attached to a GitHub Release) → a `ghcr.io`
image. See [RELEASE.md](RELEASE.md) for what's shipped in each version
and the full mechanics.

Run `make` with no arguments for the full list of available targets.
