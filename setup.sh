#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

[ -f .env ] && { set -a; source .env; set +a; }
: "${DOMAIN:=your-domain.example.com}"
echo "DOMAIN=$DOMAIN"

for tmpl in traefik/dynamic/infra.yml.tmpl cloudflared/config.yml.tmpl; do
  out="${tmpl%.tmpl}"
  printf '  %s -> %s\n' "$tmpl" "$out"
  sed "s/\${DOMAIN}/$DOMAIN/g" "$tmpl" > "$out"
done

# Generate CA bundle (system certs + corporate proxy if detected)
printf '  cloudflared/ca-bundle.pem\n'
(security export -t certs -f pemseq -k /Library/Keychains/System.keychain 2>/dev/null
 security find-certificate -a -p -c "NICE" 2>/dev/null) > cloudflared/ca-bundle.pem

echo "done. run 'docker compose up -d'"
