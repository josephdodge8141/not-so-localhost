# Architecture Note

## Architecture

- `ttyd` runs natively on macOS (LaunchAgent) — tmux-based terminal access via browser.
- Docker Compose stack: postgres, keycloak, oauth2-proxy, terminal (SSH), cloudflared, backup, registry.
- Traefik routes by hostname, Cloudflare tunnel is a single wildcard `*.YOUR_DOMAIN` → `traefik:80`.
- Keycloak + oauth2-proxy provide OIDC auth for `t.YOUR_DOMAIN` and `apps.YOUR_DOMAIN`.

## Key Constraints

- No sudo, SSH Remote Login disabled, Tailscale blocked by MDM.
- Corporate DNS/FortiGuard sinkhole `YOUR_DOMAIN` on corp network — the stack is designed for personal WiFi/cellular access via Cloudflare Tunnel.
- Postgres:16-alpine has no bash — init scripts must use `#!/bin/sh`.

## Network Layout

- `edge` — tunnel-facing services (traefik, keycloak, oauth2-proxy, terminal, cloudflared, backup, registry).
- `internal` — database only (postgres, keycloak, backup, registry).

## Hostname Routing (Traefik)

| Hostname | Target |
|---|---|
| `auth.YOUR_DOMAIN` | Keycloak (OIDC issuer) |
| `t.YOUR_DOMAIN` | oauth2-proxy → host.docker.internal:7681 |
| `apps.YOUR_DOMAIN` | oauth2-proxy → registry:7272 |

## OIDC Flow

oauth2-proxy uses `--oidc-issuer-url=http://keycloak:8080/realms/local` for server-side calls and `--login-url=https://auth.${DOMAIN}/realms/local/protocol/openid-connect/auth` for browser redirects. Keycloak runs with `KC_HOSTNAME=http://keycloak:8080` (internal) and `KC_HOSTNAME_STRICT=false` + `KC_PROXY_HEADERS=xforwarded` to accept proxied requests from Traefik.

## Keycloak Config

- Realm: `local`
- Clients: `ttyd`, `registry` (confidential, standard flow, redirect URIs match oauth2-proxy config)
- `registry` client config (manual Keycloak step):
  - Client ID: `registry`
  - Client protocol: openid-connect
  - Standard flow enabled
  - Valid redirect URIs: `https://apps.YOUR_DOMAIN/oauth2/callback`, `http://localhost:4182/oauth2/callback`
  - Client authentication ON
  - Client secret matches `OAUTH2_CLIENT_SECRET_REGISTRY`
- Users: `joe` (password: `password`)
- Admin: `admin` (password in `.env` as `KEYCLOAK_ADMIN_PASSWORD`)

## Secrets

All in `.env` (gitignored). Generated via `openssl rand -base64 14` or Node.js crypto. Placeholders in `.env.example`.

## Testing

From personal WiFi/cellular (not corp network):
- `t.YOUR_DOMAIN` → Keycloak login → terminal
- `auth.YOUR_DOMAIN` → Keycloak admin console
- `apps.YOUR_DOMAIN` → Keycloak login → App Registry

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
- Hardcoded DBs: `keycloak`, `registry` (keyed by name)
- Discovered DBs: fetched from registry API (`GET /internal/backup-targets`), keyed by app UUID
- S3 path: `s3://<bucket>/backups/<uuid-or-name>/<timestamp>.sql.gz`
- Interval: configurable via `BACKUP_INTERVAL` (default `1h`)
- Stale backups: apps deleted from registry have their backups pruned after `STALE_BACKUP_DAYS` (default 30). The `POST /api/backups/{db}/restore` endpoint refuses restore for stale orphaned backups and deletes them instead.
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
      "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject", "s3:ListBucket"],
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
- [ ] Set up Keycloak HTTPS if needed (currently HTTP behind Traefik which terminates at tunnel).
