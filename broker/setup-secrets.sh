#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")"
umask 077

read -rsp "Cloudflare API token: " cloudflare_api_token
printf '\n'
if [[ -z "$cloudflare_api_token" ]]; then
	printf 'Cloudflare API token is required.\n' >&2
	exit 1
fi

broker_admin_token="$(openssl rand -hex 32)"
node_credential_key="$(openssl rand -hex 32)"
auth_claim_token="$(openssl rand -hex 32)"

printf '%s' "$cloudflare_api_token" | npx wrangler secret put CLOUDFLARE_API_TOKEN
printf '%s' "$broker_admin_token" | npx wrangler secret put BROKER_ADMIN_TOKEN
printf '%s' "$node_credential_key" | npx wrangler secret put NODE_CREDENTIAL_KEY
printf '%s' "$auth_claim_token" | npx wrangler secret put AUTH_CLAIM_TOKEN

secrets_file="../.broker-secrets.env"
cat >"$secrets_file" <<EOF
NSL_BROKER_URL=https://nsl-enrollment-broker.josephdodge8141.workers.dev
NSL_BROKER_ADMIN_TOKEN=$broker_admin_token
NSL_AUTH_CLAIM_TOKEN=$auth_claim_token
EOF
chmod 600 "$secrets_file"

unset cloudflare_api_token broker_admin_token node_credential_key auth_claim_token
printf 'Broker secrets configured. Operator values saved to %s\n' "$secrets_file"
