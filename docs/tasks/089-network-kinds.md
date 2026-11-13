# Task 089 — Network: interfaces, addresses, routes, neighbours

- **Milestone:** M6
- **Depends on:** 088
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
Kinds `interface`, `route`, `neigh` from `ip -j` (SPEC §8.1).

## Files
- create `internal/backend/linux/parse_ip.go`, `internal/backend/linux/parse_ip_test.go`,
  `internal/backend/linux/kind_net.go`
- modify `internal/backend/linux/kinds.go`, `internal/backend/linux/probes.go`; add `testdata/linux/ip/…`

## Requirements
1. `interface` (aliases `if`, `ifaces`, `ip`): from `ip -j -s link` + `ip -j addr`: `name`, `state`, `mtu`,
   `addrs` (comma-joined CIDRs, IPv4 first), `rx` / `tx` bytes, `rx_err`, `tx_err`, `rx_drop`, `tx_drop`,
   `master`. Warn when state is `DOWN` but the interface has an address; Warn when error/drop counters are
   non-zero.
2. `route` (aliases `routes`): `ip -j route show table main` + `ip -j -6 route`: `dst`, `gateway`, `dev`, `proto`,
   `metric`, `src`. Default routes first.
3. `neigh` (aliases `arp`): `ip -j neigh`: `ip`, `mac`, `dev`, `state`. `FAILED`/`INCOMPLETE` → Warn.
4. All parsers decode into structs with only the fields they need (unknown fields ignored); empty output or
   `[]` → empty list.
5. Links host → interfaces (`i`).

## Acceptance
```
go test ./internal/backend/linux/ -run 'IPLink|IPAddr|IPRoute|IPNeigh|Interface|Route|Neigh' -v
```

## Pitfalls
- `ip -j -s link` nests stats under `stats64.rx.bytes`; older iproute2 used `stats64` only when `-s` is given.
- The `dst` of the default route is the string `default`.
