# Task 043 — Linux parsers: processes and sockets

- **Milestone:** M2
- **Depends on:** 005
- **Parallel group:** M2-D (with 039, 040, 041, 042, 049)
- **Status:** done

## Goal
Parse `ps` and `ss` output.

## Context
SPEC §8.1 (`process`, `socket`). Mockup procs and sockets screens.

## Files
- create `internal/backend/linux/parse_ps.go`, `internal/backend/linux/parse_ss.go`
- create `internal/backend/linux/parse_ps_test.go`, `internal/backend/linux/parse_ss_test.go`
- create `testdata/linux/ps/rocky9/ps.txt`, `testdata/linux/ss/rocky9/ss.txt`

## Requirements
Processes — command: `ps -eo pid=,ppid=,user:32=,stat=,pcpu=,pmem=,rss=,etimes=,args=` (no `comm`: it can contain
spaces and would break column splitting).
1. `ParsePS(out string) ([]Proc, int)` → `Proc{PID, PPID int; User, Stat string; CPU, Mem float64; RSSKB int64;
   Elapsed time.Duration; Args string}`; the first 8 fields are whitespace-separated, the remainder (trimmed) is
   `Args`. Returns the count of skipped lines.
2. `func (p Proc) Name() string` — for `[kworker/0:1]` the bracketed text; else the base name of the first word of
   `Args` (`/usr/sbin/nginx -g …` → `nginx`; `nginx: worker process` → `nginx:` trimmed of the colon → `nginx`).

Sockets — command: `ss -H -tuanp` (run with sudo so every socket shows its process).
3. `ParseSS(out string) ([]Socket, int)` → `Socket{Proto, State string; RecvQ, SendQ int; LocalAddr string;
   LocalPort string; PeerAddr, PeerPort string; Procs []SockProc}` with `SockProc{Name string; PID, FD int}`.
   Sample lines:
   ```
   tcp   LISTEN 0      511          0.0.0.0:443        0.0.0.0:*    users:(("nginx",pid=2010,fd=6),("nginx",pid=2014,fd=6))
   tcp   LISTEN 0      128             [::]:22            [::]:*    users:(("sshd",pid=1290,fd=4))
   udp   UNCONN 0      0      127.0.0.53%lo:53         0.0.0.0:*    users:(("systemd-resolve",pid=612,fd=13))
   tcp   ESTAB  0      0         10.20.4.17:22      10.20.0.5:51122 users:(("sshd",pid=88901,fd=4))
   tcp   LISTEN 0      4096               *:9100             *:*
   ```
   Address and port are split at the **last** colon. The process column is optional. `users:((…))` is parsed with
   a regexp `\("([^"]*)",pid=(\d+),fd=(\d+)\)`.
4. `func (s Socket) Listening() bool` (`LISTEN`, or `UNCONN` for UDP).
5. `func ListenersOnPort(socks []Socket, port string) []Socket`.

## Out of scope
`ss -i` TCP info. `/proc/<pid>` drill-in (091).

## Acceptance
```
go test ./internal/backend/linux/ -run 'PS|SS|Proc|Socket' -v
# ps: kernel thread, args with many spaces, user with a long name, a short/garbled line is skipped and counted
# ss: all five sample lines; duplicate processes on one socket; no process column; IPv6; "%lo" scope
```

## Pitfalls
- `etimes` is seconds as an integer; older procps prints it right-aligned with spaces — `strings.Fields` handles
  it.
- Do not split `ss` lines on a fixed number of spaces; the `users:` column contains no spaces but addresses can be
  wide.
