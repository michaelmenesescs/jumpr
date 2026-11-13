package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jumper/internal/errs"
)

func TestSecretEnvName(t *testing.T) {
	cases := map[string]string{
		"proxy-auth": "JUMPER_SECRET_PROXY_AUTH",
		"api_key":    "JUMPER_SECRET_API_KEY",
		"Token":      "JUMPER_SECRET_TOKEN",
		"db.pass":    "JUMPER_SECRET_DB_PASS",
	}
	for in, want := range cases {
		if got := SecretEnvName(in); got != want {
			t.Errorf("SecretEnvName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSecretFromEnv(t *testing.T) {
	s := NewSecrets(map[string]string{"JUMPER_SECRET_PROXY_AUTH": "Bearer env-value"}, nil)
	got, err := s.Value(context.Background(), "proxy-auth")
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got != "Bearer env-value" {
		t.Fatalf("value = %q", got)
	}
}

func TestSecretOrderEnvBeatsCommand(t *testing.T) {
	setHelperMode(t, "print")
	t.Setenv("HTTPX_HELPER_VALUE", "from-command")
	s := NewSecrets(
		map[string]string{"JUMPER_SECRET_API": "from-env"},
		map[string]SecretCommand{"api": {Command: []string{os.Args[0]}}},
	)
	got, err := s.Value(context.Background(), "api")
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got != "from-env" {
		t.Fatalf("value = %q, want the environment to win", got)
	}
}

func TestSecretFromKeychain(t *testing.T) {
	setHelperMode(t, "keychain")
	s := NewSecrets(nil, nil)
	// No {name} token: the name is appended.
	s.Security = []string{os.Args[0]}
	got, err := s.Value(context.Background(), "db-pass")
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got != "keychain-db-pass" {
		t.Fatalf("value = %q, want the keychain lookup", got)
	}
}

func TestSecretKeychainPlaceholderArgv(t *testing.T) {
	setHelperMode(t, "keychain")
	s := NewSecrets(nil, nil)
	// The {name} token is replaced in place, not appended.
	s.Security = []string{os.Args[0], "-s", "jumper", "-a", "{name}", "-w"}
	got, err := s.Value(context.Background(), "grafana")
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got != "keychain-grafana" {
		t.Fatalf("value = %q: the name is not substituted in place", got)
	}
}

// TestSecretOrderKeychainBeatsCommand checks the documented order: the
// Keychain is consulted before a configured `secrets:` command.
func TestSecretOrderKeychainBeatsCommand(t *testing.T) {
	setHelperMode(t, "keychain")
	s := NewSecrets(map[string]string{}, map[string]SecretCommand{
		"db-pass": {Command: []string{"/nonexistent/secret-command"}},
	})
	s.Security = []string{os.Args[0]}
	got, err := s.Value(context.Background(), "db-pass")
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got != "keychain-db-pass" {
		t.Fatalf("value = %q, want the keychain value before the command", got)
	}
}

func TestSecretFromCommand(t *testing.T) {
	setHelperMode(t, "print")
	t.Setenv("HTTPX_HELPER_VALUE", "token-from-command\nsecond line")
	s := NewSecrets(nil, map[string]SecretCommand{
		"grafana": {Command: []string{os.Args[0]}},
	})
	got, err := s.Value(context.Background(), "grafana")
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got != "token-from-command" {
		t.Fatalf("value = %q, want the first line", got)
	}
}

func TestSecretCommandFailureIsConfig(t *testing.T) {
	setHelperMode(t, "fail")
	s := NewSecrets(nil, map[string]SecretCommand{
		"grafana": {Command: []string{os.Args[0]}},
	})
	_, err := s.Value(context.Background(), "grafana")
	if err == nil {
		t.Fatal("a failing secrets: command was accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Fatalf("kind = %q, want config (%v)", kind, err)
	}
	if !strings.Contains(err.Error(), "could not reach the IdP") {
		t.Fatalf("the error hides the command's reason: %v", err)
	}
	if strings.Contains(err.Error(), "leak-me-1234") {
		t.Fatalf("the error leaked a credential: %v", err)
	}
}

func TestSecretMissingIsConfigError(t *testing.T) {
	s := NewSecrets(map[string]string{}, nil)
	s.Security = []string{"/nonexistent/security"}
	_, err := s.Value(context.Background(), "nope")
	if err == nil {
		t.Fatal("a missing secret was resolved")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Fatalf("kind = %q, want config (%v)", kind, err)
	}
	for _, want := range []string{"nope", "JUMPER_SECRET_NOPE", "keychain"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not mention %q: %v", want, err)
		}
	}
	if _, err := s.Value(context.Background(), ""); err == nil {
		t.Fatal("an empty secret name was resolved")
	}
}

func TestSecretExpandTemplates(t *testing.T) {
	s := NewSecrets(map[string]string{
		"JUMPER_SECRET_A": "alpha",
		"JUMPER_SECRET_B": "beta",
	}, nil)
	got, err := s.Expand(context.Background(), `Basic {{ secret "a" }}:{{secret 'b'}} end`)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if want := "Basic alpha:beta end"; got != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
	if got, err := s.Expand(context.Background(), "no template here"); err != nil || got != "no template here" {
		t.Fatalf("plain text = %q, %v", got, err)
	}
	if _, err := s.Expand(context.Background(), `{{ secret "missing" }}`); err == nil {
		t.Fatal("an unresolved template was accepted")
	}
}

func TestSecretExpandsProfileHeaders(t *testing.T) {
	rec := &headerRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
	}))
	defer srv.Close()

	p := &Profile{Headers: map[string]string{"X-Api-Key": `{{ secret "api" }}`}}
	c := newClient(t, p, Deps{Env: map[string]string{"JUMPER_SECRET_API": "k-1"}})
	if _, _, err := get(t, c, srv.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	h, _ := rec.get()
	if got := h.Get("X-Api-Key"); got != "k-1" {
		t.Fatalf("X-Api-Key = %q, want the resolved secret", got)
	}
}

func TestSecretResolutionFailureIsConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	p := &Profile{Auth: Auth{Type: AuthBearer, Token: `{{ secret "missing" }}`}}
	secs := NewSecrets(map[string]string{}, nil)
	secs.Security = []string{"/nonexistent/security"}
	c := newClient(t, p, Deps{Secrets: secs})

	_, _, err := get(t, c, srv.URL)
	if err == nil {
		t.Fatal("an unresolvable secret was sent")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Fatalf("kind = %q, want config (%v)", kind, err)
	}
}

func TestSecretLiteralCredentialWarning(t *testing.T) {
	var warned []string
	p := &Profile{
		Name: "corp",
		Auth: Auth{Type: AuthBasic, Username: "monitor", Password: "hunter2"},
	}
	c := newClient(t, p, Deps{Warn: func(msg string) { warned = append(warned, msg) }})

	warnings := c.Warnings()
	if len(warnings) == 0 {
		t.Fatal("a literal credential produced no :health warning")
	}
	if !strings.Contains(warnings[0], "corp") || !strings.Contains(warnings[0], "auth.password") {
		t.Fatalf("warning = %q, want the profile and the field", warnings[0])
	}
	if strings.Contains(strings.Join(warnings, " "), "hunter2") {
		t.Fatalf("the warning leaks the credential: %v", warnings)
	}
	if len(warned) != len(warnings) {
		t.Fatalf("Deps.Warn got %v, want the same warnings the client keeps", warned)
	}
}

func TestSecretTemplateProducesNoWarning(t *testing.T) {
	p := &Profile{
		Name: "corp",
		Auth: Auth{Type: AuthBearer, Token: `{{ secret "token" }}`},
	}
	c := newClient(t, p, Deps{Env: map[string]string{"JUMPER_SECRET_TOKEN": "t"}})
	if warnings := c.Warnings(); len(warnings) != 0 {
		t.Fatalf("a templated credential warned: %v", warnings)
	}
}

func TestLoadSecretCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "connections.yaml")
	content := `connections:
  lab:
    auth:
      type: bearer
      token: "{{ secret \"lab-token\" }}"
secrets:
  lab-token:
    command: ["/usr/bin/true"]
  other:
    command: ["/usr/bin/true", "two"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmds, err := LoadSecretCommands(path)
	if err != nil {
		t.Fatalf("LoadSecretCommands: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("commands = %v, want two", cmds)
	}
	if got := cmds["lab-token"].Command; len(got) != 1 || got[0] != "/usr/bin/true" {
		t.Fatalf("lab-token = %v", got)
	}
	if got := cmds["other"].Command; len(got) != 2 {
		t.Fatalf("other = %v", got)
	}

	empty, err := LoadSecretCommands(filepath.Join(dir, "absent.yaml"))
	if err != nil {
		t.Fatalf("a missing file must not be an error: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("a missing file produced %v", empty)
	}
}

// TestSecretConnectionsFileWithSecrets checks that the file the secrets: block
// lives in still loads strictly as profiles, which it must for the block to be
// usable at all.
func TestSecretConnectionsFileWithSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "connections.yaml")
	content := `connections:
  lab:
    auth:
      type: helper
      helper:
        command: ["~/sso-login"]
secrets:
  lab-token:
    command: ["/usr/bin/true"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	profiles, err := LoadProfiles(path, dir)
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	if len(profiles) != 1 || profiles["lab"] == nil {
		t.Fatalf("profiles = %v", profiles)
	}
	if got := profiles["lab"].Auth.Helper.Command[0]; got != "~/sso-login" {
		t.Fatalf("command = %q", got)
	}
}
