package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"jumpr/internal/errs"
)

// Timeouts and buffer sizes of the client (BUILD-PLAN §6, §6.2).
const (
	// clientDialTimeout bounds the connect to the session server socket.
	clientDialTimeout = 2 * time.Second
	// clientHandshakeTimeout bounds the HELLO/WELCOME exchange.
	clientHandshakeTimeout = 5 * time.Second
	// clientWriteTimeout bounds one frame write.
	clientWriteTimeout = 5 * time.Second
	// clientDetachWait is how long the client waits for EXIT after DETACH.
	clientDetachWait = 1 * time.Second
	// clientOutWait is how long the client waits for the reader goroutine to
	// stop writing to Out before it writes the safety postamble.
	clientOutWait = 1 * time.Second
	// clientStdinBuf is the read buffer of the stdin pump.
	clientStdinBuf = 4 << 10
	// clientPostamble restores the terminal if the server died mid-frame:
	// leave the alt screen, show the cursor, disable bracketed paste, reset
	// attributes.
	clientPostamble = "\x1b[?1049l\x1b[?25h\x1b[?2004l\x1b[0m"
)

// ClientOptions carries everything the client needs from the OS. cmd/jumpr
// injects all of it, so this package never touches os.Stdin, os.Stdout or the
// terminal (BUILD-PLAN §6.1). Nil MakeRaw and nil Size are allowed: no raw
// mode, and a size of 0×0.
type ClientOptions struct {
	SocketPath string
	Hello      Hello
	In         io.Reader
	Out        io.Writer
	MakeRaw    func() (restore func(), err error)
	Size       func() (cols, rows int)
	Resizes    <-chan struct{}
	Hangup     <-chan struct{}
}

// Attach runs the client for one session: it dials the server, says hello,
// puts the terminal in raw mode and pumps bytes both ways until the server
// sends EXIT, the context is canceled or the connection dies. It restores the
// terminal on every path, including panics.
//
// A blocked read on In cannot be interrupted, so Attach never waits for the
// stdin pump; the process exits right after it returns.
func Attach(ctx context.Context, o ClientOptions) (Exit, error) {
	if o.SocketPath == "" {
		return Exit{}, errs.New(errs.Config, "client.attach", "empty socket path")
	}
	if o.In == nil || o.Out == nil {
		return Exit{}, errs.New(errs.Config, "client.attach", "missing reader or writer")
	}

	conn, err := net.DialTimeout("unix", o.SocketPath, clientDialTimeout)
	if err != nil {
		return Exit{}, errs.Wrap(errs.Unreachable, "client.dial", err)
	}
	defer conn.Close()

	var writeMu sync.Mutex
	write := func(typ byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if derr := conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout)); derr != nil {
			return errs.Wrap(errs.Protocol, "client.write", derr)
		}
		return WriteFrame(conn, typ, payload)
	}

	// HELLO with the current size.
	cols, rows := clientSize(o)
	hello := o.Hello
	if hello.Protocol == 0 {
		hello.Protocol = ProtocolVersion
	}
	hello.Cols, hello.Rows = cols, rows
	helloPayload, err := json.Marshal(hello)
	if err != nil {
		return Exit{}, errs.Wrap(errs.Protocol, "client.hello", err)
	}
	if err := write(FrameHello, helloPayload); err != nil {
		return Exit{}, err
	}

	// WELCOME, or EXIT if the server refused us.
	if derr := conn.SetReadDeadline(time.Now().Add(clientHandshakeTimeout)); derr != nil {
		return Exit{}, errs.Wrap(errs.Protocol, "client.handshake", derr)
	}
	typ, payload, err := ReadFrame(conn)
	if err != nil {
		return Exit{}, errs.Wrap(errs.Protocol, "client.handshake", err)
	}
	var welcome Welcome
	switch typ {
	case FrameWelcome:
		if len(payload) > 0 {
			if uerr := json.Unmarshal(payload, &welcome); uerr != nil {
				return Exit{}, errs.Wrap(errs.Protocol, "client.handshake", uerr)
			}
		}
	case FrameExit:
		return decodeExit(payload), nil
	default:
		return Exit{}, errs.New(errs.Protocol, "client.handshake", fmt.Sprintf("unexpected frame type %#x", typ))
	}
	if derr := conn.SetReadDeadline(time.Time{}); derr != nil {
		return Exit{}, errs.Wrap(errs.Protocol, "client.handshake", derr)
	}

	// Raw mode from here on; restore runs on every path, panics included.
	restore := func() {}
	if o.MakeRaw != nil {
		fn, rerr := o.MakeRaw()
		if rerr != nil {
			return Exit{}, errs.Wrap(errs.Internal, "client.raw", rerr)
		}
		if fn != nil {
			restore = fn
		}
	}
	defer func() { restore() }()

	// The socket reader is the only goroutine that writes to Out; outMu keeps
	// the postamble from interleaving with it.
	var outMu sync.Mutex
	outClosed := false
	writeOut := func(p []byte) error {
		outMu.Lock()
		defer outMu.Unlock()
		if outClosed {
			return io.ErrClosedPipe
		}
		_, werr := o.Out.Write(p)
		return werr
	}

	type inEvent struct {
		typ     byte
		payload []byte
		err     error
	}
	events := make(chan inEvent, 4)
	readerStop := make(chan struct{})
	outDone := make(chan struct{})

	// Goroutine B: frames → stdout, EXIT → the main loop.
	go func() {
		defer close(outDone)
		for {
			ftyp, fpayload, ferr := ReadFrame(conn)
			if ferr != nil {
				select {
				case events <- inEvent{err: ferr}:
				case <-readerStop:
				}
				return
			}
			if ftyp == FrameStdout {
				if len(fpayload) == 0 {
					continue
				}
				if werr := writeOut(fpayload); werr != nil {
					select {
					case events <- inEvent{err: werr}:
					case <-readerStop:
					}
					return
				}
				continue
			}
			select {
			case events <- inEvent{typ: ftyp, payload: fpayload}:
			case <-readerStop:
				return
			}
			if ftyp == FrameExit {
				return
			}
		}
	}()

	// Goroutine A: stdin → STDIN frames. Never waited for on return.
	go func() {
		buf := make([]byte, clientStdinBuf)
		for {
			n, rerr := o.In.Read(buf)
			if n > 0 {
				if werr := write(FrameStdin, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	var (
		exit     Exit
		done     bool
		detached bool // DETACH has been sent
		detachC  <-chan time.Time
		resizes  = o.Resizes
		hangup   = o.Hangup
		ctxDone  = ctx.Done()
	)
	for !done {
		select {
		case ev := <-events:
			switch {
			case ev.err != nil:
				if detached {
					// We asked to detach; the server went away instead of
					// sending EXIT. Report the detach we requested.
					exit = Exit{Reason: "detached"}
				} else {
					exit = Exit{Reason: "error", Message: "server connection lost"}
				}
			case ev.typ == FrameExit:
				exit = decodeExit(ev.payload)
			default:
				// RESULT and anything else has no meaning while attached.
				continue
			}
			done = true

		case <-resizes:
			c, r := clientSize(o)
			rpayload, merr := json.Marshal(Resize{Cols: c, Rows: r})
			if merr != nil {
				continue
			}
			_ = write(FrameResize, rpayload)

		case <-hangup:
			hangup = nil
			if !detached {
				detached = true
				_ = write(FrameDetach, nil)
				detachC = time.After(clientDetachWait)
			}

		case <-ctxDone:
			ctxDone = nil
			if !detached {
				detached = true
				_ = write(FrameDetach, nil)
				detachC = time.After(clientDetachWait)
			}

		case <-detachC:
			// No EXIT within a second of asking; stop waiting. Closing the
			// socket also unblocks the reader so the postamble goes out
			// promptly instead of after clientOutWait.
			conn.Close()
			exit = Exit{Reason: "detached"}
			done = true
		}
	}

	if exit.Reason == "detached" && exit.Message == "" {
		exit.Message = welcome.Session
	}

	// Stop the reader before writing the postamble, so nothing lands after it.
	close(readerStop)
	select {
	case <-outDone:
	case <-time.After(clientOutWait):
	}
	outMu.Lock()
	outClosed = true
	_, _ = io.WriteString(o.Out, clientPostamble)
	outMu.Unlock()

	return exit, nil
}

// Control runs a control command (ls, kill-session, kill-server, new -d):
// say hello, wait for RESULT, and turn EXIT into an error.
func Control(ctx context.Context, socketPath string, h Hello) (Result, error) {
	if socketPath == "" {
		return Result{}, errs.New(errs.Config, "client.control", "empty socket path")
	}
	conn, err := net.DialTimeout("unix", socketPath, clientDialTimeout)
	if err != nil {
		return Result{}, errs.Wrap(errs.Unreachable, "client.dial", err)
	}
	defer conn.Close()

	// Canceling the context unblocks the frame read.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stopped:
		}
	}()

	deadline := time.Now().Add(clientHandshakeTimeout)
	if derr := conn.SetDeadline(deadline); derr != nil {
		return Result{}, errs.Wrap(errs.Protocol, "client.control", derr)
	}

	hello := h
	if hello.Protocol == 0 {
		hello.Protocol = ProtocolVersion
	}
	if err := WriteJSON(conn, FrameHello, hello); err != nil {
		return Result{}, err
	}
	typ, payload, err := ReadFrame(conn)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, errs.Wrap(errs.Canceled, "client.control", ctx.Err())
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			// The deadline above may fire just before the context timer.
			return Result{}, errs.Wrap(errs.Timeout, "client.control", err)
		}
		return Result{}, errs.Wrap(errs.Protocol, "client.control", err)
	}
	switch typ {
	case FrameResult:
		var res Result
		if len(payload) > 0 {
			if uerr := json.Unmarshal(payload, &res); uerr != nil {
				return Result{}, errs.Wrap(errs.Protocol, "client.control", uerr)
			}
		}
		return res, nil
	case FrameExit:
		exit := decodeExit(payload)
		msg := exit.Message
		if msg == "" {
			msg = exit.Reason
		}
		if msg == "" {
			msg = "server refused the command"
		}
		return Result{}, errs.New(errs.Protocol, "client.control", msg)
	default:
		return Result{}, errs.New(errs.Protocol, "client.control", fmt.Sprintf("unexpected frame type %#x", typ))
	}
}

// UserMessage is the line cmd/jumpr prints after Attach returns.
func (e Exit) UserMessage() string {
	switch e.Reason {
	case "detached":
		if e.Message != "" {
			return "[detached (from session " + e.Message + ")]"
		}
		return "[detached]"
	case "replaced":
		return "[detached: another client attached]"
	case "shutdown":
		return "[server exited]"
	case "ended":
		return "[session ended]"
	default:
		if e.Message != "" {
			return e.Message
		}
		return "[session ended]"
	}
}

// clientSize asks the injected Size func, tolerating its absence.
func clientSize(o ClientOptions) (cols, rows int) {
	if o.Size == nil {
		return 0, 0
	}
	return o.Size()
}

// decodeExit parses an EXIT payload; a malformed one still ends the client.
func decodeExit(payload []byte) Exit {
	var e Exit
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &e)
	}
	return e
}
