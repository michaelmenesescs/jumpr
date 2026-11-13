# Task 091 — Process drill-in from `/proc`

- **Milestone:** M6
- **Depends on:** 044, 084
- **Parallel group:** — (serial; M6 exit)
- **Status:** done

## Goal
`Enter` on a process: status, limits, cgroup, open-file count, environment keys, listening sockets — one batch.

## Files
- create `internal/backend/linux/proc_detail.go`, `internal/backend/linux/proc_detail_test.go`
- create `internal/tui/views/proc.go`, `internal/tui/views/proc_test.go`
- create `internal/backend/linux/lab_m6_test.go` (`//go:build lab`); modify `docs/HANDOFF.md`

## Requirements
1. One sudo batch for PID `n` (validated numeric): `cat /proc/n/status`, `cat /proc/n/limits`,
   `cat /proc/n/cgroup`, `ls /proc/n/fd | wc -l`, `readlink /proc/n/exe`, `readlink /proc/n/cwd`,
   `tr '\0' '\n' < /proc/n/environ | cut -d= -f1 | sort` (**keys only**, never values),
   `cat /proc/n/io`, `cat /proc/n/oom_score`, `ps -o lstart= -p n`.
2. Tabs (reuse `tabs.go`): **overview** (name, state, threads, RSS/VM, fds used vs `Max open files` soft limit
   with a Warn above 80 %, OOM score, start time, exe, cwd, cgroup → systemd unit link), **limits** (table),
   **env** (keys), **sockets** (this PID's rows from the socket list), **journal** (`_PID=n` filter).
3. The process disappeared (sections fail with "No such file") → the view says `process exited` and offers `esc`.
4. `x` → kill action (062).
5. Lab test `TestLabM6`: unit detail of `sshd`/`ssh` on each distro has non-empty status/show/cat; timers list
   non-empty; mounts include `/`; interfaces include a non-loopback address; `:unitdiff nginx.service` across web
   hosts yields at most 2 groups; process drill-in of PID 1 works.

## Acceptance
```
go test ./internal/backend/linux/ -run ProcDetail -v
go test -race ./internal/tui/views/ -run Proc -v
make check && make lab-test
```

## Pitfalls
- Environment **values** often contain secrets. The command must cut them off on the remote side; they never
  reach the laptop.
