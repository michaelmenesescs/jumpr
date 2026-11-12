# Agent instructions — sitrep

Read these files in order before doing anything:

1. `docs/HANDOFF.md` — current state and next steps
2. `docs/SPEC.md` — the spec (source of truth for design)
3. `docs/INTERVIEW.md` — the user's actual answers (wins over the spec if they conflict)

Rules:
- Go, single static binary (`CGO_ENABLED=0`), macOS client. Zero footprint on remote nodes: no installs, no file
  writes, only pre-existing commands over SSH.
- Neutral repo: no employer-specific names, hosts, or conventions. Use `example.internal` in examples.
- Never commit secrets or real hostnames/IPs. Sanitize any captured `testdata/`.
- Follow the phase order in SPEC §19. Keep `go test ./...` green, make small commits, and update `docs/HANDOFF.md`
  before ending a session.
