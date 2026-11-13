package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

// doctorAllFixture is a temp home and config directory for the full report: a
// static inventory with a bastion and a host, an ssh_config that jumps through
// the bastion, one connection profile and two HTTP backends.
type doctorAllFixture struct {
	home      string
	dir       string
	sshCfg    string
	hostsFile string
	socketDir string
}

// newDoctorAllFixture writes everything `jumper doctor` reads, none of which
// touches the network. The session socket lives in a short, private directory
// under /tmp because macOS caps a unix socket path at 103 bytes.
func newDoctorAllFixture(t *testing.T) *doctorAllFixture {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "jumper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".cache", "jumper"), 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "jumper-doctor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	if err := os.Chmod(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}

	hostsFile := filepath.Join(work, "hosts.yaml")
	hosts := "hosts:\n" +
		"  - name: bastion1.example.internal\n" +
		"    addr: bastion1.example.internal\n" +
		"    labels: {role: bastion}\n" +
		"  - name: web-01.example.internal\n" +
		"    addr: web-01.example.internal\n" +
		"    labels: {role: web}\n"
	if err := os.WriteFile(hostsFile, []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}

	sshCfg := filepath.Join(work, "ssh_config")
	sshConfig := "Host bastion1.example.internal\n" +
		"  HostName 127.0.0.1\n" +
		"  User tester\n" +
		"Host web-01.example.internal\n" +
		"  HostName 127.0.0.1\n" +
		"  User tester\n" +
		"  ProxyJump bastion1.example.internal\n"
	if err := os.WriteFile(sshCfg, []byte(sshConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	connections := "connections:\n" +
		"  corp:\n" +
		"    proxy:\n" +
		"      type: none\n"
	if err := os.WriteFile(filepath.Join(dir, "connections.yaml"), []byte(connections), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &doctorAllFixture{home: home, dir: dir, sshCfg: sshCfg, hostsFile: hostsFile, socketDir: socketDir}
	f.writeConfig(t, "")
	return f
}

// writeConfig writes config.yaml with the fixture hosts and, optionally, extra
// backend entries appended verbatim.
func (f *doctorAllFixture) writeConfig(t *testing.T, extra string) {
	t.Helper()
	cfg := fmt.Sprintf("ui:\n  color: auto\n  glyphs: auto\n"+
		"ssh:\n  config_files: [%q]\n"+
		"inventory:\n  sources:\n    - type: static\n      path: %q\n"+
		"knowledge:\n  root: %q\n"+
		"prometheus:\n  - name: prom\n    url: https://prom.example.internal\n    connection: corp\n"+
		"grafana:\n  - name: grafana\n    url: https://grafana.example.internal\n    connection: corp\n%s",
		f.sshCfg, f.hostsFile, filepath.Join(f.home, "jumper"), extra)
	if err := os.WriteFile(filepath.Join(f.dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// doctorAllCalls records what the report asked for.
type doctorAllCalls struct {
	connects []string
	execs    []string
	reaches  []string
}

// runDoctorAll runs the full report with hooks that never touch the network.
func (f *doctorAllFixture) runDoctorAll(t *testing.T, env map[string]string, hooks doctorHooks) (int, string) {
	t.Helper()
	if env == nil {
		env = map[string]string{}
	}
	env["JUMPER_SOCKET"] = filepath.Join(f.socketDir, "jumper.sock")
	var out strings.Builder
	code := doctorAllWith(context.Background(), Options{
		Home:      f.home,
		ConfigDir: f.dir,
		Env:       env,
	}, &out, hooks)
	return code, out.String()
}

// doctorAllEnv is a terminal the report is happy with.
func doctorAllEnv() map[string]string {
	return map[string]string{
		"TERM":          "xterm-256color",
		"COLORTERM":     "truecolor",
		"SSH_AUTH_SOCK": "/tmp/doctor-agent.sock",
	}
}

// passingHooks answers every remote check successfully and records it.
func passingHooks(t *testing.T, calls *doctorAllCalls) doctorHooks {
	t.Helper()
	return doctorHooks{
		AgentKeys: func(socket string) (int, error) {
			if socket != "/tmp/doctor-agent.sock" {
				t.Errorf("agent socket = %q", socket)
			}
			return 2, nil
		},
		Connect: func(_ context.Context, host resource.HostID) error {
			calls.connects = append(calls.connects, string(host))
			return nil
		},
		Exec: func(_ context.Context, host resource.HostID, script string) (transport.ExecResult, error) {
			calls.execs = append(calls.execs, string(host)+": "+script)
			switch script {
			case "echo ok":
				return transport.ExecResult{Stdout: []byte("ok\n"), RC: 0, Duration: 3 * time.Millisecond}, nil
			case "sudo -n true":
				return transport.ExecResult{RC: 0}, nil
			}
			return transport.ExecResult{RC: 127}, nil
		},
		Health: func(context.Context) []doctorHealth {
			return []doctorHealth{
				{Backend: "linux", Health: backend.Health{State: backend.HealthOK, Detail: "0/2 hosts failed"}},
				{Backend: "salt", Health: backend.Health{State: backend.HealthUnconfigured, Detail: "salt.master is empty"}},
			}
		},
		Reach: func(_ context.Context, profile, url string) error {
			calls.reaches = append(calls.reaches, profile+" "+url)
			return nil
		},
	}
}

// checkEveryLineStatus proves each report line starts with a known status.
func checkEveryLineStatus(t *testing.T, report string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(report, "\n"), "\n") {
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "ok "),
			strings.HasPrefix(line, "warn "),
			strings.HasPrefix(line, "FAIL "),
			strings.HasPrefix(line, "needs-privilege "):
		default:
			t.Errorf("line has no status: %q", line)
		}
	}
}

// lineContaining returns the first report line containing substr.
func lineContaining(report, substr string) string {
	for _, line := range strings.Split(report, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

// statusOf returns the status word of the first line labelled label.
func statusOf(report, label string) string {
	fields := strings.Fields(lineWith(report, label))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// requireStatus fails unless the first line labelled label has status.
func requireStatus(t *testing.T, report, label, status string) {
	t.Helper()
	if got := statusOf(report, label); got != status {
		t.Errorf("status of %q = %q, want %q\n%s", label, got, status, report)
	}
}

// TestDoctorAllPasses runs the whole report offline and expects exit 0, with a
// line for every check SPEC §14 asks for.
func TestDoctorAllPasses(t *testing.T) {
	f := newDoctorAllFixture(t)
	var calls doctorAllCalls
	code, out := f.runDoctorAll(t, doctorAllEnv(), passingHooks(t, &calls))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	checkEveryLineStatus(t, out)
	for _, label := range []string{
		"config", "ssh config", "agent", "bastion", "sample", "backend",
		"connection", "knowledge", "socket dir", "term", "colour", "tmux",
	} {
		requireStatus(t, out, label, "ok")
	}

	for _, want := range []string{
		"2 keys",
		"bastion1.example.internal: connected",
		"echo ok",
		"sudo -n true ok",
		"prometheus/prom via corp",
		"grafana/grafana via corp",
		"writable",
		"(0700)",
		"24-bit",
		"not inside tmux",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not contain %q:\n%s", want, out)
		}
	}
	if len(calls.connects) != 1 || calls.connects[0] != "bastion1.example.internal" {
		t.Errorf("bastions connected = %v, want [bastion1.example.internal]", calls.connects)
	}
	if len(calls.execs) < 2 {
		t.Errorf("execs = %v, want echo and sudo", calls.execs)
	}
	if len(calls.reaches) != 2 {
		t.Errorf("reachability probes = %v, want two", calls.reaches)
	}
}

// TestDoctorAllFailuresProveExitOne turns every remote check red, adds a
// backend that names a missing profile, and expects exit 1 with FAIL lines.
func TestDoctorAllFailuresProveExitOne(t *testing.T) {
	f := newDoctorAllFixture(t)
	f.writeConfig(t, "  - name: broken\n    url: https://broken.example.internal\n    connection: nope\n")

	hooks := doctorHooks{
		AgentKeys: func(string) (int, error) {
			return 0, errs.New(errs.Unreachable, "doctor.agent", "no such file")
		},
		Connect: func(context.Context, resource.HostID) error {
			return errs.New(errs.Unreachable, "doctor.connect", "i/o timeout")
		},
		Exec: func(_ context.Context, _ resource.HostID, script string) (transport.ExecResult, error) {
			switch script {
			case "echo ok":
				return transport.ExecResult{Stdout: []byte("nope\n"), RC: 0}, nil
			case "sudo -n true":
				return transport.ExecResult{RC: 1, Stderr: []byte("sudo: a password is required\n")}, nil
			}
			return transport.ExecResult{RC: 127}, nil
		},
		Health: func(context.Context) []doctorHealth {
			return []doctorHealth{{Backend: "linux", Health: backend.Health{
				State: backend.HealthDown, Detail: "all hosts failed"}}}
		},
		Reach: func(_ context.Context, _ string, url string) error {
			if strings.Contains(url, "grafana") {
				return errs.New(errs.Timeout, "doctor.connection", "context deadline exceeded")
			}
			return nil
		},
	}
	code, out := f.runDoctorAll(t, doctorAllEnv(), hooks)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	checkEveryLineStatus(t, out)
	for _, label := range []string{"agent", "bastion", "sample", "backend"} {
		requireStatus(t, out, label, "FAIL")
	}
	if ln := lineContaining(out, "grafana/grafana via corp"); !strings.HasPrefix(ln, "FAIL") {
		t.Errorf("reachability line = %q, want FAIL", ln)
	}
	if ln := lineContaining(out, "sudo -n true"); !strings.HasPrefix(ln, "needs-privilege") {
		t.Errorf("sudo line = %q, want needs-privilege", ln)
	}
	if !strings.Contains(out, `no profile "nope"`) {
		t.Errorf("report does not name the missing profile:\n%s", out)
	}
}

// TestDoctorAllWarnsWithoutAgentOrTerminal proves a laptop with no agent and
// no terminal is warned about, not failed.
func TestDoctorAllWarnsWithoutAgentOrTerminal(t *testing.T) {
	f := newDoctorAllFixture(t)
	var calls doctorAllCalls
	// An empty SSH_AUTH_SOCK and TERM are what a detached run looks like.
	code, out := f.runDoctorAll(t, map[string]string{"SSH_AUTH_SOCK": "", "TERM": ""}, passingHooks(t, &calls))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	for _, label := range []string{"agent", "term", "colour"} {
		requireStatus(t, out, label, "warn")
	}
}

// TestDoctorAllInvalidConfigStops proves a broken config.yaml fails the report
// and still checks what does not need it.
func TestDoctorAllInvalidConfigStops(t *testing.T) {
	f := newDoctorAllFixture(t)
	if err := os.WriteFile(filepath.Join(f.dir, "config.yaml"), []byte("ui:\n  colour: auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls doctorAllCalls
	code, out := f.runDoctorAll(t, doctorAllEnv(), passingHooks(t, &calls))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	requireStatus(t, out, "config", "FAIL")
	requireStatus(t, out, "term", "ok")
	if len(calls.connects) != 0 {
		t.Errorf("dialled %v with an invalid config", calls.connects)
	}
}
