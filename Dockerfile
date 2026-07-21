FROM alpine:3.19
RUN apk add --no-cache dropbear tmux && adduser -D phone && \
    printf '#!/bin/sh\nset -e\nmkdir -p /data/keys /home/phone/.ssh\n\n[ -f /data/keys/dropbear_rsa_host_key ] || dropbearkey -t rsa -f /data/keys/dropbear_rsa_host_key\n[ -f /data/keys/dropbear_ed25519_host_key ] || dropbearkey -t ed25519 -f /data/keys/dropbear_ed25519_host_key\n\ncat /import/*.pub >> /home/phone/.ssh/authorized_keys 2>/dev/null\nchmod 700 /home/phone/.ssh\nchmod 600 /home/phone/.ssh/authorized_keys\nchown -R phone:phone /home/phone/.ssh\n\nexec dropbear -p 2222 \\\n  -r /data/keys/dropbear_rsa_host_key \\\n  -r /data/keys/dropbear_ed25519_host_key \\\n  -F -E' > /entrypoint.sh && chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
