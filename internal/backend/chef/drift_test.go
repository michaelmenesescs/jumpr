package chef

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
)

// driftFixture answers the second read of a drift check: the run-status
// attribute of every node. The status fixture (task 113) is the first one:
// web-01 checked in 5 m ago, web-02 3 h, web-03 has no ohai_time, db-01 30 h.
func driftFixture(t *testing.T) *transporttest.Fake {
	t.Helper()
	body := `{"total":4,"start":0,"rows":[
		{"web-01":{"run_status":{"success":true,"failed":false,"elapsed":1.25}}},
		{"web-02":{"run_status":{"success":true,"failed":false,"elapsed":0.75}}},
		{"db-01":{"run_status":{"success":false,"failed":true,"failed_resource":"execute[check-replica]"}}}
	]}`
	fake := fixtureFake(t)
	fake.OnFunc(func(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if strings.Contains(cmd.Script, "'*:*'") {
			return transporttest.Reply{Stdout: body}, true
		}
		return transporttest.Reply{}, false
	})
	return fake
}

// TestDriftRowsAndRunListGroups checks requirement 1: a row per requested host
// with the check-in age and its status, and the run lists grouped.
func TestDriftRowsAndRunListGroups(t *testing.T) {
	b := newTestBackend(t, driftFixture(t))
	hosts := []resource.HostID{
		"web-01.lab.example.internal", "web-02", "db-01", "ghost-01",
	}
	rep, err := b.Drift(context.Background(), hosts, "")
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if len(rep.Rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rep.Rows))
	}
	if rep.Attr != "" {
		t.Errorf("Attr = %q, want hidden by default", rep.Attr)
	}

	web := rep.Rows[0]
	if web.Host != "web-01.lab.example.internal" || web.Node != "web-01" || !web.Known {
		t.Errorf("web-01 row = %+v, want the FQDN host matched to the short node name", web)
	}
	if !web.HasCheckin || web.Checkin != 5*time.Minute {
		t.Errorf("web-01 check-in = %v (have %v), want 5m", web.Checkin, web.HasCheckin)
	}
	if web.Status != resource.StatusOK {
		t.Errorf("web-01 status = %v, want ok", web.Status)
	}
	if got := strings.Join(web.RunList, ","); got != "role[base],role[web]" {
		t.Errorf("web-01 run list = %q", got)
	}
	if got := rep.Rows[1]; got.Status != resource.StatusWarn || got.Checkin.Round(time.Minute) != 3*time.Hour {
		t.Errorf("web-02 row = %+v, want warn at 3h", got)
	}
	if got := rep.Rows[2]; got.Status != resource.StatusCrit || got.Checkin != 30*time.Hour {
		t.Errorf("db-01 row = %+v, want crit at 30h", got)
	}
	ghost := rep.Rows[3]
	if ghost.Known || ghost.Status != resource.StatusNA || ghost.Node != "" {
		t.Errorf("ghost-01 row = %+v, want an unknown node reported as NA", ghost)
	}
	if rep.Unknown() != 1 {
		t.Errorf("Unknown() = %d, want 1", rep.Unknown())
	}

	if len(rep.Groups) != 2 {
		t.Fatalf("groups = %+v, want 2", rep.Groups)
	}
	if got := strings.Join(rep.Groups[0].Nodes, ","); got != "web-01,web-02" {
		t.Errorf("largest group = %q, want the two web nodes", got)
	}
	if got := strings.Join(rep.Groups[1].Nodes, ","); got != "db-01" {
		t.Errorf("second group = %q, want db-01", got)
	}
	if got := rep.Headline(); got != "2 identical · 1 differ" {
		t.Errorf("Headline() = %q", got)
	}
}

// TestDriftRunStatusColumn checks requirement 1's configurable attribute path:
// empty hides the column, a path fills it from the nested attribute.
func TestDriftRunStatusColumn(t *testing.T) {
	b := newTestBackend(t, driftFixture(t))
	hosts := []resource.HostID{"web-01", "db-01"}

	rep, err := b.Drift(context.Background(), hosts, "")
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	for _, row := range rep.Rows {
		if row.RunStatus != "" {
			t.Errorf("%s run status = %q, want the column hidden", row.Host, row.RunStatus)
		}
	}

	rep, err = b.Drift(context.Background(), hosts, "run_status.failed")
	if err != nil {
		t.Fatalf("Drift with attribute: %v", err)
	}
	if rep.Attr != "run_status.failed" {
		t.Errorf("Attr = %q", rep.Attr)
	}
	if got := rep.Rows[0].RunStatus; got != "false" {
		t.Errorf("web-01 run status = %q, want false", got)
	}
	if got := rep.Rows[1].RunStatus; got != "true" {
		t.Errorf("db-01 run status = %q, want true (the lab's failing run)", got)
	}
}

// TestDriftRejectsBadAttribute checks the attribute path never reaches a
// command line unchecked.
func TestDriftRejectsBadAttribute(t *testing.T) {
	fake := driftFixture(t)
	b := newTestBackend(t, fake)
	_, err := b.Drift(context.Background(), []resource.HostID{"web-01"}, "run_status; rm -rf /")
	if errs.KindOf(err) != errs.Config {
		t.Fatalf("kind = %q, want config (%v)", errs.KindOf(err), err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %d, want none", len(fake.Calls()))
	}
}

// TestDriftNeedsHosts checks an empty selection is a config error, not an empty
// report.
func TestDriftNeedsHosts(t *testing.T) {
	b := newTestBackend(t, driftFixture(t))
	if _, err := b.Drift(context.Background(), nil, ""); errs.KindOf(err) != errs.Config {
		t.Errorf("kind = %q, want config", errs.KindOf(err))
	}
}

// TestDriftParseRunStatusTolerance checks the parser follows a nested path,
// formats non-string scalars and skips values it cannot read.
func TestDriftParseRunStatusTolerance(t *testing.T) {
	body := `{"rows":[
		{"web-01":{"run_status":{"success":true}}},
		{"web-02":{"run_status":{"success":"yes"}}},
		{"web-03":42},
		{"broken":null},
		{"db-01":"direct"}
	]}`
	got, err := ParseRunStatus([]byte(body), "run_status.success")
	if err != nil {
		t.Fatalf("ParseRunStatus: %v", err)
	}
	if got["web-01"] != "true" || got["web-02"] != "yes" {
		t.Errorf("values = %v, want the nested booleans as strings", got)
	}
	if got["db-01"] != "direct" {
		t.Errorf("db-01 = %q, want the leaf knife returned", got["db-01"])
	}
	if got["web-03"] != "42" {
		t.Errorf("web-03 = %q, want the numeric leaf formatted", got["web-03"])
	}
	if _, ok := got["broken"]; ok {
		t.Errorf("a null value parsed: %v", got)
	}
}
