package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
)

const (
	// MaxBodyBytes caps every response body a Client hands out: 16 MiB.
	MaxBodyBytes = 16 << 20
	// retryBaseDelay is the first backoff step; it doubles per attempt.
	retryBaseDelay = 200 * time.Millisecond
	// retryMaxDelay caps one backoff step.
	retryMaxDelay = 2 * time.Second
	// drainLimit is how much of a discarded response body is read before it is
	// closed, so the connection can be reused.
	drainLimit = 64 << 10
)

// Deps is what a client needs from its owner. Env supplies the values for an
// `env` proxy (never read from the process), SSHDial supplies the SSH pool for
// `ssh-socks`, Redactor cleans error text and Warn receives :health warnings.
type Deps struct {
	// Env is the environment the `env` proxy type reads (HTTPS_PROXY,
	// HTTP_PROXY, NO_PROXY, in either case).
	Env map[string]string
	// SSHDial dials (network, addr) through the SSH host, without a local
	// listener. ssh.Pool.DialContext has exactly this shape.
	SSHDial func(ctx context.Context, host resource.HostID, network, addr string) (net.Conn, error)
	// Redactor cleans error strings. Nil means the built-in patterns.
	Redactor *redact.Redactor
	// Warn receives health warnings, notably insecure_skip_verify.
	Warn func(msg string)
	// Secrets resolves the `{{ secret "name" }}` references of profile
	// strings. Nil builds one from Env and no configured commands.
	Secrets *Secrets
	// Home expands a leading ~ in an auth helper's argv[0]. Empty leaves the
	// path untouched.
	Home string
}

// Client is one connection profile's HTTP client. It owns a single transport,
// reused for every request, and is safe for concurrent use.
type Client struct {
	name     string
	profile  *Profile
	hc       *http.Client
	red      *redact.Redactor
	warnings []string
	retries  int
	// helper is the in-memory auth-helper cache; nil for every other auth
	// type.
	helper *helperState
	// secrets resolves secret templates in the profile's strings.
	secrets *Secrets
	// home expands ~ in the auth helper's argv[0].
	home string
	// proxyRequestHeaders are added to absolute-form requests through an http
	// proxy (CONNECT requests use the transport's ProxyConnectHeader).
	proxyRequestHeaders map[string]string
	// backoff and sleep are replaceable in tests; they default to exponential
	// backoff and a ctx-aware wait.
	backoff func(attempt int) time.Duration
	sleep   func(ctx context.Context, d time.Duration) error
}

// Client builds the profile's client. One transport is created here and reused;
// every remote call it makes goes through Do, which applies the profile's proxy,
// TLS, auth, headers, timeout and retries.
func (p *Profile) Client(deps Deps) (*Client, error) {
	if p == nil {
		return nil, &errs.Error{Kind: errs.Config, Op: "httpx.client", Msg: "nil profile"}
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	red, err := redactorFor(deps)
	if err != nil {
		return nil, err
	}

	tlsCfg, tlsWarnings, err := p.tlsConfig()
	if err != nil {
		return nil, err
	}

	timeout := p.timeout()
	base := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	dial := dialFunc(base.DialContext)

	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: time.Second,
	}

	var proxyHeaders map[string]string
	switch p.Proxy.Type {
	case "", ProxyNone:
	case ProxyEnv:
		env := deps.Env
		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			return proxyEnvURL(env, req.URL)
		}
	case ProxyHTTP:
		u, err := parseProxyURL(p.Proxy.URL, ProxyHTTP)
		if err != nil {
			return nil, ProfileError(p.Name, "proxy.url", "%v", err)
		}
		tr.Proxy = http.ProxyURL(u)
		if len(p.Proxy.Headers) > 0 {
			h := http.Header{}
			for k, v := range p.Proxy.Headers {
				h.Set(k, v)
			}
			tr.ProxyConnectHeader = h
			proxyHeaders = p.Proxy.Headers
		}
	case ProxySOCKS5:
		u, err := parseProxyURL(p.Proxy.URL, ProxySOCKS5)
		if err != nil {
			return nil, ProfileError(p.Name, "proxy.url", "%v", err)
		}
		dial := dialFunc(tr.DialContext)
		tr.DialContext = socks5Dial(dial, socksAddr(u), p.Proxy.Username, p.Proxy.Password)
	case ProxySSHSOCKS:
		if deps.SSHDial == nil {
			return nil, ProfileError(p.Name, "proxy.via", "no SSH dialer supplied for %s", ProxySSHSOCKS)
		}
		via := resource.HostID(p.Proxy.Via)
		sshDial := deps.SSHDial
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return sshDial(ctx, via, network, addr)
		}
	}

	warnings := append([]string{}, tlsWarnings...)
	if p.TLS.InsecureSkipVerify {
		w := fmt.Sprintf("profile %q: tls.insecure_skip_verify is on; certificate verification is disabled", p.Name)
		warnings = append(warnings, w)
		if deps.Warn != nil {
			deps.Warn(w)
		}
	}
	for _, w := range p.SecretWarnings() {
		warnings = append(warnings, w)
		if deps.Warn != nil {
			deps.Warn(w)
		}
	}

	secs := deps.Secrets
	if secs == nil {
		secs = NewSecrets(deps.Env, nil)
	}

	hc := &http.Client{Transport: tr}
	var helper *helperState
	if p.authType() == AuthHelper {
		// A helper profile never follows a redirect: a redirect to a login
		// page is the signal that the credentials are stale (SPEC §7.2).
		hc.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		helper = newHelperState()
	}

	return &Client{
		name:                p.Name,
		profile:             p,
		hc:                  hc,
		red:                 red,
		warnings:            warnings,
		retries:             p.Retries,
		helper:              helper,
		secrets:             secs,
		home:                deps.Home,
		proxyRequestHeaders: proxyHeaders,
		backoff:             expBackoff,
		sleep:               sleepCtx,
	}, nil
}

// Warnings returns the :health warnings this profile produced, for example the
// insecure_skip_verify notice.
func (c *Client) Warnings() []string {
	return append([]string{}, c.warnings...)
}

// Profile returns the profile name the client was built from.
func (c *Client) Profile() string { return c.name }

// Close releases the client's idle connections. A Client is normally shared for
// the life of the program; call Close when a profile is dropped.
func (c *Client) Close() error {
	if t, ok := c.hc.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	return nil
}

// Do sends req with the profile's auth, headers, timeout and retries and caps
// the response body at MaxBodyBytes.
//
// Any HTTP status is returned in the response. A status that means a failure
// (401/403 → Auth, 404 → NotAvailable, 408 and 504 → Timeout, the rest of 4xx
// and 5xx → Remote) is also returned as an *errs.Error: callers that inspect the
// response can do so, and the body is still readable and must be closed.
// Transport failures (dial, TLS, timeout, cancellation) return only the error.
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errs.New(errs.Internal, "httpx.do", "nil request")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.profile.timeout())

	resp, err := c.send(ctx, req)
	if err != nil {
		cancel()
		return nil, err
	}

	// A helper profile retries once with fresh credentials when the session
	// looks stale (SPEC §7.2). A second failure is the caller's answer.
	if c.helper != nil {
		if reason, stale := c.needsRefresh(resp); stale {
			drain(resp)
			c.refreshOnce()
			if req.Body != nil && req.Body != http.NoBody && req.GetBody != nil {
				if b, rerr := req.GetBody(); rerr == nil {
					req.Body = b
				}
			}
			resp, err = c.send(ctx, req)
			if err != nil {
				cancel()
				return nil, err
			}
			if again, stale2 := c.needsRefresh(resp); stale2 {
				reason = again
				resp.Body = &cancelBody{rc: newCappedBody(resp.Body, MaxBodyBytes), cancel: cancel}
				return resp, c.helperAuthError(req, reason)
			}
		}
	}

	resp.Body = &cancelBody{rc: newCappedBody(resp.Body, MaxBodyBytes), cancel: cancel}
	if serr := c.statusError(req, resp); serr != nil {
		return resp, serr
	}
	return resp, nil
}

// helperAuthError is the error a helper profile returns when the session could
// not be renewed: it names the profile, because that is what the user has to
// fix in connections.yaml.
func (c *Client) helperAuthError(req *http.Request, reason string) *errs.Error {
	return c.errorf(errs.Auth, "httpx.helper", req.URL.Host,
		"profile %q: the auth helper did not renew the session: %s", c.name, reason)
}

// send runs the request, retrying idempotent methods on connection failures and
// on 502/503/504. It returns the response for the final attempt.
func (c *Client) send(ctx context.Context, req *http.Request) (*http.Response, error) {
	attempts := 1
	if idempotent(req.Method) && c.retries > 0 {
		attempts += c.retries
	}
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil && attempts > 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		r, err := c.cloneForAttempt(req, ctx, attempt)
		if err != nil {
			return nil, err
		}
		if aerr := c.apply(r); aerr != nil {
			return nil, aerr
		}

		resp, err := c.hc.Do(r)
		if err != nil {
			lastErr = c.transportError(req, err)
			if attempt+1 < attempts && retryable(lastErr) {
				if serr := c.sleep(ctx, c.backoff(attempt)); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, lastErr
		}
		if attempt+1 < attempts && retryStatus(resp.StatusCode) {
			drain(resp)
			if serr := c.sleep(ctx, c.backoff(attempt)); serr != nil {
				return nil, serr
			}
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// cloneForAttempt returns a copy of req that carries ctx and a fresh body.
func (c *Client) cloneForAttempt(req *http.Request, ctx context.Context, attempt int) (*http.Request, error) {
	r := req.Clone(ctx)
	if attempt > 0 && req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return nil, errs.New(errs.Internal, "httpx.do", "request body cannot be replayed")
		}
		b, err := req.GetBody()
		if err != nil {
			return nil, errs.New(errs.Internal, "httpx.do", "cannot replay the request body").WithHost(req.URL.Host)
		}
		r.Body = b
	}
	return r, nil
}

// apply adds the profile's static auth, extra headers and proxy headers to r,
// resolving `{{ secret "name" }}` on the way. A helper profile runs its helper
// here, so the credentials are fresh for every attempt. Nothing in this
// function is ever logged or put into a URL.
func (c *Client) apply(r *http.Request) error {
	a := &c.profile.Auth
	ctx := r.Context()
	switch c.profile.authType() {
	case AuthBasic:
		pw, err := c.secret(ctx, a.Password)
		if err != nil {
			return err
		}
		r.SetBasicAuth(a.Username, pw)
	case AuthBearer:
		if r.Header.Get("Authorization") == "" {
			tok, err := c.secret(ctx, a.Token)
			if err != nil {
				return err
			}
			r.Header.Set("Authorization", "Bearer "+tok)
		}
	case AuthHeaders:
		for k, v := range a.Headers {
			ev, err := c.secret(ctx, v)
			if err != nil {
				return err
			}
			r.Header.Set(k, ev)
		}
	case AuthCookie:
		if cookieInScope(a.Cookie, r.URL.Host) {
			v, err := c.secret(ctx, a.Cookie.Value)
			if err != nil {
				return err
			}
			r.Header.Add("Cookie", a.Cookie.Name+"="+v)
		}
	case AuthHelper:
		creds, err := c.helperCredentials(ctx, r.URL)
		if err != nil {
			return err
		}
		for k, v := range creds.Headers {
			r.Header.Set(k, v)
		}
		for _, ck := range creds.Cookies {
			if cookieInScope(ck, r.URL.Host) {
				r.Header.Add("Cookie", ck.Name+"="+ck.Value)
			}
		}
	}
	for k, v := range c.profile.Headers {
		ev, err := c.secret(ctx, v)
		if err != nil {
			return err
		}
		r.Header.Set(k, ev)
	}
	if c.proxyRequestHeaders != nil && r.URL.Scheme == "http" {
		for k, v := range c.proxyRequestHeaders {
			ev, err := c.secret(ctx, v)
			if err != nil {
				return err
			}
			r.Header.Set(k, ev)
		}
	}
	return nil
}

// secret expands the `{{ secret "name" }}` references in one profile string.
// A string without a template is returned as it is.
func (c *Client) secret(ctx context.Context, s string) (string, error) {
	if s == "" || c.secrets == nil || !isTemplate(s) {
		return s, nil
	}
	return c.secrets.Expand(ctx, s)
}

// cookieInScope reports whether host (optionally with a port) is inside the
// cookie's domain.
func cookieInScope(ck Cookie, host string) bool {
	if ck.Domain == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	domain := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ck.Domain), "."))
	if domain == "" {
		return true
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// tlsConfig builds the profile's TLS configuration and its :health warnings.
func (p *Profile) tlsConfig() (*tls.Config, []string, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: p.TLS.ServerName,
	}
	if p.TLS.CAFile != "" {
		pem, err := os.ReadFile(p.TLS.CAFile)
		if err != nil {
			return nil, nil, ProfileError(p.Name, "tls.ca_file", "cannot read %s", p.TLS.CAFile)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, nil, ProfileError(p.Name, "tls.ca_file", "no certificates in %s", p.TLS.CAFile)
		}
		cfg.RootCAs = pool
	}
	if p.TLS.ClientCert != "" {
		pair, err := tls.LoadX509KeyPair(p.TLS.ClientCert, p.TLS.ClientKey)
		if err != nil {
			return nil, nil, ProfileError(p.Name, "tls.client_cert", "cannot load the client certificate")
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	cfg.InsecureSkipVerify = p.TLS.InsecureSkipVerify
	return cfg, nil, nil
}

// statusError classifies a response status.
func (c *Client) statusError(req *http.Request, resp *http.Response) error {
	var kind errs.Kind
	switch code := resp.StatusCode; {
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		kind = errs.Auth
	case code == http.StatusNotFound:
		kind = errs.NotAvailable
	case code == http.StatusRequestTimeout, code == http.StatusGatewayTimeout:
		kind = errs.Timeout
	case code >= 400:
		kind = errs.Remote
	default:
		return nil
	}
	return c.errorf(kind, "httpx.do", req.URL.Host, "%s %s: status %d %s",
		req.Method, c.redURL(req.URL), resp.StatusCode, http.StatusText(resp.StatusCode))
}

// transportError converts a transport failure into an *errs.Error. The text is
// redacted; request headers never appear in it.
func (c *Client) transportError(req *http.Request, err error) *errs.Error {
	kind := errs.Unreachable
	if ce := contextCause(err); ce != nil {
		kind = ctxError(ce).Kind
	} else if classified, ok := firstErrs(err); ok {
		kind = classified.Kind
	} else {
		var certErr *tls.CertificateVerificationError
		var unknown x509.UnknownAuthorityError
		var hostErr x509.HostnameError
		var invalid x509.CertificateInvalidError
		switch {
		case errors.As(err, &certErr), errors.As(err, &unknown), errors.As(err, &hostErr), errors.As(err, &invalid):
			kind = errs.Auth
		case isTimeout(err):
			kind = errs.Timeout
		}
	}
	detail := c.red.String(errorText(err))
	return c.errorf(kind, "httpx.do", req.URL.Host, "%s %s: %s", req.Method, c.redURL(req.URL), detail)
}

// redURL renders a URL for an error message without credentials or query
// values: a token belongs in neither.
func (c *Client) redURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	clean := *u
	clean.User = nil
	clean.RawQuery = ""
	clean.ForceQuery = false
	clean.Fragment = ""
	return c.red.String(clean.String())
}

// errorf builds a redacted *errs.Error.
func (c *Client) errorf(kind errs.Kind, op, host, format string, args ...any) *errs.Error {
	return &errs.Error{Kind: kind, Op: op, Host: host, Msg: c.red.String(fmt.Sprintf(format, args...))}
}

// GetJSON GETs url and decodes the JSON body into out.
func (c *Client) GetJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctxOr(ctx), http.MethodGet, rawURL, nil)
	if err != nil {
		return c.errorf(errs.Config, "httpx.get", "", "invalid URL")
	}
	resp, err := c.Do(ctx, req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return err
	}
	return c.decode(resp, out)
}

// PostJSON POSTs in as JSON to url and decodes the JSON response into out. POST
// is not idempotent, so it is never retried.
func (c *Client) PostJSON(ctx context.Context, rawURL string, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return c.errorf(errs.Internal, "httpx.post", "", "cannot encode the request body")
		}
		body = b
	}
	req, err := http.NewRequestWithContext(ctxOr(ctx), http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return c.errorf(errs.Config, "httpx.post", "", "invalid URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	resp, err := c.Do(ctx, req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return c.decode(resp, out)
}

// decode reads one JSON document from the response.
func (c *Client) decode(resp *http.Response, out any) error {
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return c.errorf(errs.Parse, "httpx.decode", "", "%s %s: %s",
			resp.Request.Method, c.redURL(resp.Request.URL), errorText(err))
	}
	return nil
}

// cappedBody stops a response body at the limit and closes the real body.
type cappedBody struct {
	rc io.ReadCloser
	r  io.Reader
}

// newCappedBody limits b to max bytes.
func newCappedBody(b io.ReadCloser, max int64) io.ReadCloser {
	return &cappedBody{rc: b, r: io.LimitReader(b, max)}
}

func (b *cappedBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *cappedBody) Close() error               { return b.rc.Close() }

// cancelBody frees the request's timeout context when the body is closed.
type cancelBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Read(p []byte) (int, error) { return b.rc.Read(p) }
func (b *cancelBody) Close() error {
	err := b.rc.Close()
	b.cancel()
	return err
}

// retryStatus reports the statuses worth retrying: a gateway is momentarily
// unable to serve the request.
func retryStatus(code int) bool {
	switch code {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryable reports whether a transport failure is worth a retry.
func retryable(err error) bool {
	switch errs.KindOf(err) {
	case errs.Timeout, errs.Unreachable:
		return true
	}
	return false
}

// idempotent reports whether method may be retried without side effects.
func idempotent(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace,
		http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// drain reads a little of the response body then closes it, so the connection
// can be reused by the next attempt.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
}

// expBackoff doubles from retryBaseDelay up to retryMaxDelay.
func expBackoff(attempt int) time.Duration {
	d := retryBaseDelay
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= retryMaxDelay {
			return retryMaxDelay
		}
	}
	return d
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctxError(ctx.Err())
	}
}

// ctxOr returns ctx, or a background context when it is nil.
func ctxOr(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// ctxError converts a context failure into the matching *errs.Error.
func ctxError(err error) *errs.Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &errs.Error{Kind: errs.Timeout, Op: "httpx.do", Msg: "the request timed out"}
	}
	return &errs.Error{Kind: errs.Canceled, Op: "httpx.do", Msg: "the request was canceled"}
}

// contextCause returns the context error behind err, if any.
func contextCause(err error) error {
	for _, target := range []error{context.DeadlineExceeded, context.Canceled} {
		if errors.Is(err, target) {
			return target
		}
	}
	return nil
}

// asNetError reports whether err unwraps to a net.Error.
func asNetError(err error, out *net.Error) bool {
	return errors.As(err, out)
}

// isTimeout reports whether err is a network timeout.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// firstErrs returns the first *errs.Error inside err, if one is there.
func firstErrs(err error) (*errs.Error, bool) {
	var e *errs.Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// errorText renders an error without the wrapping url.Error noise and without
// credentials: only the innermost message is used.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		err = oe.Err
	}
	return err.Error()
}
