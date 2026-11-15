package redact

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jumpr/internal/errs"

	"jumpr/internal/testload"
)

// Values used below are deliberately fake test data, not real credentials.
const (
	fakeBearer    = "tok_9fQ2xV7pLm4Zb1RtY6uW"
	fakePassword  = "hunter2-not-real"
	fakeAWSKey    = "AKIAIOSFODNN7EXAMPLE"
	fakeAWSSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	fakeJWT       = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
)

func newRedactor(t *testing.T, extra ...string) *Redactor {
	t.Helper()
	r, err := New(extra)
	if err != nil {
		t.Fatalf("New(%v): %v", extra, err)
	}
	return r
}

var patternCases = []struct {
	name string
	in   string
	want string
}{
	// bearer
	{"bearer header", "Authorization: Bearer " + fakeBearer,
		"Authorization: Bearer ‹redacted:bearer›"},
	{"bearer header lowercase", "authorization:  bearer  AbCdEf0123456789+/=",
		"authorization:  bearer  ‹redacted:bearer›"},
	{"bearer bare token", "proxy log: forward bearer abcdef0123456789abcdef ok",
		"proxy log: forward bearer ‹redacted:bearer› ok"},
	{"bearer prose", "the bearer of bad news", "the bearer of bad news"},
	{"bearer short token", "bearer abc123", "bearer abc123"},
	{"bearer mention without token", "Bearer authentication is disabled", "Bearer authentication is disabled"},

	// basic
	{"basic header", "Authorization: Basic dXNlcjpwYXNzd29yZA==",
		"Authorization: Basic ‹redacted:basic›"},
	{"basic prose", "basic auth is enabled on this endpoint", "basic auth is enabled on this endpoint"},

	// aws-key
	{"aws key", "key " + fakeAWSKey + " loaded", "key ‹redacted:aws-key› loaded"},
	{"aws key asia", "cred ASIAIOSFODNN7EXAMPL2 here", "cred ‹redacted:aws-key› here"},
	{"aws key short", "AKIA123 is too short", "AKIA123 is too short"},

	// aws-secret
	{"aws secret", "aws_secret_access_key = " + fakeAWSSecret,
		"aws_secret_access_key = ‹redacted:aws-secret›"},
	{"aws secret named", "aws_secret_access_key: abcdefghijklmnop",
		"aws_secret_access_key: ‹redacted:aws-secret›"},
	{"aws secret prefix", "aws_secret_access_key_present: yes", "aws_secret_access_key_present: yes"},

	// private-key
	{"private key rsa", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
		"‹redacted:private-key›"},
	{"private key openssh unterminated", "before\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n",
		"before\n‹redacted:private-key›"},
	{"private key public not redacted", "-----BEGIN PUBLIC KEY-----\nMIIBIjANBg\n-----END PUBLIC KEY-----",
		"-----BEGIN PUBLIC KEY-----\nMIIBIjANBg\n-----END PUBLIC KEY-----"},

	// password
	{"password assignment", "password=" + fakePassword, "password=‹redacted:password›"},
	{"passwd colon", "passwd: " + fakePassword, "passwd: ‹redacted:password›"},
	{"password quoted", `password="two words"`, `password=‹redacted:password›`},
	{"token assignment", "token=abcdef123456", "token=‹redacted:password›"},
	{"secret single quoted", "secret: 'abc def'", "secret: ‹redacted:password›"},
	{"api key underscore", "api_key = xyzzy", "api_key = ‹redacted:password›"},
	{"api-key dash", "api-key: xyzzy", "api-key: ‹redacted:password›"},
	// The value pattern is \S+, so it also swallows the rest of a query
	// string. The secret is gone; the trailing parameters go with it.
	{"password in url query", "https://example.internal/login?password=" + fakePassword,
		"https://example.internal/login?password=‹redacted:password›"},
	{"sshd passwordauthentication untouched", "PasswordAuthentication yes",
		"PasswordAuthentication yes"},
	{"token bucket untouched", "token bucket refill rate", "token bucket refill rate"},
	{"secret sauce untouched", "the secret sauce recipe", "the secret sauce recipe"},

	// url-cred
	{"url credentials", "postgres://app:" + fakePassword + "@db.example.internal:5432/x",
		"postgres://‹redacted:url-cred›@db.example.internal:5432/x"},
	{"url credentials http", "http://user:p%40ss@example.internal/",
		"http://‹redacted:url-cred›@example.internal/"},
	{"url without credentials", "https://example.internal/x", "https://example.internal/x"},
	{"url user only", "ssh://git@example.internal/repo", "ssh://git@example.internal/repo"},

	// jwt
	{"jwt", "token " + fakeJWT + " end", "token ‹redacted:jwt› end"},
	{"jwt too short", "eyJhbGci.short.thing", "eyJhbGci.short.thing"},

	// cookie
	{"cookie header", "Cookie: session=abc123; theme=dark", "Cookie: ‹redacted:cookie›"},
	{"set-cookie header", "Set-Cookie: sid=zzz; Path=/; HttpOnly", "Set-Cookie: ‹redacted:cookie›"},
	{"cookie prose", "I like cookies with coffee", "I like cookies with coffee"},
}

func TestBuiltinPatterns(t *testing.T) {
	r := newRedactor(t)
	for _, c := range patternCases {
		t.Run(c.name, func(t *testing.T) {
			got := r.String(c.in)
			if got != c.want {
				t.Fatalf("String(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
			if again := r.String(got); again != got {
				t.Fatalf("not idempotent for %q:\n once %q\n twice %q", c.in, got, again)
			}
		})
	}
}

// reference applies every pattern to the whole text in order, with no
// pre-filter and no line splitting: the semantics the line-scoped fast path
// must reproduce.
func reference(r *Redactor, s string) string {
	for _, ru := range r.whole {
		s = ru.re.ReplaceAllString(s, ru.repl)
	}
	for _, ru := range r.lines {
		s = ru.re.ReplaceAllString(s, ru.repl)
	}
	for _, ru := range r.user {
		s = ru.re.ReplaceAllString(s, ru.repl)
	}
	return s
}

func TestFastPathMatchesReference(t *testing.T) {
	r := newRedactor(t, `internal-\d+`)
	inputs := []string{
		"",
		"no secrets here at all",
		strings.Join([]string{
			"Authorization: Bearer " + fakeBearer,
			"aws_secret_access_key = " + fakeAWSSecret,
			"key " + fakeAWSKey,
			"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
			"password=" + fakePassword + " token=abc123",
			"postgres://app:" + fakePassword + "@db.example.internal:5432/x",
			fakeJWT,
			"Set-Cookie: sid=zzz; Path=/",
			"host internal-7 is up",
		}, "\n"),
	}
	for _, in := range inputs {
		if got, want := r.String(in), reference(r, in); got != want {
			t.Errorf("fast path differs from reference\ninput %q\n got  %q\n want %q", in, got, want)
		}
	}
	for _, c := range patternCases {
		if got, want := r.String(c.in), reference(r, c.in); got != want {
			t.Errorf("%s: fast path %q, reference %q", c.name, got, want)
		}
	}
}

func TestIdempotent(t *testing.T) {
	r := newRedactor(t)
	in := strings.Join([]string{
		"Authorization: Bearer " + fakeBearer,
		"aws_secret_access_key = " + fakeAWSSecret,
		"key " + fakeAWSKey,
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
		"password=" + fakePassword + " token=abc123",
		"postgres://app:" + fakePassword + "@db.example.internal:5432/x",
		fakeJWT,
		"Set-Cookie: sid=zzz; Path=/",
	}, "\n")
	once := r.String(in)
	twice := r.String(once)
	if once != twice {
		t.Fatalf("not idempotent:\n once %q\n twice %q", once, twice)
	}
	for _, leak := range []string{fakePassword, "wJalrXUtnFEMI", fakeAWSKey, "MIIE", "dozjgNryP4J3", "sid=zzz"} {
		if strings.Contains(once, leak) {
			t.Errorf("leaked %q in %q", leak, once)
		}
	}
}

func TestBytes(t *testing.T) {
	r := newRedactor(t)
	in := []byte("token=abc123 and more")
	got := string(r.Bytes(in))
	want := "token=‹redacted:password› and more"
	if got != want {
		t.Fatalf("Bytes = %q, want %q", got, want)
	}
	if r.Bytes(nil) != nil {
		t.Fatal("Bytes(nil) should stay nil")
	}
}

func TestExtraPatterns(t *testing.T) {
	r := newRedactor(t, `internal-[0-9]+\.example\.internal`)
	got := r.String("host internal-42.example.internal is up")
	want := "host ‹redacted:extra:1› is up"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// A second pattern gets its own marker, and order is preserved.
	r2 := newRedactor(t, `alpha-\d+`, `beta-\d+`)
	got2 := r2.String("alpha-1 beta-2")
	want2 := "‹redacted:extra:1› ‹redacted:extra:2›"
	if got2 != want2 {
		t.Fatalf("got %q, want %q", got2, want2)
	}
	// User patterns may span lines.
	r3 := newRedactor(t, `(?s)BEGIN-USER-.*?END-USER`)
	got3 := r3.String("a\nBEGIN-USER-secret\nEND-USER\nb")
	if got3 != "a\n‹redacted:extra:1›\nb" {
		t.Fatalf("multi-line extra pattern: %q", got3)
	}
}

func TestBadPattern(t *testing.T) {
	_, err := New([]string{"("})
	if err == nil {
		t.Fatal("expected an error for a bad pattern")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *errs.Error, got %T", err)
	}
	if e.Kind != errs.Config {
		t.Fatalf("kind = %q, want %q", e.Kind, errs.Config)
	}
	if !strings.Contains(e.Msg, "1") {
		t.Errorf("message should name the pattern position: %q", e.Msg)
	}
}

func TestCap(t *testing.T) {
	r := newRedactor(t)
	lines := []string{
		"line one password=" + fakePassword,
		"line two",
		"line three",
		"line four",
		"line five",
		"line six",
	}
	in := strings.Join(lines, "\n")

	// Text that fits is returned redacted and unchanged in shape.
	if got := r.Cap(in, len(in)+100); got != r.String(in) {
		t.Fatalf("Cap should not cut a fitting input: %q", got)
	}

	const max = 70
	got := r.Cap(in, max)
	if len(got) > max {
		t.Fatalf("Cap returned %d bytes, want <= %d: %q", len(got), max, got)
	}
	if strings.Contains(got, fakePassword) {
		t.Fatalf("Cap did not redact: %q", got)
	}
	if !strings.Contains(got, "more lines)") {
		t.Fatalf("Cap missing the omitted-lines notice: %q", got)
	}
	body, notice, ok := strings.Cut(got, "\n… (")
	if !ok {
		t.Fatalf("Cap notice malformed: %q", got)
	}
	kept := strings.Split(body, "\n")
	n, err := strconv.Atoi(strings.TrimSuffix(notice, " more lines)"))
	if err != nil {
		t.Fatalf("cannot read omitted count from %q: %v", notice, err)
	}
	if len(kept)+n != len(lines) {
		t.Fatalf("kept %d + omitted %d != %d lines: %q", len(kept), n, len(lines), got)
	}
	if !strings.HasPrefix(r.String(in), body) {
		t.Fatalf("kept body is not a prefix of the redacted input: %q", body)
	}
	if strings.HasSuffix(body, "\n") {
		t.Fatalf("Cap should cut on a line boundary: %q", body)
	}

	if got := r.Cap(in, 0); got != "" {
		t.Fatalf("Cap(_, 0) = %q, want empty", got)
	}
	if got := r.Cap("", 10); got != "" {
		t.Fatalf("Cap(\"\", 10) = %q, want empty", got)
	}
}

func TestCapTinyBudget(t *testing.T) {
	r := newRedactor(t)
	in := "aaaaaaaaaaaaaaaaaaaa\nbbbbbbbbbbbbbbbbbbbb"
	got := r.Cap(in, 8)
	if len(got) > 8 {
		t.Fatalf("Cap(%d) returned %d bytes: %q", 8, len(got), got)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := newRedactor(t, `internal-\d+`)
	in := "Authorization: Bearer " + fakeBearer + " password=" + fakePassword + " internal-7"
	want := r.String(in)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := r.String(in); got != want {
					t.Errorf("concurrent String = %q, want %q", got, want)
					return
				}
				_ = r.Bytes([]byte(in))
				_ = r.Cap(in, 20)
			}
		}()
	}
	wg.Wait()
}

// realisticLog builds a megabyte of ordinary syslog noise with a secret every
// few dozen lines — what a journal or investigation capture actually looks
// like.
func realisticLog() string {
	noise := []string{
		`Nov 12 14:02:11 web-ewr-001 sshd[4421]: Accepted publickey for deploy from 10.20.0.9 port 51234 ssh2: ED25519 SHA256:9xQ2k8pQ3wV7`,
		`Nov 12 14:02:11 web-ewr-001 systemd[1]: Started Session 8812 of user deploy.`,
		`Nov 12 14:02:12 web-ewr-001 nginx[2210]: 10.20.0.9 - - [12/Nov/2026:14:02:12 -0500] "GET /health HTTP/1.1" 200 2 "-" "kube-probe/1.28"`,
		`Nov 12 14:02:13 web-ewr-002 kernel: [ 8812.442133] TCP: request_sock_TCP: Possible SYN flooding on port 443. Sending cookies.`,
		`Nov 12 14:02:14 web-ewr-002 salt-minion[1102]: [INFO    ] Salt request timed out. Retrying in 3s.`,
	}
	secret := []string{
		"Authorization: Bearer " + fakeBearer,
		"password=" + fakePassword + " user=deploy",
		"https://svc:s3cr3t@example.internal/health",
	}
	var b strings.Builder
	for i := 0; b.Len() < 1<<20; i++ {
		b.WriteString(noise[i%len(noise)])
		b.WriteString("\n")
		if i%37 == 0 {
			b.WriteString(secret[(i/37)%len(secret)])
			b.WriteString("\n")
		}
	}
	return b.String()
}

func TestLargeInputFast(t *testing.T) {
	r := newRedactor(t)
	in := realisticLog()
	if len(in) < 1<<20 {
		t.Fatalf("input is only %d bytes", len(in))
	}

	start := time.Now()
	out := r.String(in)
	elapsed := time.Since(start)
	// A wall-clock budget is only meaningful on a machine that is not already
	// oversubscribed; the parallel build runs six `go test` worktrees on one
	// 10-core box and starves every timing assertion. The redaction itself is
	// still asserted below.
	if load, ok := osLoadPerCPU(); ok && load > 0.75 {
		t.Logf("load average %.2f per CPU: skipping the 200 ms budget, redacted %d bytes in %v",
			load, len(in), elapsed)
	} else if elapsed > testload.Scale(200*time.Millisecond) {
		t.Fatalf("redacting %d bytes took %v, want < 200ms", len(in), elapsed)
	}
	for _, leak := range []string{fakePassword, "s3cr3t", fakeBearer} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q", leak)
		}
	}
	if !strings.Contains(out, "‹redacted:bearer›") || !strings.Contains(out, "‹redacted:url-cred›") {
		t.Fatalf("expected marks are missing from the output")
	}
}

func TestNilRedactor(t *testing.T) {
	var r *Redactor
	if got := r.String("password=x"); got != "password=x" {
		t.Fatalf("nil Redactor changed the text: %q", got)
	}
	var nilBytes *Redactor
	if got := nilBytes.String(""); got != "" {
		t.Fatalf("nil Redactor changed the text: %q", got)
	}
}

// osLoadPerCPU returns the 1-minute load average per logical CPU.
func osLoadPerCPU() (float64, bool) {
	var fields []string
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields = strings.Fields(string(raw))
	} else if out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
		fields = strings.FieldsFunc(string(out), func(r rune) bool {
			return r == ' ' || r == '{' || r == '}' || r == '\n' || r == '\t'
		})
	}
	if len(fields) == 0 {
		return 0, false
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return load / float64(runtime.NumCPU()), true
}
