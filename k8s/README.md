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
| `10-configmap.yaml` | Non-secret config (namespace to watch, retention, batching). |
| `11-secret.example.yaml` | Placeholder — grepod needs no secrets today. Copy to `11-secret.yaml` (gitignored) if you add any (e.g. Ingress auth credentials). |
| `12-serviceaccount.yaml` | Identity the pod runs as. |
| `13-role.yaml` | Namespace-scoped `Role`/`RoleBinding`: `get/watch/list` on `pods`, `get` on `pods/log`. Nothing cluster-wide. |
| `14-pvc.yaml` | 10Gi `ReadWriteOnce` volume for the SQLite shards. |
| `20-deployment.yaml` | The workload. Edit `image:` before applying. |
| `30-service.yaml` | `ClusterIP` on port 80 → container port 8080. |
| `31-ingress.yaml` | Optional. Edit `host:` and add auth before exposing outside the cluster — see below. |
| `32-hpa.yaml` | Optional, capped at 1 replica (see above). |

## Applying

```sh
# Edit metadata.namespace in every file (default: "default") and NAMESPACE
# in 10-configmap.yaml to the namespace you want grepod to watch, then:
kubectl apply -f k8s/
```

Or via the Makefile: `make k8s-apply` / `make k8s-delete`.

## Exposing it safely

grepod has no built-in authentication — the search UI and `/api/search`
are wide open to anything that can reach the Service. Keep it `ClusterIP`
and `kubectl port-forward` for personal use, or if you apply
`31-ingress.yaml`, put an auth layer in front of it (e.g. an
`nginx.ingress.kubernetes.io/auth-basic` annotation, or an OAuth2 proxy) —
don't expose it unauthenticated on the public internet.
