FROM alpine:latest
RUN apk add --no-cache dropbear tmux ttyd
RUN adduser -D phone && \
    echo '\nif command -v tmux >/dev/null 2>&1; then\n  [ -z "$TMUX" ] && exec tmux new-session -A -s phone\nfi' >> /etc/profile
COPY entrypoint.sh /
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
