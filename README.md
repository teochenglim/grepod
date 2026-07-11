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
  grepod:latest
```

(In-cluster deployments use the pod's ServiceAccount instead of a mounted
kubeconfig — see below.)

### Kubernetes

Plain manifests:

```sh
kubectl apply -f k8s/    # edit metadata.namespace + NAMESPACE first — see k8s/README.md
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
| `NAMESPACE` | `default` | The namespace grepod watches. |
| `DATA_DIR` | `/data` | Where daily SQLite shards are written. |
| `LISTEN_ADDR` | `:8080` | HTTP listen address. |
| `RETENTION_DAYS` | `7` | Shards older than this are deleted nightly at 03:00 local time. |
| `BATCH_SIZE` | `200` | Lines buffered before a write flush. |
| `BATCH_INTERVAL` | `500ms` | Max time buffered lines wait before a flush. |
| `INCLUDE_INIT_CONTAINERS` | `false` | Also tail init containers. |

## Features

- **One namespace, every pod.** A `client-go` informer discovers pods and
  tails every container, including init containers if enabled.
- **Crash-safe.** Fetches a crashed container's previous logs before
  resuming the live stream, so a panic's last lines are never lost.
- **Full-text search**, not just `grep` — SQLite FTS5 with `bm25()`
  ranking and highlighted snippets, across a date range.
- **Zero external dependencies.** No Loki, no message queue, no separate
  database — one binary, one PVC.
- **Automatic retention.** Old shards are deleted (and remaining ones
  vacuumed) on a nightly cron.

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
make release VERSION=x.y.z
```

Bumps `VERSION`, commits, tags `vx.y.z`, and pushes — which triggers the
tag-driven release pipeline: tests → cross-platform binaries (attached to
a GitHub Release) → a `ghcr.io` image. See [RELEASE.md](RELEASE.md) for
what's shipped in each version.

Run `make` with no arguments for the full list of available targets.
