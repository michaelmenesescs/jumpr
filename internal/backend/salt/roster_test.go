package salt

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport/transporttest"
)

const rosterYAML = `# salt-ssh roster
web-01.example.internal:
  host: 192.0.2.11
  user: deploy
  port: 2222
web-02.example.internal:
  host: 192.0.2.12
  user: deploy
k8s-01.example.internal:
  host: 192.0.2.21
`

func TestRosterParse(t *testing.T) {
	entries, err := ParseRoster([]byte(rosterYAML))
	if err != nil {
		t.Fatalf("ParseRoster: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v, want 3", entries)
	}
	first := entries[0]
	if first.ID != "k8s-01.example.internal" || first.Address() != "192.0.2.21" {
		t.Fatalf("first = %+v", first)
	}
	if entries[2].ID != "web-02.example.internal" || entries[2].Port != 0 {
		t.Fatalf("web-02 = %+v, want no port", entries[2])
	}
	web01 := entries[1]
	if web01.User != "deploy" || web01.Port != 2222 {
		t.Fatalf("web-01 = %+v, want user deploy and port 2222", web01)
	}
}

func TestRosterParseSkipsUnusable(t *testing.T) {
	entries, err := ParseRoster([]byte("bad: just-a-string\nempty: {}\ngood:\n  host: 192.0.2.9\n"))
	if err != nil {
		t.Fatalf("ParseRoster: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "good" {
		t.Fatalf("entries = %+v, want only the usable one", entries)
	}
	if _, err := ParseRoster([]byte("a: [1, 2\n")); !errs.Is(err, errs.Parse) {
		t.Fatalf("broken yaml = %v, want a parse error", err)
	}
}

func TestRosterRead(t *testing.T) {
	ex := transporttest.New()
	ex.On("cat /etc/salt/roster", transporttest.Reply{Stdout: rosterYAML})
	runner := NewRunner(ex, master, RunnerOptions{})
	b := New(Options{Runner: runner, Master: master})

	if got := b.RosterPath(); got != DefaultRosterPath {
		t.Fatalf("RosterPath = %q, want the default", got)
	}
	entries, err := b.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if !b.RosterReadable(context.Background()) {
		t.Fatalf("RosterReadable = false with a readable roster")
	}
	calls := ex.Calls()
	if len(calls) < 1 {
		t.Fatalf("no call recorded")
	}
	if !strings.Contains(calls[0].Command.Script, "sudo -n cat /etc/salt/roster") {
		t.Fatalf("script = %q, want sudo -n cat of the roster", calls[0].Command.Script)
	}
}

func TestRosterMissing(t *testing.T) {
	ex := transporttest.New()
	ex.On("cat ", transporttest.Reply{Stderr: "cat: /etc/salt/roster: No such file or directory", RC: 1})
	b := New(Options{Runner: NewRunner(ex, master, RunnerOptions{}), Master: master})
	if _, err := b.Roster(context.Background()); !errs.Is(err, errs.NotAvailable) {
		t.Fatalf("missing roster = %v, want not-available", err)
	}
	if b.RosterReadable(context.Background()) {
		t.Fatalf("RosterReadable = true without a roster")
	}
}

func TestRosterSourceLoad(t *testing.T) {
	ex := transporttest.New()
	ex.On("cat /srv/roster", transporttest.Reply{Stdout: rosterYAML})
	runner := NewRunner(ex, master, RunnerOptions{})
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	src := NewRosterSource(runner, "/srv/roster")
	src.Now = func() time.Time { return now }

	if src.Name() != RosterSourceName {
		t.Fatalf("Name = %q, want %q", src.Name(), RosterSourceName)
	}
	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(hosts) != 3 {
		t.Fatalf("hosts = %+v, want 3", hosts)
	}
	h := hosts[1]
	if h.ID != "192.0.2.11" {
		t.Fatalf("ID = %q, want the roster address", h.ID)
	}
	if h.Labels["salt.roster"] != "true" {
		t.Fatalf("labels = %v, want salt.roster=true", h.Labels)
	}
	if h.Labels["salt.roster.user"] != "deploy" || h.Labels["salt.roster.port"] != "2222" {
		t.Fatalf("labels = %v, want the roster user and port", h.Labels)
	}
	if len(h.Aliases) != 1 || h.Aliases[0] != "web-01.example.internal" {
		t.Fatalf("aliases = %v, want the roster key", h.Aliases)
	}
	if !h.SeenAt.Equal(now) {
		t.Fatalf("SeenAt = %v, want the injected clock", h.SeenAt)
	}
	if len(h.Sources) != 1 || h.Sources[0] != RosterSourceName {
		t.Fatalf("sources = %v", h.Sources)
	}
}

func TestSaltSSHCommand(t *testing.T) {
	line, err := SaltSSHCommand([]string{"web-02", "web-01"}, "test.ping", nil)
	if err != nil {
		t.Fatalf("SaltSSHCommand: %v", err)
	}
	if want := "salt-ssh -L web-01,web-02 test.ping"; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	if _, err := SaltSSHCommand(nil, "test.ping", nil); !errs.Is(err, errs.Config) {
		t.Fatalf("no hosts = %v, want a config error", err)
	}
	if _, err := SaltSSHCommand([]string{"web-01"}, "bad fun; rm -rf /", nil); !errs.Is(err, errs.Config) {
		t.Fatalf("bad function = %v, want a config error", err)
	}
}

func TestSaltSSHRefusedByDefault(t *testing.T) {
	ex := transporttest.New()
	b := New(Options{Runner: NewRunner(ex, master, RunnerOptions{}), Master: master})
	if b.SaltSSHAllowed() {
		t.Fatalf("SaltSSHAllowed = true by default")
	}
	_, err := b.SaltSSH(context.Background(), []string{"web-01"}, "test.ping", nil)
	if !errs.Is(err, errs.Config) {
		t.Fatalf("SaltSSH = %v, want a config error", err)
	}
	if !strings.Contains(err.Error(), "salt.allow_salt_ssh") {
		t.Fatalf("error = %q, want it to name the config key", err)
	}
	if calls := ex.Calls(); len(calls) != 0 {
		t.Fatalf("calls = %d, want none: a refused call runs nothing", len(calls))
	}
}

func TestSaltSSHPlan(t *testing.T) {
	ex := transporttest.New()
	b := New(Options{
		Runner: NewRunner(ex, master, RunnerOptions{}),
		Master: master,
		// Explicitly allowed only here: the test proves the opt-in path.
		AllowSaltSSH: true,
	})
	req := action.Request{
		SpecID:  SpecModuleRun,
		Targets: []action.Target{{Host: "web-01", Ref: resource.Ref{Backend: backendName, Kind: KindMinion, Name: "web-01"}}},
		Params:  map[string]string{ParamFun: "test.ping", ParamSSH: "true"},
		Actor:   action.Human,
	}
	plan, err := b.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Via != saltSSHTool {
		t.Fatalf("Via = %q, want salt-ssh", plan.Via)
	}
	found := false
	for _, w := range plan.Warnings {
		if w == SaltSSHWarning {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want %q", plan.Warnings, SaltSSHWarning)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("steps = %d, want one", len(plan.Steps))
	}
	if got := plan.Steps[0].Display; got != "salt-ssh -L web-01 test.ping" {
		t.Fatalf("display = %q", got)
	}

	// Without the opt-in the plan is refused before it is shown.
	locked := New(Options{Runner: NewRunner(ex, master, RunnerOptions{}), Master: master})
	if _, err := locked.Plan(context.Background(), req); !errs.Is(err, errs.Config) {
		t.Fatalf("Plan without allow_salt_ssh = %v, want a config error", err)
	}
}
