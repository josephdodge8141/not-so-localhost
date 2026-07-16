FROM golang:alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o registry ./cmd

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/registry /registry
EXPOSE 7272
CMD ["/registry"]
