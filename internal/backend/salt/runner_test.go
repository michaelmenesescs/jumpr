package salt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/transport/transporttest"
)

const master = "saltmaster.example.internal"

func TestCommandLine(t *testing.T) {
	tests := []struct {
		name string
		call Call
		want string
	}{
		{
			name: "list target with async flag and kwarg",
			call: Call{
				Tool:   "salt",
				Target: &Target{Type: "list", Expr: "web-01,web-02"},
				Fun:    "state.apply",
				Flags:  []string{"--async"},
				Kwargs: map[string]string{"test": "True"},
			},
			want: "salt --out=json --out-indent=-1 --no-color --async -L web-01,web-02 state.apply test=True",
		},
		{
			name: "salt-run takes no target",
			call: Call{
				Tool: "salt-run",
				Fun:  "jobs.list_job",
				Args: []string{"20261112135501123456"},
			},
			want: "salt-run --out=json --out-indent=-1 --no-color jobs.list_job 20261112135501123456",
		},
		{
			name: "compound target is quoted",
			call: Call{
				Tool:   "salt",
				Target: &Target{Type: "compound", Expr: "G@role:web and G@dc:ewr"},
				Fun:    "cmd.run_all",
				Kwargs: map[string]string{"cmd": "uptime"},
			},
			want: "salt --out=json --out-indent=-1 --no-color -C 'G@role:web and G@dc:ewr' cmd.run_all cmd=uptime",
		},
		{
			name: "glob target has no flag",
			call: Call{
				Tool:   "salt",
				Target: &Target{Type: "glob", Expr: "*"},
				Fun:    "test.ping",
			},
			want: "salt --out=json --out-indent=-1 --no-color '*' test.ping",
		},
		{
			name: "grain target",
			call: Call{
				Tool:   "salt",
				Target: &Target{Type: "grain", Expr: "role:web"},
				Fun:    "grains.get",
				Args:   []string{"os"},
			},
			want: "salt --out=json --out-indent=-1 --no-color -G role:web grains.get os",
		},
		{
			name: "pcre target",
			call: Call{
				Tool:   "salt",
				Target: &Target{Type: "pcre", Expr: "web-0[12]"},
				Fun:    "test.ping",
			},
			want: "salt --out=json --out-indent=-1 --no-color -E 'web-0[12]' test.ping",
		},
		{
			name: "nodegroup target",
			call: Call{
				Tool:   "salt",
				Target: &Target{Type: "nodegroup", Expr: "web"},
				Fun:    "test.ping",
			},
			want: "salt --out=json --out-indent=-1 --no-color -N web test.ping",
		},
		{
			name: "kwargs sorted by key",
			call: Call{
				Tool:   "salt",
				Target: &Target{Type: "glob", Expr: "*"},
				Fun:    "state.sls",
				Args:   []string{"common"},
				Kwargs: map[string]string{"b": "2", "a": "1"},
			},
			want: "salt --out=json --out-indent=-1 --no-color '*' state.sls common a=1 b=2",
		},
		{
			name: "salt-key lists keys, arguments may live in flags",
			call: Call{
				Tool:  "salt-key",
				Flags: []string{"-L"},
			},
			want: "salt-key --out=json --out-indent=-1 --no-color -L",
		},
		{
			name: "salt-key accept keeps its flag order",
			call: Call{
				Tool:  "salt-key",
				Flags: []string{"-a", "web-01", "-y"},
			},
			want: "salt-key --out=json --out-indent=-1 --no-color -a web-01 -y",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.call.CommandLine()
			if err != nil {
				t.Fatalf("CommandLine: %v", err)
			}
			if got != tt.want {
				t.Errorf("CommandLine\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestCommandLineRejects(t *testing.T) {
	tests := []struct {
		name string
		call Call
	}{
		{"unknown tool", Call{Tool: "salt-call", Fun: "test.ping"}},
		{"empty tool", Call{Fun: "test.ping"}},
		{"fun with uppercase", Call{Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "Test.Ping"}},
		{"fun with hyphen", Call{Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "state-apply"}},
		{"fun with empty segment", Call{Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "state..apply"}},
		{"fun with trailing dot", Call{Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "state."}},
		{"kwarg key uppercase", Call{Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "test.ping", Kwargs: map[string]string{"Cmd": "x"}}},
		{"kwarg key dashed", Call{Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "test.ping", Kwargs: map[string]string{"max-time": "1"}}},
		{"positional keyword argument", Call{Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "cmd.run", Args: []string{"a=b"}}},
		{"unknown target type", Call{Tool: "salt", Target: &Target{Type: "globx", Expr: "*"}, Fun: "test.ping"}},
		{"empty target expression", Call{Tool: "salt", Target: &Target{Type: "glob"}, Fun: "test.ping"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.call.CommandLine()
			if err == nil {
				t.Fatalf("CommandLine(%+v) = %q, want error", tt.call, got)
			}
			if !errs.Is(err, errs.Config) {
				t.Errorf("kind = %q, want config (%v)", errs.KindOf(err), err)
			}
		})
	}
}

func TestRunSuccess(t *testing.T) {
	f := transporttest.New().On("test.ping", transporttest.Reply{Stdout: "{\"web-01\": true}\n"})
	r := NewRunner(f, master, RunnerOptions{Timeout: 5 * time.Second})

	out, err := r.Run(context.Background(), Call{
		Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "test.ping",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out) != "{\"web-01\": true}\n" {
		t.Errorf("stdout = %q", out)
	}
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Host != master {
		t.Errorf("host = %q, want %q", calls[0].Host, master)
	}
	if calls[0].Command.Sudo {
		t.Error("Sudo = true, want false")
	}
	if calls[0].Command.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", calls[0].Command.Timeout)
	}
	if !strings.HasPrefix(calls[0].Command.Script, "salt --out=json") {
		t.Errorf("script = %q", calls[0].Command.Script)
	}
}

func TestRunSudoAndCallTimeout(t *testing.T) {
	f := transporttest.New().On("test.ping", transporttest.Reply{Stdout: "{}\n"})
	r := NewRunner(f, master, RunnerOptions{Sudo: true, Timeout: 30 * time.Second})

	if _, err := r.Run(context.Background(), Call{
		Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "test.ping",
		Timeout: 3 * time.Second,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := f.Calls()[0]
	if !call.Command.Sudo {
		t.Error("Sudo = false, want true")
	}
	if call.Command.Timeout != 3*time.Second {
		t.Errorf("timeout = %v, want the call's 3s", call.Command.Timeout)
	}
}

func TestRunNonZeroWithStdoutIsData(t *testing.T) {
	f := transporttest.New().On("state.apply", transporttest.Reply{
		Stdout: "{\"web-02\": \"Minion did not return. [No response]\"}\n",
		Stderr: "web-02: Minion did not return\n",
		RC:     2,
	})
	r := NewRunner(f, master, RunnerOptions{})

	out, err := r.Run(context.Background(), Call{
		Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "state.apply",
	})
	if err != nil {
		t.Fatalf("Run returned error for data: %v", err)
	}
	if !strings.Contains(string(out), "Minion did not return") {
		t.Errorf("stdout = %q", out)
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name string
		rep  transporttest.Reply
		want errs.Kind
		msg  string
	}{
		{"sudo needs password", transporttest.Reply{RC: 1, Stderr: "sudo: a password is required\n"}, errs.Sudo, "a password is required"},
		{"sudo not permitted", transporttest.Reply{RC: 1, Stderr: "sudo: a terminal is required to read the password\n"}, errs.Sudo, "a terminal"},
		{"salt missing", transporttest.Reply{RC: 127, Stderr: "sh: salt: command not found\n"}, errs.NotAvailable, "command not found"},
		{"request timed out", transporttest.Reply{RC: 1, Stderr: "Salt request timed out. The master is not responding.\n"}, errs.Unreachable, "timed out"},
		{"master unreachable", transporttest.Reply{RC: 1, Stderr: "The salt master could not be contacted: connection refused\n"}, errs.Unreachable, "could not be contacted"},
		{"plain failure uses first stderr line", transporttest.Reply{RC: 1, Stderr: "Error: target returns empty\nsecond line\n"}, errs.Remote, "target returns empty"},
		{"failure with empty stderr", transporttest.Reply{RC: 1}, errs.Remote, "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := transporttest.New().On("test.ping", tt.rep)
			r := NewRunner(f, master, RunnerOptions{})
			out, err := r.Run(context.Background(), Call{
				Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "test.ping",
			})
			if err == nil {
				t.Fatalf("Run = %q, nil; want error", out)
			}
			if got := errs.KindOf(err); got != tt.want {
				t.Errorf("kind = %q, want %q (%v)", got, tt.want, err)
			}
			if !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.msg)
			}
			var e *errs.Error
			if !errors.As(err, &e) {
				t.Fatalf("error is not *errs.Error: %T", err)
			}
			if e.Host != master {
				t.Errorf("host = %q, want %q", e.Host, master)
			}
		})
	}
}

func TestRunTransportErrorPassesThrough(t *testing.T) {
	boom := errs.New(errs.Timeout, "ssh.exec", "timed out")
	f := transporttest.New().On("test.ping", transporttest.Reply{Err: boom})
	r := NewRunner(f, master, RunnerOptions{})

	_, err := r.Run(context.Background(), Call{
		Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "test.ping",
	})
	if err != boom {
		t.Fatalf("err = %v, want the transport error as-is", err)
	}
}

func TestRunInvalidCallDoesNotExec(t *testing.T) {
	f := transporttest.New()
	r := NewRunner(f, master, RunnerOptions{})

	if _, err := r.Run(context.Background(), Call{Tool: "salt", Target: &Target{Type: "glob", Expr: "*"}, Fun: "Bad"}); err == nil {
		t.Fatal("Run accepted an invalid function name")
	}
	if n := len(f.Calls()); n != 0 {
		t.Errorf("executor called %d times, want 0", n)
	}
}

func TestVersion(t *testing.T) {
	f := transporttest.New().On("--version", transporttest.Reply{Stdout: "salt 3007.1 (Chlorine)\n"})
	r := NewRunner(f, master, RunnerOptions{})

	v, err := r.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "3007.1" {
		t.Errorf("version = %q, want 3007.1", v)
	}
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Command.Script != "salt --version" {
		t.Errorf("script = %q, want %q", calls[0].Command.Script, "salt --version")
	}
}

func TestVersionUnparsable(t *testing.T) {
	f := transporttest.New().On("--version", transporttest.Reply{Stdout: "something odd\n"})
	r := NewRunner(f, master, RunnerOptions{})

	if _, err := r.Version(context.Background()); !errs.Is(err, errs.Parse) {
		t.Fatalf("kind = %q, want parse (%v)", errs.KindOf(err), err)
	}
}
