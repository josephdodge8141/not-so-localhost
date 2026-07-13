FROM alpine:latest
RUN apk add --no-cache dropbear tmux
RUN adduser -D phone
COPY entrypoint.sh /
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
