#!/bin/bash
# Usage: chef-node.sh <chef-ip> <node-name> <role>. Installs cinc client (lab only) and runs it once.
set -uo pipefail
CHEF="$1"; NODE="$2"; ROLE="$3"
if ! command -v cinc-client >/dev/null; then
  curl -fsSL https://omnitruck.cinc.sh/install.sh | bash -s -- -v 19 >/dev/null 2>&1
fi
mkdir -p /etc/cinc
[ -f /etc/cinc/validation.pem ] || openssl genrsa -out /etc/cinc/validation.pem 2048 2>/dev/null
cat > /etc/cinc/client.rb <<C
chef_server_url 'http://$CHEF:8889'
node_name '$NODE'
validation_client_name 'lab-validator'
validation_key '/etc/cinc/validation.pem'
environment 'lab'
chef_license 'accept-silent'
C
cinc-client -r "role[$ROLE]" --chef-license accept-silent -L /var/log/cinc-client.log >/dev/null 2>&1
echo "cinc-client rc=$?"
exit 0
