# Registry API versioning

The registry HTTP API at `:7272` follows path-based versioning for stable
contracts between the server and the `nsl` CLI.

## Route layout

| Prefix | Purpose | Stability |
|--------|---------|-----------|
| `/api/v2/` | Active distributed direct-service API used by `nsl` | Stable |
| `/api/v1/` | Legacy single-route adapter | Compatibility only |
| `/api/` | Legacy unversioned adapter | Compatibility only |

## Version endpoint

`GET /api/v2/version` returns `{"version":"..."}`.

The version is embedded at build time via `-ldflags="-X main.version=$VERSION"`.
When built with `docker compose`, the `VERSION` build arg controls this
(default `"dev"`).

The current `nsl` CLI calls v2 directly.

## When to bump the API version

- `/api/v2/` → `/api/v3/` when an existing endpoint changes its request or
  response shape in a backward-incompatible way.
- Adding new fields to responses (not removing/renaming existing ones) does
  NOT require a bump.
- Adding new endpoints under `/api/v1/` does NOT require a bump.

## How to add a new version

1. Add new handlers under the new prefix in `registry/cmd/main.go`.
2. Update the `nsl` CLI's `apiPath` constant in `client.go`.
3. Tag a new release of `nsl` matching the registry release.
4. Keep the old prefix routes alive during the migration window.

## Git tagging

Tag registry releases with `vMAJOR.MINOR.PATCH` (e.g. `v1.0.0`) and rebuild
with `REGISTRY_VERSION=v1.0.0 docker compose up -d --build registry`.

The `nsl` CLI is versioned independently with matching semver tags in its own
repo (`github.com/josephdodge8141/nsl`).
