FROM cloudflare/cloudflared:2026.7.1 AS upstream
FROM golang:alpine AS cert-source
FROM alpine:3.19
COPY --from=cert-source /etc/ssl/cert.pem /etc/ssl/cert.pem
COPY --from=upstream /usr/local/bin/cloudflared /usr/local/bin/cloudflared
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]