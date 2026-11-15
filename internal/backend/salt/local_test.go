package salt

import (
	"context"
	"strings"
	"testing"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport/transporttest"
)

const localHost = resource.HostID("web-01.example.internal")

func TestLocalCommands(t *testing.T) {
	if got, want := LocalGrainsCommand(), "sudo -n salt-call --local --out=json grains.items"; got != want {
		t.Fatalf("grains command = %q, want %q", got, want)
	}
	if got, want := LocalStateCommand(""), "sudo -n salt-call --out=json state.apply test=True"; got != want {
		t.Fatalf("state command = %q, want %q", got, want)
	}
	if got, want := LocalStateCommand("nginx"), "sudo -n salt-call --out=json state.apply nginx test=True"; got != want {
		t.Fatalf("state command with sls = %q, want %q", got, want)
	}
}

func TestLocalParseGrains(t *testing.T) {
	out := []byte(`{"local": {"id": "web-01", "os": "Rocky", "num_cpus": 4}}`)
	g, err := ParseLocalGrains(out)
	if err != nil {
		t.Fatalf("ParseLocalGrains: %v", err)
	}
	if g.Str("id") != "web-01" || g.Str("num_cpus") != "4" {
		t.Fatalf("grains = %v, want the unwrapped local values", g)
	}
	if _, err := ParseLocalGrains([]byte(`{"local": "ERROR: not a minion"}`)); !errs.Is(err, errs.Parse) {
		t.Fatalf("string local = %v, want a parse error", err)
	}
	if _, err := ParseLocalGrains([]byte(`{"other": {}}`)); !errs.Is(err, errs.Parse) {
		t.Fatalf("no local key = %v, want a parse error", err)
	}
}

func TestLocalParseState(t *testing.T) {
	out := []byte(`{"local": {"pkg_|-nginx_|-nginx_|-installed": {"result": null, "changes": {}, "comment": "would install", "__sls__": "nginx", "__run_num__": 0, "__id__": "nginx", "fun": "installed"}}}`)
	run, err := ParseLocalState(out)
	if err != nil {
		t.Fatalf("ParseLocalState: %v", err)
	}
	if !run.Test {
		t.Fatalf("run.Test = false, want a dry run")
	}
	if len(run.Rows) != 1 || run.Rows[0].ID != "nginx" {
		t.Fatalf("rows = %+v, want the nginx state", run.Rows)
	}
	if got := run.Summary(); !strings.Contains(got, "1 would change") {
		t.Fatalf("summary = %q, want one would change", got)
	}
	if run, err := ParseLocalState([]byte(`{"local": "no return"}`)); err != nil || len(run.Errors) != 1 {
		t.Fatalf("string local = %+v, %v; want it kept as a run error", run, err)
	}
}

func TestLocalGrainsOverSSH(t *testing.T) {
	ex := transporttest.New()
	ex.On("--local", transporttest.Reply{Stdout: `{"local": {"id": "web-01", "role": "web"}}`})
	b := New(Options{Executor: ex})

	g, err := b.LocalGrains(context.Background(), localHost)
	if err != nil {
		t.Fatalf("LocalGrains: %v", err)
	}
	if g.Str("role") != "web" {
		t.Fatalf("grains = %v", g)
	}
	if !b.LocalAvailable() {
		t.Fatalf("LocalAvailable = false with an executor")
	}
	calls := ex.Calls()
	if len(calls) != 1 || calls[0].Host != localHost {
		t.Fatalf("calls = %+v, want one call on the host itself", calls)
	}
	if got := calls[0].Command.Script; got != LocalGrainsCommand() {
		t.Fatalf("script = %q, want %q", got, LocalGrainsCommand())
	}
	if calls[0].Command.Sudo {
		t.Fatalf("command is run with sudo twice: the script already says sudo -n")
	}
}

func TestLocalStateOverSSH(t *testing.T) {
	ex := transporttest.New()
	ex.On("state.apply", transporttest.Reply{
		Stdout: `{"local": {"file_|-/etc/motd_|-/etc/motd_|-managed": {"result": true, "changes": {}, "comment": "ok", "__sls__": "common", "__run_num__": 0, "__id__": "/etc/motd", "fun": "managed"}}}`,
	})
	b := New(Options{Executor: ex})

	run, err := b.LocalState(context.Background(), localHost, "")
	if err != nil {
		t.Fatalf("LocalState: %v", err)
	}
	if len(run.Rows) != 1 || !run.Test {
		t.Fatalf("run = %+v, want one state of a dry run", run)
	}
	if _, err := b.LocalState(context.Background(), localHost, "bad sls"); !errs.Is(err, errs.Config) {
		t.Fatalf("bad sls = %v, want a config error", err)
	}
}

func TestLocalErrorKinds(t *testing.T) {
	tests := []struct {
		name string
		req  transporttest.Reply
		want errs.Kind
	}{
		{"sudo", transporttest.Reply{Stderr: "sudo: a password is required", RC: 1}, errs.Sudo},
		{"missing", transporttest.Reply{Stderr: "salt-call: command not found", RC: 127}, errs.NotAvailable},
		{"master", transporttest.Reply{Stderr: "The salt master could not be contacted", RC: 1}, errs.Unreachable},
		{"other", transporttest.Reply{Stderr: "boom", RC: 2}, errs.Remote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := transporttest.New()
			ex.On("salt-call", tt.req)
			b := New(Options{Executor: ex})
			_, err := b.LocalGrains(context.Background(), localHost)
			if !errs.Is(err, tt.want) {
				t.Fatalf("err = %v, want kind %s", err, tt.want)
			}
		})
	}
}

func TestLocalNeedsExecutor(t *testing.T) {
	b := New(Options{})
	if b.LocalAvailable() {
		t.Fatalf("LocalAvailable = true without an executor")
	}
	if _, err := b.LocalGrains(context.Background(), localHost); !errs.Is(err, errs.Config) {
		t.Fatalf("LocalGrains = %v, want a config error", err)
	}
	if b.UseLocalFallback(context.Background()) {
		t.Fatalf("UseLocalFallback = true without an executor")
	}
}

func TestLocalFallbackOnMasterDown(t *testing.T) {
	ex := transporttest.New()
	runner := NewRunner(ex, master, RunnerOptions{})
	ex.On("manage.status", transporttest.Reply{Stderr: "the salt master could not be contacted", RC: 1})
	b := New(Options{Executor: ex, Runner: runner, Master: master})
	if !b.UseLocalFallback(context.Background()) {
		t.Fatalf("UseLocalFallback = false for a down master with an executor")
	}
}
