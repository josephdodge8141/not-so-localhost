#!/bin/sh
# registry/seed.sh - Register known apps with the registry API.
# Idempotent: skips apps that already exist (HTTP 409).
# Usage: ./registry/seed.sh [REGISTRY_URL]
# Default REGISTRY_URL: http://localhost:7272

set -e

REGISTRY_URL="${1:-http://localhost:7272}"

register() {
  name="$1"
  payload="$2"
  printf '  %s... ' "$name"
  status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$REGISTRY_URL/api/apps" \
    -H "Content-Type: application/json" \
    -d "$payload") || true
  case "$status" in
    2*)     echo "created" ;;
    409)    echo "already exists" ;;
    000)    echo "FAILED (no response from $REGISTRY_URL)"; exit 1 ;;
    *)      echo "FAILED (HTTP $status)"; exit 1 ;;
  esac
}

echo "Registering known apps with registry at $REGISTRY_URL"

register "Todo" '{"name":"Todo","path_prefix":"/todo","port":3000,"app_type":"frontend","container_name":"todo","technology":"Node.js","description":"todo list app"}'

register "Todo DB" '{"name":"Todo DB","path_prefix":"/todo-db","port":5432,"app_type":"db","container_name":"postgres","technology":"Postgres","description":"Postgres database for the todo app","metadata":"{\"db_name\":\"todo\",\"db_user\":\"todo\"}"}'

register "API Docs" '{"name":"API Docs","path_prefix":"/api-docs","port":7274,"app_type":"backend","container_name":"docs","technology":"Swagger/OpenAPI","description":"API documentation for backend services"}'
