FROM golang:alpine AS builder
RUN apk add --no-cache ca-certificates
RUN --mount=type=secret,id=ca-bundle,required=false \
    if [ -f /run/secrets/ca-bundle ]; then cat /run/secrets/ca-bundle >> /etc/ssl/cert.pem; fi
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o registry ./cmd

FROM alpine:latest
COPY --from=builder /build/registry /registry
EXPOSE 7272
CMD ["/registry"]
