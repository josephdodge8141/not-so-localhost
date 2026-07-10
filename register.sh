#!/usr/bin/env bash
set -e

COMPOSE="docker-compose.yml"
CADDY="Caddyfile"
DIR="$(cd "$(dirname "$0")" && pwd)"

cd "$DIR"

echo "=============================="
echo " Not-So-Localhost: Register"
echo "=============================="
echo ""

read -rp "Service name (e.g. my-app): " NAME
read -rp "Internal port       (e.g. 3000): " PORT
read -rp "Path prefix         (e.g. /my-app): " PREFIX
read -rp "Docker image or build path (e.g. my-image:latest or ../my-app): " SOURCE
read -rp "Host port (empty = internal only): " HOST_PORT

[[ -z "$NAME" || -z "$PORT" || -z "$PREFIX" || -z "$SOURCE" ]] && {
  echo "Error: name, port, prefix, and source are required"
  exit 1
}

# Build the compose block
if [[ "$SOURCE" == *":"* && "$SOURCE" != *"/"* ]]; then
  IMG="    image: $SOURCE"
else
  IMG="    build: $SOURCE"
fi

PORTS=""
[[ -n "$HOST_PORT" ]] && PORTS="    ports:
      - \"$HOST_PORT:$PORT\""

echo ""
echo "Environment variables (KEY=VALUE, one per line, blank line to stop):"
ENV=""
while IFS= read -r line; do
  [[ -z "$line" ]] && break
  ENV="$ENV      - $line"$'\n'
done
[[ -n "$ENV" ]] && ENV="    environment:
$ENV"

COMPOSE_BLOCK="
  $NAME:
    $IMG
$PORTS
$ENV    restart: unless-stopped"

CADDY_BLOCK="    handle_path $PREFIX/* {
        reverse_proxy $NAME:$PORT
    }"

echo ""
echo "--- Preview ---"
echo ""
echo "docker-compose.yml:"
echo "$COMPOSE_BLOCK"
echo ""
echo "Caddyfile:"
echo "$CADDY_BLOCK"
echo ""
read -rp "Apply? (y/N): " CONFIRM
[[ "$CONFIRM" != "y" && "$CONFIRM" != "Y" ]] && { echo "Cancelled."; exit 0; }

# Backup
cp "$COMPOSE" "$COMPOSE.bak"
cp "$CADDY" "$CADDY.bak"

# Insert into compose before the volumes: lines
awk -v block="$COMPOSE_BLOCK" '
/^volumes:/ && !done { print block; done=1 }
{ print }
' "$COMPOSE" > "$COMPOSE.tmp" && mv "$COMPOSE.tmp" "$COMPOSE"

# Insert into Caddyfile before the last handle { block
awk -v block="$CADDY_BLOCK" '
/^    handle \{/ && !done {
  print block; print ""; done=1
  print; next
}
{ print }
' "$CADDY" > "$CADDY.tmp" && mv "$CADDY.tmp" "$CADDY"

echo ""
echo "Registered $NAME at $PREFIX"
echo "Backups saved as $COMPOSE.bak and $CADDY.bak"
echo "Run: docker compose up -d"
