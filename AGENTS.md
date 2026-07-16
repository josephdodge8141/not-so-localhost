# Architecture Note

## Architecture

- `ttyd` runs natively on macOS (LaunchAgent) — tmux-based terminal access via browser.
- Docker Compose stack: postgres, keycloak, oauth2-proxy (×3), caddy, terminal (for SSH), todo, homarr, cloudflared, backup, registry, pgweb.
- Caddy routes by hostname, Cloudflare tunnel is a single wildcard `*.joedodge.dev` → `caddy:80`.
- Keycloak + oauth2-proxy provide OIDC auth for `t.joedodge.dev`, `home.joedodge.dev`, and `apps.joedodge.dev`.

## Key Constraints

- No sudo, SSH Remote Login disabled, Tailscale blocked by MDM.
- Corporate DNS/FortiGuard sinkhole `joedodge.dev` on corp network — the stack is designed for personal WiFi/cellular access via Cloudflare Tunnel.
- Postgres:16-alpine has no bash — init scripts must use `#!/bin/sh`.

## Network Layout

- `edge` — tunnel-facing services (caddy, keycloak, oauth2-proxy, terminal, todo, homarr, cloudflared, backup, registry).
- `internal` — database only (postgres, keycloak, todo, backup, registry, pgweb).

## Hostname Routing (Caddyfile)

| Hostname | Target |
|---|---|
| `joedodge.dev` | hello world (no auth) |
| `auth.joedodge.dev` | Keycloak (OIDC issuer) |
| `t.joedodge.dev` | oauth2-proxy-ttyd → host.docker.internal:7681 |
| `home.joedodge.dev` | oauth2-proxy-homarr → homarr:3000 |
| `apps.joedodge.dev` | oauth2-proxy-registry → registry:7272 |
| `/todo/*` | todo app (behind t.joedodge.dev auth) |

## OIDC Flow

oauth2-proxy uses `--oidc-issuer-url=http://keycloak:8080/realms/local` for server-side calls and `--login-url=https://auth.joedodge.dev/realms/local/protocol/openid-connect/auth` for browser redirects. Keycloak runs with `KC_HOSTNAME=http://keycloak:8080` (internal) and `KC_HOSTNAME_STRICT=false` + `KC_PROXY_HEADERS=xforwarded` to accept proxied requests from Caddy.

## Keycloak Config

- Realm: `local`
- Clients: `ttyd`, `homarr`, `registry` (confidential, standard flow, redirect URIs match oauth2-proxy config)
- `registry` client config (manual Keycloak step):
  - Client ID: `registry`
  - Client protocol: openid-connect
  - Standard flow enabled
  - Valid redirect URIs: `https://apps.joedodge.dev/oauth2/callback`, `http://localhost:4182/oauth2/callback`
  - Client authentication ON
  - Client secret matches `OAUTH2_CLIENT_SECRET_REGISTRY`
- Users: `joe` (password: `password`)
- Admin: `admin` (password in `.env` as `KEYCLOAK_ADMIN_PASSWORD`)

## Secrets

All in `.env` (gitignored). Generated via `openssl rand -base64 14` or Node.js crypto. Placeholders in `.env.example`.

## Registry Service

Go app registry at `registry:7272`. Proxies registered frontend/backend apps by path prefix. Dashboard at `apps.joedodge.dev`.

| Method | Path | Description |
|---|---|---|
| GET | /api/apps | List all apps |
| GET | /api/apps?type=db | List DB apps (used by backup service) |
| POST | /api/apps | Register a new app |
| PUT | /api/apps/{id} | Update an app |
| DELETE | /api/apps/{id} | Delete an app |

Required fields for POST/PUT: `name`, `path_prefix` (starts with `/`), `port` (1-65535), `app_type` (`frontend`|`backend`|`db`). Optional: `description`, `technology`, `container_name`, `metadata` (JSON-encoded string, e.g. `"{\"db_name\":\"todo\"}"`), `device_id` (default `"local"`), `enabled` (default `true`).

Seed script: `./registry/seed.sh` — registers known apps, idempotent.

`/pgweb` is reserved (hardcoded handler for pgweb proxy).

## Testing

From personal WiFi/cellular (not corp network):
- `t.joedodge.dev` → Keycloak login → terminal
- `home.joedodge.dev` → Keycloak login → Homarr
- `auth.joedodge.dev` → Keycloak admin console
- `apps.joedodge.dev` → Keycloak login → App Registry
- `joedodge.dev` → hello world (no auth)

## Backup Service

The `backup/` service periodically pg_dumps registered databases to S3.

### Building

If behind a corporate SSL-inspecting proxy, generate the CA bundle before Docker build:
```bash
security export -t certs -f pemseq -k /Library/Keychains/System.keychain > backup/ca-bundle.pem
docker build --secret id=ca-bundle,src=backup/ca-bundle.pem -t backup ./backup
```
Without a corporate proxy, build normally (the secret mount is optional):
```bash
docker build -t backup ./backup
```

- Port: `:7273`
- Endpoints: `GET /api/backups`, `POST /api/backups/{db}/backup`, `POST /api/backups/{db}/restore`
- Hardcoded DBs: `keycloak`, `registry`
- Discovered DBs: fetched from registry API (`GET /api/apps?type=db`)
- S3 path: `s3://<bucket>/backups/<dbname>/<timestamp>.sql.gz`
- Interval: configurable via `BACKUP_INTERVAL` (default `1h`)
- `POSTGRES_ADMIN_PASSWORD` must be set for the restore endpoint — it drops and recreates the target DB as the `postgres` superuser. Restore returns 500 without it.

### S3 Bucket Setup

```bash
# Create bucket (AWS credentials must be configured)
aws s3 mb s3://not-so-localhost-backups --region us-east-1

# Verify
aws s3 ls s3://not-so-localhost-backups
```

IAM policy (minimal) for backup service credentials:
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:ListBucket"],
      "Resource": [
        "arn:aws:s3:::not-so-localhost-backups",
        "arn:aws:s3:::not-so-localhost-backups/backups/*"
      ]
    }
  ]
}
```

## To Do

- [ ] Test full auth flow from iPhone (not possible from corp network).
- [ ] Set up Keycloak HTTPS if needed (currently HTTP behind Caddy which terminates at tunnel).
