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
