# jumper test lab

Local OrbStack machines that stand in for a real fleet: a bastion, a Salt master, web/db nodes (Rocky 9 + Ubuntu),
a two-node k3s cluster, and a Chef workstation. Everything here is lab-only. jumper itself never installs anything
on nodes.

```sh
go run ./cmd/@@ up            # create + provision (idempotent; ~3 min first time)
go run ./cmd/@@ status
ssh -F lab/ssh_config web-01.lab.example.internal      # via ProxyJump bastion, user sre, NOPASSWD sudo
KUBECONFIG=lab/.keys/kubeconfig kubectl get nodes      # context @@
go run ./cmd/@@ fault list    # reproducible incidents (haproxy-443, disk-fill, oom-pod, unit-flap)
go run ./cmd/@@ fault haproxy-443 [--undo]
go run ./cmd/@@ down [--stop]
```

Generated, git-ignored: `lab/.keys/` (lab SSH key, known_hosts, kubeconfig), `lab/.logs/`, `lab/ssh_config`.
Salt master: `sr-salt` (`orb -m sr-salt -u root salt '*' test.ping`). Grains: `role`, `dc=lab`. Nodegroups: `webs`, `k8s`.
Chef: `sr-chef` runs chef-zero on :8889 with knife configured for user `sre` (`~/.cinc/config.rb`, repo in
`~/chef-repo`). web-01..03 and db-01 are registered nodes (roles `web`/`db`); db-01's run fails on purpose so the
"last run failed" signal exists. Use `knife status`, `knife node show web-01 -l -F json` on the workstation.
