#!/bin/bash
set -euo pipefail
if command -v dnf >/dev/null; then dnf -y -q install nginx >/dev/null; else DEBIAN_FRONTEND=noninteractive apt-get -qq install -y nginx >/dev/null; fi
# listen on :443 too, so the haproxy port-conflict fault is reproducible
cat > /etc/nginx/conf.d/lab-443.conf <<'C'
server { listen 443 default_server; return 200 "ok\n"; }
C
systemctl enable --now nginx
