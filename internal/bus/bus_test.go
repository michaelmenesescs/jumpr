package bus

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/action"
	"jumper/internal/bus/bustest"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// recording is a Recorder that remembers what it was given.
type recording struct {
	mu      sync.Mutex
	actions []recorded
	denied  []Proposal
	rec     chan struct{} // one item per RecordAction
}

// recorded is one RecordAction call.
type recorded struct {
	proposal Proposal
	summary  Summary
	results  []action.StepResult
}

func newRecording() *recording {
	return &recording{rec: make(chan struct{}, 1024)}
}

func (r *recording) RecordAction(p Proposal, s Summary, results []action.StepResult) error {
	r.mu.Lock()
	r.actions = append(r.actions, recorded{proposal: p, summary: s, results: results})
	r.mu.Unlock()
	r.rec <- struct{}{}
	return nil
}

func (r *recording) RecordDenied(p Proposal) error {
	r.mu.Lock()
	r.denied = append(r.denied, p)
	r.mu.Unlock()
	return nil
}

func (r *recording) actionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.actions)
}

func (r *recording) deniedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.denied)
}

func (r *recording) last(t *testing.T) recorded {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.actions) == 0 {
		t.Fatal("recorder got no action")
	}
	return r.actions[len(r.actions)-1]
}

// waitRecorded waits for one RecordAction call.
func (r *recording) waitRecorded(t *testing.T) {
	t.Helper()
	select {
	case <-r.rec:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder was never called")
	}
}

// manyTargets makes n distinct targets.
func manyTargets(n int) []action.Target {
	out := make([]action.Target, n)
	for i := range out {
		out[i] = action.Target{Host: resource.HostID(fmt.Sprintf("web-ewr-%03d", i+1))}
	}
	return out
}

// request builds a request for specID on targets.
func request(specID string, targets []action.Target) action.Request {
	return action.Request{SpecID: specID, Targets: targets, Actor: action.Human}
}

// newTestBus builds a Bus with the default rollout unless one is given.
func newTestBus(t *testing.T, o Options) *bus {
	t.Helper()
	if o.Rollout == (RolloutConfig{}) {
		o.Rollout = defaultRollout()
	}
	b, ok := New(o).(*bus)
	if !ok {
		t.Fatal("New did not return *bus")
	}
	return b
}

// register adds a planner and fails the test on error.
func register(t *testing.T, b *bus, name string, pl action.Planner) {
	t.Helper()
	if err := b.Register(name, pl); err != nil {
		t.Fatalf("Register(%q): %v", name, err)
	}
}

// nextEvent reads one event, failing on timeout.
func nextEvent(t *testing.T, r Run) Event {
	t.Helper()
	select {
	case ev, ok := <-r.Events():
		if !ok {
			t.Fatal("events closed before the expected event")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

// nextPhase reads events until phase arrives.
func nextPhase(t *testing.T, r Run, phase Phase) Event {
	t.Helper()
	for {
		if ev := nextEvent(t, r); ev.Phase == phase {
			return ev
		}
	}
}

// drain reads until the events channel is closed.
func drain(t *testing.T, r Run) []Event {
	t.Helper()
	var out []Event
	for {
		select {
		case ev, ok := <-r.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(10 * time.Second):
			t.Fatal("timed out draining events")
		}
	}
}

// final returns the last event, which must carry a summary.
func final(t *testing.T, events []Event) Event {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	ev := events[len(events)-1]
	if ev.Summary == nil {
		t.Fatalf("last event %v has no summary", ev.Phase)
	}
	return ev
}

// start proposes and executes req on b.
func start(t *testing.T, b *bus, req action.Request) (Proposal, Run) {
	t.Helper()
	p, err := b.Propose(context.Background(), req)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	r, err := b.Execute(context.Background(), p, Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return p, r
}

func TestRunSingleTargetNoCanary(t *testing.T) {
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Default: bustest.Result{Output: "ok"}})
	rec := newRecording()
	b := newTestBus(t, Options{Recorder: rec})
	register(t, b, "fake", pl)

	p, err := b.Propose(context.Background(), request("fake.restart", manyTargets(1)))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.ID == "" {
		t.Error("proposal has no ID")
	}
	if !regexp.MustCompile(`^\d{8}T\d{6}-[0-9a-f]{4}$`).MatchString(p.ID) {
		t.Errorf("proposal ID %q is not time-ordered (want e.g. 20261112T140512-3f9a)", p.ID)
	}
	if p.Confirm != ConfirmYesNo {
		t.Errorf("Confirm = %v, want yes-no", p.Confirm)
	}
	if p.Rollout.Canary != 0 {
		t.Errorf("canary = %d, want 0 for a single target", p.Rollout.Canary)
	}

	r, err := b.Execute(context.Background(), p, Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ev := final(t, drain(t, r))
	if ev.Phase != PhaseDone {
		t.Errorf("final phase = %v, want done", ev.Phase)
	}
	if ev.Summary.OK != 1 || ev.Summary.Failed != 0 || ev.Summary.Skipped != 0 {
		t.Errorf("summary = %+v, want 1 ok", *ev.Summary)
	}
	if got := pl.Count(); got != 1 {
		t.Errorf("planner ran %d steps, want 1", got)
	}
	res := r.(RunInfo).Results()
	if len(res) != 1 || res[0].Output != "ok" {
		t.Errorf("results = %+v, want one ok result", res)
	}
	if n := len(b.Active()); n != 0 {
		t.Errorf("Active() = %d runs, want 0 after the run finished", n)
	}
	if rec.actionCount() != 1 {
		t.Errorf("recorded %d actions, want 1", rec.actionCount())
	}
}

func TestRunCanaryFailureStopsAndSkipsRest(t *testing.T) {
	targets := manyTargets(4)
	pl := bustest.New(bustest.Options{
		SpecID:  "fake.restart",
		Default: bustest.Result{Output: "ok"},
		Results: map[resource.HostID]bustest.Result{
			targets[0].Host: {RC: 1, Output: "systemctl failed"},
		},
	})
	rec := newRecording()
	b := newTestBus(t, Options{Recorder: rec})
	register(t, b, "fake", pl)

	_, r := start(t, b, request("fake.restart", targets))
	ev := final(t, drain(t, r))
	if ev.Phase != PhaseStopped {
		t.Fatalf("final phase = %v, want stopped", ev.Phase)
	}
	if ev.Summary.OK != 0 || ev.Summary.Failed != 1 || ev.Summary.Skipped != 3 {
		t.Errorf("summary = %+v, want 1 failed 3 skipped", *ev.Summary)
	}
	if got := pl.Ran(); len(got) != 1 || got[0] != targets[0].Host {
		t.Errorf("hosts touched = %v, want only the canary %s", got, targets[0].Host)
	}
	if rec.actionCount() != 1 {
		t.Errorf("recorded %d actions, want 1", rec.actionCount())
	}
}

func TestRunCanaryPauseThenContinue(t *testing.T) {
	targets := manyTargets(3)
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Default: bustest.Result{Output: "ok"}})
	b := newTestBus(t, Options{})
	register(t, b, "fake", pl)

	_, r := start(t, b, request("fake.restart", targets))
	nextPhase(t, r, PhasePaused)
	if got := pl.Count(); got != 1 {
		t.Errorf("ran %d steps while paused, want the canary only", got)
	}
	if n := len(b.Active()); n != 1 {
		t.Errorf("Active() = %d, want the paused run", n)
	}

	r.Continue()
	r.Continue() // idempotent: the second call must not start another batch

	ev := final(t, drain(t, r))
	if ev.Phase != PhaseDone {
		t.Fatalf("final phase = %v, want done", ev.Phase)
	}
	if ev.Summary.OK != 3 || ev.Summary.Failed != 0 || ev.Summary.Skipped != 0 {
		t.Errorf("summary = %+v, want 3 ok", *ev.Summary)
	}
	if got := pl.Count(); got != 3 {
		t.Errorf("ran %d steps, want 3", got)
	}
	if n := len(b.Active()); n != 0 {
		t.Errorf("Active() = %d after finishing, want 0", n)
	}
}

func TestRunAbortAfterCanary(t *testing.T) {
	targets := manyTargets(3)
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Default: bustest.Result{Output: "ok"}})
	b := newTestBus(t, Options{})
	register(t, b, "fake", pl)

	_, r := start(t, b, request("fake.restart", targets))
	nextPhase(t, r, PhasePaused)
	r.Abort()
	r.Abort() // idempotent

	ev := final(t, drain(t, r))
	if ev.Phase != PhaseAborted {
		t.Fatalf("final phase = %v, want aborted", ev.Phase)
	}
	if ev.Summary.OK != 1 || ev.Summary.Skipped != 2 {
		t.Errorf("summary = %+v, want 1 ok 2 skipped", *ev.Summary)
	}
	if got := pl.Count(); got != 1 {
		t.Errorf("ran %d steps after abort, want the canary only", got)
	}
}

func TestRunAbortBetweenBatches(t *testing.T) {
	targets := manyTargets(4)
	hold := make(chan struct{})
	pl := bustest.New(bustest.Options{
		SpecID:  "fake.restart",
		Default: bustest.Result{Output: "ok"},
		Results: map[resource.HostID]bustest.Result{
			targets[1].Host: {Hold: hold},
		},
	})
	b := newTestBus(t, Options{Rollout: RolloutConfig{
		Canary:                  0,
		BatchPercent:            100,
		BatchMax:                2,
		FailureThresholdPercent: 100,
		TypedConfirmOver:        20,
	}})
	register(t, b, "fake", pl)

	_, r := start(t, b, request("fake.restart", targets))

	// Wait for the fast half of the first batch, then abort while the
	// second half is still in flight. In-flight steps finish; the next
	// batch never starts.
	for {
		ev := nextEvent(t, r)
		if ev.Result != nil && ev.Result.Target.Host == targets[0].Host {
			break
		}
	}
	r.Abort()
	close(hold)

	ev := final(t, drain(t, r))
	if ev.Phase != PhaseAborted {
		t.Fatalf("final phase = %v, want aborted", ev.Phase)
	}
	if ev.Summary.OK != 2 || ev.Summary.Skipped != 2 {
		t.Errorf("summary = %+v, want 2 ok 2 skipped", *ev.Summary)
	}
	if got := pl.Count(); got != 2 {
		t.Errorf("ran %d steps, want the two in flight", got)
	}
}

func TestRunThresholdStops(t *testing.T) {
	targets := manyTargets(5)
	pl := bustest.New(bustest.Options{
		SpecID:  "fake.restart",
		Default: bustest.Result{Output: "ok"},
		Results: map[resource.HostID]bustest.Result{
			targets[1].Host: {RC: 1, Output: "boom"},
		},
	})
	b := newTestBus(t, Options{Rollout: RolloutConfig{
		Canary:                  1,
		BatchPercent:            100,
		BatchMax:                2,
		FailureThresholdPercent: 0,
		TypedConfirmOver:        20,
	}})
	register(t, b, "fake", pl)

	_, r := start(t, b, request("fake.restart", targets))
	nextPhase(t, r, PhasePaused)
	r.Continue()

	ev := final(t, drain(t, r))
	if ev.Phase != PhaseStopped {
		t.Fatalf("final phase = %v, want stopped", ev.Phase)
	}
	if ev.Summary.OK != 2 || ev.Summary.Failed != 1 || ev.Summary.Skipped != 2 {
		t.Errorf("summary = %+v, want 2 ok 1 failed 2 skipped", *ev.Summary)
	}
	if got := pl.Count(); got != 3 {
		t.Errorf("ran %d steps, want canary plus one batch", got)
	}
}

func TestBusDeniedCannotExecute(t *testing.T) {
	pl := bustest.New(bustest.Options{SpecID: "fake.restart"})
	rec := newRecording()
	b := newTestBus(t, Options{Recorder: rec, ReadOnly: func() bool { return true }})
	register(t, b, "fake", pl)

	p, err := b.Propose(context.Background(), request("fake.restart", manyTargets(1)))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.Denied == nil {
		t.Fatal("proposal was not denied")
	}
	if rec.deniedCount() != 1 {
		t.Errorf("recorded %d denials, want 1", rec.deniedCount())
	}

	_, err = b.Execute(context.Background(), p, Confirmation{ProposalID: p.ID})
	if !errs.Is(err, errs.Policy) {
		t.Fatalf("Execute error = %v, want kind policy", err)
	}
	if got := pl.Count(); got != 0 {
		t.Errorf("denied proposal ran %d steps, want 0", got)
	}
	if rec.actionCount() != 0 {
		t.Errorf("denied proposal was recorded as an action")
	}
}

func TestBusTypedCountRequired(t *testing.T) {
	targets := manyTargets(21)
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Default: bustest.Result{Output: "ok"}})
	b := newTestBus(t, Options{})
	register(t, b, "fake", pl)

	p, err := b.Propose(context.Background(), request("fake.restart", targets))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.Confirm != ConfirmTypedCount {
		t.Fatalf("Confirm = %v, want typed-count for 21 targets", p.Confirm)
	}
	if _, err := b.Execute(context.Background(), p, Confirmation{ProposalID: p.ID, Typed: "20"}); !errs.Is(err, errs.Policy) {
		t.Fatalf("wrong typed count error = %v, want policy", err)
	}
	if pl.Count() != 0 {
		t.Fatal("wrong typed count started the rollout")
	}

	r, err := b.Execute(context.Background(), p, Confirmation{ProposalID: p.ID, Typed: "21"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	nextPhase(t, r, PhasePaused)
	r.Continue()
	ev := final(t, drain(t, r))
	if ev.Phase != PhaseDone || ev.Summary.OK != 21 {
		t.Errorf("final = %v %+v, want done with 21 ok", ev.Phase, ev.Summary)
	}
}

func TestBusReadOnlyFlippedAfterPropose(t *testing.T) {
	pl := bustest.New(bustest.Options{SpecID: "fake.restart"})
	readonly := false
	b := newTestBus(t, Options{ReadOnly: func() bool { return readonly }})
	register(t, b, "fake", pl)

	p, err := b.Propose(context.Background(), request("fake.restart", manyTargets(1)))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.Denied != nil {
		t.Fatalf("proposal denied before read-only was set: %+v", p.Denied)
	}

	readonly = true
	if _, err := b.Execute(context.Background(), p, Confirmation{ProposalID: p.ID}); !errs.Is(err, errs.Policy) {
		t.Fatalf("Execute error = %v, want policy", err)
	}
	if got := pl.Count(); got != 0 {
		t.Errorf("ran %d steps under read-only, want 0", got)
	}
}

func TestRunRecorderGetsRedactedOutput(t *testing.T) {
	pl := bustest.New(bustest.Options{
		SpecID:  "fake.restart",
		Default: bustest.Result{Output: "password=hunter2\nrestarted"},
	})
	rec := newRecording()
	b := newTestBus(t, Options{Recorder: rec})
	register(t, b, "fake", pl)

	_, r := start(t, b, request("fake.restart", manyTargets(1)))
	drain(t, r)

	got := rec.last(t).results
	if len(got) != 1 {
		t.Fatalf("recorded %d results, want 1", len(got))
	}
	if strings.Contains(got[0].Output, "hunter2") {
		t.Errorf("recorded output still contains the secret: %q", got[0].Output)
	}
	if !strings.Contains(got[0].Output, "redacted") {
		t.Errorf("recorded output = %q, want a redaction marker", got[0].Output)
	}
	if strings.Contains(r.(RunInfo).Results()[0].Output, "hunter2") {
		t.Error("Results() still contains the secret")
	}
}

func TestRunSurvivesCallerCancel(t *testing.T) {
	targets := manyTargets(2)
	pl := bustest.New(bustest.Options{
		SpecID:  "fake.restart",
		Default: bustest.Result{Output: "ok", Delay: 20 * time.Millisecond},
	})
	b := newTestBus(t, Options{Rollout: RolloutConfig{
		Canary:                  0,
		BatchPercent:            100,
		BatchMax:                10,
		FailureThresholdPercent: 100,
		TypedConfirmOver:        20,
	}})
	register(t, b, "fake", pl)

	p, err := b.Propose(context.Background(), request("fake.restart", targets))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	caller, cancel := context.WithCancel(context.Background())
	r, err := b.Execute(caller, p, Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cancel() // closing the dialog must not kill the rollout

	ev := final(t, drain(t, r))
	if ev.Phase != PhaseDone || ev.Summary.OK != 2 {
		t.Errorf("final = %v %+v, want done with 2 ok", ev.Phase, ev.Summary)
	}
}

func TestRunSlowConsumerDoesNotBlock(t *testing.T) {
	targets := manyTargets(300)
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Default: bustest.Result{Output: "ok"}})
	rec := newRecording()
	b := newTestBus(t, Options{
		Recorder: rec,
		Rollout: RolloutConfig{
			Canary:                  0,
			BatchPercent:            100,
			BatchMax:                50,
			FailureThresholdPercent: 100,
			TypedConfirmOver:        1000,
		},
	})
	register(t, b, "fake", pl)

	// Deliberately do not read the events channel: 300 results do not fit
	// in the 256-slot buffer, and the run must still finish.
	p, err := b.Propose(context.Background(), request("fake.restart", targets))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	r, err := b.Execute(context.Background(), p, Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rec.waitRecorded(t)

	events := drain(t, r)
	ev := final(t, events)
	if ev.Phase != PhaseDone || ev.Summary.OK != 300 {
		t.Errorf("final = %v %+v, want done with 300 ok", ev.Phase, ev.Summary)
	}
	if got := rec.last(t).summary.OK; got != 300 {
		t.Errorf("recorder summary OK = %d, want 300", got)
	}
	if results := r.(RunInfo).Results(); len(results) != 300 {
		t.Errorf("Results() = %d, want all 300 kept", len(results))
	}
}

func TestRunStepPanicIsFailure(t *testing.T) {
	targets := manyTargets(2)
	pl := bustest.New(bustest.Options{
		SpecID:  "fake.restart",
		Default: bustest.Result{Output: "ok"},
		Results: map[resource.HostID]bustest.Result{
			targets[0].Host: {Panic: true},
		},
	})
	b := newTestBus(t, Options{Rollout: RolloutConfig{
		Canary:                  0,
		BatchPercent:            100,
		BatchMax:                10,
		FailureThresholdPercent: 100,
		TypedConfirmOver:        20,
	}})
	register(t, b, "fake", pl)

	_, r := start(t, b, request("fake.restart", targets))
	ev := final(t, drain(t, r))
	if ev.Phase != PhaseDone {
		t.Fatalf("final phase = %v, want done", ev.Phase)
	}
	if ev.Summary.Failed != 1 || ev.Summary.OK != 1 {
		t.Errorf("summary = %+v, want 1 failed 1 ok", *ev.Summary)
	}
	res := r.(RunInfo).Results()
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	var panicRes *action.StepResult
	for i := range res {
		if res[i].Err != nil {
			panicRes = &res[i]
		}
	}
	if panicRes == nil {
		t.Fatal("panicking step recorded no error")
	}
	if errs.KindOf(panicRes.Err) != errs.Internal {
		t.Errorf("panic error kind = %v, want internal", errs.KindOf(panicRes.Err))
	}
}

func TestRunStepTimeoutIsFailure(t *testing.T) {
	targets := manyTargets(2)
	pl := bustest.New(bustest.Options{
		SpecID:  "fake.restart",
		Default: bustest.Result{Output: "ok"},
		Results: map[resource.HostID]bustest.Result{
			// Never released: only the per-step timeout ends it.
			targets[0].Host: {Hold: make(chan struct{})},
		},
	})
	b := newTestBus(t, Options{
		StepTimeout: 20 * time.Millisecond,
		Rollout: RolloutConfig{
			Canary:                  0,
			BatchPercent:            100,
			BatchMax:                10,
			FailureThresholdPercent: 100,
			TypedConfirmOver:        20,
		},
	})
	register(t, b, "fake", pl)

	_, r := start(t, b, request("fake.restart", targets))
	ev := final(t, drain(t, r))
	if ev.Phase != PhaseDone || ev.Summary.Failed != 1 || ev.Summary.OK != 1 {
		t.Fatalf("final = %v %+v, want done with 1 failed 1 ok", ev.Phase, ev.Summary)
	}
	for _, res := range r.(RunInfo).Results() {
		if res.Target.Host != targets[0].Host {
			continue
		}
		if errs.KindOf(res.Err) != errs.Timeout {
			t.Errorf("timed-out step error kind = %v, want timeout", errs.KindOf(res.Err))
		}
	}
}

// noOwns is a planner without the Owner interface: ownership falls back to the
// registration name's prefix.
type noOwns struct{ pl action.Planner }

func (n noOwns) Actions(sel []resource.Resource) []action.Spec { return n.pl.Actions(sel) }
func (n noOwns) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	return n.pl.Plan(ctx, req)
}

func TestBusOwnershipByPrefixAndOwns(t *testing.T) {
	b := newTestBus(t, Options{})
	register(t, b, "linux", noOwns{bustest.New(bustest.Options{SpecID: "linux.systemctl.restart"})})
	register(t, b, "odd", bustest.New(bustest.Options{
		SpecID: "custom.op",
		OwnsFn: func(id string) bool { return id == "custom.op" },
	}))

	if _, err := b.Propose(context.Background(), request("linux.systemctl.restart", manyTargets(1))); err != nil {
		t.Errorf("prefix-owned spec: %v", err)
	}
	if _, err := b.Propose(context.Background(), request("custom.op", manyTargets(1))); err != nil {
		t.Errorf("Owns-owned spec: %v", err)
	}
	if _, err := b.Propose(context.Background(), request("nobody.knows", manyTargets(1))); !errs.Is(err, errs.Config) {
		t.Errorf("unknown spec error = %v, want kind config", err)
	}
}

func TestBusProposeValidation(t *testing.T) {
	b := newTestBus(t, Options{})
	register(t, b, "fake", bustest.New(bustest.Options{SpecID: "fake.restart"}))

	if _, err := b.Propose(context.Background(), request("fake.restart", nil)); !errs.Is(err, errs.Config) {
		t.Errorf("empty targets error = %v, want kind config", err)
	}

	dup := []action.Target{manyTargets(1)[0], manyTargets(1)[0]}
	p, err := b.Propose(context.Background(), request("fake.restart", dup))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(p.Plan.Steps) != 1 || len(p.Plan.Request.Targets) != 1 {
		t.Errorf("duplicate targets not de-duplicated: %d steps, %d targets",
			len(p.Plan.Steps), len(p.Plan.Request.Targets))
	}
	if p.Rollout.Canary != 0 {
		t.Errorf("canary = %d, want 0 after de-duplication", p.Rollout.Canary)
	}
}

func TestBusRegisterValidation(t *testing.T) {
	b := newTestBus(t, Options{})
	pl := bustest.New(bustest.Options{SpecID: "fake.restart"})
	register(t, b, "fake", pl)

	if err := b.Register("fake", pl); !errs.Is(err, errs.Config) {
		t.Errorf("duplicate name error = %v, want kind config", err)
	}
	if err := b.Register("other", nil); !errs.Is(err, errs.Config) {
		t.Errorf("nil planner error = %v, want kind config", err)
	}
	if err := b.Register(" ", pl); !errs.Is(err, errs.Config) {
		t.Errorf("empty name error = %v, want kind config", err)
	}
}
