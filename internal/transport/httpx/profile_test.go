package httpx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
)

// writeFile writes content into a fresh temp dir and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestLoadProfiles(t *testing.T) {
	path := writeFile(t, "connections.yaml", `
connections:
  corp-sso:
    proxy:
      type: http
      url: http://proxy.example.internal:3128
      headers:
        Proxy-Authorization: Basic abc
    tls:
      ca_file: ~/config/ca/corp-root.pem
      server_name: app.example.internal
    auth:
      type: bearer
      token: s3cr3t
    headers:
      X-Requested-By: jumpr
    retries: 2
  plain:
    proxy:
      type: none
`)
	profiles, err := LoadProfiles(path, "/home/tester")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("got %d profiles, want 2", len(profiles))
	}
	p := profiles["corp-sso"]
	if p == nil {
		t.Fatal("corp-sso is missing")
	}
	if p.Name != "corp-sso" {
		t.Fatalf("Name = %q, want corp-sso", p.Name)
	}
	if p.Proxy.Type != ProxyHTTP || p.Proxy.URL != "http://proxy.example.internal:3128" {
		t.Fatalf("proxy = %+v", p.Proxy)
	}
	if got := p.Proxy.Headers["Proxy-Authorization"]; got != "Basic abc" {
		t.Fatalf("proxy headers = %v", p.Proxy.Headers)
	}
	if want := filepath.Join("/home/tester", "config/ca/corp-root.pem"); p.TLS.CAFile != want {
		t.Fatalf("ca_file = %q, want %q", p.TLS.CAFile, want)
	}
	if p.TLS.ServerName != "app.example.internal" {
		t.Fatalf("server_name = %q", p.TLS.ServerName)
	}
	if p.Auth.Type != AuthBearer || p.Auth.Token != "s3cr3t" {
		t.Fatalf("auth = %+v", p.Auth)
	}
	if p.Headers["X-Requested-By"] != "jumpr" {
		t.Fatalf("headers = %v", p.Headers)
	}
	if p.Retries != 2 {
		t.Fatalf("retries = %d, want 2", p.Retries)
	}
	if got := p.Timeout.Duration(); got != DefaultTimeout {
		t.Fatalf("timeout = %s, want the default %s", got, DefaultTimeout)
	}
	if profiles["plain"].Proxy.Type != ProxyNone {
		t.Fatalf("plain proxy type = %q", profiles["plain"].Proxy.Type)
	}
}

func TestLoadProfilesPlainMap(t *testing.T) {
	path := writeFile(t, "connections.yaml", "lab:\n  timeout: 5s\n")
	profiles, err := LoadProfiles(path, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := profiles["lab"].Timeout.Duration(); got != 5*time.Second {
		t.Fatalf("timeout = %s, want 5s", got)
	}
}

func TestLoadProfilesMissingFile(t *testing.T) {
	profiles, err := LoadProfiles(filepath.Join(t.TempDir(), "absent.yaml"), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(profiles) != 0 {
		t.Fatalf("got %d profiles, want none", len(profiles))
	}
}

func TestLoadProfilesStrict(t *testing.T) {
	path := writeFile(t, "connections.yaml", `
connections:
  corp:
    proxy:
      type: none
    unknown_key: true
`)
	_, err := LoadProfiles(path, "")
	if err == nil {
		t.Fatal("unknown key was accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Fatalf("kind = %q, want config", kind)
	}
	if !strings.Contains(err.Error(), "unknown_key") {
		t.Fatalf("error does not name the unknown key: %v", err)
	}
}

func TestValidateProfile(t *testing.T) {
	cases := []struct {
		name    string
		profile Profile
		want    string // substring the error must contain
	}{
		{
			name:    "unknown proxy type",
			profile: Profile{Name: "p", Proxy: Proxy{Type: "carrier-pigeon"}},
			want:    `profile "p": proxy.type`,
		},
		{
			name:    "http without url",
			profile: Profile{Name: "p", Proxy: Proxy{Type: ProxyHTTP}},
			want:    `profile "p": proxy.url`,
		},
		{
			name:    "http without port",
			profile: Profile{Name: "p", Proxy: Proxy{Type: ProxyHTTP, URL: "http://proxy.example.internal"}},
			want:    `profile "p": proxy.url`,
		},
		{
			name:    "ssh-socks without via",
			profile: Profile{Name: "p", Proxy: Proxy{Type: ProxySSHSOCKS}},
			want:    `profile "p": proxy.via`,
		},
		{
			name:    "env with url",
			profile: Profile{Name: "p", Proxy: Proxy{Type: ProxyEnv, URL: "http://proxy:3128"}},
			want:    `profile "p": proxy.url`,
		},
		{
			name:    "half a client certificate",
			profile: Profile{Name: "p", Proxy: Proxy{Type: ProxyNone}, TLS: TLSConfig{ClientCert: "/tmp/c.pem"}},
			want:    `profile "p": tls.client_cert`,
		},
		{
			name:    "bearer without token",
			profile: Profile{Name: "p", Auth: Auth{Type: AuthBearer}},
			want:    `profile "p": auth.token`,
		},
		{
			name:    "basic without username",
			profile: Profile{Name: "p", Auth: Auth{Type: AuthBasic}},
			want:    `profile "p": auth.username`,
		},
		{
			name:    "cookie without a name",
			profile: Profile{Name: "p", Auth: Auth{Type: AuthCookie, Cookie: Cookie{Value: "v"}}},
			want:    `profile "p": auth.cookie.name`,
		},
		{
			name:    "unknown auth type",
			profile: Profile{Name: "p", Auth: Auth{Type: "kerberos"}},
			want:    `profile "p": auth.type`,
		},
		{
			name:    "too many retries",
			profile: Profile{Name: "p", Retries: MaxRetries + 1},
			want:    `profile "p": retries`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.profile.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if kind := errs.KindOf(err); kind != errs.Config {
				t.Fatalf("kind = %q, want config", kind)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestProfileTimeoutDefault(t *testing.T) {
	p := &Profile{Name: "p"}
	if got := p.timeout(); got != DefaultTimeout {
		t.Fatalf("timeout() = %s, want %s", got, DefaultTimeout)
	}
	p.Timeout = Duration(2 * time.Second)
	if got := p.timeout(); got != 2*time.Second {
		t.Fatalf("timeout() = %s, want 2s", got)
	}
}

func TestProxyURL(t *testing.T) {
	if u, err := parseProxyURL("socks5://bastion.example.internal:1080", ProxySOCKS5); err != nil {
		t.Fatalf("socks5 url: %v", err)
	} else if got := socksAddr(u); got != "bastion.example.internal:1080" {
		t.Fatalf("socksAddr = %q", got)
	}
	if _, err := parseProxyURL("http://user:pass@proxy:3128", ProxyHTTP); err == nil {
		t.Fatal("credentials in the URL were accepted")
	}
	if _, err := parseProxyURL("socks5://proxy:1080", ProxyHTTP); err == nil {
		t.Fatal("a socks5 scheme was accepted as an http proxy")
	}
}
