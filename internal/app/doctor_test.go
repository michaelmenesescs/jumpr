package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/skeema/knownhosts"
	gossh "golang.org/x/crypto/ssh"

	"jumpr/internal/transport/ssh/sshtest"
)

// doctorHostAlias is the lab-style alias every doctor test resolves.
const doctorHostAlias = "web-01.lab.example.internal"

// doctorBastionAlias is the first hop when a test dials through one.
const doctorBastionAlias = "bastion.lab.example.internal"

// doctorHandler is the remote side of a doctor test.
type doctorHandler func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int

// doctorFixture is a temp config directory, a temp home and the SSH servers a
// doctor run talks to.
type doctorFixture struct {
	dir    string // --config-dir
	home   string // home for ~ expansion
	sshCfg string // the generated ssh_config
	alias  string
}

// newDoctorFixture starts the loopback servers, writes ssh_config and
// config.yaml, and returns the directories Doctor needs.
func newDoctorFixture(t *testing.T, bastion bool, handler doctorHandler) *doctorFixture {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	keyFile := filepath.Join(base, "id_ed25519")
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	var sshCfg strings.Builder
	var known strings.Builder
	knownFile := filepath.Join(base, "known_hosts")

	bastionPort := 0
	if bastion {
		bc := sshtest.Start(t, sshtest.Options{AllowForward: true,
			AuthorizedKeys: []gossh.PublicKey{signer.PublicKey()}})
		bastionPort = bc.Port()
		fmt.Fprintf(&sshCfg, "Host %s\n  HostName 127.0.0.1\n  Port %d\n  User tester\n"+
			"  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n"+
			"  StrictHostKeyChecking yes\n",
			doctorBastionAlias, bastionPort, keyFile, knownFile)
		known.WriteString(knownhosts.Line([]string{knownAddr("127.0.0.1", bastionPort)}, bc.HostKey()))
		known.WriteString("\n")
	}

	srv := sshtest.Start(t, sshtest.Options{
		AuthorizedKeys: []gossh.PublicKey{signer.PublicKey()},
		Handler:        handler,
	})
	port := srv.Port()
	known.WriteString(knownhosts.Line([]string{knownAddr("127.0.0.1", port)}, srv.HostKey()))
	known.WriteString("\n")

	if err := os.WriteFile(knownFile, []byte(known.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	fmt.Fprintf(&sshCfg, "Host %s\n  HostName 127.0.0.1\n  Port %d\n  User tester\n"+
		"  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n"+
		"  StrictHostKeyChecking yes\n  ConnectTimeout 5\n",
		doctorHostAlias, port, keyFile, knownFile)
	if bastion {
		fmt.Fprintf(&sshCfg, "  ProxyJump %s\n", doctorBastionAlias)
	}

	cfgFile := filepath.Join(base, "ssh_config")
	if err := os.WriteFile(cfgFile, []byte(sshCfg.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(base, "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf("ssh:\n  config_files: [%q]\n  known_hosts: [%q]\n", cfgFile, knownFile)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	return &doctorFixture{dir: dir, home: filepath.Join(base, "home"), sshCfg: cfgFile, alias: doctorHostAlias}
}

// knownAddr is the known_hosts form of host:port.
func knownAddr(host string, port int) string {
	return knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))
}

// runDoctor runs Doctor against the fixture and returns the exit code and the
// whole report.
func (f *doctorFixture) runDoctor(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out strings.Builder
	code := Doctor(context.Background(), Options{
		ConfigDir: f.dir,
		Home:      f.home,
		Env:       map[string]string{"USER": "tester", "SSH_AUTH_SOCK": ""},
	}, args, &out)
	return code, out.String()
}

// lineWith returns the report line whose label column is label, or "".
func lineWith(report, label string) string {
	for _, ln := range strings.Split(report, "\n") {
		if strings.Contains(ln, "  "+label+"  ") || strings.HasSuffix(ln, "  "+label) {
			return ln
		}
	}
	return ""
}

// doctorOKTable is the sshd side of the happy path.
func doctorOKTable() doctorHandler {
	return sshtest.CommandTable(map[string]string{
		"echo ok":           "ok\n",
		"uname -sr":         "Linux 6.8.0-lab-generic\n",
		". /etc/os-release": "Rocky Linux 9.4 (Blue Onyx)\n",
		"sudo -n true":      "",
	})
}

// TestDoctorSSHAllChecksPass drives the whole report through a bastion.
func TestDoctorSSHAllChecksPass(t *testing.T) {
	f := newDoctorFixture(t, true, doctorOKTable())
	code, out := f.runDoctor(t, "ssh", f.alias)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, f.alias+" via "+doctorBastionAlias) {
		t.Errorf("resolved line does not name the chain:\n%s", out)
	}
	for _, label := range []string{"resolved", "dial + auth", "host key", "echo ok",
		"sudo -n true", "uname -sr", "os-release"} {
		ln := lineWith(out, label)
		if ln == "" {
			t.Fatalf("no %q line in:\n%s", label, out)
		}
		if !strings.HasPrefix(ln, "ok") {
			t.Errorf("%s line is not ok: %q", label, ln)
		}
	}
	for _, want := range []string{"accepted (yes)", "Linux 6.8.0-lab-generic", "Rocky Linux 9.4 (Blue Onyx)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not contain %q:\n%s", want, out)
		}
	}
}

// TestDoctorSSHNeedsPrivilege reports a password-protected sudo without failing.
func TestDoctorSSHNeedsPrivilege(t *testing.T) {
	h := func(cmd string, _ io.Reader, stdout, stderr io.Writer) int {
		switch {
		case strings.Contains(cmd, "sudo -n true"):
			_, _ = io.WriteString(stderr, "sudo: a password is required\n")
			return 1
		case strings.Contains(cmd, "echo ok"):
			_, _ = io.WriteString(stdout, "ok\n")
			return 0
		case strings.Contains(cmd, "uname -sr"):
			_, _ = io.WriteString(stdout, "Linux 6.8.0-lab-generic\n")
			return 0
		case strings.Contains(cmd, ". /etc/os-release"):
			_, _ = io.WriteString(stdout, "Ubuntu 24.04.1 LTS\n")
			return 0
		}
		_, _ = io.WriteString(stderr, "not found\n")
		return 127
	}
	f := newDoctorFixture(t, false, h)
	code, out := f.runDoctor(t, "ssh", f.alias)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (sudo does not decide it)\n%s", code, out)
	}
	ln := lineWith(out, "sudo -n true")
	if !strings.HasPrefix(ln, "needs-privilege") {
		t.Errorf("sudo line = %q, want needs-privilege", ln)
	}
	if strings.Contains(out, "resolved "+f.alias+" via") {
		t.Errorf("a direct host must not be reported via a bastion:\n%s", out)
	}
}

// TestDoctorSSHDialFails exits 1 and marks the checks it could not run.
func TestDoctorSSHDialFails(t *testing.T) {
	f := newDoctorFixture(t, false, doctorOKTable())
	// Point the alias at a port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(f.dir, "config.yaml")
	if _, err := os.Stat(cfg); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(f.sshCfg)
	if err != nil {
		t.Fatal(err)
	}
	replaced := replacePort(string(text), closedPort)
	if err := os.WriteFile(f.sshCfg, []byte(replaced), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := f.runDoctor(t, "ssh", f.alias)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	if ln := lineWith(out, "dial + auth"); !strings.HasPrefix(ln, "FAIL") {
		t.Errorf("dial line = %q, want FAIL", ln)
	}
	if ln := lineWith(out, "echo ok"); !strings.HasPrefix(ln, "FAIL") {
		t.Errorf("echo line = %q, want FAIL", ln)
	}
}

// replacePort rewrites the target host's Port line to port.
func replacePort(text string, port int) string {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "Port ") {
			lines[i] = "  Port " + strconv.Itoa(port)
		}
	}
	return strings.Join(lines, "\n")
}

// TestDoctorSSHEchoFails exits 1 when the round trip does not come back right.
func TestDoctorSSHEchoFails(t *testing.T) {
	h := func(cmd string, _ io.Reader, stdout, stderr io.Writer) int {
		if strings.Contains(cmd, "echo ok") {
			_, _ = io.WriteString(stdout, "nope\n")
			return 0
		}
		_, _ = io.WriteString(stdout, "Linux 6.8.0-lab-generic\n")
		return 0
	}
	f := newDoctorFixture(t, false, h)
	code, out := f.runDoctor(t, "ssh", f.alias)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	if ln := lineWith(out, "echo ok"); !strings.HasPrefix(ln, "FAIL") {
		t.Errorf("echo line = %q, want FAIL", ln)
	}
}

// TestDoctorUsage rejects anything but `doctor ssh <host>` with exit code 2.
// The bare `jumpr doctor` is the full report now, so it is not a usage case.
func TestDoctorUsage(t *testing.T) {
	cases := map[string][]string{
		"no host":   {"ssh"},
		"two hosts": {"ssh", "a", "b"},
		"other":     {"conn", "https://prom.example.internal"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			if code := Doctor(context.Background(), Options{}, args, &out); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(out.String(), "usage: jumpr doctor") {
				t.Errorf("no usage line: %q", out.String())
			}
		})
	}
}
