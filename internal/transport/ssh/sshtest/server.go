// Package sshtest provides an in-process SSH server for tests. It behaves
// enough like sshd to exercise dialing, bastions, session limits, exec,
// streams and PTYs over loopback only.
//
// It is test scaffolding: nothing in it is used by the product binary.
package sshtest

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Options configures a test server.
type Options struct {
	// Name is the SSH server version string ("SSH-2.0-<Name>"); empty means
	// "sshtest".
	Name string
	// AuthorizedKeys is the set of accepted public keys. Empty accepts any
	// key; password and keyboard-interactive auth are never accepted.
	AuthorizedKeys []gossh.PublicKey
	// Handler runs an exec request. cmd is the raw (unsplit) command string;
	// the returned value is the remote exit status. A nil Handler behaves like
	// CommandTable(nil): every command fails with "not found" and status 127.
	Handler func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int
	// MaxSessions caps concurrently open session channels per connection
	// (sshd's MaxSessions). Zero means unlimited.
	MaxSessions int
	// AllowForward accepts direct-tcpip channels and proxies them to the
	// requested address, so the server can act as a bastion.
	AllowForward bool
	// ConnDelay is injected before the SSH handshake of every connection.
	ConnDelay time.Duration
	// HostKeyType selects the generated host key: "ed25519" (default) or "rsa".
	HostKeyType string
}

// Server is a running test SSH server. All methods are safe for concurrent use
// and may be called from any goroutine.
type Server struct {
	opts    Options
	srv     *ssh.Server
	ln      net.Listener
	addr    string
	hostKey gossh.PublicKey
	done    chan struct{}
	once    sync.Once

	mu                    sync.Mutex
	sessions              int
	activeSessions        int
	maxConcurrentSessions int
	conns                 int
	activeChannels        int
	maxConcurrentChannels int
	perConn               map[*gossh.ServerConn]int
}

// Start listens on 127.0.0.1:0, generates a host key and serves until the
// server is killed. The server is stopped automatically in t.Cleanup.
func Start(t testing.TB, o Options) *Server {
	t.Helper()
	signer, err := hostSigner(o.HostKeyType)
	if err != nil {
		t.Fatalf("sshtest: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sshtest: listen: %v", err)
	}

	s := &Server{
		opts:    o,
		ln:      ln,
		addr:    ln.Addr().String(),
		hostKey: signer.PublicKey(),
		done:    make(chan struct{}),
		perConn: make(map[*gossh.ServerConn]int),
	}
	version := o.Name
	if version == "" {
		version = "sshtest"
	}
	srv := &ssh.Server{
		Version: version,
		PublicKeyHandler: func(_ ssh.Context, key ssh.PublicKey) bool {
			return s.authorized(key)
		},
		ConnCallback: s.connCallback,
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session": s.sessionHandler,
		},
	}
	srv.AddHostKey(signer)
	if o.AllowForward {
		srv.ChannelHandlers["direct-tcpip"] = s.forwardHandler
		srv.LocalPortForwardingCallback = func(ssh.Context, string, uint32) bool { return true }
	}
	s.srv = srv

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(s.Kill)
	return s
}

// Addr returns the listening address, "127.0.0.1:port".
func (s *Server) Addr() string { return s.addr }

// Port returns the listening TCP port.
func (s *Server) Port() int {
	_, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}

// HostKey returns the server's public host key.
func (s *Server) HostKey() gossh.PublicKey { return s.hostKey }

// KnownHostsLine returns the host key as a known_hosts entry (no trailing
// newline), e.g. "[127.0.0.1]:34567 ssh-ed25519 AAAA...".
func (s *Server) KnownHostsLine() string {
	line := strings.TrimRight(string(gossh.MarshalAuthorizedKey(s.hostKey)), "\r\n")
	return fmt.Sprintf("[127.0.0.1]:%d %s", s.Port(), line)
}

// Sessions returns the total number of session channels the server accepted.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions
}

// MaxConcurrentSessions returns the highest number of simultaneously open
// session channels seen.
func (s *Server) MaxConcurrentSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxConcurrentSessions
}

// Conns returns the total number of TCP connections that reached the SSH
// handshake (a connection dropped by ConnDelay is not counted).
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// MaxConcurrentChannels returns the highest number of simultaneously open
// direct-tcpip (forwarded) channels seen.
func (s *Server) MaxConcurrentChannels() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxConcurrentChannels
}

// Kill closes the listener and every open connection, as sshd does when it goes
// away. It is safe to call more than once. The address stays unchanged, so a
// dial after Kill gets connection refused — that is how reconnect logic is
// tested.
func (s *Server) Kill() {
	s.once.Do(func() {
		close(s.done)
		_ = s.srv.Close()
	})
}

// CommandTable builds a Handler from a command → output map. The longest key
// that is a substring of the raw command wins; its value is written to stdout
// verbatim and the status is 0. With no match it writes "not found" to stderr
// and returns 127, like a shell that cannot find a command.
func CommandTable(m map[string]string) func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			keys = append(keys, k)
		}
	}
	// Longest first; ties broken lexicographically so the table is deterministic.
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return func(cmd string, _ io.Reader, stdout, stderr io.Writer) int {
		for _, k := range keys {
			if strings.Contains(cmd, k) {
				_, _ = io.WriteString(stdout, m[k])
				return 0
			}
		}
		_, _ = io.WriteString(stderr, "not found\n")
		return 127
	}
}

func hostSigner(kind string) (gossh.Signer, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "ed25519":
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate ed25519 host key: %w", err)
		}
		return gossh.NewSignerFromKey(priv)
	case "rsa":
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("generate rsa host key: %w", err)
		}
		return gossh.NewSignerFromKey(priv)
	default:
		return nil, fmt.Errorf("unsupported host key type %q", kind)
	}
}

func (s *Server) authorized(key gossh.PublicKey) bool {
	if len(s.opts.AuthorizedKeys) == 0 {
		return true
	}
	for _, want := range s.opts.AuthorizedKeys {
		if want != nil && ssh.KeysEqual(want, key) {
			return true
		}
	}
	return false
}

func (s *Server) connCallback(ctx ssh.Context, conn net.Conn) net.Conn {
	if d := s.opts.ConnDelay; d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil
		case <-s.done:
			return nil
		}
	}
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()
	return conn
}

// winchPayload is a window-change request (RFC 4254 §6.7).
type winchPayload struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

// ptyRequest is a pty-req request (RFC 4254 §6.2).
type ptyRequest struct {
	Term    string
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
	Modes   string
}

// channelSession is the state of one session channel. The request loop fills it
// in before the handler goroutine starts, so the handler reads it only once and
// needs no lock.
type channelSession struct {
	hasPTY  bool
	started bool
	cmd     string
	winch   chan winchPayload
}

// sessionHandler serves "session" channels itself instead of delegating to
// gliderlabs' DefaultSessionHandler: that handler races on window-change under
// -race and stalls its request loop when nothing is draining the size channel.
func (s *Server) sessionHandler(_ *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, _ ssh.Context) {
	if !s.acquireSession(conn) {
		_ = newChan.Reject(gossh.Prohibited, "max sessions")
		return
	}
	defer s.releaseSession(conn)

	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	sess := &channelSession{}
	for req := range reqs {
		switch req.Type {
		case "env", "signal", "break":
			req.Reply(true, nil)
		case "pty-req":
			if sess.hasPTY || sess.started {
				req.Reply(false, nil)
				continue
			}
			var p ptyRequest
			if err := gossh.Unmarshal(req.Payload, &p); err != nil {
				req.Reply(false, nil)
				continue
			}
			sess.hasPTY = true
			sess.winch = make(chan winchPayload, 8)
			req.Reply(true, nil)
		case "window-change":
			var w winchPayload
			if !sess.hasPTY || gossh.Unmarshal(req.Payload, &w) != nil {
				req.Reply(false, nil)
				continue
			}
			select {
			case sess.winch <- w:
			default: // a client resizing faster than the shell drains
			}
			req.Reply(true, nil)
		case "shell", "exec":
			if sess.started {
				req.Reply(false, nil)
				continue
			}
			sess.started = true
			if req.Type == "exec" {
				var payload struct{ Value string }
				_ = gossh.Unmarshal(req.Payload, &payload)
				sess.cmd = payload.Value
			}
			req.Reply(true, nil)
			go func() {
				status := 0
				if sess.cmd != "" {
					h := s.opts.Handler
					if h == nil {
						h = CommandTable(nil)
					}
					status = h(sess.cmd, ch, ch, ch.Stderr())
				} else {
					status = serveShell(ch, sess.winch)
				}
				_ = sendExitStatus(ch, status)
				_ = ch.Close()
			}()
		default:
			req.Reply(false, nil)
		}
	}
}

func sendExitStatus(ch gossh.Channel, status int) error {
	payload := struct{ Status uint32 }{uint32(status)}
	_, err := ch.SendRequest("exit-status", false, gossh.Marshal(&payload))
	return err
}

// serveShell is the stand-in login shell for a session without a command: it
// echoes every line back, reports window changes as "resize <cols>x<rows>" and
// leaves with status 0 on "exit".
func serveShell(ch gossh.Channel, winch <-chan winchPayload) int {
	type line struct {
		text string
		err  error
	}
	lines := make(chan line, 1)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		br := bufio.NewReader(ch)
		for {
			text, err := br.ReadString('\n')
			if text != "" || err != nil {
				select {
				case lines <- line{text: text, err: err}:
				case <-quit:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case w := <-winch: // nil when no PTY was requested: never ready
			fmt.Fprintf(ch, "resize %dx%d\n", w.Columns, w.Rows)
		case ln := <-lines:
			if text := strings.TrimRight(ln.text, "\r\n"); text != "" {
				if text == "exit" {
					return 0
				}
				_, _ = io.WriteString(ch, ln.text)
			}
			if ln.err != nil {
				return 0
			}
		}
	}
}

// forwardHandler accepts a direct-tcpip channel and proxies it. It mirrors
// gliderlabs' handler but waits for the tunnel to finish, so the channel
// counter spans the whole life of the forward.
func (s *Server) forwardHandler(_ *ssh.Server, _ *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
	var req struct {
		DestAddr   string
		DestPort   uint32
		OriginAddr string
		OriginPort uint32
	}
	if err := gossh.Unmarshal(newChan.ExtraData(), &req); err != nil {
		_ = newChan.Reject(gossh.ConnectionFailed, "error parsing forward data: "+err.Error())
		return
	}
	if !s.opts.AllowForward {
		_ = newChan.Reject(gossh.Prohibited, "port forwarding is disabled")
		return
	}

	dest := net.JoinHostPort(req.DestAddr, strconv.FormatUint(uint64(req.DestPort), 10))
	dconn, err := (&net.Dialer{}).DialContext(ctx, "tcp", dest)
	if err != nil {
		_ = newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}

	ch, chReqs, err := newChan.Accept()
	if err != nil {
		_ = dconn.Close()
		return
	}
	go gossh.DiscardRequests(chReqs)

	s.channelStart()
	defer s.channelEnd()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer ch.Close()
		defer dconn.Close()
		_, _ = io.Copy(ch, dconn)
	}()
	go func() {
		defer wg.Done()
		defer ch.Close()
		defer dconn.Close()
		_, _ = io.Copy(dconn, ch)
	}()
	wg.Wait()
}

func (s *Server) acquireSession(conn *gossh.ServerConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opts.MaxSessions > 0 && s.perConn[conn] >= s.opts.MaxSessions {
		return false
	}
	s.perConn[conn]++
	s.sessions++
	s.activeSessions++
	if s.activeSessions > s.maxConcurrentSessions {
		s.maxConcurrentSessions = s.activeSessions
	}
	return true
}

func (s *Server) releaseSession(conn *gossh.ServerConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perConn[conn]--
	if s.perConn[conn] <= 0 {
		delete(s.perConn, conn)
	}
	s.activeSessions--
}

func (s *Server) channelStart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeChannels++
	if s.activeChannels > s.maxConcurrentChannels {
		s.maxConcurrentChannels = s.activeChannels
	}
}

func (s *Server) channelEnd() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeChannels--
}
