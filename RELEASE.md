# Releases

One row per version, newest first. Each links to its detail file in
`RELEASE/`.

| Version | Theme | Status | Notes |
| :--- | :--- | :--- | :--- |
| [v0.1.0](RELEASE/v0.1.0.md) | Core tail-index-search loop | In progress | Not yet tagged — see below. |

## Cutting a release

```sh
make release VERSION=0.1.0
```

This bumps `VERSION`, commits, tags `v0.1.0`, and pushes — which triggers
`.github/workflows/release.yml` (cross-platform binaries + GitHub Release +
GHCR image). See [RELEASE/v0.1.0.md](RELEASE/v0.1.0.md) for what's actually
in scope for that first tag.
