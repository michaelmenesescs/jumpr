#!/bin/bash
# Usage: salt-minion.sh <master-ip> <minion-id> [master]
set -euo pipefail
MASTER="$1"; ID="$2"; ROLE="${3:-}"
if ! command -v salt-call >/dev/null; then
  curl -fsSL -o /tmp/bootstrap-salt.sh https://github.com/saltstack/salt-bootstrap/releases/latest/download/bootstrap-salt.sh
  if [ "$ROLE" = master ]; then sh /tmp/bootstrap-salt.sh -M -P stable
  else sh /tmp/bootstrap-salt.sh -P stable; fi
fi
mkdir -p /etc/salt/minion.d
printf 'master: %s\nid: %s\n' "$MASTER" "$ID" > /etc/salt/minion.d/lab.conf
echo "$ID" > /etc/salt/minion_id
systemctl enable salt-minion >/dev/null 2>&1; systemctl restart salt-minion
