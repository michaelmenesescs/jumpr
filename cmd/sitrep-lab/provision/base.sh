#!/bin/bash
# Runs as root inside a lab machine. Idempotent. Lab-only: real nodes get nothing installed by jumpr.
set -euo pipefail
PUBKEY="$1"
if command -v dnf >/dev/null; then
  dnf -y -q install openssh-server sudo iproute procps-ng util-linux lsof curl tar hostname >/dev/null
else
  export DEBIAN_FRONTEND=noninteractive
  apt-get -qq update && apt-get -qq install -y openssh-server sudo iproute2 procps util-linux lsof curl >/dev/null
fi
id sre >/dev/null 2>&1 || useradd -m -s /bin/bash sre
echo 'sre ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-sre && chmod 440 /etc/sudoers.d/90-sre
install -d -m 700 -o sre -g sre /home/sre/.ssh
echo "$PUBKEY" > /home/sre/.ssh/authorized_keys
chown sre:sre /home/sre/.ssh/authorized_keys && chmod 600 /home/sre/.ssh/authorized_keys
systemctl enable --now sshd 2>/dev/null || systemctl enable --now ssh
# node_exporter (lab stand-in for what real fleets already run)
if ! systemctl is-active -q node_exporter 2>/dev/null; then
  V=1.9.1; cd /tmp
  curl -fsSL -o ne.tgz "https://github.com/prometheus/node_exporter/releases/download/v${V}/node_exporter-${V}.linux-arm64.tar.gz"
  tar xzf ne.tgz && install -m 755 node_exporter-${V}.linux-arm64/node_exporter /usr/local/bin/ && rm -rf ne.tgz node_exporter-*
  id node_exporter >/dev/null 2>&1 || useradd -r -s /sbin/nologin node_exporter
  cat > /etc/systemd/system/node_exporter.service <<'UNIT'
[Unit]
Description=Prometheus node exporter
After=network-online.target
[Service]
User=node_exporter
ExecStart=/usr/local/bin/node_exporter --collector.systemd
Restart=on-failure
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload && systemctl enable --now node_exporter
fi
