package httpx

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	sshpool "jumpr/internal/transport/ssh"
	"jumpr/internal/transport/ssh/sshtest"
)

// connectProxy is a minimal HTTP CONNECT proxy: it records the CONNECT requests
// it saw and tunnels the connection.
type connectProxy struct {
	url   string
	ln    net.Listener
	mu    sync.Mutex
	seen  []http.Header
	count int32
}

func startConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &connectProxy{ln: ln, url: "http://" + ln.Addr().String()}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *connectProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *connectProxy) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		_, _ = io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}
	atomic.AddInt32(&p.count, 1)
	p.mu.Lock()
	p.seen = append(p.seen, req.Header.Clone())
	p.mu.Unlock()

	up, err := net.Dial("tcp", req.Host)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

// headerAfter returns the CONNECT headers of the nth request, or nil.
func (p *connectProxy) headerAfter(n int) http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) <= n {
		return nil
	}
	return p.seen[n]
}

func TestProxyHTTPConnect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "through the proxy")
	}))
	defer srv.Close()
	proxy := startConnectProxy(t)

	p := &Profile{
		Proxy: Proxy{
			Type:    ProxyHTTP,
			URL:     proxy.url,
			Headers: map[string]string{"Proxy-Authorization": "Basic cHJveHk="},
		},
		TLS: TLSConfig{CAFile: caFile(t, srv)},
	}
	c := newClient(t, p, Deps{})
	_, body, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if body != "through the proxy" {
		t.Fatalf("body = %q", body)
	}
	if got := atomic.LoadInt32(&proxy.count); got != 1 {
		t.Fatalf("CONNECT requests = %d, want 1", got)
	}
	h := proxy.headerAfter(0)
	if h == nil || h.Get("Proxy-Authorization") != "Basic cHJveHk=" {
		t.Fatalf("proxy headers = %v", h)
	}
}

func TestProxyEnv(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	proxy := startConnectProxy(t)

	p := &Profile{Proxy: Proxy{Type: ProxyEnv}, TLS: TLSConfig{CAFile: caFile(t, srv)}}
	c := newClient(t, p, Deps{Env: map[string]string{"HTTPS_PROXY": proxy.url}})
	if _, _, err := get(t, c, srv.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := atomic.LoadInt32(&proxy.count); got != 1 {
		t.Fatalf("CONNECT requests = %d, want 1", got)
	}

	// NO_PROXY bypasses the proxy for the same target.
	bypass := newClient(t, &Profile{Proxy: Proxy{Type: ProxyEnv}, TLS: TLSConfig{CAFile: caFile(t, srv)}}, Deps{
		Env: map[string]string{"HTTPS_PROXY": proxy.url, "NO_PROXY": "127.0.0.1"},
	})
	if _, _, err := get(t, bypass, srv.URL); err != nil {
		t.Fatalf("get with NO_PROXY: %v", err)
	}
	if got := atomic.LoadInt32(&proxy.count); got != 1 {
		t.Fatalf("CONNECT requests = %d, want the bypassed request to go direct", got)
	}
}

func TestProxyEnvWithoutProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	defer srv.Close()

	p := &Profile{}
	c := newClient(t, p, Deps{Env: map[string]string{}})
	_, body, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if body != "direct" {
		t.Fatalf("body = %q", body)
	}
}

func TestBypassProxy(t *testing.T) {
	cases := []struct {
		hostport, noProxy string
		want              bool
	}{
		{"app.example.internal:443", "", false},
		{"app.example.internal:443", "*", true},
		{"app.example.internal:443", ".example.internal", true},
		{"app.example.internal:443", "example.internal", true},
		{"app.example.internal:443", "other.internal", false},
		{"app.example.internal:443", ".internal", true},
		{"app.example.internal:443", "app.example.internal", true},
		{"app.example.internal:443", "app.example.internal:443", true},
		{"app.example.internal:443", "app.example.internal:80", false},
		{"127.0.0.1:8080", "127.0.0.1", true},
	}
	for _, tc := range cases {
		if got := bypassProxy(tc.hostport, tc.noProxy); got != tc.want {
			t.Fatalf("bypassProxy(%q, %q) = %v, want %v", tc.hostport, tc.noProxy, got, tc.want)
		}
	}
}

// socksProxy is a minimal SOCKS5 server: greeting, optional RFC 1929 auth and
// CONNECT, then a plain tunnel.
type socksProxy struct {
	ln       net.Listener
	addr     string
	user     string
	password string
	count    int32
	mu       sync.Mutex
	targets  []string
}

func startSOCKS5(t *testing.T, user, password string) *socksProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks listen: %v", err)
	}
	p := &socksProxy{ln: ln, addr: ln.Addr().String(), user: user, password: password}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *socksProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *socksProxy) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)

	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return
	}
	if head[0] != socks5Version {
		return
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if p.user != "" {
		ok := false
		for _, m := range methods {
			if m == 0x02 {
				ok = true
			}
		}
		if !ok {
			_, _ = c.Write([]byte{0x05, 0xFF})
			return
		}
		if _, err := c.Write([]byte{0x05, 0x02}); err != nil {
			return
		}
		var auth [2]byte
		if _, err := io.ReadFull(br, auth[:]); err != nil || auth[0] != 0x01 {
			return
		}
		user := make([]byte, auth[1])
		if _, err := io.ReadFull(br, user); err != nil {
			return
		}
		var plen [1]byte
		if _, err := io.ReadFull(br, plen[:]); err != nil {
			return
		}
		pass := make([]byte, plen[0])
		if _, err := io.ReadFull(br, pass); err != nil {
			return
		}
		if string(user) != p.user || string(pass) != p.password {
			_, _ = c.Write([]byte{0x01, 0x01})
			return
		}
		if _, err := c.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	} else if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil || req[1] != 0x01 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(br, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(br, name); err != nil {
			return
		}
		host = string(name)
	default:
		_, _ = c.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	var port [2]byte
	if _, err := io.ReadFull(br, port[:]); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1])))

	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	atomic.AddInt32(&p.count, 1)
	p.mu.Lock()
	p.targets = append(p.targets, target)
	p.mu.Unlock()

	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func TestProxySOCKS5(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "through socks")
	}))
	defer srv.Close()
	proxy := startSOCKS5(t, "", "")

	p := &Profile{Proxy: Proxy{Type: ProxySOCKS5, URL: "socks5://" + proxy.addr}}
	c := newClient(t, p, Deps{})
	_, body, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if body != "through socks" {
		t.Fatalf("body = %q", body)
	}
	if got := atomic.LoadInt32(&proxy.count); got != 1 {
		t.Fatalf("socks CONNECTs = %d, want 1", got)
	}
}

func TestProxySOCKS5Auth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	proxy := startSOCKS5(t, "jumpr", "pr0xy")

	p := &Profile{Proxy: Proxy{Type: ProxySOCKS5, URL: "socks5://" + proxy.addr, Username: "jumpr", Password: "pr0xy"}}
	c := newClient(t, p, Deps{})
	if _, _, err := get(t, c, srv.URL); err != nil {
		t.Fatalf("get with socks5 auth: %v", err)
	}

	bad := &Profile{Proxy: Proxy{Type: ProxySOCKS5, URL: "socks5://" + proxy.addr, Username: "jumpr", Password: "wrong"}}
	c = newClient(t, bad, Deps{})
	_, _, err := get(t, c, srv.URL)
	if err == nil {
		t.Fatal("wrong socks5 credentials were accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Auth {
		t.Fatalf("kind = %q, want auth (%v)", kind, err)
	}
}

// sshFixture is one sshtest bastion with a pool that dials it.
type sshFixture struct {
	server *sshtest.Server
	pool   *sshpool.Pool
	host   resource.HostID
}

func newSSHFixture(t *testing.T) *sshFixture {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	srv := sshtest.Start(t, sshtest.Options{
		AllowForward:   true,
		AuthorizedKeys: []gossh.PublicKey{signer.PublicKey()},
	})
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}

	const alias = "bastion.example.internal"
	cfgPath := filepath.Join(dir, "ssh_config")
	cfg := fmt.Sprintf("Host %s\n  HostName 127.0.0.1\n  Port %d\n  User tester\n  IdentityFile %s\n"+
		"  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n  ConnectTimeout 5\n",
		alias, srv.Port(), keyFile, known)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write ssh_config: %v", err)
	}
	res, err := sshpool.NewResolver([]string{cfgPath}, dir, "tester")
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	pool := sshpool.NewPool(sshpool.PoolOptions{
		Resolver: res,
		Dial:     sshpool.DialOptions{ConnectTimeout: 5 * time.Second},
	})
	t.Cleanup(func() { _ = pool.Close() })
	return &sshFixture{server: srv, pool: pool, host: resource.HostID(alias)}
}

func TestPoolDialContext(t *testing.T) {
	f := newSSHFixture(t)

	// A raw TCP echo server is enough to prove the forwarded channel works.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := f.pool.DialContext(ctx, f.host, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if st := f.pool.Stats(); st.Leases != 0 {
		t.Fatalf("leases after Close = %d, want 0", st.Leases)
	}
	if got := f.server.MaxConcurrentChannels(); got < 1 {
		t.Fatalf("the forwarded channel did not reach the server (max channels = %d)", got)
	}
	if _, err := f.pool.DialContext(ctx, f.host, "udp", ln.Addr().String()); err == nil {
		t.Fatal("a udp dial was accepted")
	}
}

func TestSSHSOCKS(t *testing.T) {
	f := newSSHFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "through ssh")
	}))
	defer srv.Close()

	p := &Profile{Proxy: Proxy{Type: ProxySSHSOCKS, Via: string(f.host)}}
	c := newClient(t, p, Deps{SSHDial: f.pool.DialContext})
	_, body, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("get through ssh-socks: %v", err)
	}
	if body != "through ssh" {
		t.Fatalf("body = %q", body)
	}
	_ = c.Close()
	if st := f.pool.Stats(); st.Leases != 0 {
		t.Fatalf("leases after Close = %d, want 0", st.Leases)
	}

	// Without an SSH dialer the profile cannot be built at all.
	if _, err := p.Client(Deps{}); err == nil {
		t.Fatal("ssh-socks built a client without an SSH dialer")
	}
}
