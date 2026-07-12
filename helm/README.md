# grepod Helm chart

Templated equivalent of the [plain manifests](../k8s/) — same resources,
parameterized. Keep both in sync if you change one (see
[ARCHITECTURE.md](../ARCHITECTURE.md#adding-a-new-kubernetes-resource)).

## Install

```sh
helm upgrade --install grepod ./helm \
  --namespace kube-system \
  --create-namespace \
  --set namespace=kube-system
```

**The value you need to override is `namespace`** — it controls where
this release's resources (`Role`, `ServiceAccount`, `Deployment`, etc.)
get created, and must match `--namespace` above (Helm requires both; there
isn't a way to derive one from the other). You do **not** need to keep
anything else in sync: grepod always watches whichever namespace it's
actually running in — `NAMESPACE` is sourced from the pod's own metadata
via the Kubernetes Downward API (`fieldRef: metadata.namespace`) in
`templates/deployment.yaml`, not a value read from `values.yaml`. That
namespace is always exactly `.Values.namespace`, since that's where Helm
just deployed it — there's no separate value that could drift.

grepod is namespace-scoped by design (see
[DESIGN/01](../DESIGN/01_design_overview.md#non-goals)) — install one
release per namespace you want indexed, e.g.:

```sh
helm upgrade --install grepod-kube-system ./helm \
  --namespace kube-system --create-namespace --set namespace=kube-system

helm upgrade --install grepod-payments ./helm \
  --namespace payments --create-namespace --set namespace=payments
```

Point `--set image.repository=...,image.tag=...` at wherever you built or
pushed the image (`make docker-build`, or the `ghcr.io/teochenglim/grepod`
image published by `.github/workflows/release.yml`'s `docker` job on a
tagged push).

## Values reference

| Key | Default | Notes |
| :--- | :--- | :--- |
| `namespace` | `default` | **Override this.** Where this release's resources are created; must match `--namespace`. grepod then watches this same namespace automatically (Downward API) — nothing else to set. |
| `image.repository` / `image.tag` / `image.pullPolicy` | `ghcr.io/teochenglim/grepod` / `""` (falls back to `.Chart.AppVersion`) / `IfNotPresent` | Where to pull the image from. Empty `tag` tracks whatever version this chart checkout is (`Chart.yaml`'s `appVersion`, kept in sync by `make bump`/`make release`), not a floating `latest`. |
| `retentionDays` | `7` | Days of logs kept before a shard is deleted. |
| `batchSize` / `batchInterval` | `200` / `500ms` | Write-batching thresholds — see [DESIGN/03](../DESIGN/03_design_storage.md). |
| `includeInitContainers` | `false` | Also tail init containers. |
| `defaultSearchDays` | `7` | How many days back `/api/search` looks when the caller omits `start`. |
| `storageSize` / `storageClassName` | `10Gi` / `""` (cluster default) | The `ReadWriteOnce` PVC backing `/data`. |
| `service.type` / `.port` / `.targetPort` | `ClusterIP` / `80` / `8080` | |
| `serviceMonitor.enabled` | `false` | Renders a Prometheus Operator `ServiceMonitor` — see "Metrics" below. |
| `serviceMonitor.interval` / `.labels` | `30s` / `{}` | Scrape interval; extra labels for the Operator's `serviceMonitorSelector` to match. |
| `ingress.enabled` | `false` | See "Exposing it safely" below before enabling. |
| `ingress.className` / `.host` / `.annotations` | `nginx` / `grepod.example.com` / `{}` | |
| `autoscaling.enabled` | `false` | Renders an `HorizontalPodAutoscaler` — but see below, it can't actually scale. |
| `resources` | `256Mi`/`500m` limits, `128Mi`/`100m` requests | |
| `podSecurityContext.*` | `runAsNonRoot: true`, uid/gid `65532` | Matches the distroless-nonroot image user. |
| `nameOverride` / `fullnameOverride` | `""` | Override the generated resource name base (defaults to the release name). |

Copy `values.yaml` to a gitignored `values-secrets.yaml` if you ever add
secret-bearing overrides (e.g. Ingress auth), and layer it on:

```sh
helm upgrade --install grepod ./helm -f values.yaml -f values-secrets.yaml
```

## Why `replicas`/`autoscaling` are pinned to 1

grepod cannot run more than one replica: a second replica would run its
own pod-watching tailer against the same namespace (double-ingesting every
log line), and `/data` is a `ReadWriteOnce` volume only one pod can mount
read-write at a time. `templates/deployment.yaml` hardcodes `replicas: 1`
and `strategy: Recreate`. `templates/hpa.yaml` (behind `autoscaling.enabled`,
default off) hardcodes `minReplicas`/`maxReplicas: 1` regardless of any
value — it exists for template completeness, not because it does anything
useful. See
[DESIGN/03](../DESIGN/03_design_storage.md#why-not-horizontal-scale-out).

## Metrics

`/metrics` (Prometheus text format, v0.7.0 — see
[DESIGN/04](../DESIGN/04_design_api.md)) is served on the same port as
everything else, since grepod is one HTTP server, not a sidecar-per-concern
setup. `templates/deployment.yaml` always sets `prometheus.io/scrape` /
`prometheus.io/port` / `prometheus.io/path` pod annotations, which the
classic annotation-based Prometheus scrape config picks up with no chart
changes needed. If your cluster runs the Prometheus Operator instead, set
`serviceMonitor.enabled: true` to render a `ServiceMonitor` targeting the
`Service`'s `http` port — left off by default since enabling it without the
Operator's CRDs installed makes `helm install`/`upgrade` fail validation.

## Exposing it safely

grepod has no built-in authentication. If you set `ingress.enabled: true`,
put an auth layer in front via `ingress.annotations` (e.g.
`nginx.ingress.kubernetes.io/auth-basic`) or an OAuth2 proxy — don't expose
it unauthenticated on the public internet. For personal use, leave
`service.type: ClusterIP` and `kubectl port-forward` instead.

## Useful commands

```sh
make helm-lint       # helm lint helm/
make helm-template   # render locally without installing
make helm-install    # helm upgrade --install
```
