# Architecture Note

## Architecture

- Registry state is immutable JSON snapshots in S3 plus a CAS-updated pointer.
- Every machine has a persistent UUID and required `NODE_NAME`.
- Apps are direct HTTP services assigned to one node UUID.
- Each node reconciles only its apps into local Traefik configuration.
- The enrollment broker owns Cloudflare Tunnel and DNS edit permissions.
- `apps.joedodge.dev` uses one shared tunnel with a connector on every node.
- App and terminal names are flat: `<app>--<node>.joedodge.dev` and
  `t--<node>.joedodge.dev`.
- Keycloak and local PostgreSQL run only on the global auth-owner node through
  the Compose `auth` profile.
- No Swagger UI, pgweb, Docker socket, or registry-managed sidecars exist.

## Startup

`docker compose up -d --build` first runs `node-init`.

- `NODE_NAME` is required.
- `/var/lib/nsl/node.json` is generated once in the `node_identity` volume.
- First startup requires `ENROLLMENT_BROKER_URL` and a single-use
  `NSL_ENROLLMENT_TOKEN`.
- Enrollment stores node/portal tunnel tokens and a node credential in the same
  volume with mode `0600`.
- Reusing a volume with a different `NODE_NAME` fails.
- Reusing a node name with another UUID fails at the broker/registry.

Treat `node_identity` as credential state. Never destroy it casually with
`docker compose down -v`.

## Registry Storage

S3 layout:

```text
nsl/registry/v1/current.json
nsl/registry/v1/snapshots/<revision>-<sha256>.json
```

Writes create an immutable snapshot and update `current.json` with `If-Match`.
Disjoint concurrent mutations reload and retry. Same-app stale generations and
route/name collisions return conflict. If S3 is unavailable, writes fail.

API v2:

- `GET /api/v2/node`
- `GET /api/v2/nodes`
- `GET/POST /api/v2/apps`
- `GET/PUT/DELETE /api/v2/apps/{id}`
- `GET /api/v2/auth-owner`

PUT and DELETE require the app ETag in `If-Match`.

## Application Model

One app contains a target URL and one or more routes. Route auth is:

- `browser`: attach the Keycloak/oauth2-proxy middleware.
- `upstream`: do not redirect; the target validates its own credentials.

The default hostname is `<app-slug>--<node-slug>.<domain>`. LiteLLM uses a
browser route excluding `/v1` and an upstream-authenticated `/v1` route.

NSL routes applications but does not start them. Database credentials and app
configuration never enter the global registry.

## Enrollment Broker

The Worker under `broker/` uses a SQLite Durable Object. It:

- issues 1-15 minute single-use enrollment tokens
- creates remotely managed node and portal tunnels
- returns only tunnel-scoped and node-scoped credentials
- creates exact terminal, app, portal, and auth DNS records
- serializes full tunnel configuration updates

Required Worker secrets: `CLOUDFLARE_API_TOKEN`, `BROKER_ADMIN_TOKEN`, and
`NODE_CREDENTIAL_KEY`.

## Backup

- Backup tracking is derived from S3; there is no tracker database.
- Keys are node-namespaced:
  `backups/v2/<node-uuid>/<target-id>/<timestamp>.sql.gz`.
- Keycloak is automatically included only when `KEYCLOAK_DB_PASSWORD` exists.
- Additional targets come from a node-local JSON file and environment-backed
  secrets.
- Trigger/list/restore endpoints require `BACKUP_API_TOKEN`.

## Key Constraints

- No sudo, SSH Remote Login disabled, and Tailscale blocked by MDM.
- Corporate DNS/FortiGuard blocks the public domain on the corporate network.
- Docker builders and cloudflared may require `cloudflared/ca-bundle.pem` for
  corporate TLS inspection.
- `postgres:16-alpine` init scripts use `/bin/sh`, not bash.

## Validation

```sh
cd registry && go test -race ./... && go vet ./...
cd ../node-init && go test -race ./...
cd ../backup && go test -race ./... && go vet ./...
cd ../broker && npm run typecheck && npm test && npm run deploy:dry
docker compose --env-file .env.example config --quiet
docker compose --env-file .env.example build node-init registry backup cloudflared-node
```

## Skills

- `.agents/skills/registry-api/SKILL.md` - API versioning conventions
