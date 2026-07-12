# Plain Kubernetes manifests

These are the un-templated equivalent of the [Helm chart](../helm/) — same
resources, hand-edited instead of parameterized. Use whichever fits your
workflow; keep both in sync if you change one (see
[ARCHITECTURE.md](../ARCHITECTURE.md#adding-a-new-kubernetes-resource)).

## Why `Deployment`, not `DaemonSet`

grepod watches *one namespace*, not per-node state — there's nothing
node-local about it (no hostPath log files, no per-node metrics). A
`Deployment` with `replicas: 1` is the right shape: one long-lived instance,
backed by a `PersistentVolumeClaim` for its SQLite shards.

**It cannot run more than one replica.** Every replica would run its own
pod-watching tailer against the same namespace (double-ingesting every log
line), and `/data` is a `ReadWriteOnce` volume that only one pod can mount
read-write at a time. `20-deployment.yaml` pins `replicas: 1` and
`strategy: Recreate` (so a rollout fully stops the old pod, freeing the PVC,
before starting the new one — `RollingUpdate`'s overlap would leave both
pods trying to mount the same RWO volume). `32-hpa.yaml` is included for
completeness but pins `minReplicas`/`maxReplicas` to 1 for the same reason.

## Files, in apply order

| File | What |
| :--- | :--- |
| `10-configmap.yaml` | Non-secret config (retention, batching, default search window). Namespace is *not* here — see below. |
| `11-secret.example.yaml` | Placeholder — grepod needs no secrets today. Copy to `11-secret.yaml` (gitignored) if you add any (e.g. Ingress auth credentials). |
| `12-serviceaccount.yaml` | Identity the pod runs as. |
| `13-role.yaml` | Namespace-scoped `Role`/`RoleBinding`: `get/watch/list` on `pods`, `get` on `pods/log`. Nothing cluster-wide. |
| `14-pvc.yaml` | 10Gi `ReadWriteOnce` volume for the SQLite shards. |
| `20-deployment.yaml` | The workload. Edit `image:` before applying. |
| `30-service.yaml` | `ClusterIP` on port 80 → container port 8080. |
| `31-ingress.yaml` | Optional. Edit `host:` and add auth before exposing outside the cluster — see below. |
| `32-hpa.yaml` | Optional, capped at 1 replica (see above). |

## Applying

grepod always watches whichever namespace it's deployed into — `NAMESPACE`
is sourced from the pod's own metadata via the Kubernetes Downward API in
`20-deployment.yaml` (`fieldRef: metadata.namespace`), not a config value
you set. So the *only* thing to decide is which namespace to deploy into,
and `k8s/kustomization.yaml` makes that a one-line change instead of
hand-editing `metadata.namespace` across every file:

```sh
# Deploy into the default namespace baked into kustomization.yaml:
kubectl apply -k k8s/

# Or target a different namespace:
(cd k8s && kustomize edit set namespace payments)
kubectl apply -k k8s/
```

Also update `image:` in `20-deployment.yaml` before applying — it's not
templated. `make k8s-apply`/`make k8s-delete` run the same `-k` commands.

Plain `kubectl apply -f k8s/*.yaml` still works if you don't want
Kustomize — just hand-edit `metadata.namespace` in every file yourself
first.

Two files are deliberately **not** in `kustomization.yaml`'s `resources:`
(add them yourself if you want them): `11-secret.example.yaml` (a
template, not meant to be applied as-is) and `31-ingress.yaml`/
`32-hpa.yaml` (both opt-in — see "Exposing it safely" below and the
replica note above).

## Exposing it safely

grepod has no built-in authentication — the search UI and `/api/search`
are wide open to anything that can reach the Service. Keep it `ClusterIP`
and `kubectl port-forward` for personal use, or if you apply
`31-ingress.yaml`, put an auth layer in front of it (e.g. an
`nginx.ingress.kubernetes.io/auth-basic` annotation, or an OAuth2 proxy) —
don't expose it unauthenticated on the public internet.
