#!/bin/bash
# Chef workstation: cinc-workstation (knife) + chef-zero as the "Chef server" on :8889, with a tiny cookbook + roles.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if ! command -v knife >/dev/null; then
  curl -fsSL -o /tmp/cw.deb https://packages.cinc.sh/files/stable/cinc-workstation/26.4.0/ubuntu/22.04/cinc-workstation_26.4.0-1_arm64.deb
  apt-get -qq install -y /tmp/cw.deb >/dev/null && rm -f /tmp/cw.deb
fi
CZ=$(ls /opt/cinc-workstation/embedded/bin/cinc-zero /opt/cinc-workstation/embedded/bin/chef-zero 2>/dev/null | head -1 || true)
cat > /etc/systemd/system/chef-zero.service <<U
[Unit]
Description=chef-zero (lab Chef server)
[Service]
ExecStart=$CZ --host 0.0.0.0 --port 8889
Restart=always
[Install]
WantedBy=multi-user.target
U
systemctl daemon-reload; systemctl enable --now chef-zero
# knife config for user sre (chef-zero does not verify keys; any key works)
install -d -o sre -g sre /home/sre/.cinc /home/sre/chef-repo/cookbooks/lab/recipes /home/sre/chef-repo/roles
[ -f /home/sre/.cinc/sre.pem ] || openssl genrsa -out /home/sre/.cinc/sre.pem 2048 2>/dev/null
cat > /home/sre/.cinc/config.rb <<'C'
node_name 'sre'
client_key File.join(__dir__, 'sre.pem')
chef_server_url 'http://127.0.0.1:8889'
cookbook_path [File.expand_path('~/chef-repo/cookbooks')]
C
cat > /home/sre/chef-repo/cookbooks/lab/metadata.rb <<'C'
name 'lab'
version '1.0.0'
C
cat > /home/sre/chef-repo/cookbooks/lab/recipes/default.rb <<'C'
file '/etc/motd.chef' do
  content "Managed by Chef (#{node.name}) -- lab.example.internal\n"
end
C
cat > /home/sre/chef-repo/cookbooks/lab/recipes/db.rb <<'C'
# Deliberately broken: gives jumper a "last chef run failed" signal on db-01.
execute 'check-replica' do
  command 'pg_isready -h replica.lab.example.internal -t 2'
end
C
cat > /home/sre/chef-repo/roles/web.json <<'C'
{"name":"web","json_class":"Chef::Role","chef_type":"role","run_list":["recipe[lab]"],"default_attributes":{"lab":{"tier":"web"}}}
C
cat > /home/sre/chef-repo/roles/db.json <<'C'
{"name":"db","json_class":"Chef::Role","chef_type":"role","run_list":["recipe[lab]","recipe[lab::db]"],"default_attributes":{"lab":{"tier":"db"}}}
C
chown -R sre:sre /home/sre/.cinc /home/sre/chef-repo
for i in $(seq 30); do curl -fs http://127.0.0.1:8889 >/dev/null && break; sleep 1; done
cd /home/sre/chef-repo
sudo -u sre -H knife cookbook upload lab --chef-license accept-silent >/dev/null 2>&1 || sudo -u sre -H knife cookbook upload lab
sudo -u sre -H knife role from file roles/web.json roles/db.json
sudo -u sre -H knife environment create lab -d "lab env" --disable-editing 2>/dev/null || true
