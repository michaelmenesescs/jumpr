#!/bin/bash
# Master config + a small, realistic state tree. Keys are accepted by up.sh after minions start.
set -euo pipefail
mkdir -p /etc/salt/master.d /srv/salt /srv/pillar
cat > /etc/salt/master.d/lab.conf <<'C'
file_roots:
  base: [/srv/salt]
pillar_roots:
  base: [/srv/pillar]
nodegroups:
  webs: 'G@role:web'
  k8s: 'G@role:k8s'
C
cat > /srv/salt/top.sls <<'C'
base:
  '*':
    - common
  'role:web':
    - match: grain
    - nginx
  'role:lb':
    - match: grain
    - haproxy
  'role:db':
    - match: grain
    - postgres
C
cat > /srv/salt/common.sls <<'C'
motd:
  file.managed:
    - name: /etc/motd
    - contents: "Managed by Salt ({{ grains['id'] }}) -- lab.example.internal\n"
chrony:
  pkg.installed:
    - name: chrony
C
cat > /srv/salt/nginx.sls <<'C'
nginx:
  pkg.installed: []
  service.running:
    - enable: True
    - require:
      - pkg: nginx
C
cat > /srv/salt/haproxy.sls <<'C'
haproxy:
  pkg.installed: []
  service.running:
    - enable: True
C
cat > /srv/salt/postgres.sls <<'C'
postgresql:
  pkg.installed:
    - name: {{ 'postgresql-server' if grains['os_family'] == 'RedHat' else 'postgresql' }}
C
cat > /srv/pillar/top.sls <<'C'
base:
  '*':
    - common
C
cat > /srv/pillar/common.sls <<'C'
env: lab
db_password: not-a-real-secret   # pillar redaction test fixture
C
systemctl enable --now salt-master >/dev/null 2>&1; systemctl restart salt-master
