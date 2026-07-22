#!/bin/sh
set -e
if [ -f /etc/cloudflared/ca-bundle.pem ]; then
	cat /etc/cloudflared/ca-bundle.pem >>/etc/ssl/cert.pem
fi
exec cloudflared "$@"
