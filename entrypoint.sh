#!/bin/sh
set -e
mkdir -p /data/keys /home/phone/.ssh

[ -f /data/keys/dropbear_rsa_host_key ] || dropbearkey -t rsa -f /data/keys/dropbear_rsa_host_key
[ -f /data/keys/dropbear_ed25519_host_key ] || dropbearkey -t ed25519 -f /data/keys/dropbear_ed25519_host_key

for f in /import/*.pub; do
  [ -f "$f" ] && cat "$f" >> /home/phone/.ssh/authorized_keys
done
chmod 700 /home/phone/.ssh
chmod 600 /home/phone/.ssh/authorized_keys
chown -R phone:phone /home/phone/.ssh

exec dropbear -p 2222 \
  -r /data/keys/dropbear_rsa_host_key \
  -r /data/keys/dropbear_ed25519_host_key \
  -F -E
