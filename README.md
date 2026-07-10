# Not-So-Localhost

Orchestrate local services behind a single ngrok domain. All accessible from anywhere via the same URL.

## Quick Start

```bash
cp .env.example .env   # fill in your ngrok token + domain
docker compose up -d
```

## Services

| Path | Service |
|------|---------|
| `/` | Homarr dashboard |
| `/terminal/` | ttyd — tmux terminal (web) |
| `/todo/` | Todo app |
| port 2222 | dropbear SSH (on this network only) |

## Register a New Service

```bash
./register.sh
```

Prompts for service name, port, path prefix, Docker image or build path, and optional host port. Edits `docker-compose.yml` and `Caddyfile`, then shows a preview before applying.

The `register.sh` script also supports remote machines — enter a host:port as the source and the script prints instructions for setting up a tunnel.

## Architecture

```
ngrok → Caddy (reverse proxy) → homarr (dashboard)
                              → ttyd/terminal
                              → todo (Next.js)
                              → ... (register more)
```

Caddy routes by path prefix, strips the prefix before forwarding. Every service lives behind the same ngrok domain.
