#!/bin/bash
# Usage: k3s-agent.sh <server-ip> <token>
set -euo pipefail
command -v k3s >/dev/null || (curl -sfL https://get.k3s.io | K3S_URL="https://$1:6443" K3S_TOKEN="$2" sh - >/dev/null 2>&1)
