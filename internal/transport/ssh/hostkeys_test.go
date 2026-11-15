package ssh

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"

	"jumpr/internal/errs"
	"jumpr/internal/interact"
)

func hostAddr(host string, port int) string {
	return knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))
}

// writeKnownHosts writes a known_hosts file holding one line per key.
func writeKnownHosts(t *testing.T, host string, port int, keys ...ssh.PublicKey) string {
	t.Helper()
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(knownhosts.Line([]string{hostAddr(host, port)}, k))
		b.WriteString("\n")
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func emptyKnownHosts(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func remoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
}

func testSpec(files ...string) HostSpec {
	return HostSpec{Alias: "example", HostName: "example.internal", User: "tester", Port: 22, KnownHostsFiles: files}
}

func TestHostKeyKnown(t *testing.T) {
	_, pub := newEd25519(t)
	file := writeKnownHosts(t, "example.internal", 22, pub)

	cb, algos, err := HostKeyConfig(testSpec(file), HostKeyOptions{
		Files:    []string{file},
		Strict:   "ask",
		Prompter: interact.None(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(algos) == 0 {
		t.Fatal("no algorithms for a known host")
	}
	if err := cb("ignored:22", remoteAddr(), pub); err != nil {
		t.Fatalf("known host key rejected: %v", err)
	}
}

func TestHostKeyChanged(t *testing.T) {
	_, oldKey := newEd25519(t)
	_, newKey := newEd25519(t)
	file := writeKnownHosts(t, "example.internal", 22, oldKey)
	pr := newCountingPrompter(interact.Answer{Choice: "always"})

	cb, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{
		Files:    []string{file},
		Strict:   "ask",
		Prompter: pr,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := cb("example.internal:22", remoteAddr(), newKey)
	if errs.KindOf(got) != errs.HostKey {
		t.Fatalf("kind = %s, want hostkey (%v)", errs.KindOf(got), got)
	}
	msg := got.Error()
	if !strings.Contains(msg, "changed") || !strings.Contains(msg, "line 1") {
		t.Fatalf("message = %q, want a changed-key message naming the known_hosts line", msg)
	}
	if pr.count() != 0 {
		t.Fatal("a changed host key must never be offered to the user")
	}
}

func TestHostKeyUnknownStrictYes(t *testing.T) {
	_, pub := newEd25519(t)
	file := emptyKnownHosts(t)
	pr := newCountingPrompter(interact.Answer{Choice: "always"})

	spec := testSpec(file)
	spec.StrictHostKeyChecking = "yes"
	cb, _, err := HostKeyConfig(spec, HostKeyOptions{Files: []string{file}, Prompter: pr})
	if err != nil {
		t.Fatal(err)
	}
	if got := cb("example.internal:22", remoteAddr(), pub); errs.KindOf(got) != errs.HostKey {
		t.Fatalf("kind = %s, want hostkey (%v)", errs.KindOf(got), got)
	}
	if pr.count() != 0 {
		t.Fatal("StrictHostKeyChecking yes must not prompt")
	}
}

func TestHostKeyUnknownPromptAlways(t *testing.T) {
	_, pub := newEd25519(t)
	file := emptyKnownHosts(t)
	pr := newCountingPrompter(interact.Answer{Choice: "always"})

	cb, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{
		Files:    []string{file},
		Strict:   "ask",
		Prompter: pr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("example.internal:22", remoteAddr(), pub); err != nil {
		t.Fatalf("accepting an unknown key failed: %v", err)
	}
	if pr.count() != 1 {
		t.Fatalf("prompts = %d, want 1", pr.count())
	}
	p := pr.last()
	if p.Kind != interact.HostKeyUnknown {
		t.Fatalf("prompt kind = %v, want HostKeyUnknown", p.Kind)
	}
	if !strings.Contains(p.Body, "example.internal") ||
		!strings.Contains(p.Body, pub.Type()) ||
		!strings.Contains(p.Body, ssh.FingerprintSHA256(pub)) {
		t.Fatalf("prompt body = %q, want host, key type and SHA256 fingerprint", p.Body)
	}
	if len(p.Choices) != 3 {
		t.Fatalf("prompt choices = %v, want once/always/reject", p.Choices)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), pub.Type()) || !strings.Contains(string(data), "example.internal") {
		t.Fatalf("known_hosts after accept = %q, want the new line", data)
	}

	// A second DB load accepts the appended line without asking.
	cb2, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{
		Files:    []string{file},
		Strict:   "ask",
		Prompter: interact.None(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb2("example.internal:22", remoteAddr(), pub); err != nil {
		t.Fatalf("reloaded known_hosts rejected the appended key: %v", err)
	}
}

func TestHostKeyUnknownNoPrompter(t *testing.T) {
	_, pub := newEd25519(t)
	file := emptyKnownHosts(t)

	cb, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{Files: []string{file}, Strict: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cb("example.internal:22", remoteAddr(), pub); errs.KindOf(got) != errs.HostKey {
		t.Fatalf("kind = %s, want hostkey (%v)", errs.KindOf(got), got)
	}
}

func TestHostKeyUnknownPrompterNone(t *testing.T) {
	_, pub := newEd25519(t)
	file := emptyKnownHosts(t)

	cb, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{
		Files:    []string{file},
		Strict:   "ask",
		Prompter: interact.None(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cb("example.internal:22", remoteAddr(), pub); errs.KindOf(got) != errs.HostKey {
		t.Fatalf("kind = %s, want hostkey (%v)", errs.KindOf(got), got)
	}
}

func TestHostKeyUnknownOnce(t *testing.T) {
	_, pub := newEd25519(t)
	file := emptyKnownHosts(t)
	pr := newCountingPrompter(interact.Answer{Choice: "once"})

	cb, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{
		Files:    []string{file},
		Strict:   "ask",
		Prompter: pr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("example.internal:22", remoteAddr(), pub); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := cb("example.internal:22", remoteAddr(), pub); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if pr.count() != 1 {
		t.Fatalf("prompts = %d, want 1 (once is remembered in memory)", pr.count())
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(data))) != 0 {
		t.Fatalf("known_hosts = %q, want nothing appended for once", data)
	}
}

func TestHostKeyRejectAndCancel(t *testing.T) {
	_, pub := newEd25519(t)
	cases := map[string]interact.Answer{
		"reject":  {Choice: "reject"},
		"cancel":  {Canceled: true},
		"unknown": {Choice: "maybe"},
	}
	for name, ans := range cases {
		t.Run(name, func(t *testing.T) {
			file := emptyKnownHosts(t)
			cb, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{
				Files:    []string{file},
				Strict:   "ask",
				Prompter: interact.Static(ans),
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := cb("example.internal:22", remoteAddr(), pub); errs.KindOf(got) != errs.HostKey {
				t.Fatalf("kind = %s, want hostkey (%v)", errs.KindOf(got), got)
			}
		})
	}
}

func TestHostKeyAlgosEd25519Only(t *testing.T) {
	_, pub := newEd25519(t)
	file := writeKnownHosts(t, "example.internal", 22, pub)

	_, algos, err := HostKeyConfig(testSpec(file), HostKeyOptions{Files: []string{file}})
	if err != nil {
		t.Fatal(err)
	}
	if len(algos) != 1 || algos[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algos = %v, want only %s", algos, ssh.KeyAlgoED25519)
	}
	for _, a := range algos {
		if strings.Contains(a, "rsa") {
			t.Fatalf("algos = %v, must not offer RSA for an ed25519-only entry", algos)
		}
	}
}

func TestHostKeyMissingFileIsEmpty(t *testing.T) {
	_, pub := newEd25519(t)
	missing := filepath.Join(t.TempDir(), "nope")

	cb, algos, err := HostKeyConfig(testSpec(missing), HostKeyOptions{
		Files:  []string{missing},
		Strict: "yes",
	})
	if err != nil {
		t.Fatalf("missing known_hosts must be treated as empty: %v", err)
	}
	if len(algos) != 0 {
		t.Fatalf("algos = %v, want none", algos)
	}
	if got := cb("example.internal:22", remoteAddr(), pub); errs.KindOf(got) != errs.HostKey {
		t.Fatalf("kind = %s, want hostkey (%v)", errs.KindOf(got), got)
	}
}

func TestHostKeyAcceptNew(t *testing.T) {
	_, pub := newEd25519(t)
	file := emptyKnownHosts(t)

	cb, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{
		Files:    []string{file},
		Strict:   "accept-new",
		Prompter: interact.None(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("example.internal:22", remoteAddr(), pub); err != nil {
		t.Fatalf("accept-new rejected an unknown key: %v", err)
	}

	cb2, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{Files: []string{file}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb2("example.internal:22", remoteAddr(), pub); err != nil {
		t.Fatalf("appended line not accepted on reload: %v", err)
	}
}

func TestHostKeyAppendHook(t *testing.T) {
	_, pub := newEd25519(t)
	file := emptyKnownHosts(t)

	var (
		gotFile string
		gotLine string
	)
	cb, _, err := HostKeyConfig(testSpec(file), HostKeyOptions{
		Files:    []string{file},
		Strict:   "ask",
		Prompter: interact.Static(interact.Answer{Choice: "always"}),
		Append: func(f, line string) error {
			gotFile, gotLine = f, line
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("example.internal:22", remoteAddr(), pub); err != nil {
		t.Fatal(err)
	}
	if gotFile != file {
		t.Fatalf("Append file = %q, want %q", gotFile, file)
	}
	if !strings.Contains(gotLine, "example.internal") || !strings.Contains(gotLine, "ssh-ed25519") {
		t.Fatalf("Append line = %q, want a known_hosts line", gotLine)
	}
}

func TestHostKeyPortInAddress(t *testing.T) {
	_, pub := newEd25519(t)
	// The entry is stored with a non-standard port, so the lookup must use the
	// bracketed host:port form.
	file := writeKnownHosts(t, "example.internal", 2222, pub)

	spec := testSpec(file)
	spec.Port = 2222
	cb, algos, err := HostKeyConfig(spec, HostKeyOptions{Files: []string{file}, Prompter: interact.None()})
	if err != nil {
		t.Fatal(err)
	}
	if len(algos) != 1 || algos[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algos = %v, want the ed25519 entry of port 2222", algos)
	}
	if err := cb("example.internal:2222", remoteAddr(), pub); err != nil {
		t.Fatalf("key for port 2222 rejected: %v", err)
	}
}
