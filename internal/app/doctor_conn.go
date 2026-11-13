package app

// doctor.go implements `jumper doctor conn <profile> <url>`: the main tool for
// adapting jumper to an environment (SPEC §7.2). It walks one connection
// profile step by step — profile, proxy, DNS/connect, TLS, auth (including the
// auth helper), the request itself — and prints `ok`/`FAIL` with a timing per
// step. Credentials, cookie values and header values never reach the writer:
// everything that could be one passes through internal/redact first.
//
// The probe runs through the same httpx.Client a backend would use, so what it
// reports is what a backend will see. DNS, connect and TLS are observed with
// net/http/httptrace, which needs no cooperation from httpx and keeps the
// package boundary clean.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/redact"
	"jumper/internal/transport/httpx"
)

const (
	// doctorBodyPreview is how many leading body bytes are shown.
	doctorBodyPreview = 200
	// doctorHelperLimit caps what the auth helper may print on either stream.
	doctorHelperLimit = 1 << 20
	// doctorHelperTimeout is the helper's default timeout (SPEC §7.2: the
	// helper may open a browser and wait for a login).
	doctorHelperTimeout = 60 * time.Second
)

// DoctorOptions is everything DoctorConn needs from the outside world. Nothing
// here is read from the process environment: cmd/jumper fills it in.
type DoctorOptions struct {
	// ConfigDir overrides ~/.config/jumper.
	ConfigDir string
	// Home is the user's home directory, for `~/` expansion.
	Home string
	// Env supplies the `env` proxy type (HTTPS_PROXY and friends).
	Env map[string]string
	// Verbose also prints the response headers, redacted.
	Verbose bool
}

// DoctorResult is the outcome of one conn probe. OK is true only when the
// endpoint answered 2xx, did not redirect to a login page and, for a helper
// profile, the helper ran successfully; the command's exit code is 0 exactly
// then.
type DoctorResult struct {
	// Status is the final HTTP status, 0 when no response arrived.
	Status int
	// Redirect is the first redirect target, when the client followed one.
	Redirect string
	// OK reports whether the profile reached the endpoint authenticated.
	OK bool
}

// DoctorConn probes one connection profile against rawURL and writes one line
// per step to w. A nil error with OK false means the probe ran and the endpoint
// did not accept the profile; an error means the probe could not run at all.
func DoctorConn(ctx context.Context, w io.Writer, profile, rawURL string, o DoctorOptions) (DoctorResult, error) {
	if w == nil {
		w = io.Discard
	}
	if ctx == nil {
		ctx = context.Background()
	}
	target, err := doctorURL(rawURL)
	if err != nil {
		return DoctorResult{}, err
	}
	red, err := redact.New(nil)
	if err != nil {
		return DoctorResult{}, err
	}
	d := &doctor{w: w, red: red, verbose: o.Verbose}

	// Step: the profile is found and validates.
	d.begin()
	paths := config.ResolvePaths(o.Home, o.ConfigDir)
	profiles, lerr := config.LoadConnections(paths.ConnectionsPath(), o.Home)
	if lerr != nil {
		d.line("profile", profile, "FAIL", d.took())
		return DoctorResult{}, lerr
	}
	p, ok := profiles[profile]
	if !ok {
		d.line("profile", profile, "FAIL", d.took())
		return DoctorResult{}, errs.New(errs.Config, "app.doctorconn",
			fmt.Sprintf("no profile %q in %s", profile, paths.ConnectionsPath()))
	}
	d.line("profile", p.Name+" (valid)", "ok", d.took())

	// Step: the proxy type and address, with credentials redacted.
	d.begin()
	proxy := d.proxyDetail(p, o.Env)
	d.line("proxy", proxy, "ok", d.took())

	// A helper profile must run its helper before the request: its result is
	// what makes the request authenticated. The step is printed below, in the
	// documented order, with the duration measured here.
	var helper *helperRun
	requestProfile := p
	if p.Auth.Type == httpx.AuthHelper {
		fmt.Fprintf(d.w, "helper   waiting for the auth helper…\n")
		d.begin()
		helper = runAuthHelper(ctx, p, target, o.Home, d.red)
		helper.took = d.took()
		switch {
		case helper.err != nil:
			// Still probe the endpoint, without credentials: the status is
			// the answer to "would this work at all".
			plain := *p
			plain.Auth = httpx.Auth{Type: httpx.AuthNone}
			requestProfile = &plain
		default:
			cp, cerr := withHelperCredentials(p, helper)
			if cerr != nil {
				helper.err = cerr
				plain := *p
				plain.Auth = httpx.Auth{Type: httpx.AuthNone}
				requestProfile = &plain
			} else {
				requestProfile = cp
			}
		}
	}

	// The client is built before anything touches the network: a profile that
	// cannot build one has no DNS, connect or TLS to report.
	d.begin()
	c, cerr := requestProfile.Client(httpx.Deps{Env: o.Env, Redactor: d.red})
	clientTook := d.took()
	if cerr == nil {
		defer func() { _ = c.Close() }()
	}

	var (
		trace   = &traceEvents{}
		resp    *http.Response
		doErr   error
		reqTook time.Duration
	)
	if cerr == nil {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if rerr != nil {
			cerr = errs.New(errs.Config, "app.doctorconn", "cannot build the request")
		} else {
			d.begin()
			// The trace must ride on the context, not the request: Do re-derives
			// the request context from it and would drop a per-request one.
			resp, doErr = c.Do(httptrace.WithClientTrace(ctx, trace.hooks()), req)
			reqTook = d.took()
		}
	}

	// Steps: DNS, connect and TLS, as observed during the request.
	if cerr != nil {
		d.line("dns", "not attempted", "FAIL", 0)
		d.line("connect", proxy, "FAIL", clientTook)
		d.detail("error", d.red.String(cerr.Error()))
		d.line("tls", "not attempted", "FAIL", 0)
	} else {
		d.emitTrace(trace, target, p.TLS.CAFile, p.TLS.InsecureSkipVerify)
	}

	// Step: the auth type (never its values).
	d.line("auth", authDetail(p), "ok", 0)

	// Steps: the helper's exit status and the names it returned.
	if helper != nil {
		if helper.err != nil {
			d.line("helper", helperName(p), "FAIL", helper.took)
			d.detail("exit", fmt.Sprintf("%d", helper.exit))
			d.detail("error", d.red.String(helper.err.Error()))
			if helper.note != "" {
				d.detail("stderr", helper.note)
			}
		} else {
			d.line("helper", helperName(p), "ok", helper.took)
			d.detail("exit", "0")
			d.detail("headers", namesOrNone(helper.headerNames))
			d.detail("cookies", namesOrNone(helper.cookieNames))
			if helper.expires != "" {
				d.detail("expires", helper.expires)
			}
		}
	}

	// Step: the request itself.
	if cerr != nil {
		d.line("request", "GET "+d.showURL(target), "FAIL", 0)
		d.detail("error", d.red.String(cerr.Error()))
		return DoctorResult{}, cerr
	}
	if resp == nil {
		d.line("request", "GET "+d.showURL(target), "FAIL", reqTook)
		d.detail("error", d.red.String(errorText(doErr)))
		return DoctorResult{}, doErr
	}
	defer resp.Body.Close()

	redirectStatus, redirectTo := redirectInfo(resp, target)
	status := resp.StatusCode
	okStatus := status >= http.StatusOK && status < http.StatusMultipleChoices
	login := redirectStatus != 0 && looksLikeLogin(redirectTo, target)
	helperFailed := helper != nil && helper.err != nil

	state := "ok"
	if !okStatus || login || helperFailed {
		state = "FAIL"
	}
	d.line("request", "GET "+d.showURL(target), state, reqTook)

	statusText := fmt.Sprintf("%d %s", status, http.StatusText(status))
	if redirectStatus != 0 {
		statusText = fmt.Sprintf("%d %s (after %d)", status, http.StatusText(status), redirectStatus)
	}
	d.detail("status", statusText)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		d.detail("content-type", d.red.String(ct))
	}
	if redirectTo != "" {
		d.detail("redirect", fmt.Sprintf("%d → %s", redirectStatus, d.red.String(redirectTo)))
	}
	if d.verbose {
		d.emitHeaders(resp.Header)
	}
	if preview := bodyPreview(resp.Body); preview != "" {
		d.detail("body", d.red.String(preview))
	}
	if helperFailed {
		d.detail("note", "the auth helper failed; the request carried no credentials")
	}
	if login {
		fmt.Fprintln(d.w, "redirected to login: auth not accepted")
	}
	return DoctorResult{Status: status, Redirect: redirectTo, OK: okStatus && !login && !helperFailed}, nil
}

// doctor is the writer and the clock behind one probe.
type doctor struct {
	w       io.Writer
	red     *redact.Redactor
	verbose bool
	start   time.Time
}

// begin starts the clock for the next step.
func (d *doctor) begin() { d.start = time.Now() }

// took returns the time since begin.
func (d *doctor) took() time.Duration { return time.Since(d.start) }

// line prints one step: label, detail, ok/FAIL and how long it took.
func (d *doctor) line(label, detail, status string, dur time.Duration) {
	if detail == "" {
		detail = "-"
	}
	fmt.Fprintf(d.w, "%-8s %-46s %-4s %s\n", label, detail, status, durString(dur))
}

// detail prints one indented key/value line under the step above it.
func (d *doctor) detail(key, value string) {
	fmt.Fprintf(d.w, "  %-10s %s\n", key, value)
}

// emitHeaders prints the response headers under --verbose. Headers are redacted
// as whole "Name: value" lines so the cookie and authorization rules apply.
func (d *doctor) emitHeaders(h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.detail("header", d.red.String(k+": "+strings.Join(h[k], ", ")))
	}
}

// proxyDetail describes the profile's proxy without any credential.
func (d *doctor) proxyDetail(p *httpx.Profile, env map[string]string) string {
	switch p.Proxy.Type {
	case "", httpx.ProxyNone:
		return "none (direct)"
	case httpx.ProxyEnv:
		name, value := envProxy(env)
		if value == "" {
			return "env (no proxy variable set)"
		}
		return "env " + name + "=" + d.red.String(value)
	case httpx.ProxyHTTP, httpx.ProxySOCKS5:
		return string(p.Proxy.Type) + " " + d.red.String(p.Proxy.URL)
	case httpx.ProxySSHSOCKS:
		return "ssh-socks via " + p.Proxy.Via
	}
	return string(p.Proxy.Type)
}

// showURL renders a URL without user info, redacted: a token belongs in neither
// the terminal nor an error message.
func (d *doctor) showURL(u *url.URL) string {
	clean := *u
	clean.User = nil
	return d.red.String(clean.String())
}

// emitTrace prints the DNS, connect and TLS steps from what the request
// actually did.
func (d *doctor) emitTrace(t *traceEvents, target *url.URL, caFile string, insecure bool) {
	name, addrs, dnsErr, dnsDur := t.snapshotDNS()
	switch {
	case name == "":
		d.line("dns", "no lookup ("+target.Hostname()+")", "ok", 0)
	case dnsErr != nil:
		d.line("dns", name, "FAIL", dnsDur)
		d.detail("error", d.red.String(dnsErr.Error()))
	default:
		d.line("dns", name+" → "+strings.Join(addrs, ", "), "ok", dnsDur)
	}

	network, addr, connErr, connDur, reused := t.snapshotConn()
	switch {
	case addr == "" && reused:
		d.line("connect", "reused an existing connection", "ok", 0)
	case addr == "":
		d.line("connect", "not reached", "FAIL", 0)
	case connErr != nil:
		d.line("connect", network+" "+addr, "FAIL", connDur)
		d.detail("error", d.red.String(connErr.Error()))
	default:
		d.line("connect", network+" "+addr, "ok", connDur)
	}

	if target.Scheme != "https" {
		d.line("tls", "plain http (no TLS)", "ok", 0)
		return
	}
	state, tlsErr, tlsDur := t.snapshotTLS()
	switch {
	case state == nil && tlsErr == nil:
		d.line("tls", "no handshake was made", "FAIL", 0)
		return
	case tlsErr != nil:
		d.line("tls", "handshake failed", "FAIL", tlsDur)
		d.detail("error", d.red.String(errorText(tlsErr)))
		return
	}
	d.line("tls", tls.VersionName(state.Version), "ok", tlsDur)
	d.detail("cipher", tls.CipherSuiteName(state.CipherSuite))
	if len(state.PeerCertificates) > 0 {
		if leaf := state.PeerCertificates[0]; leaf != nil {
			d.detail("subject", d.red.String(leaf.Subject.String()))
			d.detail("issuer", d.red.String(leaf.Issuer.String()))
			d.detail("expires", fmt.Sprintf("%s (%s)",
				leaf.NotAfter.UTC().Format(time.RFC3339), expiresIn(leaf.NotAfter)))
		}
	}
	d.detail("chain", chainText(state, caFile, insecure))
}

// chainText names every certificate of the verified chain and says whether it
// ended at the configured CA or at the system pool.
func chainText(state *tls.ConnectionState, caFile string, insecure bool) string {
	if len(state.VerifiedChains) == 0 {
		if insecure {
			return "not verified (insecure_skip_verify)"
		}
		return "not verified"
	}
	chain := state.VerifiedChains[0]
	subjects := make([]string, 0, len(chain))
	for _, c := range chain {
		subjects = append(subjects, c.Subject.String())
	}
	root := chain[len(chain)-1]
	source := "system pool"
	if caFile != "" && caContains(caFile, root) {
		source = "configured CA (" + caFile + ")"
	}
	return strings.Join(subjects, " → ") + " · " + source
}

// helperRun is the observed result of one auth-helper run. Values stay in
// memory; only names and expires_at are ever printed.
type helperRun struct {
	exit        int
	took        time.Duration
	note        string
	headers     map[string]string
	headerNames []string
	cookies     []helperCookie
	cookieNames []string
	expires     string
	err         error
}

// helperCookie is one cookie from the helper protocol (SPEC §7.2).
type helperCookie struct {
	name  string
	value string
}

// helperJSON is the JSON an auth helper prints on stdout (SPEC §7.2).
type helperJSON struct {
	Headers map[string]string `json:"headers"`
	Cookies []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Domain string `json:"domain"`
	} `json:"cookies"`
	ExpiresAt string `json:"expires_at"`
}

// runAuthHelper executes the profile's helper, without a shell, and parses its
// output. Errors never carry the helper's output.
func runAuthHelper(ctx context.Context, p *httpx.Profile, target *url.URL, home string, red *redact.Redactor) *helperRun {
	out := &helperRun{}
	if p.Auth.Helper == nil || len(p.Auth.Helper.Command) == 0 {
		out.err = errs.New(errs.Auth, "app.doctorconn", "the profile has no helper command")
		return out
	}
	argv := helperArgv(p.Auth.Helper.Command, target.String(), p.Name, home)
	hctx, cancel := context.WithTimeout(ctx, doctorHelperTimeout)
	defer cancel()

	cmd := exec.CommandContext(hctx, argv[0], argv[1:]...)
	stdout := &limitedBuffer{max: doctorHelperLimit}
	stderr := &limitedBuffer{max: doctorHelperLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	var ee *exec.ExitError
	switch err := cmd.Run(); {
	case err == nil:
		out.exit = 0
	case errors.Is(hctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		out.exit = -1
		out.err = errs.New(errs.Timeout, "app.doctorconn", "the helper timed out")
	case errors.As(err, &ee):
		out.exit = ee.ExitCode()
		out.err = errs.New(errs.Auth, "app.doctorconn",
			fmt.Sprintf("the helper exited with status %d", out.exit))
		out.note = red.String(trimPreview(stderr.String(), 200))
	default:
		out.exit = -1
		out.err = errs.New(errs.Auth, "app.doctorconn", "the helper could not be started")
	}
	if out.err != nil {
		return out
	}

	var doc helperJSON
	if jerr := json.Unmarshal([]byte(stdout.String()), &doc); jerr != nil {
		out.err = errs.New(errs.Auth, "app.doctorconn", "the helper did not print the helper JSON")
		return out
	}
	out.headers = doc.Headers
	for _, name := range sortedKeys(doc.Headers) {
		out.headerNames = append(out.headerNames, name)
	}
	for _, c := range doc.Cookies {
		if c.Name == "" {
			continue
		}
		out.cookies = append(out.cookies, helperCookie{name: c.Name, value: c.Value})
		out.cookieNames = append(out.cookieNames, c.Name)
	}
	out.expires = doc.ExpiresAt
	if len(out.headers) == 0 && len(out.cookies) == 0 {
		out.err = errs.New(errs.Auth, "app.doctorconn", "the helper returned no credentials")
	}
	return out
}

// withHelperCredentials returns a copy of the profile that sends what the
// helper returned, so the probe request is authenticated exactly once.
func withHelperCredentials(p *httpx.Profile, h *helperRun) (*httpx.Profile, error) {
	headers := make(map[string]string, len(h.headers)+1)
	for k, v := range h.headers {
		headers[k] = v
	}
	if len(h.cookies) > 0 {
		pairs := make([]string, 0, len(h.cookies))
		for _, c := range h.cookies {
			pairs = append(pairs, c.name+"="+c.value)
		}
		headers["Cookie"] = strings.Join(pairs, "; ")
	}
	if len(headers) == 0 {
		return nil, errs.New(errs.Auth, "app.doctorconn", "the helper returned no credentials")
	}
	cp := *p
	cp.Auth = httpx.Auth{Type: httpx.AuthHeaders, Headers: headers}
	return &cp, nil
}

// helperArgv expands the helper's template values and `~/` in argv[0]. The
// command is executed directly; no shell is ever involved.
func helperArgv(command []string, rawURL, profile, home string) []string {
	repl := strings.NewReplacer(
		"{{ .URL }}", rawURL, "{{.URL}}", rawURL,
		"{{ .Profile }}", profile, "{{.Profile}}", profile,
	)
	argv := make([]string, len(command))
	for i, a := range command {
		a = repl.Replace(a)
		if i == 0 {
			a = config.ExpandHome(a, home)
		}
		argv[i] = a
	}
	return argv
}

// limitedBuffer keeps at most max bytes and silently drops the rest, so a
// helper cannot exhaust memory.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	room := l.max - l.buf.Len()
	if room <= 0 {
		return len(p), nil
	}
	if len(p) > room {
		l.buf.Write(p[:room])
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

func (l *limitedBuffer) String() string { return l.buf.String() }

// traceEvents records what net/http did on the way to the response.
type traceEvents struct {
	mu         sync.Mutex
	dnsName    string
	dnsAddrs   []string
	dnsErr     error
	dnsStarted time.Time
	dnsDone    time.Time
	conns      []connEvent
	tlsState   *tls.ConnectionState
	tlsErr     error
	tlsStarted time.Time
	tlsDone    time.Time
	reused     bool
}

// connEvent is one dial attempt: the target, or the proxy it went through.
type connEvent struct {
	network string
	addr    string
	err     error
	started time.Time
	done    time.Time
}

// hooks returns the trace callbacks for one request.
func (t *traceEvents) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			t.mu.Lock()
			t.dnsName = info.Host
			t.dnsStarted = time.Now()
			t.mu.Unlock()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			t.mu.Lock()
			t.dnsErr = info.Err
			t.dnsDone = time.Now()
			for _, a := range info.Addrs {
				if a.IP != nil {
					t.dnsAddrs = append(t.dnsAddrs, a.IP.String())
				}
			}
			t.mu.Unlock()
		},
		ConnectStart: func(network, addr string) {
			t.mu.Lock()
			t.conns = append(t.conns, connEvent{network: network, addr: addr, started: time.Now()})
			t.mu.Unlock()
		},
		ConnectDone: func(network, addr string, err error) {
			t.mu.Lock()
			for i := len(t.conns) - 1; i >= 0; i-- {
				if t.conns[i].addr == addr && t.conns[i].done.IsZero() {
					t.conns[i].err = err
					t.conns[i].done = time.Now()
					break
				}
			}
			t.mu.Unlock()
		},
		TLSHandshakeStart: func() {
			t.mu.Lock()
			t.tlsStarted = time.Now()
			t.mu.Unlock()
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			t.mu.Lock()
			cp := state
			t.tlsState = &cp
			t.tlsErr = err
			t.tlsDone = time.Now()
			t.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			t.reused = info.Reused
			t.mu.Unlock()
		},
	}
}

// snapshotDNS returns the last lookup and how long it took.
func (t *traceEvents) snapshotDNS() (string, []string, error, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	dur := time.Duration(0)
	if !t.dnsStarted.IsZero() && !t.dnsDone.IsZero() {
		dur = t.dnsDone.Sub(t.dnsStarted)
	}
	return t.dnsName, append([]string{}, t.dnsAddrs...), t.dnsErr, dur
}

// snapshotConn returns the last dial attempt and how long it took.
func (t *traceEvents) snapshotConn() (string, string, error, time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.conns) == 0 {
		return "", "", nil, 0, t.reused
	}
	last := t.conns[len(t.conns)-1]
	dur := time.Duration(0)
	if !last.started.IsZero() && !last.done.IsZero() {
		dur = last.done.Sub(last.started)
	}
	return last.network, last.addr, last.err, dur, t.reused
}

// snapshotTLS returns the last handshake.
func (t *traceEvents) snapshotTLS() (*tls.ConnectionState, error, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	dur := time.Duration(0)
	if !t.tlsStarted.IsZero() && !t.tlsDone.IsZero() {
		dur = t.tlsDone.Sub(t.tlsStarted)
	}
	return t.tlsState, t.tlsErr, dur
}

// redirectInfo returns the first redirect status and the target the client
// followed. net/http unrolls the chain, so the status is read back from the
// request that caused each hop.
func redirectInfo(resp *http.Response, orig *url.URL) (int, string) {
	// A redirect the client did not follow — a helper profile stops at the
	// redirect so the helper can be refreshed (SPEC §7.2) — is the final
	// response itself.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			return resp.StatusCode, resolveRef(orig, resp.Request, loc)
		}
	}
	status, target := 0, ""
	for req := resp.Request; req != nil && req.Response != nil; req = req.Response.Request {
		hop := req.Response
		loc := hop.Header.Get("Location")
		if loc == "" {
			break
		}
		status = hop.StatusCode
		target = resolveRef(orig, hop.Request, loc)
	}
	return status, target
}

// resolveRef resolves a Location header against the request that produced it.
func resolveRef(orig *url.URL, req *http.Request, loc string) string {
	base := orig
	if req != nil && req.URL != nil {
		base = req.URL
	}
	u, err := base.Parse(loc)
	if err != nil {
		return loc
	}
	return u.String()
}

// looksLikeLogin reports whether a redirect went to a login page: another host,
// or a path that names one.
func looksLikeLogin(target string, orig *url.URL) bool {
	if target == "" {
		return false
	}
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	if u.Host != "" && orig != nil && !strings.EqualFold(u.Host, orig.Host) {
		return true
	}
	for _, seg := range strings.Split(strings.ToLower(u.Path), "/") {
		switch seg {
		case "auth", "cas":
			return true
		}
		// A path segment that starts with a login word: login.html,
		// signin.jsp, oauth2, sso.example. The letter check keeps
		// "authorization" from matching "auth".
		for _, word := range []string{
			"login", "logon", "signin", "sign-in", "sign_in",
			"sso", "saml", "idp", "oauth", "openid", "authorize",
		} {
			if strings.HasPrefix(seg, word) && (len(seg) == len(word) || !isLetter(seg[len(word)])) {
				return true
			}
		}
	}
	return false
}

// isLetter reports whether b could continue a word.
func isLetter(b byte) bool {
	return b >= 'a' && b <= 'z'
}

// bodyPreview returns the first doctorBodyPreview bytes of the body, made
// printable. The caller redacts it.
func bodyPreview(r io.Reader) string {
	buf := make([]byte, doctorBodyPreview)
	n, _ := io.ReadFull(r, buf)
	if n == 0 {
		return ""
	}
	s := strings.ToValidUTF8(string(buf[:n]), "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimRight(s, " ")
	if n == doctorBodyPreview {
		s += "…"
	}
	return s
}

// doctorURL validates the address the user asked for.
func doctorURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errs.New(errs.Config, "app.doctorconn", "the target must be an absolute http or https URL")
	}
	u.Fragment = ""
	return u, nil
}

// authDetail names the auth type and, for a header or cookie type, the names it
// sends — never a value.
func authDetail(p *httpx.Profile) string {
	switch p.Auth.Type {
	case "", httpx.AuthNone:
		return "none"
	case httpx.AuthBasic:
		return "basic"
	case httpx.AuthBearer:
		return "bearer"
	case httpx.AuthHeaders:
		return "headers (" + strings.Join(sortedKeys(p.Auth.Headers), ", ") + ")"
	case httpx.AuthCookie:
		return "cookie (" + p.Auth.Cookie.Name + ")"
	case httpx.AuthHelper:
		return "helper (" + helperName(p) + ")"
	}
	return string(p.Auth.Type)
}

// helperName is the helper's program name, without its arguments.
func helperName(p *httpx.Profile) string {
	if p.Auth.Helper == nil || len(p.Auth.Helper.Command) == 0 {
		return "helper"
	}
	return filepath.Base(p.Auth.Helper.Command[0])
}

// envProxy picks the proxy variable a request would use.
func envProxy(env map[string]string) (string, string) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := strings.TrimSpace(env[name]); v != "" {
			return name, v
		}
	}
	return "", ""
}

// sortedKeys returns the keys of m in order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// namesOrNone renders a list of header or cookie names.
func namesOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// caContains reports whether the CA bundle at path holds cert.
func caContains(path string, cert *x509.Certificate) bool {
	if cert == nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return false
		}
		parsed, perr := x509.ParseCertificate(block.Bytes)
		if perr != nil {
			continue
		}
		if bytes.Equal(parsed.Raw, cert.Raw) {
			return true
		}
	}
}

// expiresIn describes a certificate expiry relative to now.
func expiresIn(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	return fmt.Sprintf("in %dd", int(d.Hours()/24))
}

// durString formats a step timing.
func durString(d time.Duration) string {
	if d <= 0 {
		return "0ms"
	}
	if d < time.Millisecond {
		if r := d.Round(10 * time.Microsecond); r > 0 {
			return r.String()
		}
		return "0ms"
	}
	if d < time.Second {
		return d.Round(100 * time.Microsecond).String()
	}
	return d.Round(time.Millisecond).String()
}

// errorText renders an error without the url.Error and net.OpError wrapping.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	return err.Error()
}

// trimPreview shortens s to at most n visible characters.
func trimPreview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
