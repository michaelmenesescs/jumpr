package httpx

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
)

const (
	// socks5Version is the SOCKS protocol version this package speaks.
	socks5Version = 0x05
	// socks5HandshakeTimeout bounds one SOCKS handshake when the caller's
	// context has no deadline of its own.
	socks5HandshakeTimeout = 10 * time.Second
)

// dialFunc opens a connection. http.Transport calls it for the target address;
// the proxy dialers below use the same shape so they compose.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// parseProxyURL normalises a proxy address for typ: a missing scheme takes the
// type's own ("http" or "socks5"), and the result must have a host and a port.
func parseProxyURL(raw string, typ ProxyType) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if !strings.Contains(s, "://") {
		switch typ {
		case ProxySOCKS5:
			s = "socks5://" + s
		default:
			s = "http://" + s
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("not a valid address")
	}
	if u.User != nil {
		return nil, fmt.Errorf("credentials must use username and password, not the URL")
	}
	switch typ {
	case ProxyHTTP:
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("scheme %q is not an http proxy", u.Scheme)
		}
	case ProxySOCKS5:
		if u.Scheme != "socks5" && u.Scheme != "socks5h" {
			return nil, fmt.Errorf("scheme %q is not a socks5 proxy", u.Scheme)
		}
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("missing host")
	}
	if u.Port() == "" {
		return nil, fmt.Errorf("missing port")
	}
	return u, nil
}

// socksAddr returns host:port for a socks5 proxy URL.
func socksAddr(u *url.URL) string {
	if u.Port() != "" {
		return net.JoinHostPort(u.Hostname(), u.Port())
	}
	return net.JoinHostPort(u.Hostname(), "1080")
}

// envValue returns the first non-empty value among keys.
func envValue(env map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(env[k]); v != "" {
			return v
		}
	}
	return ""
}

// proxyEnvURL picks the proxy for u from the environment map: HTTPS_PROXY for
// https targets, HTTP_PROXY for the rest, ALL_PROXY as a fallback, and NO_PROXY
// to bypass. It reads only the map it is given (BUILD-PLAN §6.1).
func proxyEnvURL(env map[string]string, u *url.URL) (*url.URL, error) {
	if u == nil {
		return nil, nil
	}
	if bypassProxy(u.Host, envValue(env, "NO_PROXY", "no_proxy")) {
		return nil, nil
	}
	var raw string
	if u.Scheme == "https" {
		raw = envValue(env, "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy")
	} else {
		raw = envValue(env, "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy")
	}
	if raw == "" {
		return nil, nil
	}
	s := raw
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	pu, err := url.Parse(s)
	if err != nil || pu.Host == "" {
		return nil, &errs.Error{Kind: errs.Config, Op: "httpx.proxy", Msg: "invalid proxy in the environment"}
	}
	return pu, nil
}

// bypassProxy reports whether hostport matches a NO_PROXY list: "*", a
// host:port entry, a bare host, or a domain suffix (".example.internal" or
// "example.internal" matching subdomains).
func bypassProxy(hostport, noProxy string) bool {
	if strings.TrimSpace(noProxy) == "" || hostport == "" {
		return false
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	for _, entry := range strings.Split(noProxy, ",") {
		e := strings.ToLower(strings.TrimSpace(entry))
		if e == "" {
			continue
		}
		if e == "*" {
			return true
		}
		if strings.EqualFold(e, hostport) {
			return true
		}
		eHost := e
		if h, p, err := net.SplitHostPort(e); err == nil {
			if p != "" && !strings.HasSuffix(hostport, ":"+p) {
				continue
			}
			eHost = h
		}
		eHost = strings.TrimPrefix(strings.TrimPrefix(eHost, "*"), ".")
		if eHost == "" {
			continue
		}
		if host == eHost || strings.HasSuffix(host, "."+eHost) {
			return true
		}
	}
	return false
}

// socks5Dial returns a dialer that reaches addr through a SOCKS5 proxy at
// proxyAddr, optionally authenticating with RFC 1929 username/password.
func socks5Dial(base dialFunc, proxyAddr, username, password string) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, &errs.Error{Kind: errs.Config, Op: "httpx.proxy", Msg: "socks5 supports tcp only"}
		}
		c, err := base(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, err
		}
		if err := socks5Connect(ctx, c, addr, username, password); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	}
}

// socks5Connect performs the greeting, optional authentication and CONNECT
// handshake on c, then clears the handshake deadline.
func socks5Connect(ctx context.Context, c net.Conn, addr, username, password string) error {
	deadline := time.Now().Add(socks5HandshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := c.SetDeadline(deadline); err != nil {
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "cannot set the handshake deadline", Err: err}
	}
	defer func() { _ = c.SetDeadline(time.Time{}) }()

	methods := []byte{0x00}
	if username != "" {
		methods = append(methods, 0x02)
	}
	greeting := append([]byte{socks5Version, byte(len(methods))}, methods...)
	if _, err := c.Write(greeting); err != nil {
		return socks5IOError(ctx, err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return socks5IOError(ctx, err)
	}
	if reply[0] != socks5Version {
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the proxy does not speak socks5"}
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		if username == "" {
			return &errs.Error{Kind: errs.Auth, Op: "httpx.socks5", Msg: "the proxy requires a username and password"}
		}
		if err := socks5Auth(c, username, password); err != nil {
			return err
		}
	default:
		return &errs.Error{Kind: errs.Auth, Op: "httpx.socks5", Msg: "the proxy accepted none of the offered authentication methods"}
	}

	host, port, err := splitTarget(addr)
	if err != nil {
		return err
	}
	req := []byte{socks5Version, 0x01, 0x00}
	switch ip := net.ParseIP(host); {
	case ip == nil:
		if len(host) > 255 {
			return &errs.Error{Kind: errs.Config, Op: "httpx.socks5", Msg: "target host name is too long"}
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	case ip.To4() != nil:
		req = append(req, 0x01)
		req = append(req, ip.To4()...)
	default:
		req = append(req, 0x04)
		req = append(req, ip.To16()...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return socks5IOError(ctx, err)
	}
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return socks5IOError(ctx, err)
	}
	if head[0] != socks5Version {
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the proxy does not speak socks5"}
	}
	// Skip the bound address; only its length depends on the type byte.
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return socks5IOError(ctx, err)
		}
		skip = int(l[0])
	default:
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the proxy sent an unknown address type"}
	}
	if _, err := io.ReadFull(c, make([]byte, skip+2)); err != nil {
		return socks5IOError(ctx, err)
	}
	switch head[1] {
	case 0x00:
		return nil
	case 0x02:
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the proxy refused the connection"}
	case 0x03:
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the network is unreachable"}
	case 0x04:
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the host is unreachable"}
	case 0x05:
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the proxy refused the connection: connection refused"}
	case 0x06:
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the proxy reports a ttl expiry"}
	case 0x07:
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the proxy does not support the command"}
	case 0x08:
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "the proxy does not support the address type"}
	default:
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: fmt.Sprintf("the proxy rejected the request (code %d)", head[1])}
	}
}

// socks5Auth performs the RFC 1929 username/password subnegotiation.
func socks5Auth(c net.Conn, username, password string) error {
	if len(username) > 255 || len(password) > 255 {
		return &errs.Error{Kind: errs.Config, Op: "httpx.socks5", Msg: "socks5 credentials are too long"}
	}
	buf := []byte{0x01, byte(len(username))}
	buf = append(buf, username...)
	buf = append(buf, byte(len(password)))
	buf = append(buf, password...)
	if _, err := c.Write(buf); err != nil {
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "cannot send the socks5 credentials", Err: err}
	}
	var reply [2]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "cannot read the socks5 auth reply", Err: err}
	}
	if reply[1] != 0x00 {
		return &errs.Error{Kind: errs.Auth, Op: "httpx.socks5", Msg: "the proxy rejected the socks5 credentials"}
	}
	return nil
}

// socks5IOError classifies a handshake I/O failure: a deadline is a timeout,
// anything else means the proxy is not usable.
func socks5IOError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctxError(ctx.Err())
	}
	var ne net.Error
	if ok := asNetError(err, &ne); ok && ne.Timeout() {
		return &errs.Error{Kind: errs.Timeout, Op: "httpx.socks5", Msg: "the socks5 handshake timed out", Err: err}
	}
	return &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "socks5 handshake failed", Err: err}
}

// splitTarget splits host:port and returns a numeric port.
func splitTarget(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "cannot parse the target address", Err: err}
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", 0, &errs.Error{Kind: errs.Unreachable, Op: "httpx.socks5", Msg: "cannot parse the target port", Err: err}
	}
	return host, int(port), nil
}
