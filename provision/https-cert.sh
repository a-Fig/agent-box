#!/bin/sh
# Gives Incus's HTTPS API and web UI a real certificate (Let's Encrypt, through Tailscale) for this
# node's tailnet name, so browsers and clients trust https://<name>:8443 without a warning.
# Needs HTTPS certificates enabled for the tailnet (admin console, DNS). A weekly systemd timer
# reruns it; Tailscale renews when under a third of the 90 days is left, and Incus restarts only
# when the certificate changed. Restarting the Incus daemon leaves running boxes alone.
set -eu
exec </dev/null
name=$(tailscale status --json | python3 -c 'import json, sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')
[ -n "$name" ] || { echo "tailscale has no DNS name for this node; is it joined?" >&2; exit 1; }
dir=/var/lib/agent-box/cert
mkdir -p "$dir"
tailscale cert --cert-file "$dir/$name.crt" --key-file "$dir/$name.key" "$name"
if cmp -s "$dir/$name.crt" /var/lib/incus/server.crt; then
  echo "certificate for $name unchanged"
  exit 0
fi
[ -f /var/lib/incus/server.crt.selfsigned ] || cp -p /var/lib/incus/server.crt /var/lib/incus/server.crt.selfsigned
[ -f /var/lib/incus/server.key.selfsigned ] || cp -p /var/lib/incus/server.key /var/lib/incus/server.key.selfsigned
install -m 0644 "$dir/$name.crt" /var/lib/incus/server.crt
install -m 0600 "$dir/$name.key" /var/lib/incus/server.key
systemctl restart incus
echo "installed a new certificate for $name and restarted Incus"
