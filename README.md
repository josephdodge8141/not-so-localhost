# Not-So-Localhost

Local services accessible anywhere via Cloudflare Tunnel.

## Prerequisites

- [ttyd](https://github.com/tsl0922/ttyd) — `brew install ttyd && brew services start ttyd`
- Docker + Docker Compose
- Cloudflare Tunnel credentials in `cloudflared/` (set up via `cloudflared tunnel login`)

## Quick Start

```bash
docker compose up -d
```

You also need ttyd running for remote terminal access (`brew services start ttyd`).

## Services

| Host | Service |
|------|---------|
| `joedodge.dev` | Homarr dashboard |
| `t.joedodge.dev` | ttyd — Mac shell (requires ttyd running locally) |
| `ssh.joedodge.dev` | dropbear SSH (internal) |

## Architecture

```
iPhone Safari → Cloudflare Tunnel → cloudflared (Docker) → caddy → homarr
                                                          → ttyd (localhost:7681) → Mac shell
```

ttyd runs natively on macOS (not in Docker) because macOS disables SSH Remote Login.
