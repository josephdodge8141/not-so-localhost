FROM golang:1.26-alpine AS builder
RUN apk add --no-cache ca-certificates
RUN --mount=type=secret,id=corp_ca,required=false \
    if [ -s /run/secrets/corp_ca ]; then cat /run/secrets/corp_ca >> /etc/ssl/certs/ca-certificates.crt; fi
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /node-init .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /node-init /node-init
ENTRYPOINT ["/node-init"]
