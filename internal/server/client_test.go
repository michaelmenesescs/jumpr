package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jumper/internal/errs"

	"jumper/internal/testload"
)

const testPostamble = "\x1b[?1049l\x1b[?25h\x1b[?2004l\x1b[0m"

// safeBuffer is a bytes.Buffer guarded by a mutex, for tests that read the
// client's output while Attach is still running.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testSocketPath returns a socket path in a short /tmp directory so it stays
// inside the macOS 103-byte sun_path limit.
func testSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "jumper-cli-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// serveOnce listens on path and handles the first connection in a goroutine.
func serveOnce(t *testing.T, path string, handle func(net.Conn)) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handle(conn)
	}()
}

// serverHandshake reads HELLO and answers WELCOME.
func serverHandshake(conn net.Conn) (Hello, error) {
	typ, payload, err := ReadFrame(conn)
	if err != nil {
		return Hello{}, err
	}
	if typ != FrameHello {
		return Hello{}, fmt.Errorf("first frame type %#x, want HELLO", typ)
	}
	var h Hello
	if err := json.Unmarshal(payload, &h); err != nil {
		return Hello{}, err
	}
	if err := WriteJSON(conn, FrameWelcome, Welcome{Protocol: ProtocolVersion, Session: "main"}); err != nil {
		return Hello{}, err
	}
	return h, nil
}

func TestClientRoundTrip(t *testing.T) {
	path := testSocketPath(t)
	helloCh := make(chan Hello, 1)
	serveOnce(t, path, func(conn net.Conn) {
		h, err := serverHandshake(conn)
		if err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		helloCh <- h
		for {
			typ, payload, err := ReadFrame(conn)
			if err != nil {
				return
			}
			switch typ {
			case FrameStdin:
				if err := WriteFrame(conn, FrameStdout, payload); err != nil {
					return
				}
				// End the session once the bytes have been echoed.
				WriteJSON(conn, FrameExit, Exit{Reason: "detached", Message: "main"})
				return
			}
		}
	})

	var out bytes.Buffer
	exit, err := Attach(context.Background(), ClientOptions{
		SocketPath: path,
		Hello:      Hello{Protocol: ProtocolVersion, Command: "attach"},
		In:         strings.NewReader("hello\n"),
		Out:        &out,
		Size:       func() (int, int) { return 132, 40 },
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if exit.Reason != "detached" || exit.Message != "main" {
		t.Errorf("exit = %#v, want detached/main", exit)
	}
	h := <-helloCh
	if h.Cols != 132 || h.Rows != 40 {
		t.Errorf("hello size = %dx%d, want 132x40", h.Cols, h.Rows)
	}
	if h.Command != "attach" || h.Protocol != ProtocolVersion {
		t.Errorf("hello = %#v", h)
	}
	got := out.String()
	if !strings.HasPrefix(got, "hello\n") {
		t.Errorf("out = %q, want it to start with the echoed bytes", got)
	}
	if !strings.HasSuffix(got, testPostamble) {
		t.Errorf("out = %q, want it to end with the postamble", got)
	}
}

func TestClientRestoreCalledOnExit(t *testing.T) {
	path := testSocketPath(t)
	serveOnce(t, path, func(conn net.Conn) {
		if _, err := serverHandshake(conn); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		WriteJSON(conn, FrameExit, Exit{Reason: "ended"})
	})

	restored := 0
	var out bytes.Buffer
	exit, err := Attach(context.Background(), ClientOptions{
		SocketPath: path,
		Hello:      Hello{Protocol: ProtocolVersion, Command: "attach"},
		In:         strings.NewReader(""),
		Out:        &out,
		MakeRaw: func() (func(), error) {
			return func() { restored++ }, nil
		},
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if restored != 1 {
		t.Errorf("restore called %d times, want 1", restored)
	}
	if exit.Reason != "ended" {
		t.Errorf("exit = %#v, want ended", exit)
	}
}

func TestClientRestoreCalledOnServerDeath(t *testing.T) {
	path := testSocketPath(t)
	serveOnce(t, path, func(conn net.Conn) {
		if _, err := serverHandshake(conn); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		// Return closes the connection without an EXIT frame.
	})

	restored := 0
	var out bytes.Buffer
	exit, err := Attach(context.Background(), ClientOptions{
		SocketPath: path,
		Hello:      Hello{Protocol: ProtocolVersion, Command: "attach"},
		In:         strings.NewReader(""),
		Out:        &out,
		MakeRaw: func() (func(), error) {
			return func() { restored++ }, nil
		},
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if restored != 1 {
		t.Errorf("restore called %d times, want 1", restored)
	}
	if exit.Reason != "error" || exit.Message != "server connection lost" {
		t.Errorf("exit = %#v, want error/server connection lost", exit)
	}
	if !strings.HasSuffix(out.String(), testPostamble) {
		t.Errorf("out = %q, want the postamble after a dead server", out.String())
	}
}

func TestClientRestoreCalledOnCtxCancel(t *testing.T) {
	path := testSocketPath(t)
	detachSeen := make(chan struct{}, 1)
	serveOnce(t, path, func(conn net.Conn) {
		if _, err := serverHandshake(conn); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		// Ignore DETACH: the client must give up after its 1 s wait.
		for {
			typ, _, err := ReadFrame(conn)
			if err != nil {
				return
			}
			if typ == FrameDetach {
				detachSeen <- struct{}{}
			}
		}
	})

	restored := 0
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	start := time.Now()
	exit, err := Attach(ctx, ClientOptions{
		SocketPath: path,
		Hello:      Hello{Protocol: ProtocolVersion, Command: "attach"},
		In:         strings.NewReader(""),
		Out:        &out,
		MakeRaw: func() (func(), error) {
			return func() { restored++ }, nil
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if restored != 1 {
		t.Errorf("restore called %d times, want 1", restored)
	}
	if exit.Reason != "detached" {
		t.Errorf("exit = %#v, want detached", exit)
	}
	if elapsed > testload.Scale(3*time.Second) {
		t.Errorf("Attach took %v, want it to give up after the detach wait", elapsed)
	}
	select {
	case <-detachSeen:
	case <-time.After(time.Second):
		t.Error("server never received a DETACH frame")
	}
}

func TestClientSendsResize(t *testing.T) {
	path := testSocketPath(t)
	resizeCh := make(chan Resize, 1)
	detachCh := make(chan struct{}, 1)
	serveOnce(t, path, func(conn net.Conn) {
		if _, err := serverHandshake(conn); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		for {
			typ, payload, err := ReadFrame(conn)
			if err != nil {
				return
			}
			switch typ {
			case FrameResize:
				var r Resize
				if err := json.Unmarshal(payload, &r); err != nil {
					t.Errorf("resize decode: %v", err)
					return
				}
				resizeCh <- r
			case FrameDetach:
				detachCh <- struct{}{}
				WriteJSON(conn, FrameExit, Exit{Reason: "detached", Message: "main"})
				return
			}
		}
	})

	resizes := make(chan struct{})
	hangup := make(chan struct{})
	var cols, rows atomic.Int64
	cols.Store(80)
	rows.Store(24)
	var out safeBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := Attach(context.Background(), ClientOptions{
			SocketPath: path,
			Hello:      Hello{Protocol: ProtocolVersion, Command: "attach"},
			In:         strings.NewReader(""),
			Out:        &out,
			Size:       func() (int, int) { return int(cols.Load()), int(rows.Load()) },
			Resizes:    resizes,
			Hangup:     hangup,
		}); err != nil {
			t.Errorf("Attach: %v", err)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	cols.Store(120)
	rows.Store(40)
	resizes <- struct{}{}
	select {
	case r := <-resizeCh:
		if r.Cols != 120 || r.Rows != 40 {
			t.Errorf("resize = %+v, want 120x40", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a RESIZE frame")
	}
	close(hangup)
	select {
	case <-detachCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a DETACH frame")
	}
	<-done
}

func TestClientHangupSendsDetach(t *testing.T) {
	path := testSocketPath(t)
	detachCh := make(chan struct{}, 1)
	serveOnce(t, path, func(conn net.Conn) {
		if _, err := serverHandshake(conn); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		for {
			typ, _, err := ReadFrame(conn)
			if err != nil {
				return
			}
			if typ == FrameDetach {
				detachCh <- struct{}{}
				WriteJSON(conn, FrameExit, Exit{Reason: "detached", Message: "main"})
				return
			}
		}
	})

	hangup := make(chan struct{})
	close(hangup)
	var out bytes.Buffer
	exit, err := Attach(context.Background(), ClientOptions{
		SocketPath: path,
		Hello:      Hello{Protocol: ProtocolVersion, Command: "attach"},
		In:         strings.NewReader(""),
		Out:        &out,
		Hangup:     hangup,
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if exit.Reason != "detached" {
		t.Errorf("exit = %#v, want detached", exit)
	}
	select {
	case <-detachCh:
	case <-time.After(time.Second):
		t.Error("server never received a DETACH frame")
	}
}

func TestClientHelloRejected(t *testing.T) {
	path := testSocketPath(t)
	serveOnce(t, path, func(conn net.Conn) {
		typ, _, err := ReadFrame(conn)
		if err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		if typ != FrameHello {
			t.Errorf("first frame %#x", typ)
			return
		}
		WriteJSON(conn, FrameExit, Exit{Reason: "error", Message: "no such session"})
	})

	called := false
	var out bytes.Buffer
	exit, err := Attach(context.Background(), ClientOptions{
		SocketPath: path,
		Hello:      Hello{Protocol: ProtocolVersion, Command: "attach", Session: "nope"},
		In:         strings.NewReader(""),
		Out:        &out,
		MakeRaw:    func() (func(), error) { called = true; return func() {}, nil },
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if called {
		t.Error("MakeRaw called although the server rejected HELLO")
	}
	if exit.Reason != "error" || exit.Message != "no such session" {
		t.Errorf("exit = %#v", exit)
	}
	if out.Len() != 0 {
		t.Errorf("out = %q, want nothing before raw mode", out.String())
	}
}

func TestClientDialFailure(t *testing.T) {
	path := testSocketPath(t)
	_, err := Attach(context.Background(), ClientOptions{
		SocketPath: path,
		Hello:      Hello{Protocol: ProtocolVersion, Command: "attach"},
		In:         strings.NewReader(""),
		Out:        io.Discard,
	})
	if err == nil {
		t.Fatal("Attach: want an error for a missing socket")
	}
	if !errs.Is(err, errs.Unreachable) {
		t.Errorf("err = %v, want unreachable", err)
	}
}

func TestControlLs(t *testing.T) {
	path := testSocketPath(t)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	helloCh := make(chan Hello, 1)
	serveOnce(t, path, func(conn net.Conn) {
		typ, payload, err := ReadFrame(conn)
		if err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		if typ != FrameHello {
			t.Errorf("first frame %#x", typ)
			return
		}
		var h Hello
		if err := json.Unmarshal(payload, &h); err != nil {
			t.Errorf("hello decode: %v", err)
			return
		}
		helloCh <- h
		WriteJSON(conn, FrameResult, Result{
			OK: true,
			Sessions: []SessionInfo{
				{Name: "main", Windows: 2, Attached: true, Created: now, Activity: now},
			},
		})
	})

	res, err := Control(context.Background(), path, Hello{Protocol: ProtocolVersion, Command: "ls"})
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	if !res.OK || len(res.Sessions) != 1 || res.Sessions[0].Name != "main" {
		t.Errorf("result = %#v", res)
	}
	if h := <-helloCh; h.Command != "ls" || h.Protocol != ProtocolVersion {
		t.Errorf("hello = %#v", h)
	}
}

func TestControlExitIsError(t *testing.T) {
	path := testSocketPath(t)
	serveOnce(t, path, func(conn net.Conn) {
		if _, _, err := ReadFrame(conn); err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		WriteJSON(conn, FrameExit, Exit{Reason: "error", Message: "no such session"})
	})

	_, err := Control(context.Background(), path, Hello{Protocol: ProtocolVersion, Command: "kill-session", Session: "nope"})
	if err == nil {
		t.Fatal("Control: want an error for an EXIT reply")
	}
	if !errs.Is(err, errs.Protocol) {
		t.Errorf("err = %v, want protocol", err)
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("err = %v, want the server message", err)
	}
}

func TestControlCtxCancel(t *testing.T) {
	path := testSocketPath(t)
	serveOnce(t, path, func(conn net.Conn) {
		// Never answer.
		if _, _, err := ReadFrame(conn); err != nil {
			return
		}
		<-time.After(5 * time.Second)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := Control(ctx, path, Hello{Protocol: ProtocolVersion, Command: "ls"})
	if err == nil {
		t.Fatal("Control: want an error when ctx is canceled")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errs.Is(err, errs.Canceled) && !errs.Is(err, errs.Timeout) {
		t.Errorf("err = %v, want a canceled/deadline error", err)
	}
}

func TestExitMessages(t *testing.T) {
	cases := []struct {
		exit Exit
		want string
	}{
		{Exit{Reason: "detached", Message: "main"}, "[detached (from session main)]"},
		{Exit{Reason: "detached", Message: "db-incident"}, "[detached (from session db-incident)]"},
		{Exit{Reason: "ended"}, "[session ended]"},
		{Exit{Reason: "replaced"}, "[detached: another client attached]"},
		{Exit{Reason: "shutdown"}, "[server exited]"},
		{Exit{Reason: "error", Message: "server connection lost"}, "server connection lost"},
		{Exit{Reason: "error", Message: "protobuf smells"}, "protobuf smells"},
	}
	for _, c := range cases {
		if got := c.exit.UserMessage(); got != c.want {
			t.Errorf("%#v.UserMessage() = %q, want %q", c.exit, got, c.want)
		}
	}
}
