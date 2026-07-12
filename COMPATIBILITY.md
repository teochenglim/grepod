# Compatibility

As of `v1.0.0`, grepod follows [SemVer](https://semver.org/): a breaking
change to anything listed below bumps the major version. Everything not
listed here (internal package APIs under `internal/`, SQLite shard file
layout, log message text/fields, `go.mod`'s dependency versions) is an
implementation detail and can change in any release — only `internal/`'s
own package boundaries matter for grepod's own maintainability, not for
callers, since Go's `internal/` visibility rules already prevent anything
outside this module from importing them.

## Env vars

Every variable in [README.md](README.md#configuration)'s configuration
table: `NAMESPACE`, `POD_NAME`, `DATA_DIR`, `LISTEN_ADDR`, `LOG_LEVEL`,
`RETENTION_DAYS`, `BATCH_SIZE`, `BATCH_INTERVAL`, `INSERT_TIMEOUT`,
`INCLUDE_INIT_CONTAINERS`, `DEFAULT_SEARCH_DAYS`, `HTTP_READ_TIMEOUT`,
`HTTP_WRITE_TIMEOUT`, `HTTP_IDLE_TIMEOUT`. Names, defaults, and value
formats (durations as Go `time.ParseDuration` strings, e.g. `"15s"`) are
stable. A new env var may be added in a minor release; an existing one
won't be renamed, removed, or have its default changed in a way that
alters existing behavior, without a major bump.

## HTTP API

Every route in [DESIGN/04](DESIGN/04_design_api.md)'s route table:

- **`/api/search`** — query params (`q`, `start`, `end`, `level`, `pod`
  since v1.1.0, `cursor`) and the JSON response shape (`{query, start,
  end, level, pod, count, results, next_cursor}`, each `results[]` entry
  a `{pod, namespace, container, timestamp, level, snippet, rank}`) are
  stable. See [DESIGN/04/01_search](DESIGN/04_design_api/01_search.md).
- **`/api/tail`** — query params (`pod`, `container`, `q`) and the SSE
  event shape (`data: {pod, namespace, container, timestamp, level,
  content}\n\n`) are stable. See
  [DESIGN/04/02_tail_and_known](DESIGN/04_design_api/02_tail_and_known.md#apitail-v040).
- **`/api/known`** — response shape (`{pods, containers}`, both string
  arrays) is stable. See
  [DESIGN/04/02_tail_and_known](DESIGN/04_design_api/02_tail_and_known.md).
- **`/healthz`, `/readyz`** — status codes (200/503) are stable; response
  body text is not (never machine-parsed by design). See
  [DESIGN/04/03_health_and_ui](DESIGN/04_design_api/03_health_and_ui.md).
- **`/metrics`** — every metric name and type listed in
  [DESIGN/04](DESIGN/04_design_api.md#metrics-v070) is stable; label sets
  and new metrics may be added in a minor release.

A new query param or response field may be added (additive) in a minor
release; an existing one won't be renamed, removed, or have its meaning
changed without a major bump.

## Helm `values.yaml`

Every key in [helm/README.md](helm/README.md#values-reference)'s values
table is stable: `namespace`, `image.*`, `retentionDays`, `batchSize`,
`batchInterval`, `insertTimeout`, `includeInitContainers`,
`defaultSearchDays`, `httpReadTimeout`, `httpWriteTimeout`,
`httpIdleTimeout`, `storageSize`, `storageClassName`, `service.*`,
`serviceMonitor.*`, `ingress.*`, `autoscaling.enabled`, `resources`,
`podSecurityContext.*`, `nameOverride`, `fullnameOverride`. Same rule: new
keys (additive) can land in a minor release; an existing key's meaning or
default won't change without a major bump. The plain [k8s/](k8s/)
manifests mirror these same values one-for-one (see
[k8s/README.md](k8s/README.md)), so the same stability applies there.

## What's explicitly out of scope (not deferred by accident)

Both considered for 1.0 and deliberately left out — worth re-litigating
for a 2.0 if real usage asks for them, not before:

- **Built-in authentication.** grepod stays "put a proxy in front" — see
  [k8s/README.md#exposing-it-safely](k8s/README.md#exposing-it-safely).
- **Multi-namespace support.** grepod stays one release per namespace by
  design — see
  [DESIGN/01](DESIGN/01_design_overview.md#non-goals).

## Security posture at 1.0.0

Re-run ahead of tagging (see `.github/workflows/security.yml`, which runs
the same three scans on every tag push): Semgrep (`p/golang`,
`p/sql-injection`, `p/secrets`, `p/owasp-top-ten`), Trivy filesystem scan,
and Trivy image scan against the actual `Dockerfile` build — all three
clean, zero CRITICAL/HIGH findings. The RBAC `Role` (both
`k8s/13-role.yaml` and `helm/templates/role.yaml`) grants exactly
`get`/`watch`/`list` on `pods` and `get` on `pods/log`, namespace-scoped,
nothing cluster-wide — confirmed identical between the two.
