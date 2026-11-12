#!/bin/bash
set -euo pipefail
command -v k3s >/dev/null || (curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--write-kubeconfig-mode 644" sh - >/dev/null 2>&1)
for i in $(seq 60); do k3s kubectl get nodes >/dev/null 2>&1 && break; sleep 2; done
