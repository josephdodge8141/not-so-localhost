# Not-So-Localhost

Local services accessible anywhere via Cloudflare Tunnel, with Keycloak authentication and Traefik routing.

## Prerequisites

- Docker + Docker Compose
- [ttyd](https://github.com/tsl0922/ttyd) on macOS — `brew install ttyd && brew services start ttyd`
- Cloudflare Tunnel login (`cloudflared tunnel login`) and credentials in `cloudflared/`
- Copy `.env.example` to `.env` in each service directory and fill in secrets
- AWS credentials for S3 backup (`backup/.env`)

## Quick Start

```bash
docker compose up -d
```

ttyd must be running separately on the macOS host (`brew services start ttyd`) for web terminal access at `t.your-domain.example.com`.

## Architecture

```
Cloudflare Tunnel → Traefik:80
  ├── auth.your-domain.example.com  → keycloak:8080
  ├── t.your-domain.example.com     → oauth2-proxy → host.docker.internal:7681 (ttyd)
  ├── apps.your-domain.example.com  → oauth2-proxy → registry:7272
  └── *.your-domain.example.com     → oauth2-proxy → registry:7272 (catchall)
```

- Cloudflare Tunnel terminates TLS and forwards to Traefik on port 80.
- Traefik uses the file provider with dynamic config in `traefik/dynamic/`.
- oauth2-proxy provides forward-auth middleware; protected routes authenticate via Keycloak.
- Wildcard DNS `*.your-domain.example.com` is routed through Cloudflare Tunnel (`cloudflared tunnel route dns`).
- ttyd runs natively on macOS (not in Docker) because macOS disables SSH Remote Login; Traefik proxies to `host.docker.internal:7681`.
- A Dropbear SSH server (`terminal` service) runs in Docker on port 2222 for SSH-based terminal access.

## Hostnames

| Hostname | Service | Auth |
|----------|---------|------|
| `auth.your-domain.example.com` | Keycloak admin console | N/A |
| `t.your-domain.example.com` | ttyd (Mac shell via oauth2-proxy) | Required |
| `apps.your-domain.example.com` | App Registry (via oauth2-proxy) | Required |
| `*.your-domain.example.com` | Catchall — App Registry (via oauth2-proxy) | Required |

## Adding a New App

Register the app at the Registry dashboard (`apps.your-domain.example.com`). The Registry writes the Traefik route to `traefik/dynamic/managed.yml` automatically. No code changes, tunnel edits, or Docker Compose modifications needed.

## Backup

The `backup` service dumps registered databases to S3 on a schedule. Configure AWS credentials and bucket in `backup/.env`.
