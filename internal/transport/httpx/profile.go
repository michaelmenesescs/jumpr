// Package httpx builds an *http.Client from a named connection profile
// (SPEC §7.2): proxy, TLS, static auth, extra headers, timeout and retries.
//
// A profile is the generic proxy/SSO layer every HTTP backend uses, so an
// environment that sits behind a SOCKS bastion or a corporate CONNECT proxy
// needs configuration, not code. One transport is created per profile and
// reused for every request: never create a client per request.
package httpx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"jumper/internal/errs"
	"jumper/internal/redact"
)

const (
	// DefaultTimeout is the profile timeout used when `timeout` is omitted
	// (SPEC §7.2 shows 15s).
	DefaultTimeout = 15 * time.Second
	// MaxRetries caps `retries`: more than a handful only hides a broken
	// endpoint behind seconds of waiting.
	MaxRetries = 5
)

// Duration is a time.Duration written as a Go duration string ("15s") in YAML.
type Duration time.Duration

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String formats the duration the way time.Duration does.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// ProxyType names how the client reaches the target (SPEC §7.2).
type ProxyType string

const (
	// ProxyNone dials the target directly.
	ProxyNone ProxyType = "none"
	// ProxyEnv takes a proxy from Deps.Env (HTTPS_PROXY, HTTP_PROXY, NO_PROXY).
	ProxyEnv ProxyType = "env"
	// ProxyHTTP speaks HTTP CONNECT (or absolute-form) through `url`.
	ProxyHTTP ProxyType = "http"
	// ProxySOCKS5 speaks SOCKS5 to `url`.
	ProxySOCKS5 ProxyType = "socks5"
	// ProxySSHSOCKS tunnels through an SSH host with the pool's DialContext:
	// the equivalent of `ssh -D`, without a local listener.
	ProxySSHSOCKS ProxyType = "ssh-socks"
)

// Proxy is the `proxy:` block of a profile.
type Proxy struct {
	// Type is one of none, env, http, socks5, ssh-socks. Empty means none.
	Type ProxyType `yaml:"type"`
	// URL is the proxy address for http (http://host:3128) and socks5
	// (socks5://host:1080). Credentials belong in Username/Password, never
	// in the URL.
	URL string `yaml:"url"`
	// Via is the inventory host ID the ssh-socks proxy dials through.
	Via string `yaml:"via"`
	// Headers are sent to an http proxy: ProxyConnectHeader on CONNECT and
	// extra headers on absolute-form requests.
	Headers map[string]string `yaml:"headers"`
	// Username and Password authenticate to a socks5 proxy (RFC 1929).
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// TLSConfig is the `tls:` block of a profile.
type TLSConfig struct {
	// CAFile is a PEM bundle added to the system pool as a trusted root.
	CAFile string `yaml:"ca_file"`
	// ClientCert and ClientKey are a PEM client certificate and key (mTLS).
	ClientCert string `yaml:"client_cert"`
	ClientKey  string `yaml:"client_key"`
	// InsecureSkipVerify disables certificate verification. It produces a
	// loud :health warning.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
	// ServerName overrides the SNI name and the certificate host name.
	ServerName string `yaml:"server_name"`
}

// AuthType names the static auth a profile applies (SPEC §7.2).
type AuthType string

const (
	// AuthNone sends no credentials.
	AuthNone AuthType = "none"
	// AuthBasic sends HTTP basic auth.
	AuthBasic AuthType = "basic"
	// AuthBearer sends "Authorization: Bearer <token>".
	AuthBearer AuthType = "bearer"
	// AuthHeaders sends the headers in Auth.Headers.
	AuthHeaders AuthType = "headers"
	// AuthCookie sends one cookie.
	AuthCookie AuthType = "cookie"
	// AuthHelper is the auth-helper extension point (task 099).
	AuthHelper AuthType = "helper"
)

// Cookie is the cookie a `cookie` auth type sends.
type Cookie struct {
	Name   string `yaml:"name"`
	Value  string `yaml:"value"`
	Domain string `yaml:"domain"`
}

// Helper is the `auth.helper` block. Task 099 implements it; this package only
// carries the schema so a profile that uses one parses and validates.
type Helper struct {
	Command   []string `yaml:"command"`
	CacheTTL  Duration `yaml:"cache_ttl"`
	RefreshOn []string `yaml:"refresh_on"`
}

// Auth is the `auth:` block of a profile.
type Auth struct {
	Type     AuthType          `yaml:"type"`
	Username string            `yaml:"username"`
	Password string            `yaml:"password"`
	Token    string            `yaml:"token"`
	Headers  map[string]string `yaml:"headers"`
	Cookie   Cookie            `yaml:"cookie"`
	Helper   *Helper           `yaml:"helper"`
}

// Profile is one named entry of connections.yaml (SPEC §7.2).
type Profile struct {
	// Name is the map key the profile was loaded under.
	Name    string            `yaml:"-"`
	Proxy   Proxy             `yaml:"proxy"`
	TLS     TLSConfig         `yaml:"tls"`
	Auth    Auth              `yaml:"auth"`
	Headers map[string]string `yaml:"headers"`
	Timeout Duration          `yaml:"timeout"`
	Retries int               `yaml:"retries"`
}

// ProfileError is a validation error that names the profile and the field.
func ProfileError(name, field, format string, args ...any) *errs.Error {
	msg := fmt.Sprintf(format, args...)
	return &errs.Error{
		Kind: errs.Config,
		Op:   "httpx.profile",
		Msg:  fmt.Sprintf("profile %q: %s: %s", name, field, msg),
	}
}

// Validate checks that the profile can build a client. Errors name the profile
// and the offending field.
func (p *Profile) Validate() error {
	name := p.Name
	if name == "" {
		return ProfileError(name, "name", "a profile needs a name")
	}
	switch p.Proxy.Type {
	case "", ProxyNone:
	case ProxyEnv:
		if p.Proxy.URL != "" || p.Proxy.Via != "" {
			return ProfileError(name, "proxy.url", "%s uses the environment; url and via must be empty", ProxyEnv)
		}
	case ProxyHTTP, ProxySOCKS5:
		if strings.TrimSpace(p.Proxy.URL) == "" {
			return ProfileError(name, "proxy.url", "%s needs a proxy address", p.Proxy.Type)
		}
		if p.Proxy.Via != "" {
			return ProfileError(name, "proxy.via", "%s does not use via", p.Proxy.Type)
		}
		if _, err := parseProxyURL(p.Proxy.URL, p.Proxy.Type); err != nil {
			return ProfileError(name, "proxy.url", "%v", err)
		}
	case ProxySSHSOCKS:
		if strings.TrimSpace(p.Proxy.Via) == "" {
			return ProfileError(name, "proxy.via", "%s needs the SSH host to dial through", ProxySSHSOCKS)
		}
		if p.Proxy.URL != "" {
			return ProfileError(name, "proxy.url", "%s does not use url", ProxySSHSOCKS)
		}
	default:
		return ProfileError(name, "proxy.type", "unknown proxy type %q (want none, env, http, socks5 or ssh-socks)", p.Proxy.Type)
	}
	if p.Proxy.Type != ProxyHTTP && len(p.Proxy.Headers) > 0 {
		return ProfileError(name, "proxy.headers", "only an http proxy takes headers")
	}

	if (p.TLS.ClientCert == "") != (p.TLS.ClientKey == "") {
		return ProfileError(name, "tls.client_cert", "client_cert and client_key must be set together")
	}

	switch p.Auth.Type {
	case "", AuthNone:
	case AuthBasic:
		if p.Auth.Username == "" {
			return ProfileError(name, "auth.username", "basic auth needs a username")
		}
	case AuthBearer:
		if strings.TrimSpace(p.Auth.Token) == "" {
			return ProfileError(name, "auth.token", "bearer auth needs a token")
		}
	case AuthHeaders:
		if len(p.Auth.Headers) == 0 {
			return ProfileError(name, "auth.headers", "headers auth needs at least one header")
		}
	case AuthCookie:
		if p.Auth.Cookie.Name == "" {
			return ProfileError(name, "auth.cookie.name", "cookie auth needs a name")
		}
		if p.Auth.Cookie.Value == "" {
			return ProfileError(name, "auth.cookie.value", "cookie auth needs a value")
		}
	case AuthHelper:
		if p.Auth.Helper == nil || len(p.Auth.Helper.Command) == 0 {
			return ProfileError(name, "auth.helper.command", "helper auth needs a command")
		}
	default:
		return ProfileError(name, "auth.type", "unknown auth type %q (want none, basic, bearer, headers, cookie or helper)", p.Auth.Type)
	}

	for k := range p.Headers {
		if strings.TrimSpace(k) == "" {
			return ProfileError(name, "headers", "a header name is empty")
		}
	}
	if p.Timeout.Duration() < 0 {
		return ProfileError(name, "timeout", "must not be negative")
	}
	if p.Retries < 0 {
		return ProfileError(name, "retries", "must not be negative")
	}
	if p.Retries > MaxRetries {
		return ProfileError(name, "retries", "at most %d", MaxRetries)
	}
	return nil
}

// timeout returns the profile timeout, defaulting to DefaultTimeout.
func (p *Profile) timeout() time.Duration {
	if d := p.Timeout.Duration(); d > 0 {
		return d
	}
	return DefaultTimeout
}

// authType returns the auth type with the empty value meaning none.
func (p *Profile) authType() AuthType {
	if p.Auth.Type == "" {
		return AuthNone
	}
	return p.Auth.Type
}

// connectionsDoc is the documented shape of connections.yaml (SPEC §7.2). The
// top-level `secrets:` block (one command per secret name) is read by
// LoadSecretCommands; it is accepted here so a file that carries it still
// decodes strictly.
type connectionsDoc struct {
	Connections map[string]*Profile      `yaml:"connections"`
	Secrets     map[string]SecretCommand `yaml:"secrets"`
}

// LoadProfiles reads a connections.yaml into profiles keyed by name. Like the
// main config loader, decoding is strict and a missing file is an empty set,
// not an error. `~/` paths are expanded with home, omitted timeouts take
// DefaultTimeout, and every profile is validated; the first error names the
// profile and the field.
func LoadProfiles(path, home string) (map[string]*Profile, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return map[string]*Profile{}, nil
	default:
		return nil, &errs.Error{
			Kind: errs.Config,
			Op:   "httpx.load",
			Msg:  fmt.Sprintf("cannot read %s", path),
			Err:  err,
		}
	}

	out := map[string]*Profile{}
	var probe struct {
		Connections yaml.Node `yaml:"connections"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, loadError(path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if probe.Connections.Kind != 0 {
		var doc connectionsDoc
		if err := dec.Decode(&doc); err != nil {
			return nil, loadError(path, err)
		}
		out = doc.Connections
	} else {
		if err := dec.Decode(&out); err != nil {
			return nil, loadError(path, err)
		}
	}
	if out == nil {
		out = map[string]*Profile{}
	}

	names := make([]string, 0, len(out))
	for name := range out {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := out[name]
		if p == nil {
			return nil, ProfileError(name, "profile", "is empty")
		}
		p.Name = name
		p.TLS.CAFile = expandHome(p.TLS.CAFile, home)
		p.TLS.ClientCert = expandHome(p.TLS.ClientCert, home)
		p.TLS.ClientKey = expandHome(p.TLS.ClientKey, home)
		if p.Timeout.Duration() == 0 {
			p.Timeout = Duration(DefaultTimeout)
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// loadError wraps a YAML decoding failure as a config error naming the file.
func loadError(path string, err error) error {
	return &errs.Error{
		Kind: errs.Config,
		Op:   "httpx.load",
		Msg:  fmt.Sprintf("invalid %s", path),
		Err:  err,
	}
}

// expandHome replaces a leading "~/" (or a bare "~") with home.
func expandHome(p, home string) string {
	if p == "" || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// redactorFor returns deps.Redactor, or the built-in patterns when it is nil.
func redactorFor(deps Deps) (*redact.Redactor, error) {
	if deps.Redactor != nil {
		return deps.Redactor, nil
	}
	return redact.New(nil)
}
