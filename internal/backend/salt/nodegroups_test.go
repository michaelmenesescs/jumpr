package salt

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jumpr/internal/errs"
	"jumpr/internal/transport/transporttest"
)

func TestNodegroupsParse(t *testing.T) {
	out := []byte(`{"webs": "G@role:web", "k8s": ["k8s-01", "k8s-02"], "odd": {"nested": 1}}`)
	groups, err := ParseNodegroups(out)
	if err != nil {
		t.Fatalf("ParseNodegroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 (the nested value is skipped): %+v", len(groups), groups)
	}
	if groups[0].Name != "k8s" || groups[1].Name != "webs" {
		t.Fatalf("names = %q, %q, want k8s, webs", groups[0].Name, groups[1].Name)
	}
	if groups[0].Expr != "k8s-01, k8s-02" {
		t.Fatalf("list expr = %q", groups[0].Expr)
	}
	if got := groups[0].Members; len(got) != 2 || got[0] != "k8s-01" {
		t.Fatalf("list members = %v, want the ids", got)
	}
	if groups[1].Expr != "G@role:web" || groups[1].Members != nil {
		t.Fatalf("string group = %+v, want the expression and no members yet", groups[1])
	}
}

func TestNodegroupsParseEmpty(t *testing.T) {
	for _, out := range [][]byte{nil, []byte("  \n"), []byte(`{}`)} {
		groups, err := ParseNodegroups(out)
		if err != nil {
			t.Fatalf("ParseNodegroups(%q): %v", out, err)
		}
		if len(groups) != 0 {
			t.Fatalf("ParseNodegroups(%q) = %v, want none", out, groups)
		}
	}
	if _, err := ParseNodegroups([]byte(`not json`)); !errs.Is(err, errs.Parse) {
		t.Fatalf("garbage = %v, want a parse error", err)
	}
}

func TestNodegroupTargetCompile(t *testing.T) {
	sel := mustSelector(t, "@salt:webs")
	target, ok := NodegroupTarget(sel)
	if !ok {
		t.Fatalf("NodegroupTarget(@salt:webs) = false")
	}
	if target.Type != "nodegroup" || target.Expr != "webs" {
		t.Fatalf("target = %+v, want the nodegroup webs", target)
	}
	if got := target.String(); got != "-N webs" {
		t.Fatalf("String = %q, want -N webs", got)
	}
	compiled := Compile(sel, []string{"web-01", "web-02", "web-03"}, nil, 200)
	if !compiled.Native || len(compiled.Targets) != 1 || compiled.Targets[0] != target {
		t.Fatalf("Compile = %+v, want the single -N target", compiled)
	}
	if !strings.Contains(compiled.Explain, "-N webs") {
		t.Fatalf("explain = %q, want the nodegroup", compiled.Explain)
	}

	// Anything else stays on the ordinary path: a set plus a term, another
	// namespace, and an invalid name.
	for _, text := range []string{"@salt:webs role=web", "@canary", "@salt:bad name"} {
		if _, ok := NodegroupTarget(mustSelector(t, text)); ok {
			t.Fatalf("NodegroupTarget(%q) = true, want false", text)
		}
	}
}

func TestNodegroupsList(t *testing.T) {
	ex := transporttest.New()
	ex.On("config.get nodegroups", transporttest.Reply{
		Stdout: `{"webs": "G@role:web"}`,
	})
	ex.On("--preview-target -N webs", transporttest.Reply{
		Stdout: `["web-01","web-02","web-03"]`,
	})
	b := New(Options{Runner: NewRunner(ex, master, RunnerOptions{}), Master: master})

	groups, err := b.Nodegroups(context.Background())
	if err != nil {
		t.Fatalf("Nodegroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want one", groups)
	}
	if got := groups[0].Members; len(got) != 3 || got[2] != "web-03" {
		t.Fatalf("members = %v, want the three web hosts", got)
	}
	if groups[0].Err != nil {
		t.Fatalf("Err = %v", groups[0].Err)
	}
	if rows := NodegroupRows(groups); rows[0][1] != "3" {
		t.Fatalf("row = %v, want member count 3", rows[0])
	}

	calls := ex.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if !strings.Contains(calls[1].Command.Script, "--preview-target") {
		t.Fatalf("preview call = %q, want --preview-target", calls[1].Command.Script)
	}
}

func TestNodegroupsPreviewFailure(t *testing.T) {
	ex := transporttest.New()
	ex.On("config.get nodegroups", transporttest.Reply{Stdout: `{"broken": "G@role:nope"}`})
	ex.On("--preview-target", transporttest.Reply{Stderr: "the master is not answering", RC: 1})
	b := New(Options{Runner: NewRunner(ex, master, RunnerOptions{}), Master: master})

	groups, err := b.Nodegroups(context.Background())
	if err != nil {
		t.Fatalf("Nodegroups: %v", err)
	}
	if len(groups) != 1 || groups[0].Err == nil {
		t.Fatalf("groups = %+v, want one group with its error kept", groups)
	}
	if rows := NodegroupRows(groups); rows[0][1] != "?" {
		t.Fatalf("row = %v, want an unknown member count", rows[0])
	}
}

func TestNodegroupMinionsRejectsBadName(t *testing.T) {
	b := New(Options{Runner: NewRunner(transporttest.New(), master, RunnerOptions{}), Master: master})
	if _, err := b.NodegroupMinions(context.Background(), "@salt:../etc"); !errs.Is(err, errs.Config) {
		t.Fatalf("bad name = %v, want a config error", err)
	}
}

func TestBeaconsParse(t *testing.T) {
	byMinion := map[string]json.RawMessage{
		"web-01": []byte(`{"load":{"enabled":true,"interval":60},"disk":{"enabled":false,"interval":120}}`),
		"web-02": []byte(`"no return"`),
	}
	rows := ParseBeacons(byMinion)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two web-01 beacons", rows)
	}
	if rows[0].Minion != "web-01" || rows[0].Name != "disk" || rows[0].Enabled {
		t.Fatalf("row 0 = %+v, want disk disabled", rows[0])
	}
	if rows[1].Name != "load" || !rows[1].Enabled {
		t.Fatalf("row 1 = %+v, want load enabled by default", rows[1])
	}
	if !strings.Contains(rows[1].Summary, "interval=60") {
		t.Fatalf("summary = %q, want the interval", rows[1].Summary)
	}
	table := BeaconRows(rows)
	if table[1][2] != "yes" {
		t.Fatalf("enabled cell = %q, want yes", table[1][2])
	}
}

func TestSchedulesParse(t *testing.T) {
	byMinion := map[string]json.RawMessage{
		"web-01": []byte(`{"highstate":{"function":"state.highstate","minutes":15},"once":{"function":"cmd.run","when":"2026-12-01T00:00:00","enabled":false}}`),
	}
	rows := ParseSchedules(byMinion)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want two schedules", rows)
	}
	if rows[0].Name != "highstate" || !rows[0].Enabled {
		t.Fatalf("row 0 = %+v", rows[0])
	}
	if !strings.Contains(rows[0].Next, "state.highstate") || !strings.Contains(rows[0].Next, "every 15 minutes") {
		t.Fatalf("next = %q, want the function and interval", rows[0].Next)
	}
	if rows[1].Enabled {
		t.Fatalf("row 1 = %+v, want the disabled schedule", rows[1])
	}
	if table := ScheduleRows(rows); table[1][2] != "no" {
		t.Fatalf("enabled cell = %q, want no", table[1][2])
	}
}

func TestBeaconsAndSchedulesList(t *testing.T) {
	ex := transporttest.New()
	ex.On("beacons.list", transporttest.Reply{
		Stdout: `{"web-01":{"load":{"enabled":true,"interval":60}},"web-02":{"load":{"interval":60}}}`,
	})
	ex.On("schedule.list", transporttest.Reply{
		Stdout: `{"web-01":{"highstate":{"function":"state.highstate","seconds":600}}}`,
	})
	b := New(Options{Runner: NewRunner(ex, master, RunnerOptions{}), Master: master})
	ctx := context.Background()

	beacons, err := b.Beacons(ctx, []string{"web-02", "web-01"})
	if err != nil {
		t.Fatalf("Beacons: %v", err)
	}
	if len(beacons) != 2 || beacons[0].Minion != "web-01" || beacons[1].Minion != "web-02" {
		t.Fatalf("beacons = %+v, want one per minion in minion order", beacons)
	}
	schedules, err := b.Schedules(ctx, []string{"web-01"})
	if err != nil {
		t.Fatalf("Schedules: %v", err)
	}
	if len(schedules) != 1 || !strings.Contains(schedules[0].Next, "every 600 seconds") {
		t.Fatalf("schedules = %+v", schedules)
	}

	for _, call := range ex.Calls() {
		if !strings.Contains(call.Command.Script, "return_yaml=False") {
			t.Fatalf("script = %q, want return_yaml=False", call.Command.Script)
		}
	}
}

func TestBeaconsListNeedsMinions(t *testing.T) {
	b := New(Options{Runner: NewRunner(transporttest.New(), master, RunnerOptions{}), Master: master})
	if _, err := b.Beacons(context.Background(), nil); !errs.Is(err, errs.Config) {
		t.Fatalf("no minions = %v, want a config error", err)
	}
	if _, err := New(Options{}).Schedules(context.Background(), []string{"web-01"}); !errs.Is(err, errs.Config) {
		t.Fatalf("unconfigured = %v, want a config error", err)
	}
}
