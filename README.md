# Not-So-Localhost

Local services accessible anywhere via Cloudflare Tunnel, with Keycloak authentication.

## Prerequisites

- [ttyd](https://github.com/tsl0922/ttyd) — `brew install ttyd && brew services start ttyd`
- Docker + Docker Compose
- Cloudflare Tunnel credentials in `cloudflared/`
- Copy `.env.example` to `.env` and fill in secrets

## Quick Start

```bash
docker compose up -d
```

ttyd must be running separately (`brew services start ttyd`) for terminal access.

## Hostnames

| Hostname | Service | Auth |
|----------|---------|------|
| `joedodge.dev` | Hello World | No |
| `home.joedodge.dev` | Homarr dashboard | Yes (Keycloak) |
| `t.joedodge.dev` | ttyd (Mac shell) | Yes (Keycloak) |
| `apps.joedodge.dev` | App Registry | Yes (Keycloak) |
| `auth.joedodge.dev` | Keycloak admin | N/A |

## Architecture

```
Cloudflare Tunnel → Caddy:80
  ├── joedodge.dev      → respond "Hello World!"
  ├── auth.joedodge.dev → keycloak:8080
  ├── t.joedodge.dev    → oauth2-proxy-ttyd:4180   → ttyd (localhost:7681)
  ├── home.joedodge.dev → oauth2-proxy-homarr:4181 → homarr:3000
  ├── apps.joedodge.dev → oauth2-proxy-registry:4182 → registry:7272
  └── /todo/*           → todo:3000
```

ttyd runs natively on macOS (not in Docker) because macOS disables SSH Remote Login.

## Adding a New Service

Add to `docker-compose.yml`, add a route to `Caddyfile`. No tunnel or DNS changes needed — `*.joedodge.dev` wildcard handles all subdomains.
