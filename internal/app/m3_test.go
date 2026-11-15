package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jumpr/internal/action"
	"jumpr/internal/bus"
	"jumpr/internal/bus/bustest"
	"jumpr/internal/config"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
)

// writePolicy writes body as <dir>/policy.yaml.
func writePolicy(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
}

// m3Fixture is the input of every buildM3 test: a temp config dir with a
// policy file, a knowledge root under it and one scripted planner.
type m3Fixture struct {
	cfg     config.Config
	paths   config.Paths
	session *tui.SessionState
	planner *bustest.Planner
	root    string
}

// newM3Fixture builds the fixture. The knowledge root is a temp directory, so
// nothing a test records can reach the real ~/jumpr.
func newM3Fixture(t *testing.T, policy string) *m3Fixture {
	t.Helper()
	dir := t.TempDir()
	if policy != "" {
		writePolicy(t, dir, policy)
	}
	knowledge := filepath.Join(dir, "knowledge-root")
	cfg := config.Default()
	cfg.Knowledge.Root = knowledge
	return &m3Fixture{
		cfg: cfg,
		paths: config.Paths{
			ConfigDir:     dir,
			CacheDir:      filepath.Join(dir, "cache"),
			KnowledgeRoot: knowledge,
		},
		session: &tui.SessionState{Name: "main"},
		planner: bustest.New(bustest.Options{
			SpecID:  "fake.restart",
			Title:   "restart nginx.service",
			Default: bustest.Result{Output: "started, token lab-token-abc"},
		}),
		root: knowledge,
	}
}

// build assembles the M3 layer over the fixture.
func (f *m3Fixture) build(t *testing.T) *m3Runtime {
	t.Helper()
	rt, err := buildM3(context.Background(), f.cfg, f.paths, f.paths.ConfigDir, f.session,
		[]namedPlanner{{name: "fake", planner: f.planner}}, nil)
	if err != nil {
		t.Fatalf("buildM3: %v", err)
	}
	return rt
}

// proposal asks the bus for the one-target plan the scripted planner serves.
func proposal(t *testing.T, rt *m3Runtime, host resource.HostID) bus.Proposal {
	t.Helper()
	p, err := rt.Bus.Propose(context.Background(), action.Request{
		SpecID:  "fake.restart",
		Targets: []action.Target{{Host: host, Labels: map[string]string{"role": "web"}}},
		Actor:   action.Human,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	return p
}

// run collects every event of a rollout.
func run(t *testing.T, r bus.Run) []bus.Event {
	t.Helper()
	var out []bus.Event
	for ev := range r.Events() {
		out = append(out, ev)
		if ev.Phase == bus.PhasePaused {
			r.Continue()
		}
	}
	return out
}

// TestBuildM3WiresTheLayer is the non-lab check that the M3 assembly really is
// assembled: policy, redactor, record, knowledge index and a bus that plans,
// runs and records through the scripted planner.
func TestBuildM3WiresTheLayer(t *testing.T) {
	f := newM3Fixture(t, "redact: ['lab-token-[a-z]+']\n")
	rt := f.build(t)

	if rt.Policy == nil || rt.Bus == nil || rt.Log == nil {
		t.Fatalf("policy/bus/log not built: %+v", rt)
	}
	if got := rt.Policy.RedactPatterns(); len(got) != 1 || got[0] != `lab-token-[a-z]+` {
		t.Errorf("RedactPatterns = %v, want the file's one pattern", got)
	}
	// The redactor is built from the policy file, so the extra pattern applies.
	if got, want := rt.Redact.String("token lab-token-abc"), "token ‹redacted:extra:1›"; got != want {
		t.Errorf("redact = %q, want %q", got, want)
	}
	if got := rt.Log.Root(); got != f.root {
		t.Errorf("log root = %q, want %q", got, f.root)
	}
	if rt.Index == nil {
		t.Error("knowledge index was not built")
	}
	if _, ok := rt.Planners["fake"]; !ok {
		t.Errorf("planners = %v, want the scripted one registered", rt.Planners)
	}

	// Record into a temp knowledge root: an open investigation first, so the
	// action bullet lands in the file rather than the audit file.
	if _, err := rt.Log.Open("lab wiring", resource.TimeWindow{}, []resource.HostID{"web-01.example.internal"}, nil); err != nil {
		t.Fatalf("open investigation: %v", err)
	}
	meta, ok := rt.Log.Current()
	if !ok {
		t.Fatal("no investigation open")
	}

	p := proposal(t, rt, "web-01.example.internal")
	events := run(t, mustExecute(t, rt, p))
	if len(events) == 0 {
		t.Fatal("the rollout emitted no events")
	}
	last := events[len(events)-1]
	if last.Phase != bus.PhaseDone || last.Summary == nil || last.Summary.OK != 1 {
		t.Fatalf("last event = %+v, want done with 1 ok", last)
	}

	body, err := os.ReadFile(meta.Path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "**action**") {
		t.Errorf("record has no action bullet:\n%s", text)
	}
	if !strings.Contains(text, "‹redacted:extra:1›") {
		t.Errorf("record kept the secret the policy refuses:\n%s", text)
	}
	if strings.Contains(text, "lab-token-abc") {
		t.Errorf("record contains the raw secret:\n%s", text)
	}
}

// TestBuildM3ReadOnlySessionDenies checks the session switch the `--readonly`
// flag sets: the bus refuses the proposal and records the denial.
func TestBuildM3ReadOnlySessionDenies(t *testing.T) {
	f := newM3Fixture(t, "")
	f.session.ReadOnly = true
	rt := f.build(t)

	if _, err := rt.Log.Open("read only", resource.TimeWindow{}, nil, nil); err != nil {
		t.Fatalf("open investigation: %v", err)
	}
	meta, ok := rt.Log.Current()
	if !ok {
		t.Fatal("no investigation open")
	}

	p := proposal(t, rt, "web-01.example.internal")
	if p.Denied == nil {
		t.Fatal("a read-only session produced an allowed proposal")
	}
	_, err := rt.Bus.Execute(context.Background(), p, bus.Confirmation{ProposalID: p.ID})
	if err == nil {
		t.Fatal("Execute succeeded in a read-only session")
	}
	if kind := errs.KindOf(err); kind != errs.Policy {
		t.Errorf("Execute error kind = %v, want policy", kind)
	}
	if len(f.planner.Ran()) != 0 {
		t.Errorf("steps ran in a read-only session: %v", f.planner.Ran())
	}

	body, rerr := os.ReadFile(meta.Path)
	if rerr != nil {
		t.Fatalf("read record: %v", rerr)
	}
	if !strings.Contains(string(body), "**denied**") {
		t.Errorf("the denial was not recorded:\n%s", body)
	}
}

// TestBuildM3PolicyReadOnly checks that `readonly: true` in the file denies
// writes without anyone touching the session switch.
func TestBuildM3PolicyReadOnly(t *testing.T) {
	f := newM3Fixture(t, "readonly: true\n")
	rt := f.build(t)

	if !rt.Policy.ReadOnly() {
		t.Fatal("policy does not report read-only")
	}
	if p := proposal(t, rt, "web-01.example.internal"); p.Denied == nil {
		t.Fatal("a read-only policy produced an allowed proposal")
	}
}

// TestBuildM3RejectsBadRedactPattern checks that a pattern that does not
// compile fails the start as a config error naming the file, instead of
// surfacing later from the redactor.
func TestBuildM3RejectsBadRedactPattern(t *testing.T) {
	f := newM3Fixture(t, "redact: ['(']\n")
	_, err := buildM3(context.Background(), f.cfg, f.paths, f.paths.ConfigDir, f.session, nil, nil)
	if err == nil {
		t.Fatal("buildM3 accepted an uncompilable redact pattern")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Errorf("error kind = %v, want config", kind)
	}
}

// TestBuildM3RolloutUsesConfig checks that the rollout shape and the typed
// confirmation threshold come from the config, not from the bus defaults.
func TestBuildM3RolloutUsesConfig(t *testing.T) {
	f := newM3Fixture(t, "")
	f.cfg.Rollout = config.RolloutConfig{Canary: 2, BatchPercent: 50, BatchMax: 4, FailureThresholdPercent: 25, TypedConfirmOver: 3}
	rt := f.build(t)

	req := action.Request{
		SpecID: "fake.restart",
		Actor:  action.Human,
		Targets: []action.Target{
			{Host: "web-01.example.internal"},
			{Host: "web-02.example.internal"},
			{Host: "web-03.example.internal"},
			{Host: "web-04.example.internal"},
		},
	}
	p, err := rt.Bus.Propose(context.Background(), req)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if p.Rollout.Canary != 2 {
		t.Errorf("canary = %d, want 2", p.Rollout.Canary)
	}
	if p.Rollout.FailureThreshold != 0.25 {
		t.Errorf("failure threshold = %v, want 0.25", p.Rollout.FailureThreshold)
	}
	if p.Confirm != bus.ConfirmTypedCount {
		t.Errorf("confirm = %v, want typed count for 4 targets over a threshold of 3", p.Confirm)
	}
	if _, err := rt.Bus.Execute(context.Background(), p, bus.Confirmation{ProposalID: p.ID}); err == nil {
		t.Error("a typed-count proposal ran without the typed count")
	}
}

// mustExecute starts a rollout or fails the test.
func mustExecute(t *testing.T, rt *m3Runtime, p bus.Proposal) bus.Run {
	t.Helper()
	r, err := rt.Bus.Execute(context.Background(), p, bus.Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return r
}
