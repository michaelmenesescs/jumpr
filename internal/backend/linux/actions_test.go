package linux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/bus"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
)

// unitRow builds a unit resource the way List does.
func unitRow(host, unit, active string) resource.Resource {
	return resource.Resource{
		Ref:    resource.Ref{Backend: backendName, Kind: KindUnit, Host: resource.HostID(host), Name: unit},
		Fields: map[string]any{"host": host, "unit": unit, "active": active},
	}
}

// processRow builds a process resource the way List does.
func processRow(host string, pid int, command string) resource.Resource {
	return resource.Resource{
		Ref: resource.Ref{Backend: backendName, Kind: KindProcess, Host: resource.HostID(host), Name: strconv.Itoa(pid)},
		Fields: map[string]any{
			"host": host, "pid": int64(pid), "command": command,
		},
	}
}

// hostRow builds a host resource the way List does.
func hostRow(host string) resource.Resource {
	return resource.Resource{
		Ref:    resource.Ref{Backend: backendName, Kind: KindHost, Host: resource.HostID(host), Name: host},
		Fields: map[string]any{"host": host},
	}
}

// specIDs lists the IDs of specs.
func specIDs(specs []action.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.ID)
	}
	return out
}

// hasSpec reports whether specs contains id.
func hasSpec(specs []action.Spec, id string) bool {
	for _, s := range specs {
		if s.ID == id {
			return true
		}
	}
	return false
}

func TestActionsForUnitSelection(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))

	allActive := b.Actions([]resource.Resource{
		unitRow("web-01", "nginx.service", "active"),
		unitRow("web-01", "haproxy.service", "active"),
	})
	if hasSpec(allActive, "linux.systemctl.start") {
		t.Errorf("start offered for an all-active selection: %v", specIDs(allActive))
	}
	for _, id := range []string{"linux.systemctl.stop", "linux.systemctl.restart", "linux.systemctl.reload"} {
		if !hasSpec(allActive, id) {
			t.Errorf("%s missing for an all-active selection: %v", id, specIDs(allActive))
		}
	}

	allInactive := b.Actions([]resource.Resource{
		unitRow("web-01", "nginx.service", "failed"),
		unitRow("web-01", "haproxy.service", "inactive"),
	})
	if !hasSpec(allInactive, "linux.systemctl.start") {
		t.Errorf("start missing for an all-inactive selection: %v", specIDs(allInactive))
	}
	for _, id := range []string{"linux.systemctl.stop", "linux.systemctl.restart", "linux.systemctl.reload"} {
		if hasSpec(allInactive, id) {
			t.Errorf("%s offered for an all-inactive selection: %v", id, specIDs(allInactive))
		}
	}
	if !hasSpec(allInactive, "linux.systemctl.disable") || !hasSpec(allInactive, "linux.systemctl.mask") {
		t.Errorf("enable/disable/mask verbs missing: %v", specIDs(allInactive))
	}

	// A mixed selection has to offer everything: the user may want to start
	// the failed one and restart the active one.
	mixed := b.Actions([]resource.Resource{
		unitRow("web-01", "nginx.service", "active"),
		unitRow("web-01", "haproxy.service", "failed"),
	})
	for _, id := range []string{
		"linux.systemctl.start", "linux.systemctl.stop", "linux.systemctl.restart",
		"linux.systemctl.reload", "linux.systemctl.enable", "linux.systemctl.disable",
		"linux.systemctl.mask", "linux.systemctl.unmask",
	} {
		if !hasSpec(mixed, id) {
			t.Errorf("%s missing for a mixed selection: %v", id, specIDs(mixed))
		}
	}

	// Mixed kinds offer nothing.
	if got := b.Actions([]resource.Resource{
		unitRow("web-01", "nginx.service", "active"),
		hostRow("web-01"),
	}); got != nil {
		t.Errorf("mixed kinds offered %v, want nothing", specIDs(got))
	}
	if got := b.Actions(nil); got != nil {
		t.Errorf("empty selection offered %v, want nothing", specIDs(got))
	}
}

func TestActionsForProcessAndHostSelection(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))

	procs := b.Actions([]resource.Resource{processRow("web-01", 2231, "nginx: worker")})
	if len(procs) != 1 || procs[0].ID != specKill {
		t.Fatalf("process selection offered %v, want only %s", specIDs(procs), specKill)
	}
	if procs[0].Risk != action.RiskDestructive {
		t.Errorf("kill risk = %v, want destructive", procs[0].Risk)
	}
	if len(procs[0].Params) != 1 || procs[0].Params[0].Name != "signal" {
		t.Fatalf("kill params = %+v, want one signal param", procs[0].Params)
	}
	if procs[0].Params[0].Default != "TERM" {
		t.Errorf("kill signal default = %q, want TERM", procs[0].Params[0].Default)
	}
	want := "TERM KILL HUP INT USR1 USR2"
	if got := strings.Join(procs[0].Params[0].Choices, " "); got != want {
		t.Errorf("kill signal choices = %q, want %q", got, want)
	}

	hosts := b.Actions([]resource.Resource{hostRow("web-01"), hostRow("web-02")})
	if len(hosts) != 2 || hosts[0].ID != specDaemonReload || hosts[1].ID != specVacuum {
		t.Fatalf("host selection offered %v, want daemon-reload and vacuum", specIDs(hosts))
	}
	if hosts[1].Risk != action.RiskDestructive {
		t.Errorf("vacuum risk = %v, want destructive", hosts[1].Risk)
	}
	if len(hosts[1].Params) != 1 || hosts[1].Params[0].Default != "1G" {
		t.Errorf("vacuum params = %+v, want one size param defaulting to 1G", hosts[1].Params)
	}
}

func TestActionsOwnsLinuxPrefix(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	for _, id := range []string{"linux.systemctl.restart", "linux.kill", "linux.journal.vacuum"} {
		if !b.Owns(id) {
			t.Errorf("Owns(%q) = false, want true", id)
		}
	}
	if b.Owns("salt.state.apply") {
		t.Error("Owns(salt.state.apply) = true, want false")
	}
}

// restartPlanner is the fake executor of a two-host restart: the action returns
// 0 and the follow-up is-active says "active".
func restartPlanner(t *testing.T) *transporttest.Fake {
	t.Helper()
	f := transporttest.New()
	f.On("systemctl restart nginx.service", transporttest.Reply{})
	f.On("systemctl is-active", transporttest.Reply{Stdout: "active\n"})
	return f
}

func TestPlanRestartDisplay(t *testing.T) {
	ex := restartPlanner(t)
	b := newTestBackend(t, ex, inv("web-01", "web-02"))

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID: specSystemctl + "restart",
		Targets: []action.Target{
			{Host: "web-01", Ref: resource.Ref{Kind: KindUnit, Host: "web-01", Name: "nginx.service"}},
			{Host: "web-02", Ref: resource.Ref{Kind: KindUnit, Host: "web-02", Name: "nginx.service"}},
		},
		Actor: action.Human,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Title != "restart nginx.service on 2 hosts" {
		t.Errorf("Title = %q, want %q", plan.Title, "restart nginx.service on 2 hosts")
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(plan.Steps))
	}
	for i, s := range plan.Steps {
		if s.Display != "sudo -n systemctl restart nginx.service" {
			t.Errorf("step %d Display = %q, want %q", i, s.Display, "sudo -n systemctl restart nginx.service")
		}
	}
	if plan.Via != "fake" {
		t.Errorf("Via = %q, want fake", plan.Via)
	}

	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("step Run: %v", res.Err)
	}
	if res.RC != 0 {
		t.Errorf("step RC = %d, want 0", res.RC)
	}
	if !strings.Contains(res.Output, "active") {
		t.Errorf("step Output = %q, want the is-active line", res.Output)
	}

	// The script is the unwrapped form and runs under sudo.
	var sawAction bool
	for _, c := range ex.Calls() {
		if strings.Contains(c.Command.Script, "systemctl restart nginx.service") {
			sawAction = true
			if !c.Command.Sudo {
				t.Error("restart ran without Sudo")
			}
			if c.Command.Script != "systemctl restart nginx.service" {
				t.Errorf("script = %q, want the shq.Join form", c.Command.Script)
			}
			if c.Command.Timeout != 60*time.Second {
				t.Errorf("timeout = %v, want 60s", c.Command.Timeout)
			}
		}
	}
	if !sawAction {
		t.Error("the restart command never reached the executor")
	}
}

func TestPlanRejectsBadUnitName(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	_, err := b.Plan(context.Background(), action.Request{
		SpecID: specSystemctl + "restart",
		Targets: []action.Target{
			{Host: "web-01", Ref: resource.Ref{Kind: KindUnit, Host: "web-01", Name: "nginx; rm -rf /"}},
		},
	})
	if err == nil {
		t.Fatal("Plan accepted a unit name with shell metacharacters")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Errorf("kind = %s, want %s", kind, errs.Config)
	}
}

func TestPlanPreviewShowsStates(t *testing.T) {
	f := transporttest.New()
	f.OnHost("web-01", "systemctl is-active nginx.service", transporttest.Reply{Stdout: "== nginx.service\nfailed\n5\n", RC: 3})
	f.OnHost("web-02", "systemctl is-active nginx.service", transporttest.Reply{Stdout: "== nginx.service\nactive\n0\n"})
	b := newTestBackend(t, f, inv("web-01", "web-02"))

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID: specSystemctl + "restart",
		Targets: []action.Target{
			{Host: "web-01", Ref: resource.Ref{Kind: KindUnit, Host: "web-01", Name: "nginx.service"}},
			{Host: "web-02", Ref: resource.Ref{Kind: KindUnit, Host: "web-02", Name: "nginx.service"}},
		},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.Preview, "web-01: failed (restarts 5)") {
		t.Errorf("Preview = %q, want a web-01 line with the restart count", plan.Preview)
	}
	if !strings.Contains(plan.Preview, "web-02: active (restarts 0)") {
		t.Errorf("Preview = %q, want a web-02 line", plan.Preview)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", plan.Warnings)
	}
	// The preview must never send a write.
	for _, c := range f.Calls() {
		if strings.Contains(c.Command.Script, "restart") && !strings.Contains(c.Command.Script, "is-active") {
			t.Errorf("preview sent a write: %q", c.Command.Script)
		}
		if c.Command.Sudo {
			t.Errorf("preview ran with sudo: %q", c.Command.Script)
		}
	}
}

func TestPlanPreviewGroupedCounts(t *testing.T) {
	f := transporttest.New()
	f.OnFunc(func(host resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if !strings.Contains(cmd.Script, "is-active") {
			return transporttest.Reply{}, false
		}
		if strings.Contains(string(host), "web-20") {
			return transporttest.Reply{Stdout: "== nginx.service\nfailed\n4\n"}, true
		}
		return transporttest.Reply{Stdout: "== nginx.service\nactive\n0\n"}, true
	})
	b := newTestBackend(t, f, inv("web-01"))

	targets := make([]action.Target, 0, 21)
	for i := 0; i < 21; i++ {
		host := resource.HostID(fmt.Sprintf("web-%02d", i))
		targets = append(targets, action.Target{Host: host, Ref: resource.Ref{Kind: KindUnit, Host: host, Name: "nginx.service"}})
	}
	plan, err := b.Plan(context.Background(), action.Request{SpecID: specSystemctl + "restart", Targets: targets})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.Preview, "active") || !strings.Contains(plan.Preview, "failed") {
		t.Fatalf("Preview = %q, want grouped counts", plan.Preview)
	}
	if strings.Contains(plan.Preview, "\n") {
		t.Errorf("Preview = %q, want one grouped line for more than 20 targets", plan.Preview)
	}
	if !strings.Contains(plan.Preview, previewSep) {
		t.Errorf("Preview = %q, want counts separated by %q", plan.Preview, previewSep)
	}
}

func TestPlanPreviewFailureWarns(t *testing.T) {
	f := transporttest.New()
	f.On("systemctl", transporttest.Reply{Err: errs.New(errs.Unreachable, "fake.exec", "no route")})
	b := newTestBackend(t, f, inv("web-01"))

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  specSystemctl + "restart",
		Targets: []action.Target{{Host: "web-01", Ref: resource.Ref{Kind: KindUnit, Host: "web-01", Name: "nginx.service"}}},
	})
	if err != nil {
		t.Fatalf("a preview failure must not fail the plan: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(plan.Steps))
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "preview failed on web-01") {
		t.Errorf("Warnings = %v, want one preview warning", plan.Warnings)
	}
}

func TestPlanStepFailsWhenUnitNotActive(t *testing.T) {
	f := transporttest.New()
	f.On("systemctl restart haproxy.service", transporttest.Reply{})
	f.On("systemctl is-active", transporttest.Reply{Stdout: "failed\n", RC: 3})
	b := newTestBackend(t, f, inv("web-01"))

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  specSystemctl + "restart",
		Targets: []action.Target{{Host: "web-01", Ref: resource.Ref{Kind: KindUnit, Host: "web-01", Name: "haproxy.service"}}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	start := time.Now()
	res := plan.Steps[0].Run(context.Background())
	if elapsed := time.Since(start); elapsed < settleDelay {
		t.Errorf("step returned after %v, want at least the %v settle delay", elapsed, settleDelay)
	}
	if res.RC != 3 {
		t.Errorf("RC = %d, want 3", res.RC)
	}
	if !strings.Contains(res.Output, "unit is failed after restart") {
		t.Errorf("Output = %q, want %q", res.Output, "unit is failed after restart")
	}
	if res.Err == nil {
		t.Error("Err = nil, want a failure")
	}
}

func TestKillPlan(t *testing.T) {
	f := transporttest.New()
	f.On("kill -TERM 2231", transporttest.Reply{})
	b := newTestBackend(t, f, inv("web-01"))

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  specKill,
		Targets: []action.Target{{Host: "web-01", Ref: resource.Ref{Kind: KindProcess, Host: "web-01", Name: "2231"}}},
		Params:  map[string]string{"signal": "TERM"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(plan.Steps))
	}
	if plan.Steps[0].Display != "sudo -n kill -TERM 2231" {
		t.Errorf("Display = %q, want %q", plan.Steps[0].Display, "sudo -n kill -TERM 2231")
	}
	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil || res.RC != 0 {
		t.Errorf("Run = %+v, want success", res)
	}

	// The signal defaults to TERM and a free-text PID is refused.
	def, err := b.Plan(context.Background(), action.Request{
		SpecID:  specKill,
		Targets: []action.Target{{Host: "web-01", Ref: resource.Ref{Kind: KindProcess, Host: "web-01", Name: "2231"}}},
	})
	if err != nil {
		t.Fatalf("Plan with default signal: %v", err)
	}
	if def.Steps[0].Display != "sudo -n kill -TERM 2231" {
		t.Errorf("Display = %q, want the TERM default", def.Steps[0].Display)
	}
	if _, err := b.Plan(context.Background(), action.Request{
		SpecID:  specKill,
		Targets: []action.Target{{Host: "web-01", Ref: resource.Ref{Kind: KindProcess, Host: "web-01", Name: "2231; rm -rf /"}}},
	}); errs.KindOf(err) != errs.Config {
		t.Errorf("bad pid: kind = %s, want %s", errs.KindOf(err), errs.Config)
	}
	if _, err := b.Plan(context.Background(), action.Request{
		SpecID:  specKill,
		Targets: []action.Target{{Host: "web-01", Ref: resource.Ref{Kind: KindProcess, Host: "web-01", Name: "2231"}}},
		Params:  map[string]string{"signal": "SIGSEGV"},
	}); errs.KindOf(err) != errs.Config {
		t.Errorf("bad signal: kind = %s, want %s", errs.KindOf(err), errs.Config)
	}
}

func TestPlanDaemonReloadAndVacuum(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01", "web-02"))

	reload, err := b.Plan(context.Background(), action.Request{
		SpecID:  specDaemonReload,
		Targets: []action.Target{{Host: "web-01"}, {Host: "web-02"}},
	})
	if err != nil {
		t.Fatalf("daemon-reload Plan: %v", err)
	}
	if reload.Steps[0].Display != "sudo -n systemctl daemon-reload" {
		t.Errorf("daemon-reload Display = %q", reload.Steps[0].Display)
	}
	if reload.Preview != "" {
		t.Errorf("daemon-reload Preview = %q, want none", reload.Preview)
	}
	if reload.Title != "daemon-reload on 2 hosts" {
		t.Errorf("daemon-reload Title = %q", reload.Title)
	}

	vacuum, err := b.Plan(context.Background(), action.Request{
		SpecID:  specVacuum,
		Targets: []action.Target{{Host: "web-01"}},
	})
	if err != nil {
		t.Fatalf("vacuum Plan: %v", err)
	}
	if vacuum.Steps[0].Display != "sudo -n journalctl --vacuum-size=1G" {
		t.Errorf("vacuum Display = %q, want the 1G default", vacuum.Steps[0].Display)
	}
	if _, err := b.Plan(context.Background(), action.Request{
		SpecID:  specVacuum,
		Targets: []action.Target{{Host: "web-01"}},
		Params:  map[string]string{"size": "1G; reboot"},
	}); errs.KindOf(err) != errs.Config {
		t.Errorf("bad vacuum size: kind = %s, want %s", errs.KindOf(err), errs.Config)
	}
}

// TestStopSSHDDeniedByDefaultPolicy runs the built-in policy against the plan
// this backend produces. The write must never be offered as safe.
func TestPlanStopSSHDDeniedByDefaultPolicy(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  specSystemctl + "stop",
		Targets: []action.Target{{Host: "web-01", Ref: resource.Ref{Kind: KindUnit, Host: "web-01", Name: "sshd.service"}}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := plan.Steps[0].Display; got != "sudo -n systemctl stop sshd.service" {
		t.Fatalf("Display = %q, want the form the policy patterns match", got)
	}

	denial := bus.DefaultPolicy().Check(plan, false)
	if denial == nil {
		t.Fatal("the default policy allowed stopping sshd")
	}
	if !strings.Contains(denial.Reason, "SSH") {
		t.Errorf("Reason = %q, want the SSH rule's reason", denial.Reason)
	}

	// restarting nginx is still allowed.
	ok, err := b.Plan(context.Background(), action.Request{
		SpecID:  specSystemctl + "restart",
		Targets: []action.Target{{Host: "web-01", Ref: resource.Ref{Kind: KindUnit, Host: "web-01", Name: "nginx.service"}}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if denial := bus.DefaultPolicy().Check(ok, false); denial != nil {
		t.Errorf("the default policy denied restarting nginx: %+v", denial)
	}
}
