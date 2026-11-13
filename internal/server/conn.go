package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"jumper/internal/errs"
)

// conn is one client connection. Every write to it goes through writeMu, so a
// STDOUT chunk, an EXIT and a RESULT never interleave on the wire.
type conn struct {
	s  *server
	nc net.Conn

	writeMu  sync.Mutex
	endOnce  sync.Once
	finished chan struct{}

	mu    sync.Mutex
	entry *sessionEntry
	pipeW *io.PipeWriter
}

func newConn(s *server, nc net.Conn) *conn {
	return &conn{s: s, nc: nc, finished: make(chan struct{})}
}

// serve reads HELLO and runs whatever command it names. The connection lives
// until the command ends: for attach that is the session's detach or the
// session's end.
func (c *conn) serve() {
	defer func() { _ = c.nc.Close() }()

	hello, err := c.readHello()
	if err != nil {
		// A bad handshake has no reply: the protocol says nothing else.
		return
	}
	if hello.Protocol != ProtocolVersion {
		_ = c.writeJSON(FrameExit, Exit{
			Reason:  "error",
			Message: fmt.Sprintf("server speaks protocol %d; run \"jumper kill-server\" and start again", ProtocolVersion),
		})
		return
	}

	switch hello.Command {
	case "attach":
		c.attachCommand(hello)
	case "new":
		c.newCommand(hello)
	case "ls":
		c.lsCommand()
	case "kill-session":
		c.killSessionCommand(hello)
	case "kill-server":
		c.killServerCommand()
	default:
		_ = c.writeJSON(FrameExit, Exit{Reason: "error", Message: "unknown command " + hello.Command})
	}
}

// readHello waits up to helloTimeout for the first frame and requires HELLO.
func (c *conn) readHello() (Hello, error) {
	if err := c.nc.SetReadDeadline(time.Now().Add(helloTimeout)); err != nil {
		return Hello{}, errs.Wrap(errs.Protocol, "server.hello", err)
	}
	typ, payload, err := ReadFrame(c.nc)
	if err != nil {
		return Hello{}, err
	}
	if typ != FrameHello {
		return Hello{}, errs.New(errs.Protocol, "server.hello", "first frame was not HELLO")
	}
	var h Hello
	if len(payload) > 0 {
		if uerr := json.Unmarshal(payload, &h); uerr != nil {
			return Hello{}, errs.Wrap(errs.Protocol, "server.hello", uerr)
		}
	}
	if derr := c.nc.SetReadDeadline(time.Time{}); derr != nil {
		return Hello{}, errs.Wrap(errs.Protocol, "server.hello", derr)
	}
	return h, nil
}

// attachCommand attaches to the named session, the most recently active one, or
// a new "main" when there is none. An unknown name is refused.
func (c *conn) attachCommand(h Hello) {
	e, created, err := c.sessionForAttach(h.Session)
	if err != nil {
		_ = c.writeJSON(FrameExit, Exit{Reason: "error", Message: messageOf(err)})
		return
	}
	c.attach(e, created, h)
}

// sessionForAttach resolves Hello.Session for attach.
func (c *conn) sessionForAttach(name string) (*sessionEntry, bool, error) {
	if name != "" {
		if e := c.s.lookupEntry(name); e != nil {
			return e, false, nil
		}
		return nil, false, errs.New(errs.Protocol, "server.attach", "no such session")
	}
	if e := c.s.mostRecent(); e != nil {
		return e, false, nil
	}
	return c.s.createEntry("main")
}

// attach sends WELCOME, replaces whatever client the session had, and wires the
// session's input and output to this connection.
func (c *conn) attach(e *sessionEntry, created bool, h Hello) {
	c.mu.Lock()
	c.entry = e
	c.mu.Unlock()

	// One client per session at a time: the previous one is told why.
	if prev := c.s.setClient(e, c); prev != nil && prev != c {
		prev.end("replaced", "", true)
	}

	if err := c.writeJSON(FrameWelcome, Welcome{
		Protocol: ProtocolVersion,
		Version:  c.s.o.Version,
		Session:  e.name,
		Created:  created,
	}); err != nil {
		c.release()
		c.end("error", "could not write WELCOME", true)
		return
	}
	c.s.touch(e)

	pr, pw := io.Pipe()
	c.mu.Lock()
	c.pipeW = pw
	c.mu.Unlock()

	go c.readFrames(pw)
	go c.watchSession(e)

	// Attach blocks for the session's life on the real implementation, so it
	// runs here and the serve goroutine waits for this connection to end.
	go func() {
		err := e.handle.Attach(pr, c.stdoutWriter(), h.Cols, h.Rows, h.Env, func(reason string) {
			c.end(reason, "", true)
		})
		if err != nil {
			c.end("error", messageOf(err), true)
		}
	}()

	<-c.finished
}

// watchSession turns the session's end into EXIT{ended} for its client.
func (c *conn) watchSession(e *sessionEntry) {
	select {
	case <-e.handle.Done():
		c.end("ended", "", true)
	case <-c.finished:
	}
}

// newCommand creates a session and attaches to it unless Hello.Detached.
func (c *conn) newCommand(h Hello) {
	name := h.Session
	if name == "" {
		name = c.s.autoName("main")
	} else if !ValidSessionName(name) {
		_ = c.writeJSON(FrameResult, Result{OK: false, Error: "invalid session name"})
		return
	}
	e, created, err := c.s.createEntry(name)
	if err != nil {
		_ = c.writeJSON(FrameResult, Result{OK: false, Error: messageOf(err)})
		return
	}
	if h.Detached {
		_ = c.writeJSON(FrameResult, Result{OK: true, Sessions: []SessionInfo{sessionInfo(e, false)}})
		c.end("", "", false)
		return
	}
	c.attach(e, created, h)
}

// lsCommand answers with the registry.
func (c *conn) lsCommand() {
	_ = c.writeJSON(FrameResult, Result{OK: true, Sessions: c.s.sessionInfos()})
	c.end("", "", false)
}

// killSessionCommand kills the named session (or the most recent one) and
// removes it from the registry at once.
func (c *conn) killSessionCommand(h Hello) {
	e, err := c.entryForName(h.Session)
	if err != nil {
		_ = c.writeJSON(FrameResult, Result{OK: false, Error: messageOf(err)})
		c.end("", "", false)
		return
	}
	_ = c.writeJSON(FrameResult, Result{OK: true})
	c.s.removeSession(e)
	e.handle.Kill()
	c.end("", "", false)
}

// killServerCommand answers, then shuts the server down.
func (c *conn) killServerCommand() {
	_ = c.writeJSON(FrameResult, Result{OK: true})
	c.s.shutdown()
	c.end("", "", false)
}

// entryForName resolves a control command's session name.
func (c *conn) entryForName(name string) (*sessionEntry, error) {
	if name != "" {
		if e := c.s.lookupEntry(name); e != nil {
			return e, nil
		}
		return nil, errs.New(errs.Protocol, "server.lookup", "no such session")
	}
	if e := c.s.mostRecent(); e != nil {
		return e, nil
	}
	return nil, errs.New(errs.Protocol, "server.lookup", "no sessions")
}

// readFrames pumps STDIN into the session, forwards RESIZE and turns DETACH
// into a detach. A dead connection detaches the session and leaves it running.
func (c *conn) readFrames(pw *io.PipeWriter) {
	defer func() { _ = pw.Close() }()
	for {
		typ, payload, err := ReadFrame(c.nc)
		if err != nil {
			c.clientGone()
			return
		}
		e := c.session()
		switch typ {
		case FrameStdin:
			if len(payload) == 0 {
				continue
			}
			c.s.touch(e)
			if _, werr := pw.Write(payload); werr != nil {
				return
			}
		case FrameResize:
			var r Resize
			if len(payload) > 0 {
				if uerr := json.Unmarshal(payload, &r); uerr != nil {
					continue
				}
			}
			c.s.touch(e)
			if e != nil {
				e.handle.Resize(r.Cols, r.Rows)
			}
		case FrameDetach:
			// The client asks to go away; the session keeps running.
			if e != nil {
				e.handle.Detach("detached")
			}
			c.end("detached", "", true)
			return
		default:
			// Unknown frames are ignored: newer clients may send more.
		}
	}
}

// clientGone handles a dropped connection: the session is detached with reason
// "hangup" and stays alive.
func (c *conn) clientGone() {
	if e := c.session(); e != nil {
		e.handle.Detach("hangup")
	}
	c.end("hangup", "", false)
}

// session is the entry this connection is attached to, if any.
func (c *conn) session() *sessionEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entry
}

// release forgets the session and clears the server's client pointer.
func (c *conn) release() {
	c.mu.Lock()
	e := c.entry
	c.entry = nil
	c.mu.Unlock()
	if e != nil {
		c.s.clearClient(e, c)
	}
}

// end finishes the connection exactly once, optionally sending EXIT first.
func (c *conn) end(reason, message string, send bool) {
	c.endOnce.Do(func() {
		if send {
			_ = c.writeJSON(FrameExit, Exit{Reason: reason, Message: message})
		}
		c.release()
		c.mu.Lock()
		pw := c.pipeW
		c.pipeW = nil
		c.mu.Unlock()
		if pw != nil {
			_ = pw.Close()
		}
		_ = c.nc.Close()
		close(c.finished)
	})
}

// stdoutWriter frames the session's output as STDOUT with a write deadline per
// frame. A client that cannot take the bytes within writeTimeout is detached by
// the failing write, never by a blocked session.
func (c *conn) stdoutWriter() io.Writer {
	return ChunkWriter(deadlineWriter{c: c}, FrameStdout, &c.writeMu)
}

// write writes one frame under writeMu with a write deadline.
func (c *conn) write(typ byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.nc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return errs.Wrap(errs.Protocol, "server.write", err)
	}
	return WriteFrame(c.nc, typ, payload)
}

// writeJSON marshals v and writes it as one frame.
func (c *conn) writeJSON(typ byte, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return errs.Wrap(errs.Protocol, "server.write_json", err)
	}
	return c.write(typ, payload)
}

// deadlineWriter is the raw writer ChunkWriter frames through: each Write gets a
// fresh deadline, and a failure marks the client gone.
type deadlineWriter struct {
	c *conn
}

func (w deadlineWriter) Write(p []byte) (int, error) {
	if err := w.c.nc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		go w.c.clientGone()
		return 0, err
	}
	n, err := w.c.nc.Write(p)
	if err != nil {
		// Outside this write, so the detach does not need writeMu.
		go w.c.clientGone()
	}
	return n, err
}
