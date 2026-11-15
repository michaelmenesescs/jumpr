package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"golang.org/x/sync/singleflight"

	"jumpr/internal/errs"
)

const (
	// HelperTimeout is how long one auth helper may run before it is killed.
	// A helper may open a browser and wait for a login, so it is generous.
	HelperTimeout = 60 * time.Second
	// helperOutputCap caps helper stdout. It is a credential, not a document.
	helperOutputCap = 64 << 10
	// helperStderrCap caps the stderr text kept for the "waiting for auth
	// helper…" dialog.
	helperStderrCap = 4 << 10
)

// refreshToRE matches a `302-to-login` style refresh_on entry: a status and the
// word to look for in Location.
var refreshToRE = regexp.MustCompile(`^\d+-to-(.+)$`)

// Credentials is what an auth helper returned (SPEC §7.2). It lives in memory
// only: it is never logged, written to disk or put into an error message.
type Credentials struct {
	Headers   map[string]string
	Cookies   []Cookie
	ExpiresAt time.Time
}

// helperWire is the JSON document a helper prints on stdout.
type helperWire struct {
	Headers   map[string]string `json:"headers"`
	Cookies   []Cookie          `json:"cookies"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// helperState is one client's in-memory helper cache: the credentials, when
// they stop being valid, the last stderr for the dialog, and the singleflight
// group that makes concurrent requests share one helper run.
type helperState struct {
	mu     sync.Mutex
	creds  *Credentials
	until  time.Time // zero = valid until invalidated
	stderr string
	group  singleflight.Group
	// timeout and now are replaceable in tests.
	timeout time.Duration
	now     func() time.Time
}

// newHelperState returns an empty cache with the default timeout.
func newHelperState() *helperState {
	return &helperState{timeout: HelperTimeout, now: time.Now}
}

// cached returns the credentials while they are still valid.
func (s *helperState) cached() (*Credentials, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds == nil {
		return nil, false
	}
	if !s.until.IsZero() && !s.now().Before(s.until) {
		return nil, false
	}
	return s.creds, true
}

// store caches creds until their expires_at, or until ttl from now, or
// forever when neither is set.
func (s *helperState) store(creds *Credentials, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = creds
	switch {
	case !creds.ExpiresAt.IsZero():
		s.until = creds.ExpiresAt
	case ttl > 0:
		s.until = s.now().Add(ttl)
	default:
		s.until = time.Time{}
	}
}

// invalidate drops the credentials, so the next request runs the helper.
func (s *helperState) invalidate() {
	s.mu.Lock()
	s.creds = nil
	s.until = time.Time{}
	s.mu.Unlock()
}

// setStderr keeps the redacted stderr of the last run for the dialog.
func (s *helperState) setStderr(text string) {
	s.mu.Lock()
	s.stderr = text
	s.mu.Unlock()
}

// lastStderr returns the redacted stderr of the last run.
func (s *helperState) lastStderr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stderr
}

// HelperStderr returns the redacted stderr of the profile's last auth-helper
// run: what the "waiting for auth helper…" dialog shows. It is never stored.
func (c *Client) HelperStderr() string {
	if c.helper == nil {
		return ""
	}
	return c.helper.lastStderr()
}

// HelperCredentials returns the profile's credentials, running the helper when
// the cache is empty or expired. Only a helper profile has any; the CLI uses
// it to show what the helper returned (values redacted).
func (c *Client) HelperCredentials(ctx context.Context, u *url.URL) (Credentials, error) {
	if c.helper == nil {
		return Credentials{}, errs.New(errs.Config, "httpx.helper",
			fmt.Sprintf("profile %q has no auth helper", c.name))
	}
	creds, err := c.helperCredentials(ctx, u)
	if err != nil {
		return Credentials{}, err
	}
	return *creds, nil
}

// helperCredentials returns the cached credentials or runs the helper. A
// concurrent burst results in exactly one run: the cache is checked again
// inside the singleflight, so a run that finished while the caller was queued
// is reused.
func (c *Client) helperCredentials(ctx context.Context, u *url.URL) (*Credentials, error) {
	if c.helper == nil {
		return nil, errs.New(errs.Config, "httpx.helper",
			fmt.Sprintf("profile %q has no auth helper", c.name))
	}
	if creds, ok := c.helper.cached(); ok {
		return creds, nil
	}
	v, err, _ := c.helper.group.Do("auth-helper", func() (any, error) {
		if creds, ok := c.helper.cached(); ok {
			return creds, nil
		}
		creds, err := c.runHelper(ctx, u)
		if err != nil {
			return nil, err
		}
		c.helper.store(creds, c.helperTTL())
		return creds, nil
	})
	if err != nil {
		return nil, err
	}
	creds, ok := v.(*Credentials)
	if !ok {
		return nil, errs.New(errs.Internal, "httpx.helper", "the auth helper returned no credentials")
	}
	return creds, nil
}

// helperTTL is the profile's cache_ttl: how long credentials live when the
// helper does not send an expires_at.
func (c *Client) helperTTL() time.Duration {
	h := c.profile.Auth.Helper
	if h == nil {
		return 0
	}
	return h.CacheTTL.Duration()
}

// runHelper executes the configured argv and parses its stdout. The helper is
// run directly, without a shell, under its own generous timeout: it may open a
// browser on the laptop and wait for a login.
func (c *Client) runHelper(ctx context.Context, u *url.URL) (*Credentials, error) {
	argv, err := c.helperArgv(u)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.helper.timeout)
	defer cancel()

	cmd := exec.CommandContext(hctx, argv[0], argv[1:]...)
	out := &capBuffer{limit: helperOutputCap}
	errOut := &capBuffer{limit: helperStderrCap}
	cmd.Stdout = out
	cmd.Stderr = errOut

	runErr := cmd.Run()
	stderr := strings.TrimSpace(errOut.String())
	c.helper.setStderr(c.red.String(stderr))

	switch {
	case errors.Is(hctx.Err(), context.DeadlineExceeded):
		return nil, c.helperError(errs.Timeout,
			fmt.Sprintf("the auth helper did not finish within %s", c.helper.timeout), "")
	case errors.Is(runErr, exec.ErrNotFound):
		return nil, c.helperError(errs.NotAvailable,
			fmt.Sprintf("the auth helper %q was not found", argv[0]), "")
	case runErr != nil:
		detail := c.red.String(stderr)
		if detail == "" {
			detail = runErr.Error()
		}
		if i := strings.IndexByte(detail, '\n'); i >= 0 {
			detail = detail[:i]
		}
		return nil, c.helperError(errs.Auth, "the auth helper failed", detail)
	}

	var wire helperWire
	dec := json.NewDecoder(strings.NewReader(out.String()))
	if err := dec.Decode(&wire); err != nil {
		// stdout is a credential: the parse error is reported, the text never.
		return nil, c.helperError(errs.Auth, "the auth helper printed invalid JSON", "")
	}
	if len(wire.Headers) == 0 && len(wire.Cookies) == 0 {
		return nil, c.helperError(errs.Auth,
			"the auth helper returned no headers and no cookies", "")
	}
	return &Credentials{Headers: wire.Headers, Cookies: wire.Cookies, ExpiresAt: wire.ExpiresAt}, nil
}

// helperError builds an *errs.Error that names the profile. detail is already
// safe to show.
func (c *Client) helperError(kind errs.Kind, msg, detail string) *errs.Error {
	full := fmt.Sprintf("profile %q: %s", c.name, msg)
	if detail != "" {
		full += ": " + detail
	}
	return &errs.Error{Kind: kind, Op: "httpx.helper", Msg: c.red.String(full)}
}

// helperArgv templates the configured argv: {{ .URL }} is the request URL and
// {{ .Profile }} the profile name. argv[0] has a leading ~ expanded, because a
// YAML list of strings cannot rely on the shell for it.
func (c *Client) helperArgv(u *url.URL) ([]string, error) {
	h := c.profile.Auth.Helper
	if h == nil || len(h.Command) == 0 {
		return nil, c.helperError(errs.Config, "the auth helper has no command", "")
	}
	data := struct{ URL, Profile string }{URL: "", Profile: c.name}
	if u != nil {
		data.URL = c.redURL(u)
	}
	out := make([]string, 0, len(h.Command))
	for i, arg := range h.Command {
		s, err := renderArg(arg, data)
		if err != nil {
			return nil, c.helperError(errs.Config,
				fmt.Sprintf("the auth helper command argument %d is not a valid template", i+1), "")
		}
		if i == 0 {
			s = expandHome(s, c.home)
		}
		out = append(out, s)
	}
	if strings.TrimSpace(out[0]) == "" {
		return nil, c.helperError(errs.Config, "the auth helper command is empty", "")
	}
	return out, nil
}

// renderArg executes one argv element as a text template when it looks like
// one. A plain argument is returned unchanged.
func renderArg(arg string, data any) (string, error) {
	if !isTemplate(arg) {
		return arg, nil
	}
	t, err := template.New("arg").Parse(arg)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// needsRefresh reports whether the response means the credentials are stale,
// and why. 401 and 403 always do; a redirect does when its Location matches a
// refresh_on pattern (the client never follows redirects for a helper
// profile). The reason is safe to show.
func (c *Client) needsRefresh(resp *http.Response) (string, bool) {
	if resp == nil {
		return "", false
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)), true
	}
	codes, patterns := c.refreshOn()
	code := resp.StatusCode
	if code >= 300 && code < 400 {
		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", false
		}
		for _, pat := range patterns {
			if pat.matches(loc) {
				return fmt.Sprintf("redirected to login: auth not accepted (status %d)", code), true
			}
		}
		// A bare status entry (302) accepts any redirect of that status.
		if len(patterns) == 0 && codes[code] {
			return fmt.Sprintf("redirected to %s: auth not accepted", c.redURLString(loc)), true
		}
		return "", false
	}
	if codes[code] {
		return fmt.Sprintf("status %d %s", code, http.StatusText(code)), true
	}
	return "", false
}

// refreshPattern is one non-numeric refresh_on entry: an upper-case-insensitive
// regexp, plus the words of a `302-to-login` style entry.
type refreshPattern struct {
	re   *regexp.Regexp
	word string
}

// matches reports whether a Location matches the pattern.
func (p refreshPattern) matches(loc string) bool {
	if p.word != "" && strings.Contains(strings.ToLower(loc), p.word) {
		return true
	}
	return p.re != nil && p.re.MatchString(loc)
}

// refreshOn parses the profile's refresh_on list once: numeric entries add a
// redirect status, `NNN-to-word` adds the status and the word, anything else is
// a case-insensitive pattern over Location.
func (c *Client) refreshOn() (map[int]bool, []refreshPattern) {
	h := c.profile.Auth.Helper
	codes := map[int]bool{}
	if h == nil {
		return codes, nil
	}
	var patterns []refreshPattern
	for _, raw := range h.RefreshOn {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if n, err := strconv.Atoi(entry); err == nil {
			codes[n] = true
			continue
		}
		word := ""
		if m := refreshToRE.FindStringSubmatch(entry); m != nil {
			if n, err := strconv.Atoi(strings.SplitN(entry, "-", 2)[0]); err == nil {
				codes[n] = true
			}
			word = strings.ToLower(strings.TrimSpace(m[1]))
		}
		re, _ := regexp.Compile("(?i)" + entry)
		patterns = append(patterns, refreshPattern{re: re, word: word})
	}
	return codes, patterns
}

// redURLString redacts a URL-shaped string for a message.
func (c *Client) redURLString(s string) string {
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return c.redURL(u)
	}
	return c.red.String(s)
}

// refreshOnce invalidates the cached credentials so the next attempt runs the
// helper again. Concurrent invalidations collapse into one run through the
// helper's singleflight.
func (c *Client) refreshOnce() {
	if c.helper != nil {
		c.helper.invalidate()
	}
}
