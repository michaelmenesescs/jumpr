package chef

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport/transporttest"
)

// ws is the test workstation every test runs against.
const ws = resource.HostID("chef.lab.example.internal")

// TestRunnerCommandLine checks the command line the runner builds: the knife
// command, the arguments, then the JSON output flags, every word quoted.
func TestRunnerCommandLine(t *testing.T) {
	cases := []struct {
		name  string
		opts  RunnerOptions
		args  []string
		want  string
		fails bool
	}{
		{
			name: "status",
			args: []string{"status"},
			want: "knife status -F json",
		},
		{
			name: "node detail",
			args: []string{"node", "show", "web-01", "-l"},
			want: "knife node show web-01 -l -F json",
		},
		{
			name: "custom knife command",
			opts: RunnerOptions{Knife: "cinc"},
			args: []string{"node", "list"},
			want: "cinc node list -F json",
		},
		{
			name: "solr query stays one quoted argument",
			args: []string{"search", "node", "role:web AND chef_environment:production", "-a", "platform"},
			want: "knife search node 'role:web AND chef_environment:production' -a platform -F json",
		},
		{
			name: "quotes in the query cannot break out",
			args: []string{"search", "node", `name:x'; touch /tmp/pwned; '`},
			want: `knife search node 'name:x'\''; touch /tmp/pwned; '\''' -F json`,
		},
		{
			name:  "empty argument",
			args:  []string{"node", ""},
			fails: true,
		},
	}
	for _, c := range cases {
		line, err := NewRunner(nil, ws, c.opts).CommandLine(c.args...)
		if c.fails {
			if err == nil {
				t.Errorf("%s: want an error, got %q", c.name, line)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if line != c.want {
			t.Errorf("%s: line = %q, want %q", c.name, line, c.want)
		}
	}
}

// TestRunnerErrorKinds checks knife's failure modes map onto jumper's error
// kinds (BUILD-PLAN §7, task 113 requirement 1).
func TestRunnerErrorKinds(t *testing.T) {
	cases := []struct {
		name   string
		rc     int
		stderr string
		want   errs.Kind
	}{
		{"knife missing", 127, "sh: 1: knife: command not found", errs.NotAvailable},
		{"not on PATH", 1, "bash: /usr/local/bin/knife: No such file or directory", errs.NotAvailable},
		{"bad key", 1, "ERROR: Failed to authenticate to https://chef.lab.example.internal/ as sre", errs.Auth},
		{"http 401", 1, "ERROR: Server returned error 401 Unauthorized for https://chef.lab.example.internal/nodes", errs.Auth},
		{"server refused", 1, `ERROR: Errno::ECONNREFUSED: Connection refused - connect(2) for "10.0.0.20" port 8889`, errs.Unreachable},
		{"sudo password", 1, "sudo: a password is required", errs.Sudo},
		{"unknown node", 1, "ERROR: Cannot find a node named 'nope'", errs.NotAvailable},
		{"anything else", 1, "ERROR: something odd happened", errs.Remote},
	}
	for _, c := range cases {
		fake := transporttest.New().On("knife", transporttest.Reply{RC: c.rc, Stderr: c.stderr})
		r := NewRunner(fake, ws, RunnerOptions{})
		_, err := r.Run(context.Background(), "status")
		if err == nil {
			t.Errorf("%s: want an error", c.name)
			continue
		}
		if got := errs.KindOf(err); got != c.want {
			t.Errorf("%s: kind = %q, want %q (%v)", c.name, got, c.want, err)
		}
		var e *errs.Error
		if !errors.As(err, &e) || e.Host != string(ws) {
			t.Errorf("%s: error does not name the workstation: %#v", c.name, err)
		}
	}
}

// TestRunnerDecodeSkipsBanner checks RunJSON tolerates the shell banner a
// workstation prints before knife's JSON.
func TestRunnerDecodeSkipsBanner(t *testing.T) {
	body := "knife: warning: deprecated config\n[\"web-01\",\"web-02\"]\n"
	fake := transporttest.New().On("knife node list", transporttest.Reply{Stdout: body})
	r := NewRunner(fake, ws, RunnerOptions{})
	var names []string
	if err := r.RunJSON(context.Background(), &names, "node", "list"); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if len(names) != 2 || names[1] != "web-02" {
		t.Errorf("names = %v, want [web-01 web-02]", names)
	}
}

// TestRunnerRunsOnTheWorkstation checks every call goes to the configured
// workstation, without sudo by default.
func TestRunnerRunsOnTheWorkstation(t *testing.T) {
	fake := transporttest.New().On("knife", transporttest.Reply{Stdout: "[]"})
	r := NewRunner(fake, ws, RunnerOptions{})
	if _, err := r.Run(context.Background(), "node", "list"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Host != ws {
		t.Errorf("host = %q, want %q", calls[0].Host, ws)
	}
	if calls[0].Command.Sudo {
		t.Error("sudo = true, want false by default")
	}
}

// TestRunnerNoExecutor checks a runner without an executor fails cleanly.
func TestRunnerNoExecutor(t *testing.T) {
	r := NewRunner(nil, ws, RunnerOptions{})
	_, err := r.Run(context.Background(), "status")
	if errs.KindOf(err) != errs.Internal {
		t.Fatalf("kind = %q, want internal (%v)", errs.KindOf(err), err)
	}
}

// TestRunnerVersionIsPlainText checks RunShell gets the raw output of a
// non-JSON knife command.
func TestRunnerVersionIsPlainText(t *testing.T) {
	fake := transporttest.New().On("knife --version", transporttest.Reply{Stdout: "Chef Infra Client: 18.4.12\n"})
	r := NewRunner(fake, ws, RunnerOptions{})
	out, err := r.RunShell(context.Background(), "knife --version")
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	var v any
	if err := json.Unmarshal(out, &v); err == nil {
		t.Error("version output parsed as JSON, want plain text")
	}
	if !strings.Contains(string(out), "18.4.12") {
		t.Errorf("output = %q", out)
	}
}
