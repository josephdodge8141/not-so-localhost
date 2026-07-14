# Architecture Note

## Architecture

- `ttyd` runs natively on macOS (LaunchAgent) — tmux-based terminal access via browser.
- Docker Compose stack: postgres, keycloak, oauth2-proxy (×2), caddy, terminal (for SSH), todo, homarr, cloudflared.
- Caddy routes by hostname, Cloudflare tunnel is a single wildcard `*.joedodge.dev` → `caddy:80`.
- Keycloak + oauth2-proxy provide OIDC auth for `t.joedodge.dev` and `home.joedodge.dev`.

## Key Constraints

- No sudo, SSH Remote Login disabled, Tailscale blocked by MDM.
- Corporate DNS/FortiGuard sinkhole `joedodge.dev` on corp network — the stack is designed for personal WiFi/cellular access via Cloudflare Tunnel.
- Postgres:16-alpine has no bash — init scripts must use `#!/bin/sh`.

## Network Layout

- `edge` — tunnel-facing services (caddy, keycloak, oauth2-proxy, terminal, todo, homarr, cloudflared).
- `internal` — database only (postgres, keycloak, todo).

## Hostname Routing (Caddyfile)

| Hostname | Target |
|---|---|
| `joedodge.dev` | hello world (no auth) |
| `auth.joedodge.dev` | Keycloak (OIDC issuer) |
| `t.joedodge.dev` | oauth2-proxy-ttyd → host.docker.internal:7681 |
| `home.joedodge.dev` | oauth2-proxy-homarr → homarr:3000 |
| `/todo/*` | todo app (behind t.joedodge.dev auth) |

## OIDC Flow

oauth2-proxy uses `--oidc-issuer-url=http://keycloak:8080/realms/local` for server-side calls and `--login-url=https://auth.joedodge.dev/realms/local/protocol/openid-connect/auth` for browser redirects. Keycloak runs with `KC_HOSTNAME=http://keycloak:8080` (internal) and `KC_HOSTNAME_STRICT=false` + `KC_PROXY_HEADERS=xforwarded` to accept proxied requests from Caddy.

## Keycloak Config

- Realm: `local`
- Clients: `ttyd`, `homarr` (confidential, standard flow, redirect URIs match oauth2-proxy config)
- Users: `joe` (password: `password`)
- Admin: `admin` (password in `.env` as `KEYCLOAK_ADMIN_PASSWORD`)

## Secrets

All in `.env` (gitignored). Generated via `openssl rand -base64 14` or Node.js crypto. Placeholders in `.env.example`.

## Testing

From personal WiFi/cellular (not corp network):
- `t.joedodge.dev` → Keycloak login → terminal
- `home.joedodge.dev` → Keycloak login → Homarr
- `auth.joedodge.dev` → Keycloak admin console
- `joedodge.dev` → hello world (no auth)

## To Do

- [ ] Test full auth flow from iPhone (not possible from corp network).
- [ ] Set up Keycloak HTTPS if needed (currently HTTP behind Caddy which terminates at tunnel).
