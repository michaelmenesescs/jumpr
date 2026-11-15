# Task 164 — Sockets, processes and the fleet join (`connects` across hosts)

- **Milestone:** M15
- **Depends on:** 162, 043
- **Parallel group:** —
- **Status:** todo

## Context
SPEC §23.2 (process + sockets, fleet join), §23.1 rule 1 (confidence). BUILD-PLAN §14.3.

## Files
- create `internal/graph/collect/procsock.go`, `internal/graph/collect/fleet.go` and tests
- modify `internal/app/graph.go` (register `procsock` and `fleet`; `fleet` runs after `procsock` for the same scope)
- modify `internal/tui/views/tree.go` (process children: listeners and connections; `Enter` on a peer opens that host's tree at the peer process)

## Requirements
1. `procsock`: one batch per host with `ss -tanpiOH`, `ss -uanpH`, `ps -eo pid,lstart,…` and `ip -j addr` (reuse the
   043 parsers). Listener nodes `listener:<host>/<proto>:<addr>:<port>`, conn nodes for established sockets, `listens`
   and `connects` edges to the owning process (process IDs now `pid@starttime`; 162's IDs are re-keyed).
2. `fleet`: pure function over the graph. Maps peer addresses to hosts through inventory `HostName`s and the
   addresses `ip -j addr` reported. A conn to a listener on another host in scope → process→process `connects`
   edge with attrs `port`, `proto`; Observed if the far host has the matching reverse socket, else Inferred. Peers
   outside scope → `external` node per address:port.
3. Wildcard listeners (`0.0.0.0`, `::`) match any local address of the host.
4. Tree: under a process, `▸ listens :443` and `→ db-01 :5432 postgres.service (observed)`.
5. Test with three sanitized hosts' socket tables (web→db, web→web via haproxy, one external peer).

## Acceptance
```
go test ./internal/graph/collect/ -run 'Procsock|Fleet' -v
make check
make lab-test   # TestLabFleetJoin: web-0x nginx → upstream observed when the lab demo app is up (lab request L2)
```
