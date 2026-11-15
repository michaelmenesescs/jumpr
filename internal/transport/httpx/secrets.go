package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"jumpr/internal/errs"
	"jumpr/internal/redact"
)

const (
	// SecretEnvPrefix prefixes the environment form of a secret:
	// JUMPR_SECRET_<NAME>, with the name upper-cased and anything that is not
	// a letter or digit turned into an underscore.
	SecretEnvPrefix = "JUMPR_SECRET_"
	// SecurityBin is the macOS Keychain CLI.
	SecurityBin = "/usr/bin/security"
	// KeychainService is the `-s` jumpr looks a secret up under.
	KeychainService = "jumpr"
	// SecretTimeout caps one Keychain or command lookup.
	SecretTimeout = 15 * time.Second
	// secretOutputCap caps what a secret command may print.
	secretOutputCap = 64 << 10
)

// secretRE matches a {{ secret "name" }} reference. Single quotes work too,
// because YAML values are often single-quoted.
var secretRE = regexp.MustCompile(`\{\{\s*secret\s+["']([^"']+)["']\s*\}\}`)

// SecretCommand is one entry of the `secrets:` block of connections.yaml: the
// argv that prints one secret on stdout. It is run directly, never through a
// shell.
type SecretCommand struct {
	Command []string `yaml:"command"`
}

// Secrets resolves the `{{ secret "name" }}` references of profile strings
// (SPEC §7.2) in three places, in this order: the environment, the macOS
// Keychain, then a configured command. Resolved values are cached in memory
// for the lifetime of the resolver and are never written anywhere.
//
// A Secrets is safe for concurrent use.
type Secrets struct {
	env      map[string]string
	commands map[string]SecretCommand

	// Security is the argv prefix used for a Keychain lookup. The literal
	// token {name} is replaced by the secret name; without it the name is
	// appended as the last argument. The empty value means the default
	// `/usr/bin/security find-generic-password -s jumpr -a {name} -w`.
	Security []string
	// Timeout caps one Keychain or command lookup. Zero means SecretTimeout.
	Timeout time.Duration

	mu    sync.Mutex
	cache map[string]string
	// red cleans a command's stderr before it reaches an error message.
	red *redact.Redactor
}

// NewSecrets builds a resolver from the environment (typically Deps.Env) and
// the configured commands. Both may be nil; nothing is read from the process
// environment.
func NewSecrets(env map[string]string, commands map[string]SecretCommand) *Secrets {
	red, _ := redact.New(nil)
	return &Secrets{env: env, commands: commands, cache: map[string]string{}, red: red}
}

// SecretEnvName is the environment key for a secret name.
func SecretEnvName(name string) string {
	var b strings.Builder
	b.WriteString(SecretEnvPrefix)
	for _, r := range strings.ToUpper(strings.TrimSpace(name)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Value resolves one secret. The error names the secret and the sources that
// were tried, never a value.
func (s *Secrets) Value(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errs.New(errs.Config, "httpx.secrets", "an empty secret name")
	}
	s.mu.Lock()
	if v, ok := s.cache[name]; ok {
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()

	var tried []string
	if s.env != nil {
		key := SecretEnvName(name)
		tried = append(tried, key)
		if v := strings.TrimSpace(s.env[key]); v != "" {
			s.remember(name, v)
			return v, nil
		}
	}

	tried = append(tried, "the keychain")
	if v, err := s.keychain(ctx, name); err == nil && v != "" {
		s.remember(name, v)
		return v, nil
	}

	tried = append(tried, "a secrets: command")
	if cmd, ok := s.commands[name]; ok && len(cmd.Command) > 0 {
		v, err := s.run(ctx, cmd.Command)
		if err != nil {
			return "", err
		}
		s.remember(name, v)
		return v, nil
	}

	return "", errs.New(errs.Config, "httpx.secrets",
		fmt.Sprintf("secret %q is not set (checked %s)", name, strings.Join(tried, ", ")))
}

// remember caches a resolved value.
func (s *Secrets) remember(name, value string) {
	s.mu.Lock()
	s.cache[name] = value
	s.mu.Unlock()
}

// keychain looks the secret up in the macOS Keychain. A missing entry is not
// an error here: the next source is tried.
func (s *Secrets) keychain(ctx context.Context, name string) (string, error) {
	argv := s.Security
	if len(argv) == 0 {
		argv = []string{SecurityBin, "find-generic-password", "-s", KeychainService, "-a", "{name}", "-w"}
	}
	args := make([]string, 0, len(argv)+1)
	replaced := false
	for _, a := range argv {
		if strings.Contains(a, "{name}") {
			replaced = true
			a = strings.ReplaceAll(a, "{name}", name)
		}
		args = append(args, a)
	}
	if !replaced {
		args = append(args, name)
	}
	return s.run(ctx, args)
}

// run executes a secret command directly and returns the first line of its
// stdout. The output is a credential: it never reaches an error message.
func (s *Secrets) run(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return "", errs.New(errs.Config, "httpx.secrets", "a secrets: command is empty")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = SecretTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out := &capBuffer{limit: secretOutputCap}
	errsOut := &capBuffer{limit: secretOutputCap}
	cmd.Stdout = out
	cmd.Stderr = errsOut
	if err := cmd.Run(); err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return "", errs.New(errs.Timeout, "httpx.secrets",
				fmt.Sprintf("the secret command timed out after %s", timeout))
		case errors.Is(err, exec.ErrNotFound):
			return "", errs.New(errs.NotAvailable, "httpx.secrets",
				fmt.Sprintf("the secret command %q was not found", argv[0]))
		default:
			detail := strings.TrimSpace(errsOut.String())
			if detail == "" {
				detail = err.Error()
			}
			detail = strings.SplitN(detail, "\n", 2)[0]
			if s.red != nil {
				detail = s.red.String(detail)
			}
			return "", errs.New(errs.Config, "httpx.secrets",
				"the secret command failed: "+detail)
		}
	}
	line := strings.TrimSpace(out.String())
	if line == "" {
		return "", errs.New(errs.Config, "httpx.secrets", "the secret command printed nothing")
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return line, nil
}

// Expand replaces every `{{ secret "name" }}` in text. The first unresolvable
// name ends the scan and is returned as an error.
func (s *Secrets) Expand(ctx context.Context, text string) (string, error) {
	if text == "" || !strings.Contains(text, "{{") {
		return text, nil
	}
	var firstErr error
	out := secretRE.ReplaceAllStringFunc(text, func(m string) string {
		if firstErr != nil {
			return m
		}
		sub := secretRE.FindStringSubmatch(m)
		v, err := s.Value(ctx, sub[1])
		if err != nil {
			firstErr = err
			return m
		}
		return v
	})
	if firstErr != nil {
		return text, firstErr
	}
	return out, nil
}

// capBuffer keeps at most limit bytes of what is written to it.
type capBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *capBuffer) String() string { return b.buf.String() }

// SecretWarnings returns :health warnings for credentials written as literals
// in connections.yaml. A value that contains a `{{` template is not a literal.
// The warning never contains the value.
func (p *Profile) SecretWarnings() []string {
	name := p.Name
	var out []string
	warn := func(field, value string) {
		if value == "" || isTemplate(value) {
			return
		}
		out = append(out, fmt.Sprintf(
			"profile %q: %s is a literal credential; use {{ secret \"name\" }} instead", name, field))
	}
	switch p.authType() {
	case AuthBasic:
		warn("auth.password", p.Auth.Password)
	case AuthBearer:
		warn("auth.token", p.Auth.Token)
	case AuthCookie:
		warn("auth.cookie.value", p.Auth.Cookie.Value)
	}
	warn("proxy.password", p.Proxy.Password)
	for k, v := range p.Auth.Headers {
		warn(fmt.Sprintf("auth.headers.%s", k), v)
	}
	for k, v := range p.Proxy.Headers {
		warn(fmt.Sprintf("proxy.headers.%s", k), v)
	}
	for k, v := range p.Headers {
		warn(fmt.Sprintf("headers.%s", k), v)
	}
	return out
}

// isTemplate reports whether s is (or contains) a `{{ … }}` template.
func isTemplate(s string) bool { return strings.Contains(s, "{{") }

// LoadSecretCommands reads the top-level `secrets:` block of a YAML file —
// normally connections.yaml, next to the profiles that use it. A missing file
// or a missing block is an empty set, not an error; other top-level keys are
// ignored.
func LoadSecretCommands(path string) (map[string]SecretCommand, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return map[string]SecretCommand{}, nil
	default:
		return nil, &errs.Error{
			Kind: errs.Config,
			Op:   "httpx.secrets",
			Msg:  fmt.Sprintf("cannot read %s", path),
			Err:  err,
		}
	}
	var doc map[string]yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &errs.Error{
			Kind: errs.Config,
			Op:   "httpx.secrets",
			Msg:  fmt.Sprintf("invalid %s", path),
			Err:  err,
		}
	}
	out := map[string]SecretCommand{}
	node, ok := doc["secrets"]
	if !ok || node.Kind == 0 {
		return out, nil
	}
	if err := node.Decode(&out); err != nil {
		return nil, &errs.Error{
			Kind: errs.Config,
			Op:   "httpx.secrets",
			Msg:  fmt.Sprintf("invalid secrets: block in %s", path),
			Err:  err,
		}
	}
	return out, nil
}
