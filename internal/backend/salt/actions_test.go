package salt

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
)

// cleanState is a dry-run return with one state that is already in place.
const cleanState = `{
  "file_|-nginx-config_|-/etc/nginx/nginx.conf_|-managed": {
    "result": true,
    "changes": {},
    "comment": "File is in the correct state",
    "name": "/etc/nginx/nginx.conf",
    "__sls__": "nginx.config",
    "__run_num__": 1,
    "__id__": "nginx-config"
  }
}`

// diffState is a dry-run return with one state that would change, carrying the
// diff the plan preview must show.
const diffState = `{
  "service_|-nginx-service_|-nginx_|-running": {
    "result": null,
    "changes": {"diff": "--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new"},
    "comment": "Service nginx is set to start",
    "name": "nginx",
    "__sls__": "nginx.service",
    "__run_num__": 1,
    "__id__": "nginx-service"
  }
}`

// failedState is a real apply return with one failed state.
const failedState = `{
  "file_|-motd_|-/etc/motd_|-managed": {
    "result": false,
    "changes": {},
    "comment": "Unable to manage file: no such directory",
    "name": "/etc/motd",
    "__sls__": "base.motd",
    "__run_num__": 1,
    "__id__": "motd"
  }
}`

// newActionBackend wires a Backend to the scripted master.
func newActionBackend(t *testing.T, m *scriptedMaster, minions ...string) *Backend {
	t.Helper()
	r := NewRunner(m.fake, master, RunnerOptions{})
	return New(Options{Runner: r, Inventory: newExeInv(minions...), Master: master})
}

// minionRow is a salt-minion selection row.
func minionRes(id string) resource.Resource {
	return resource.Resource{
		Ref:    resource.Ref{Backend: backendName, Kind: KindMinion, Name: id},
		Fields: map[string]any{"minion": id},
	}
}

// jobRow is a salt-job selection row.
func jobRes(state string) resource.Resource {
	return resource.Resource{
		Ref:    resource.Ref{Backend: backendName, Kind: KindJobs, Name: jidA},
		Fields: map[string]any{"job": jidA, "state": state},
	}
}

// hostRow is a host selection row with the labels the inventory gives it.
func hostRes(id string, labels map[string]string) resource.Resource {
	return resource.Resource{
		Ref:    resource.Ref{Kind: "host", Name: id},
		Labels: labels,
		Fields: map[string]any{"host": id},
	}
}

// specIDs is the IDs of a spec list, for comparison.
func specIDs(specs []action.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.ID)
	}
	return out
}

// TestPlannerActionsForSelections covers every selection the planner offers actions
// for and the ones it must refuse.
func TestPlannerActionsForSelections(t *testing.T) {
	b := New(Options{})
	minions := []string{"salt.state.apply", "salt.service.restart", "salt.cmd.run",
		"salt.saltutil.refresh_pillar", "salt.saltutil.sync_all"}

	cases := []struct {
		name  string
		sel   []resource.Resource
		want  []string
		nilOK bool
	}{
		{
			name: "one minion",
			sel:  []resource.Resource{minionRes("web-01")},
			want: minions,
		},
		{
			name: "two minions",
			sel:  []resource.Resource{minionRes("web-01"), minionRes("web-02")},
			want: minions,
		},
		{
			name: "running job",
			sel:  []resource.Resource{jobRes(JobRunning)},
			want: []string{SpecJobKill},
		},
		{
			name:  "finished job",
			sel:   []resource.Resource{jobRes(JobDone)},
			nilOK: true,
		},
		{
			name: "host that is a minion",
			sel:  []resource.Resource{hostRes("web-01.lab.example.internal", map[string]string{"salt.id": "web-01"})},
			want: []string{SpecStateApply},
		},
		{
			name:  "host that is not a minion",
			sel:   []resource.Resource{hostRes("db-01.lab.example.internal", nil)},
			nilOK: true,
		},
		{
			name:  "mixed kinds",
			sel:   []resource.Resource{minionRes("web-01"), jobRes(JobRunning)},
			nilOK: true,
		},
		{
			name:  "empty selection",
			sel:   nil,
			nilOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := specIDs(b.Actions(tc.sel))
			if tc.nilOK {
				if len(got) != 0 {
					t.Fatalf("Actions = %v, want none", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Actions = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Actions = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestPlanDisplayStrings: the plan shows the line the user would type, and it names
// the master it runs through.
func TestPlanDisplayStrings(t *testing.T) {
	minions := []string{"web-01"}
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, okMinion("web-01", "up"))
	})
	b := newActionBackend(t, m, minions...)

	req := action.Request{
		SpecID:  SpecStateApply,
		Targets: []action.Target{{Host: exeHost("web-01"), Ref: minionRes("web-01").Ref}},
		Params:  map[string]string{ParamSLS: "nginx"},
	}
	plan, err := b.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(plan.Steps))
	}
	const want = "salt -L web-01 state.apply nginx"
	if got := plan.Steps[0].Display; got != want {
		t.Errorf("Display = %q, want %q", got, want)
	}
	if want := "salt:" + master; plan.Via != want {
		t.Errorf("Via = %q, want %q", plan.Via, want)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "root") {
		t.Errorf("Warnings = %v, want the root warning", plan.Warnings)
	}

	restart := req
	restart.SpecID = SpecServiceRestart
	restart.Params = map[string]string{ParamName: "nginx"}
	plan, err = b.Plan(context.Background(), restart)
	if err != nil {
		t.Fatalf("Plan restart: %v", err)
	}
	const wantRestart = "salt -L web-01 service.restart nginx"
	if got := plan.Steps[0].Display; got != wantRestart {
		t.Errorf("Display = %q, want %q", got, wantRestart)
	}
}

// TestDryRun: the dry run is state.apply test=True on the compiled list target,
// with no queue=True, and it streams every minion's result.
func TestDryRun(t *testing.T) {
	minions := []string{"web-01", "web-02"}
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions,
			minion{ID: "web-01", Return: cleanState, Retcode: 0, Success: true},
			minion{ID: "web-02", Return: diffState, Retcode: 0, Success: true})
	})
	b := newActionBackend(t, m, minions...)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, compiled, err := b.DryRun(ctx, minions, "", resource.Selector{})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	defer job.Cancel()
	if len(compiled.Targets) != 1 || compiled.Targets[0].Type != "list" {
		t.Fatalf("compiled targets = %+v, want one list target", compiled.Targets)
	}
	if compiled.Explain == "" {
		t.Error("compiled Explain is empty")
	}

	got := map[string]StateRun{}
	for mr := range job.Results() {
		got[mr.Minion] = StateRunFromResult(mr)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if groups := StateGroups(got); len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}

	line := submissionLine(t, m)
	for _, want := range []string{"state.apply", "test=True", "-L web-01,web-02"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q is missing %q", line, want)
		}
	}
	if strings.Contains(line, "queue=True") {
		t.Errorf("line %q passes queue=True", line)
	}
}

// TestPlanStateApplyHasPreview: an apply plan always carries the grouped dry
// run, with the diff of every group that would change.
func TestPlanStateApplyHasPreview(t *testing.T) {
	minions := []string{"web-01", "web-02", "web-03", "web-04"}
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions,
			minion{ID: "web-01", Return: cleanState, Success: true},
			minion{ID: "web-02", Return: cleanState, Success: true},
			minion{ID: "web-03", Return: diffState, Success: true},
			minion{ID: "web-04", Return: diffState, Success: true})
	})
	b := newActionBackend(t, m, minions...)

	req := action.Request{SpecID: SpecStateApply, Params: map[string]string{ParamSLS: "nginx"}}
	for _, id := range minions {
		req.Targets = append(req.Targets, action.Target{Host: exeHost(id), Ref: minionRes(id).Ref})
	}
	plan, err := b.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Steps) != len(minions) {
		t.Fatalf("steps = %d, want %d", len(plan.Steps), len(minions))
	}
	for _, want := range []string{"2 clean", "2 would change", "+++ b", "-old", "+new", "web-03, web-04"} {
		if !strings.Contains(plan.Preview, want) {
			t.Errorf("preview %q is missing %q", plan.Preview, want)
		}
	}
	if !strings.Contains(plan.Preview, "included anyway") {
		t.Errorf("preview %q does not call out the clean minions", plan.Preview)
	}
	if plan.Spec.Risk != action.RiskDestructive || !plan.Spec.HasDryRun {
		t.Errorf("state.apply spec = %+v, want destructive with a dry run", plan.Spec)
	}
}

// TestPlanFailsWhenDryRunFailsEverywhere: no preview and no plan when every
// minion refuses the dry run.
func TestPlanFailsWhenDryRunFailsEverywhere(t *testing.T) {
	minions := []string{"web-01", "web-02"}
	const busy = "The minion is running a state job; not starting another one"
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions,
			minion{ID: "web-01", Return: quoteJSON(busy), Success: true},
			minion{ID: "web-02", Return: quoteJSON(busy), Success: true})
	})
	b := newActionBackend(t, m, minions...)

	req := action.Request{SpecID: SpecStateApply, Params: map[string]string{ParamSLS: "nginx"}}
	for _, id := range minions {
		req.Targets = append(req.Targets, action.Target{Host: exeHost(id), Ref: minionRes(id).Ref})
	}
	plan, err := b.Plan(context.Background(), req)
	if err == nil {
		t.Fatalf("Plan succeeded with preview %q, want the dry run error", plan.Preview)
	}
	if plan.Preview != "" || len(plan.Steps) != 0 {
		t.Errorf("plan = %+v, want nothing", plan)
	}
	if !strings.Contains(err.Error(), "state job") {
		t.Errorf("error %q does not carry the minion's message", err)
	}
}

// TestPlanStepFailsOnFailedState: a step whose state return carries a failed state
// exits 1 and keeps the normalized text.
func TestPlanStepFailsOnFailedState(t *testing.T) {
	minions := []string{"web-01"}
	m := newScriptedMaster([]string{jidA, jidB}, func(jid string, _ int) transporttest.Reply {
		if jid == jidA {
			return jobReply(t, jid, minions, minion{ID: "web-01", Return: cleanState, Success: true})
		}
		return jobReply(t, jid, minions, minion{ID: "web-01", Return: failedState, Success: true})
	})
	b := newActionBackend(t, m, minions...)

	req := action.Request{
		SpecID:  SpecStateApply,
		Targets: []action.Target{{Host: exeHost("web-01"), Ref: minionRes("web-01").Ref}},
	}
	plan, err := b.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(plan.Steps))
	}

	res := plan.Steps[0].Run(context.Background())
	if res.RC != 1 {
		t.Errorf("RC = %d, want 1", res.RC)
	}
	if res.Err == nil {
		t.Error("Err is nil, want the failed state")
	}
	for _, want := range []string{"1 failed", "FAILED base.motd :: motd"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output %q is missing %q", res.Output, want)
		}
	}
}

// TestKillJobPlan: killing a running job is one step per minion the job still
// runs on, naming the jid.
func TestKillJobPlan(t *testing.T) {
	minions := []string{"web-01", "web-02"}
	const running = "cmd.run"
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		reply := jobReply(t, jid, minions,
			minion{ID: "web-01", Return: quoteJSON("out"), Success: true})
		return reply
	})
	b := newActionBackend(t, m, minions...)

	req := action.Request{
		SpecID:  SpecJobKill,
		Targets: []action.Target{{Ref: jobRes(JobRunning).Ref}},
	}
	plan, err := b.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want 2 (one per minion)", len(plan.Steps))
	}
	for i, m := range minions {
		want := "salt -L " + m + " saltutil.kill_job " + jidA
		if got := plan.Steps[i].Display; got != want {
			t.Errorf("step %d Display = %q, want %q", i, got, want)
		}
	}
	if !strings.Contains(plan.Title, jidA) {
		t.Errorf("title %q does not name the job", plan.Title)
	}
	if plan.Spec.Risk != action.RiskDestructive {
		t.Errorf("kill risk = %v, want destructive", plan.Spec.Risk)
	}
}

// TestPlannerRefusesBadRequests: an unknown spec, no targets and a bad
// parameter fail whole, before anything is submitted.
func TestPlannerRefusesBadRequests(t *testing.T) {
	minions := []string{"web-01"}
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, minion{ID: "web-01", Return: cleanState, Success: true})
	})
	b := newActionBackend(t, m, minions...)

	cases := []struct {
		name string
		req  action.Request
		want errs.Kind
	}{
		{"unknown spec", action.Request{SpecID: "salt.nope", Targets: []action.Target{{Ref: minionRes("web-01").Ref}}}, errs.Config},
		{"no targets", action.Request{SpecID: SpecStateApply}, errs.Config},
		{"foreign spec", action.Request{SpecID: "linux.kill", Targets: []action.Target{{Ref: minionRes("web-01").Ref}}}, errs.Config},
		{"bad sls", action.Request{SpecID: SpecStateApply, Targets: []action.Target{{Ref: minionRes("web-01").Ref}},
			Params: map[string]string{ParamSLS: "nginx; rm -rf /"}}, errs.Config},
		{"no cmd", action.Request{SpecID: SpecCmdRun, Targets: []action.Target{{Ref: minionRes("web-01").Ref}}}, errs.Config},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Plan(context.Background(), tc.req)
			if k := errs.KindOf(err); k != tc.want {
				t.Fatalf("kind = %q (%v), want %q", k, err, tc.want)
			}
		})
	}
	if subs := m.submitCount(); subs != 0 {
		t.Errorf("submitted %d jobs, want 0", subs)
	}
}

// ---- key management (task 095) ----

const (
	// keyListJSON is salt-key -L with one accepted, two pending and one rejected key.
	keyListJSON = `{"minions": ["web-01"], "minions_pre": ["new-01", "new-02"],
		"minions_rejected": ["old-01"], "minions_denied": []}`
	// keyFPJSON is salt-key -f/F: the master's own key plus every minion's.
	keyFPJSON = `{"minions": {"web-01": "aa:bb:cc"}, "minions_pre": {"new-01": "44:55:66", "new-02": "77:88:99"},
		"minions_rejected": {"old-01": "de:ad:00"}, "local": {"master.pub": "99:88:77"}}`
	// keyUpJSON is salt-run manage.status: web-01 answers, old-01 does not.
	keyUpJSON = `{"up": ["web-01"], "down": ["old-01"]}`
)

// keyRes is a salt-key selection row in the given state.
func keyRes(id, state string) resource.Resource {
	return resource.Resource{
		Ref:    resource.Ref{Backend: backendName, Kind: KindKey, Name: id},
		Fields: map[string]any{"id": id, "state": state},
	}
}

// keyTarget is the request target a salt-key row produces.
func keyTarget(id string) action.Target {
	return action.Target{Ref: keyRes(id, KeyPending).Ref}
}

// newKeyBackend wires a Backend to a fake master for the key actions.
func newKeyBackend(f *transporttest.Fake) *Backend {
	return New(Options{Runner: NewRunner(f, master, RunnerOptions{}), Master: master})
}

// keyMaster is the fake master that answers the key reads.
func keyMaster() *transporttest.Fake {
	return transporttest.New().
		On("-f ", transporttest.Reply{Stdout: keyFPJSON}).
		On("-L", transporttest.Reply{Stdout: keyListJSON}).
		On("manage.status", transporttest.Reply{Stdout: keyUpJSON})
}

// TestKeyActionsForStates: a pending key is accepted or rejected, a key the
// master already decided on is deleted, and a mixed selection offers nothing.
func TestKeyActionsForStates(t *testing.T) {
	b := New(Options{})
	cases := []struct {
		state string
		want  []string
	}{
		{KeyPending, []string{SpecKeyAccept, SpecKeyReject}},
		{KeyAccepted, []string{SpecKeyDelete}},
		{KeyRejected, []string{SpecKeyDelete}},
		{KeyDenied, []string{SpecKeyDelete}},
		{"", nil},
	}
	for _, tc := range cases {
		name := tc.state
		if name == "" {
			name = "unknown state"
		}
		t.Run(name, func(t *testing.T) {
			got := specIDs(b.Actions([]resource.Resource{keyRes("new-01", tc.state)}))
			if len(got) != len(tc.want) {
				t.Fatalf("Actions = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Actions = %v, want %v", got, tc.want)
				}
			}
		})
	}

	mixed := b.Actions([]resource.Resource{keyRes("new-01", KeyPending), keyRes("web-01", KeyAccepted)})
	if len(mixed) != 0 {
		t.Errorf("a mixed selection offers %v, want nothing", specIDs(mixed))
	}
	pending := b.Actions([]resource.Resource{keyRes("new-01", KeyPending), keyRes("new-02", KeyPending)})
	if got := specIDs(pending); len(got) != 2 || got[0] != SpecKeyAccept || got[1] != SpecKeyReject {
		t.Errorf("two pending keys offer %v, want accept and reject", got)
	}
	// Key rows keep their state: the risk of the offer comes from the spec.
	for _, s := range keySpecs() {
		switch s.ID {
		case SpecKeyReject, SpecKeyDelete:
			if s.Risk != action.RiskDestructive {
				t.Errorf("%s risk = %v, want destructive", s.ID, s.Risk)
			}
		case SpecKeyAccept:
			if s.Risk == action.RiskDestructive {
				t.Errorf("accept is marked destructive")
			}
		}
	}
}

// TestKeyPlanSteps: one salt-key step per key, on the master, with the exact
// display line, the fingerprint preview and the delete label that forces the
// typed count.
func TestKeyPlanSteps(t *testing.T) {
	b := newKeyBackend(keyMaster())
	ctx := context.Background()

	accept := action.Request{
		SpecID:  SpecKeyAccept,
		Targets: []action.Target{keyTarget("new-01"), keyTarget("new-02")},
	}
	plan, err := b.Plan(ctx, accept)
	if err != nil {
		t.Fatalf("Plan accept: %v", err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(plan.Steps))
	}
	for i, want := range []string{"salt-key -a new-01 -y", "salt-key -a new-02 -y"} {
		if got := plan.Steps[i].Display; got != want {
			t.Errorf("step %d Display = %q, want %q", i, got, want)
		}
	}
	for i, id := range []string{"new-01", "new-02"} {
		st := plan.Steps[i].Target
		if st.Host != resource.HostID(master) {
			t.Errorf("step %d host = %q, want the master %q", i, st.Host, master)
		}
		if st.Ref.Kind != KindKey || st.Ref.Name != id || st.Ref.Backend != backendName {
			t.Errorf("step %d Ref = %s, want the salt key %s", i, st.Ref, id)
		}
		if _, ok := st.Labels[confirmTypedLabel]; ok {
			t.Errorf("step %d carries the typed-count label on an accept", i)
		}
	}
	if !strings.Contains(plan.Title, "accept") || !strings.Contains(plan.Title, "2 keys") {
		t.Errorf("title = %q, want an accept of 2 keys", plan.Title)
	}
	if plan.Spec.Risk == action.RiskDestructive {
		t.Errorf("accept risk = %v, want non-destructive", plan.Spec.Risk)
	}
	if want := "salt:" + master; plan.Via != want {
		t.Errorf("Via = %q, want %q", plan.Via, want)
	}
	for _, want := range []string{"new-01  fingerprint 44:55:66", "new-02  fingerprint 77:88:99", keyVerifyHint} {
		if !strings.Contains(plan.Preview, want) {
			t.Errorf("accept preview %q is missing %q", plan.Preview, want)
		}
	}

	reject := accept
	reject.SpecID = SpecKeyReject
	plan, err = b.Plan(ctx, reject)
	if err != nil {
		t.Fatalf("Plan reject: %v", err)
	}
	if got := plan.Steps[0].Display; got != "salt-key -r new-01 -y" {
		t.Errorf("reject Display = %q", got)
	}
	if plan.Spec.Risk != action.RiskDestructive {
		t.Errorf("reject risk = %v, want destructive", plan.Spec.Risk)
	}
	if strings.Contains(plan.Preview, keyVerifyHint) {
		t.Errorf("reject preview carries the accept hint: %q", plan.Preview)
	}

	del := action.Request{
		SpecID:  SpecKeyDelete,
		Targets: []action.Target{keyTarget("web-01"), keyTarget("old-01")},
	}
	plan, err = b.Plan(ctx, del)
	if err != nil {
		t.Fatalf("Plan delete: %v", err)
	}
	for i, want := range []string{"salt-key -d web-01 -y", "salt-key -d old-01 -y"} {
		if got := plan.Steps[i].Display; got != want {
			t.Errorf("delete step %d Display = %q, want %q", i, got, want)
		}
		if got := plan.Steps[i].Target.Labels[confirmTypedLabel]; got != confirmTypedValue {
			t.Errorf("delete step %d %s label = %q, want %q", i, confirmTypedLabel, got, confirmTypedValue)
		}
	}
	if !strings.Contains(plan.Title, "delete") || !strings.Contains(plan.Title, "2 keys") {
		t.Errorf("title = %q, want a delete of 2 keys", plan.Title)
	}
	for _, want := range []string{"web-01  fingerprint aa:bb:cc", "last seen: up", "old-01  fingerprint de:ad:00",
		"last seen: down", "warning: web-01 is currently up"} {
		if !strings.Contains(plan.Preview, want) {
			t.Errorf("delete preview %q is missing %q", plan.Preview, want)
		}
	}
	if n := strings.Count(plan.Preview, "warning:"); n != 1 {
		t.Errorf("delete preview has %d warnings, want 1 (only web-01 is up):\n%s", n, plan.Preview)
	}
}

// TestKeyPlanRefusesGlobsAndBadIDs: only an exact, validated key id reaches a
// salt-key line, so `-a '*'` cannot be planned; nothing is submitted.
func TestKeyPlanRefusesGlobsAndBadIDs(t *testing.T) {
	f := keyMaster()
	b := newKeyBackend(f)
	cases := []struct {
		name   string
		target action.Target
	}{
		{"glob", keyTarget("*")},
		{"glob prefix", keyTarget("web-*")},
		{"class", keyTarget("web[0-9]")},
		{"comma list", keyTarget("web-01,web-02")},
		{"space", keyTarget("web 01")},
		{"empty", keyTarget("")},
		{"minion row", action.Target{Ref: minionRes("web-01").Ref}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := action.Request{SpecID: SpecKeyAccept, Targets: []action.Target{tc.target}}
			if plan, err := b.Plan(context.Background(), req); !errs.Is(err, errs.Config) {
				t.Fatalf("Plan = %+v, %v; want a config error", plan, err)
			}
		})
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("%d commands were run for rejected plans, want 0", len(calls))
	}
}

// TestKeyActionRunsAndForgetsAcceptedKeys: a key step passes -y, reports what
// salt-key printed, and drops the accepted-key cache so the next read sees the
// new key.
func TestKeyActionRunsAndForgetsKeys(t *testing.T) {
	var (
		lists  int
		accept string
	)
	f := transporttest.New().OnFunc(func(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		switch {
		case strings.Contains(cmd.Script, "salt-key") && strings.Contains(cmd.Script, "-a "):
			accept = cmd.Script
			return transporttest.Reply{Stdout: `{"minions": ["new-01"]}` + "\n"}, true
		case strings.Contains(cmd.Script, "salt-key") && strings.Contains(cmd.Script, "-L"):
			lists++
			if lists == 1 {
				return transporttest.Reply{Stdout: `{"minions": ["web-01"]}`}, true
			}
			return transporttest.Reply{Stdout: `{"minions": ["web-01", "new-01"]}`}, true
		case strings.Contains(cmd.Script, "-f "):
			return transporttest.Reply{Stdout: keyFPJSON}, true
		}
		return transporttest.Reply{}, false
	})
	b := newKeyBackend(f)
	ctx := context.Background()

	accepted, err := b.acceptedKeys(ctx)
	if err != nil {
		t.Fatalf("acceptedKeys: %v", err)
	}
	if !accepted["web-01"] || accepted["new-01"] {
		t.Fatalf("accepted = %v, want only web-01 before the accept", accepted)
	}

	plan, err := b.Plan(ctx, action.Request{SpecID: SpecKeyAccept, Targets: []action.Target{keyTarget("new-01")}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	res := plan.Steps[0].Run(ctx)
	if res.RC != 0 {
		t.Fatalf("step RC = %d (%v), want 0", res.RC, res.Err)
	}
	if !strings.Contains(res.Output, "new-01") {
		t.Errorf("output = %q, want what salt-key printed", res.Output)
	}
	if res.Target.Host != resource.HostID(master) || res.Target.Ref.Name != "new-01" {
		t.Errorf("result target = %+v, want the master and the key", res.Target)
	}
	for _, want := range []string{"salt-key", "-a new-01", "-y"} {
		if !strings.Contains(accept, want) {
			t.Errorf("salt-key line %q is missing %q", accept, want)
		}
	}

	accepted, err = b.acceptedKeys(ctx)
	if err != nil {
		t.Fatalf("acceptedKeys after the accept: %v", err)
	}
	if !accepted["new-01"] {
		t.Errorf("accepted = %v; the accepted-key cache was not invalidated", accepted)
	}
	if lists != 2 {
		t.Errorf("salt-key -L ran %d times, want 2 (one before, one after)", lists)
	}
}
