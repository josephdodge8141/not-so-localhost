FROM alpine:3.20
RUN apk add --no-cache ca-certificates gettext
RUN --mount=type=secret,id=corp_ca,required=false \
    if [ -s /run/secrets/corp_ca ]; then cat /run/secrets/corp_ca >> /etc/ssl/certs/ca-certificates.crt; fi
