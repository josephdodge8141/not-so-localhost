#!/bin/sh
set -e
mkdir -p /data/keys /home/phone/.ssh

[ -f /data/keys/dropbear_rsa_host_key ] || dropbearkey -t rsa -f /data/keys/dropbear_rsa_host_key
[ -f /data/keys/dropbear_ed25519_host_key ] || dropbearkey -t ed25519 -f /data/keys/dropbear_ed25519_host_key

[ -f /import/id_ed25519.pub ] && cp /import/id_ed25519.pub /home/phone/.ssh/authorized_keys
chown -R phone:phone /home/phone/.ssh

ttyd -p 7681 tmux new-session -A -s phone &

exec dropbear -p 2222 \
  -r /data/keys/dropbear_rsa_host_key \
  -r /data/keys/dropbear_ed25519_host_key \
  -F -E
