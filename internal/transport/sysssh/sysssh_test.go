package sysssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

// TestMain makes the test binary double as the fake ssh: when JUMPR_FAKE_SSH
// is set — which only the Transport under test does, through the inherited
// environment — the process behaves like /usr/bin/ssh and exits before any test
// runs. That is the "re-execute itself as the fake via a path option" trick:
// Options.SSHPath is os.Args[0], and no shell script is involved.
func TestMain(m *testing.M) {
	if os.Getenv("JUMPR_FAKE_SSH") == "1" {
		os.Exit(fakeSSH())
	}
	os.Exit(m.Run())
}

// fakeSSH is the fake ssh client. It records its argv, then writes the
// configured streams and exits with the configured code.
func fakeSSH() int {
	if path := os.Getenv("JUMPR_FAKE_ARGV"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = f.WriteString(strings.Join(os.Args[1:], "\x00") + "\n")
			_ = f.Close()
		}
	}
	if ms, err := strconv.Atoi(os.Getenv("JUMPR_FAKE_SLEEP_MS")); err == nil && ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	if s := os.Getenv("JUMPR_FAKE_STDOUT"); s != "" {
		fmt.Fprint(os.Stdout, strings.ReplaceAll(s, `\n`, "\n"))
	}
	if s := os.Getenv("JUMPR_FAKE_STDERR"); s != "" {
		fmt.Fprint(os.Stderr, strings.ReplaceAll(s, `\n`, "\n"))
	}
	if os.Getenv("JUMPR_FAKE_ECHO_STDIN") == "1" {
		_, _ = io.Copy(os.Stdout, os.Stdin)
	}
	rc, _ := strconv.Atoi(os.Getenv("JUMPR_FAKE_EXIT"))
	return rc
}

// fakeEnv configures the next fake ssh invocation.
type fakeEnv struct {
	stdout  string
	stderr  string
	exit    string
	sleepMS string
}

// newFake returns a Transport whose ssh is this test binary acting as the fake,
// and the file its argv is recorded to.
func newFake(t *testing.T, env fakeEnv, o Options) (*Transport, string) {
	t.Helper()
	argvFile := t.TempDir() + "/argv"
	t.Setenv("JUMPR_FAKE_SSH", "1")
	t.Setenv("JUMPR_FAKE_STDOUT", env.stdout)
	t.Setenv("JUMPR_FAKE_STDERR", env.stderr)
	t.Setenv("JUMPR_FAKE_EXIT", env.exit)
	t.Setenv("JUMPR_FAKE_SLEEP_MS", env.sleepMS)
	t.Setenv("JUMPR_FAKE_ARGV", argvFile)
	o.SSHPath = os.Args[0]
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 10 * time.Second
	}
	if o.CacheDir == "" {
		o.CacheDir = t.TempDir()
	}
	return New(o), argvFile
}

// argv returns the last recorded invocation, split into its arguments.
func argv(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	return strings.Split(lines[len(lines)-1], "\x00")
}

func TestSysSSHExec(t *testing.T) {
	tr, argvFile := newFake(t, fakeEnv{stdout: `hello\n`, exit: "0"}, Options{ConfigFile: "/etc/ssh_test_config"})
	host := resource.HostID("web.example.internal")

	res, err := tr.Exec(context.Background(), host, transport.Command{Script: "echo hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got, want := string(res.Stdout), "hello\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if res.RC != 0 {
		t.Fatalf("rc = %d, want 0", res.RC)
	}
	if res.Host != host {
		t.Fatalf("host = %q, want %q", res.Host, host)
	}
	if res.Via != "system-ssh" {
		t.Fatalf("via = %q, want system-ssh", res.Via)
	}

	args := argv(t, argvFile)
	want := []string{
		"-F", "/etc/ssh_test_config",
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + tr.controlPath(),
		"-o", "ControlPersist=5m",
		"-o", "ConnectTimeout=10",
		"--", string(host), shq.Wrap("echo hello", false),
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv =\n%q\nwant\n%q", args, want)
	}
	if !strings.HasSuffix(tr.controlPath(), "/cm/%C") {
		t.Fatalf("control path = %q, want <cache>/cm/%%C", tr.controlPath())
	}
	info, err := os.Stat(tr.controlDir())
	if err != nil {
		t.Fatalf("control dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("control dir mode = %o, want 700", perm)
	}
}

func TestSysSSHSudoWrapping(t *testing.T) {
	tr, argvFile := newFake(t, fakeEnv{exit: "0"}, Options{})
	if _, err := tr.Exec(context.Background(), "db1", transport.Command{Script: "id", Sudo: true}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	args := argv(t, argvFile)
	if got, want := args[len(args)-1], shq.Wrap("id", true); got != want {
		t.Fatalf("script arg = %q, want %q", got, want)
	}
}

func TestSysSSHRemoteFailureIsNotAnError(t *testing.T) {
	tr, _ := newFake(t, fakeEnv{stderr: "boom\n", exit: "3"}, Options{})
	res, err := tr.Exec(context.Background(), "h", transport.Command{Script: "false"})
	if err != nil {
		t.Fatalf("a remote rc must not be an error: %v", err)
	}
	if res.RC != 3 {
		t.Fatalf("rc = %d, want 3", res.RC)
	}
	if string(res.Stderr) != "boom\n" {
		t.Fatalf("stderr = %q", res.Stderr)
	}
}

func TestSysSSHClassifiesSSHFailures(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   errs.Kind
	}{
		{"auth", "user@h: Permission denied (publickey).\n", errs.Auth},
		{"hostkey", "@@@@@@@@\nHost key verification failed.\n", errs.HostKey},
		{"timeout", "ssh: connect to host h port 22: Connection timed out\n", errs.Timeout},
		{"resolve", "ssh: Could not resolve hostname h: nodename nor servname provided\n", errs.Unreachable},
		{"refused", "ssh: connect to host h port 22: Connection refused\n", errs.Unreachable},
		{"unknown", "ssh: something odd\n", errs.Unreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, _ := newFake(t, fakeEnv{stderr: strings.ReplaceAll(tt.stderr, "\n", `\n`), exit: "255"}, Options{})
			res, err := tr.Exec(context.Background(), "h", transport.Command{Script: "true"})
			if err == nil {
				t.Fatalf("rc 255 was not an error")
			}
			if got := errs.KindOf(err); got != tt.want {
				t.Fatalf("kind = %s, want %s (%v)", got, tt.want, err)
			}
			var e *errs.Error
			if !errors.As(err, &e) || e.Host != "h" {
				t.Fatalf("error does not name the host: %v", err)
			}
			// The command never ran, so there is no remote exit code.
			if res.RC != -1 {
				t.Fatalf("rc = %d, want -1", res.RC)
			}
			if !strings.Contains(err.Error(), strings.Split(strings.TrimSpace(tt.stderr), "\n")[0]) {
				t.Fatalf("message does not carry ssh's own line: %v", err)
			}
		})
	}
}

func TestSysSSHSudoFailure(t *testing.T) {
	tr, _ := newFake(t, fakeEnv{stderr: "sudo: a password is required\n", exit: "1"}, Options{})
	_, err := tr.Exec(context.Background(), "h", transport.Command{Script: "id", Sudo: true})
	if got := errs.KindOf(err); got != errs.Sudo {
		t.Fatalf("kind = %s, want sudo (%v)", got, err)
	}
}

func TestSysSSHTimeout(t *testing.T) {
	tr, _ := newFake(t, fakeEnv{exit: "0", sleepMS: "5000"}, Options{})
	start := time.Now()
	_, err := tr.Exec(context.Background(), "h", transport.Command{Script: "sleep 5", Timeout: 50 * time.Millisecond})
	if got := errs.KindOf(err); got != errs.Timeout {
		t.Fatalf("kind = %s, want timeout (%v)", got, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Exec blocked for %s after its timeout", elapsed)
	}
}

func TestSysSSHCanceled(t *testing.T) {
	tr, _ := newFake(t, fakeEnv{exit: "0", sleepMS: "5000"}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tr.Exec(ctx, "h", transport.Command{Script: "true"})
	if got := errs.KindOf(err); got != errs.Canceled {
		t.Fatalf("kind = %s, want canceled (%v)", got, err)
	}
}

func TestSysSSHTruncatesOutput(t *testing.T) {
	tr, _ := newFake(t, fakeEnv{stdout: `0123456789\n`, exit: "0"}, Options{})
	res, err := tr.Exec(context.Background(), "h", transport.Command{Script: "yes", MaxOutput: 4})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Stdout) != "0123" {
		t.Fatalf("stdout = %q, want the first 4 bytes", res.Stdout)
	}
	if !res.Truncated {
		t.Fatal("Truncated was not set")
	}
}

func TestSysSSHStream(t *testing.T) {
	tr, _ := newFake(t, fakeEnv{stdout: `one\ntwo\n`, stderr: `warning\n`, exit: "0"}, Options{})
	st, err := tr.Start(context.Background(), "h", transport.Command{Script: "journalctl -f"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var lines []string
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case ln, ok := <-st.Lines():
			if !ok {
				break loop
			}
			lines = append(lines, string(ln))
		case <-deadline:
			_ = st.Close()
			t.Fatalf("lines = %v, want one and two", lines)
		}
	}
	if strings.Join(lines, ",") != "one,two" {
		t.Fatalf("lines = %v, want [one two]", lines)
	}
	res, err := st.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.RC != 0 || res.Via != "system-ssh" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(string(res.Stderr), "warning") {
		t.Fatalf("stderr tail = %q", res.Stderr)
	}
	if len(res.Stdout) != 0 {
		t.Fatalf("stream Wait carries stdout: %q", res.Stdout)
	}
}

func TestSysSSHStreamCloseCancels(t *testing.T) {
	tr, _ := newFake(t, fakeEnv{exit: "0", sleepMS: "5000"}, Options{})
	st, err := tr.Start(context.Background(), "h", transport.Command{Script: "sleep 5"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	res, err := st.Wait()
	if got := errs.KindOf(err); got != errs.Canceled {
		t.Fatalf("kind = %s, want canceled (%v)", got, err)
	}
	if res.RC != -1 {
		t.Fatalf("rc = %d, want -1", res.RC)
	}
}

func TestSysSSHShell(t *testing.T) {
	tr, argvFile := newFake(t, fakeEnv{stdout: "ready\n", exit: "0"}, Options{})
	sh, err := tr.OpenShell(context.Background(), "h", transport.PTYRequest{Term: "xterm-256color", Cols: 100, Rows: 40})
	if err != nil {
		t.Fatalf("OpenShell: %v", err)
	}
	defer sh.Close()

	if err := sh.Resize(120, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	var buf []byte
	var mu sync.Mutex
	go func() {
		p := make([]byte, 1024)
		for {
			n, err := sh.Read(p)
			mu.Lock()
			buf = append(buf, p[:n]...)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := string(buf)
		mu.Unlock()
		if strings.Contains(got, "ready") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := string(buf)
	mu.Unlock()
	if !strings.Contains(got, "ready") {
		t.Fatalf("shell output = %q, want ready", got)
	}
	if err := sh.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	args := argv(t, argvFile)
	found := false
	for _, a := range args {
		if a == "-tt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("shell argv has no -tt: %q", args)
	}
}

func TestSysSSHName(t *testing.T) {
	tr := New(Options{})
	if tr.Name() != "system-ssh" {
		t.Fatalf("Name = %q, want system-ssh", tr.Name())
	}
}

func TestSysSSHControlDirFailure(t *testing.T) {
	file := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := New(Options{SSHPath: os.Args[0], CacheDir: file})
	_, err := tr.Exec(context.Background(), "h", transport.Command{Script: "true"})
	if got := errs.KindOf(err); got != errs.Config {
		t.Fatalf("kind = %s, want config (%v)", got, err)
	}
}
