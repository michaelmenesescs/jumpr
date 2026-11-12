# sitrep test lab

Local OrbStack machines that stand in for a real fleet: a bastion, a Salt master, web/db nodes (Rocky 9 + Ubuntu),
a two-node k3s cluster, and a Chef workstation. Everything here is lab-only. sitrep itself never installs anything
on nodes.

```sh
go run ./cmd/sitrep-lab up            # create + provision (idempotent; ~3 min first time)
go run ./cmd/sitrep-lab status
ssh -F lab/ssh_config web-01.lab.example.internal      # via ProxyJump bastion, user sre, NOPASSWD sudo
KUBECONFIG=lab/.keys/kubeconfig kubectl get nodes      # context sitrep-lab
go run ./cmd/sitrep-lab fault list    # reproducible incidents (haproxy-443, disk-fill, oom-pod, unit-flap)
go run ./cmd/sitrep-lab fault haproxy-443 [--undo]
go run ./cmd/sitrep-lab down [--stop]
```

Generated, git-ignored: `lab/.keys/` (lab SSH key, known_hosts, kubeconfig), `lab/.logs/`, `lab/ssh_config`.
Salt master: `sr-salt` (`orb -m sr-salt -u root salt '*' test.ping`). Grains: `role`, `dc=lab`. Nodegroups: `webs`, `k8s`.
