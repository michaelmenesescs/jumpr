#!/bin/bash
set -euo pipefail
DEBIAN_FRONTEND=noninteractive apt-get -qq install -y postgresql >/dev/null
systemctl enable --now postgresql
