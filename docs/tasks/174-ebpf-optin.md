# Task 174 — Opt-in eBPF: embedded bpftrace programs with consent

- **Milestone:** M16
- **Depends on:** 164, 061
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §24.4: only an already-installed `bpftrace`, fixed embedded programs, consent per host per session, hard time cap, audit.

## Scope
- config `ebpf:` (§14.6); policy class `privileged-read`
- programs: tcp connect/accept latency, retransmits, execve; parsers for their printf output
- consent dialog (host, program, duration); kill on cap or view close; audit entries
- results become Observed edges (connects) and hunting events (execve)

## Likely files
`internal/ebpf/{programs.go,runner.go,parse.go}`, consent dialog in `internal/tui`

## Acceptance (sketch)
unit tests on recorded output; lab test on a lab host with bpftrace installed by the lab track (lab request L5); denied when `enabled: false`
