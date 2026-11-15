package ssh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"jumpr/internal/errs"
)

const (
	// defaultConnectTimeout is the SSH connect timeout from BUILD-PLAN §8.
	defaultConnectTimeout = 10 * time.Second
	// defaultKeepalive is the config default for ssh.keepalive.
	defaultKeepalive = 30 * time.Second
	// keepaliveTimeout bounds one keepalive request; no reply means the peer
	// is gone.
	keepaliveTimeout = 10 * time.Second
	// keepaliveFailures is how many consecutive failed keepalives declare a
	// connection dead.
	keepaliveFailures = 2
	// keepaliveRequest is OpenSSH's liveness probe; sshd answers it.
	keepaliveRequest = "keepalive@openssh.com"
)

// DialOptions configures DialDirect and DialVia.
type DialOptions struct {
	Auth           AuthOptions
	HostKeys       HostKeyOptions
	ConnectTimeout time.Duration
	Keepalive      time.Duration
	Log            *slog.Logger
}

// clientConfig builds the ssh client configuration for spec, its auth cleanup
// (closes the agent connection; call it once the handshake is over) and a
// config error when the host cannot be dialled by this transport at all.
func clientConfig(spec HostSpec, o DialOptions) (*ssh.ClientConfig, func(), error) {
	host := specHost(spec)
	if strings.TrimSpace(spec.ProxyCommand) != "" {
		return nil, func() {}, errs.New(errs.Config, "", fmt.Sprintf(
			"ProxyCommand is not supported by the native transport; use transport: system-ssh for %s", host))
	}

	cb, algos, err := HostKeyConfig(spec, o.HostKeys)
	if err != nil {
		return nil, func() {}, err
	}
	methods, cleanup := AuthMethods(spec, o.Auth)

	cfg := &ssh.ClientConfig{
		User:              spec.User,
		Auth:              methods,
		HostKeyCallback:   cb,
		HostKeyAlgorithms: algos,
		Timeout:           connectTimeout(spec, o),
	}
	return cfg, cleanup, nil
}

// DialDirect dials spec over TCP and completes the SSH handshake. The
// connection and the handshake are bounded by o.ConnectTimeout (falling back to
// the ssh_config ConnectTimeout, then 10s) and by ctx.
func DialDirect(ctx context.Context, spec HostSpec, o DialOptions) (*ssh.Client, error) {
	host := specHost(spec)
	cfg, cleanup, err := clientConfig(spec, o)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	addr := dialAddr(spec)
	timeout := connectTimeout(spec, o)

	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, classifyDial(cerr, host)
		}
		return nil, classifyDial(err, host)
	}
	client, err := handshake(ctx, conn, addr, cfg, timeout)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, classifyDial(cerr, host)
		}
		return nil, classifyDial(err, host)
	}
	logDial(o, host, "")
	return client, nil
}

// DialVia opens a direct-tcpip channel to spec through the bastion client via
// and completes the SSH handshake over it. The channel open runs in a goroutine
// so ctx can abandon it; a channel that arrives after the abandon is closed.
func DialVia(ctx context.Context, via *ssh.Client, spec HostSpec, o DialOptions) (*ssh.Client, error) {
	host := specHost(spec)
	if via == nil {
		return nil, errs.New(errs.Internal, "ssh.dial", "no bastion connection for "+host).WithHost(host)
	}
	cfg, cleanup, err := clientConfig(spec, o)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	addr := dialAddr(spec)
	timeout := connectTimeout(spec, o)

	type opened struct {
		conn net.Conn
		err  error
	}
	ready := make(chan opened, 1)
	go func() {
		conn, err := via.Dial("tcp", addr)
		if err != nil {
			select {
			case ready <- opened{err: err}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case ready <- opened{conn: conn}:
		case <-ctx.Done():
			_ = conn.Close() // arrived too late; nobody will read it
		}
	}()

	var res opened
	select {
	case res = <-ready:
	case <-ctx.Done():
		return nil, classifyDial(ctx.Err(), host)
	}
	if res.err != nil {
		return nil, classifyDial(res.err, host)
	}

	client, err := handshake(ctx, res.conn, addr, cfg, timeout)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, classifyDial(cerr, host)
		}
		return nil, classifyDial(err, host)
	}
	logDial(o, host, viaAddr(via))
	return client, nil
}

// handshake runs ssh.NewClientConn over conn with the connect deadline applied,
// so the handshake itself respects the timeout, and clears the deadline
// afterwards. The conn is closed on every error path.
func handshake(ctx context.Context, conn net.Conn, addr string, cfg *ssh.ClientConfig, timeout time.Duration) (*ssh.Client, error) {
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	// Best effort: a direct TCP conn honours the deadline; a channel opened
	// through a bastion (ssh's chanConn) does not support deadlines at all, so
	// the timer below is what enforces the timeout there.
	_ = conn.SetDeadline(deadline)

	// Neither a deadline nor a canceled context can interrupt a blocked read
	// on every conn type, but closing the conn can.
	var timedOut atomic.Bool
	timer := time.AfterFunc(time.Until(deadline), func() {
		timedOut.Store(true)
		_ = conn.Close()
	})
	defer timer.Stop()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		// The conn may have been closed by the deadline timer or by ctx, which
		// surfaces as a read error rather than a timeout.
		if timedOut.Load() || !time.Now().Before(deadline) {
			return nil, context.DeadlineExceeded
		}
		return nil, err
	}
	if !timer.Stop() {
		// The timeout fired as the handshake finished; the conn is closing.
		_ = c.Close()
		return nil, context.DeadlineExceeded
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// startKeepalive probes c every interval with keepalive@openssh.com until ctx
// ends. Two consecutive failures, or a request with no reply within 10s, close
// the client and call onDead. A reply that is merely a failure still proves the
// peer is alive and does not count.
func startKeepalive(ctx context.Context, c *ssh.Client, every time.Duration, onDead func()) {
	if c == nil {
		return
	}
	if every <= 0 {
		every = defaultKeepalive
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		failures := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			err := keepaliveOnce(ctx, c)
			if err == nil {
				failures = 0
				continue
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			failures++
			if failures >= keepaliveFailures || errors.Is(err, errKeepaliveTimeout) {
				_ = c.Close()
				if onDead != nil {
					onDead()
				}
				return
			}
		}
	}()
}

// errKeepaliveTimeout is a keepalive the peer never answered.
var errKeepaliveTimeout = errors.New("ssh: keepalive reply timeout")

// keepaliveOnce sends one keepalive request and waits for the reply.
func keepaliveOnce(ctx context.Context, c *ssh.Client) error {
	reply := make(chan error, 1)
	go func() {
		// ok is false for a server that does not know the request; the round
		// trip itself is what matters.
		_, _, err := c.SendRequest(keepaliveRequest, true, nil)
		reply <- err
	}()
	timer := time.NewTimer(keepaliveTimeout)
	defer timer.Stop()
	select {
	case err := <-reply:
		return err
	case <-timer.C:
		return errKeepaliveTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

// classifyDial turns a dial or handshake error into a *errs.Error with the kind
// from BUILD-PLAN §7. Errors raised by the host key callback keep their kind
// and are recovered with errors.As from the handshake wrapper.
func classifyDial(err error, host string) *errs.Error {
	if err == nil {
		return nil
	}
	var already *errs.Error
	if errors.As(err, &already) {
		// Already classified (host key callback, config): its kind wins and the
		// value is returned as-is so callers can errors.Is against it.
		return already
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.Timeout, "ssh.dial", err).WithHost(host)
	}
	if errors.Is(err, context.Canceled) {
		return errs.Wrap(errs.Canceled, "ssh.dial", err).WithHost(host)
	}

	msg := strings.ToLower(err.Error())
	switch {
	case isTimeout(err), strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline exceeded"):
		return errs.Wrap(errs.Timeout, "ssh.dial", err).WithHost(host)
	case strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "no supported methods remain"):
		return errs.Wrap(errs.Auth, "ssh.dial", err).WithHost(host)
	case strings.Contains(msg, "key mismatch"), strings.Contains(msg, "key is unknown"):
		return errs.Wrap(errs.HostKey, "ssh.dial", err).WithHost(host)
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no route to host"),
		strings.Contains(msg, "no such host"), strings.Contains(msg, "administratively prohibited"),
		strings.Contains(msg, "connect failed"), strings.Contains(msg, "network is unreachable"):
		return errs.Wrap(errs.Unreachable, "ssh.dial", err).WithHost(host)
	default:
		// Anything else is a bug in the dial path, not a statement about the
		// host: errs.Internal is what the UI shows for unclassified failures.
		return errs.Wrap(errs.Internal, "ssh.dial", err).WithHost(host)
	}
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// specHost is the name used in messages: the ssh_config alias when there is
// one, because that is what a `transport: system-ssh` pattern matches.
func specHost(spec HostSpec) string {
	if spec.Alias != "" {
		return spec.Alias
	}
	if spec.HostName != "" {
		return spec.HostName
	}
	return "unknown host"
}

// dialAddr is host:port, defaulting the port to 22.
func dialAddr(spec HostSpec) string {
	port := spec.Port
	if port <= 0 {
		port = 22
	}
	return net.JoinHostPort(spec.HostName, strconv.Itoa(port))
}

// connectTimeout is o.ConnectTimeout, else the ssh_config ConnectTimeout, else
// the 10s default.
func connectTimeout(spec HostSpec, o DialOptions) time.Duration {
	if o.ConnectTimeout > 0 {
		return o.ConnectTimeout
	}
	if spec.ConnectTimeout > 0 {
		return spec.ConnectTimeout
	}
	return defaultConnectTimeout
}

// viaAddr names the bastion in a log line: its configured address, if known.
func viaAddr(c *ssh.Client) string {
	if c == nil {
		return ""
	}
	if a := c.RemoteAddr(); a != nil {
		return a.String()
	}
	return ""
}

func logDial(o DialOptions, host, via string) {
	if o.Log == nil {
		return
	}
	attrs := []any{"host", host}
	if via != "" {
		attrs = append(attrs, "via", via)
	}
	o.Log.Debug("ssh connected", attrs...)
}
