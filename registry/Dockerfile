ARG VERSION=dev
FROM golang:alpine AS builder
ARG VERSION
RUN apk add --no-cache ca-certificates
RUN --mount=type=secret,id=corp_ca,required=false \
    if [ -s /run/secrets/corp_ca ]; then cat /run/secrets/corp_ca >> /etc/ssl/certs/ca-certificates.crt; fi
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-X main.version=$VERSION" -o registry ./cmd

FROM alpine:3.19
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /build/registry /registry
EXPOSE 7272
CMD ["/registry"]
