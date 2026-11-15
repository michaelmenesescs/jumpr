// Package server implements the session server: the wire protocol spoken
// between the client and the per-user server, and the sessions the server
// owns.
package server

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"sync"
	"time"

	"jumpr/internal/errs"
)

// ProtocolVersion is the version of the wire protocol below. If Hello.Protocol
// differs the server replies EXIT{reason:"error"} and the client tells the user
// to run "jumpr kill-server" and start again.
const ProtocolVersion = 1

// MaxPayload is the largest payload a frame may carry. A larger length field is
// a protocol error and the connection is closed.
const MaxPayload = 1 << 20

// MaxChunk is the largest payload a single chunked frame carries (STDIN, STDOUT).
const MaxChunk = 32 << 10

// Frame type codes, BUILD-PLAN §6.2.
const (
	FrameHello   byte = 0x01 // C→S JSON Hello (must be the first frame)
	FrameWelcome byte = 0x02 // S→C JSON Welcome
	FrameStdin   byte = 0x03 // C→S raw bytes from the terminal
	FrameStdout  byte = 0x04 // S→C raw bytes for the terminal
	FrameResize  byte = 0x05 // C→S JSON {"cols":132,"rows":40}
	FrameDetach  byte = 0x06 // C→S empty (client asks to detach, e.g. on SIGHUP)
	FrameExit    byte = 0x07 // S→C JSON {"reason":"…","message":"…"}
	FrameResult  byte = 0x08 // S→C JSON reply to a control command
)

// Hello is the first frame the client sends.
type Hello struct {
	Protocol int               `json:"protocol"`
	Version  string            `json:"version"`  // binary version, informational
	Command  string            `json:"command"`  // "attach" | "new" | "ls" | "kill-session" | "kill-server"
	Session  string            `json:"session"`  // name; "" = most recently used, or create "main"
	Detached bool              `json:"detached"` // new -d: create without attaching
	Cols     int               `json:"cols"`
	Rows     int               `json:"rows"`
	Env      map[string]string `json:"env"` // TERM, COLORTERM, SSH_AUTH_SOCK, TMUX only
}

// Welcome answers Hello.
type Welcome struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	Session  string `json:"session"`
	Created  bool   `json:"created"`
}

// Resize is the client's terminal size.
type Resize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Exit ends a session.
type Exit struct {
	Reason  string `json:"reason"` // detached|ended|replaced|shutdown|error
	Message string `json:"message"`
}

// SessionInfo is one entry of the RESULT of "ls".
type SessionInfo struct {
	Name     string    `json:"name"`
	Windows  int       `json:"windows"`
	Attached bool      `json:"attached"`
	Created  time.Time `json:"created"`
	Activity time.Time `json:"activity"`
}

// Result is the RESULT of a control command.
type Result struct {
	OK       bool          `json:"ok"`
	Error    string        `json:"error"`
	Sessions []SessionInfo `json:"sessions"`
}

// WriteFrame writes one frame: type, big-endian length, payload. Header and
// payload go out in a single Write so a frame is never split on the wire.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return errs.New(errs.Protocol, "protocol.write_frame", "payload too large")
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	n, err := w.Write(buf)
	if err != nil {
		return errs.Wrap(errs.Protocol, "protocol.write_frame", err)
	}
	if n != len(buf) {
		return errs.Wrap(errs.Protocol, "protocol.write_frame", io.ErrShortWrite)
	}
	return nil
}

// WriteJSON marshals v as JSON and writes it as one frame of type typ.
func WriteJSON(w io.Writer, typ byte, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return errs.Wrap(errs.Protocol, "protocol.write_json", err)
	}
	return WriteFrame(w, typ, payload)
}

// ReadFrame reads one frame. A clean EOF before a header returns io.EOF; any
// other truncation, or a length over MaxPayload, returns an errs.Protocol error
// without allocating the payload.
func ReadFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [5]byte
	n, err := io.ReadFull(r, hdr[:])
	switch {
	case err == io.EOF && n == 0:
		return 0, nil, io.EOF
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		return 0, nil, errs.New(errs.Protocol, "protocol.read_frame", "truncated header")
	case err != nil:
		return 0, nil, errs.Wrap(errs.Protocol, "protocol.read_frame", err)
	}
	typ = hdr[0]
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length > MaxPayload {
		return 0, nil, errs.New(errs.Protocol, "protocol.read_frame", "payload too large")
	}
	if length == 0 {
		return typ, nil, nil
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, errs.New(errs.Protocol, "protocol.read_frame", "truncated payload")
	}
	return typ, payload, nil
}

// chunkWriter is an io.Writer that sends each Write as one or more frames of at
// most MaxChunk bytes. It is used for STDIN and STDOUT.
type chunkWriter struct {
	w   io.Writer
	typ byte
	mu  *sync.Mutex
}

// ChunkWriter returns an io.Writer that frames every Write under mu as frames of
// type typ, each carrying at most MaxChunk bytes. mu may be nil for a writer no
// other goroutine shares.
func ChunkWriter(w io.Writer, typ byte, mu *sync.Mutex) io.Writer {
	return &chunkWriter{w: w, typ: typ, mu: mu}
}

func (c *chunkWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > MaxChunk {
			n = MaxChunk
		}
		if c.mu != nil {
			c.mu.Lock()
		}
		err := WriteFrame(c.w, c.typ, p[:n])
		if c.mu != nil {
			c.mu.Unlock()
		}
		if err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// ValidSessionName reports whether s is an acceptable session name:
// ^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$.
func ValidSessionName(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case i > 0 && (c == '_' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}
