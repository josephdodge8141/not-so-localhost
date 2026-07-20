FROM golang:alpine AS builder
RUN apk add --no-cache ca-certificates
RUN --mount=type=secret,id=ca-bundle,required=false \
    if [ -f /run/secrets/ca-bundle ]; then cat /run/secrets/ca-bundle >> /etc/ssl/cert.pem; fi
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /backup ./cmd/...

FROM alpine:3.19
RUN apk add --no-cache postgresql-client
COPY --from=builder /backup /backup
EXPOSE 7273
CMD ["/backup"]
