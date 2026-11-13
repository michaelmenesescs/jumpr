# Task 077 — Session-server wire protocol codec (pure)

- **Milestone:** M5
- **Depends on:** 002
- **Parallel group:** M5-A (with 078)
- **Status:** done

## Goal
Encode and decode the frames of BUILD-PLAN §6.2.

## Context
BUILD-PLAN §6.2 (normative: frame layout, type codes, JSON payloads, size limit, version rule).

## Files
- create `internal/server/protocol.go`, `internal/server/protocol_test.go`

## Requirements
1. Constants for the eight frame types, `ProtocolVersion = 1`, `MaxPayload = 1 << 20`.
2. Structs `Hello`, `Welcome`, `Resize{Cols, Rows int}`, `Exit{Reason, Message string}`, `SessionInfo`, and
   `Result{OK bool; Error string; Sessions []SessionInfo}` with the JSON tags from BUILD-PLAN §6.2.
3. `func WriteFrame(w io.Writer, typ byte, payload []byte) error` — header and payload in **one** `Write` call
   (callers share the connection between goroutines under a mutex; a single write keeps frames intact).
   Payload over `MaxPayload` → `errs.Protocol`. `func WriteJSON(w io.Writer, typ byte, v any) error`.
4. `func ReadFrame(r io.Reader) (typ byte, payload []byte, err error)` — `io.ReadFull` for the 5-byte header and
   the payload; length over `MaxPayload` → `errs.Protocol` (do not allocate it); clean EOF before a header →
   `io.EOF`; EOF mid-frame → `errs.Protocol`.
5. `func ChunkWriter(w io.Writer, typ byte, mu *sync.Mutex) io.Writer` — an `io.Writer` that sends each `Write` as
   one or more frames of at most 32 KiB (used for `STDOUT` and `STDIN`).
6. `func ValidSessionName(s string) bool` — `^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`.

## Out of scope
Sockets, sessions.

## Acceptance
```
go test ./internal/server/ -run 'Frame|JSON|Chunk|SessionName' -v
# round trip for every type; oversize rejected on both sides; truncated header/payload; ChunkWriter splits 100 KiB
# into 4 frames that reassemble; a fuzz-style test feeding random bytes to ReadFrame never panics
```

## Pitfalls
- Big endian (`binary.BigEndian.PutUint32`).
- `ReadFrame` must not trust the length field before checking it.
