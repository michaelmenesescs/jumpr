package salt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jumper/internal/errs"
	"jumper/internal/group"
	"jumper/internal/resource"
)

// stateSample is a dry-run return with three states in an order that is not
// RunNum order: ok, would-change (null + diff), failed.
const stateSample = `{
  "file_|-motd_|-/etc/motd_|-managed": {
    "result": false,
    "changes": {},
    "comment": "Unable to manage file: no such directory\nsecond line\nthird line\nfourth line",
    "name": "/etc/motd",
    "__sls__": "base.motd",
    "__run_num__": 3,
    "__id__": "motd",
    "duration": 4.5,
    "start_time": "13:55:06.2"
  },
  "file_|-nginx-config_|-/etc/nginx/nginx.conf_|-managed": {
    "result": true,
    "changes": {},
    "comment": "File is in the correct state",
    "name": "/etc/nginx/nginx.conf",
    "__sls__": "nginx.config",
    "__run_num__": 1,
    "__id__": "nginx-config",
    "duration": 12.3,
    "start_time": "13:55:02.1"
  },
  "service_|-nginx-service_|-nginx_|-running": {
    "result": null,
    "changes": {"diff": "--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new"},
    "comment": "Service nginx is set to start",
    "name": "nginx",
    "__sls__": "nginx.service",
    "__run_num__": 2,
    "__id__": "nginx-service",
    "duration": 30.1,
    "start_time": "13:55:03.4"
  }
}`

func mustParse(t *testing.T, raw string) StateRun {
	t.Helper()
	run, err := ParseStateReturn(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("ParseStateReturn: %v", err)
	}
	return run
}

func TestParseStateDict(t *testing.T) {
	run := mustParse(t, stateSample)
	if len(run.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(run.Rows))
	}
	// Sorted by RunNum.
	wantIDs := []string{"nginx-config", "nginx-service", "motd"}
	for i, want := range wantIDs {
		if run.Rows[i].ID != want {
			t.Errorf("row %d id = %q, want %q", i, run.Rows[i].ID, want)
		}
	}
	ok := run.Rows[0]
	if ok.Module != "file" || ok.Function != "managed" || ok.SLS != "nginx.config" {
		t.Errorf("row 0 = %+v, want module file, function managed, sls nginx.config", ok)
	}
	if ok.Result == nil || !*ok.Result {
		t.Errorf("row 0 result = %v, want true", ok.Result)
	}
	if ok.Changed || ok.RunNum != 1 || ok.Duration != 12.3 {
		t.Errorf("row 0 = %+v, want unchanged, run 1, duration 12.3", ok)
	}
	pending := run.Rows[1]
	if pending.Result != nil {
		t.Errorf("row 1 result = %v, want nil (JSON null)", *pending.Result)
	}
	if !pending.Changed {
		t.Error("row 1 should report changes")
	}
	if pending.Name != "nginx" || pending.Function != "running" {
		t.Errorf("row 1 = %+v", pending)
	}
	failed := run.Rows[2]
	if failed.Result == nil || *failed.Result {
		t.Errorf("row 2 result = %v, want false", failed.Result)
	}
	if failed.SLS != "base.motd" || failed.Name != "/etc/motd" {
		t.Errorf("row 2 = %+v", failed)
	}
}

func TestParseStateDictDefaultNotTest(t *testing.T) {
	if run := mustParse(t, stateSample); run.Test {
		t.Error("Test = true, want false (ParseStateReturn never guesses test mode)")
	}
}

func TestParseStateOddKeyFallsBackToFields(t *testing.T) {
	raw := `{"weird_|-key": {"result": true, "changes": {}, "__id__": "real-id", "name": "real-name",
		"__sls__": "x", "__run_num__": 0, "duration": 1, "start_time": "00:00:01"}}`
	run := mustParse(t, raw)
	if len(run.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(run.Rows))
	}
	r := run.Rows[0]
	if r.ID != "real-id" || r.Name != "real-name" {
		t.Errorf("row = %+v, want id/name from fields", r)
	}
}

func TestParseStateErrorList(t *testing.T) {
	raw := `["Rendering SLS 'nginx' failed: Jinja error", "second error"]`
	run := mustParse(t, raw)
	if len(run.Rows) != 0 {
		t.Errorf("rows = %d, want 0", len(run.Rows))
	}
	if len(run.Errors) != 2 || run.Errors[0] != "Rendering SLS 'nginx' failed: Jinja error" {
		t.Fatalf("errors = %v", run.Errors)
	}
	if got := run.Status(); got != resource.StatusCrit {
		t.Errorf("status = %v, want crit", got)
	}
	if n := run.Normalized(); !strings.HasPrefix(n, "ERROR\n") || !strings.Contains(n, "Jinja error") {
		t.Errorf("normalized = %q", n)
	}
}

func TestParseStateString(t *testing.T) {
	run := mustParse(t, `"ERROR: master unreachable"`)
	if len(run.Rows) != 0 || len(run.Errors) == 0 {
		t.Fatalf("run = %+v, want only errors", run)
	}
	if !strings.Contains(run.Normalized(), "ERROR: master unreachable") {
		t.Errorf("normalized = %q", run.Normalized())
	}
	if got := run.Status(); got != resource.StatusCrit {
		t.Errorf("status = %v, want crit", got)
	}
}

func TestParseStateNoReturn(t *testing.T) {
	for _, raw := range []string{`false`, `"Minion did not return. [No response]"`} {
		run := mustParse(t, raw)
		if len(run.Errors) == 0 {
			t.Errorf("%s: errors empty, want a no-return error", raw)
		}
		if got := run.Status(); got != resource.StatusCrit {
			t.Errorf("%s: status = %v, want crit", raw, got)
		}
	}
}

func TestParseStateInvalid(t *testing.T) {
	for _, raw := range []string{`{"a": `, `42`} {
		if _, err := ParseStateReturn(json.RawMessage(raw)); err == nil {
			t.Errorf("ParseStateReturn(%s) succeeded, want an error", raw)
		} else if !errs.Is(err, errs.Parse) {
			t.Errorf("ParseStateReturn(%s) kind = %q, want parse", raw, errs.KindOf(err))
		}
	}
}

func TestStateCountsTestMode(t *testing.T) {
	run := mustParse(t, stateSample)
	run.Test = true
	ok, changed, failed, pending := run.Counts()
	// nginx-config: true, no changes -> ok. nginx-service: null -> pending.
	// motd: false -> failed. None of the sample rows has true + changes.
	if ok != 1 || changed != 0 || failed != 1 || pending != 1 {
		t.Fatalf("counts = %d/%d/%d/%d, want 1/0/1/1", ok, changed, failed, pending)
	}

	extra := `{"a_|-a_|-a_|-cmd.run": {"result": true, "changes": {"out": "x"}, "__run_num__": 0}}`
	more := mustParse(t, extra)
	more.Test = true
	if ok, changed, failed, pending := more.Counts(); ok != 0 || changed != 1 || failed != 0 || pending != 0 {
		t.Fatalf("counts = %d/%d/%d/%d, want 0/1/0/0", ok, changed, failed, pending)
	}
}

func TestStateStatus(t *testing.T) {
	if got := mustParse(t, stateSample).Status(); got != resource.StatusCrit {
		t.Errorf("failed run status = %v, want crit", got)
	}
	clean := mustParse(t, `{"a_|-a_|-a_|-cmd.run": {"result": true, "changes": {}, "__run_num__": 0}}`)
	if got := clean.Status(); got != resource.StatusOK {
		t.Errorf("clean run status = %v, want ok", got)
	}
	would := mustParse(t, `{"a_|-a_|-a_|-cmd.run": {"result": null, "changes": {}, "__run_num__": 0}}`)
	would.Test = true
	if got := would.Status(); got != resource.StatusWarn {
		t.Errorf("pending run status = %v, want warn", got)
	}
}

func TestStateNormalizedStableAcrossDurations(t *testing.T) {
	a := mustParse(t, stateSample)
	a.Test = true
	bRaw := strings.Replace(stateSample, `"duration": 12.3`, `"duration": 99.9`, 1)
	bRaw = strings.Replace(bRaw, `"duration": 30.1`, `"duration": 0.001`, 1)
	bRaw = strings.Replace(bRaw, `"start_time": "13:55:02.1"`, `"start_time": "23:59:59.9"`, 1)
	bRaw = strings.Replace(bRaw, `"start_time": "13:55:03.4"`, `"start_time": "00:00:00.0"`, 1)
	b := mustParse(t, bRaw)
	b.Test = true
	if a.Normalized() != b.Normalized() {
		t.Fatalf("normalized differs:\n%q\n%q", a.Normalized(), b.Normalized())
	}
	na := a.Normalized()
	for _, forbidden := range []string{"12.3", "99.9", "13:55", "23:59", "duration", "start_time"} {
		if strings.Contains(na, forbidden) {
			t.Errorf("normalized contains %q:\n%s", forbidden, na)
		}
	}
}

func TestStateNormalizedShape(t *testing.T) {
	run := mustParse(t, stateSample)
	run.Test = true
	got := run.Normalized()
	for _, want := range []string{
		"FAILED base.motd :: motd (file.managed) /etc/motd",
		"WOULD-CHANGE nginx.service :: nginx-service (service.running) nginx",
		"  Service nginx is set to start",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("normalized missing %q:\n%s", want, got)
		}
	}
	// A clean row never appears.
	if strings.Contains(got, "nginx-config") {
		t.Errorf("normalized has the ok row:\n%s", got)
	}
	// Comment is capped at three lines.
	if strings.Contains(got, "fourth line") {
		t.Errorf("comment not capped at 3 lines:\n%s", got)
	}
	if !strings.Contains(got, "third line") {
		t.Errorf("comment missing its third line:\n%s", got)
	}
}

func TestStateNormalizedDiffVerbatim(t *testing.T) {
	const diff = "--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new"
	run := mustParse(t, stateSample)
	run.Test = true
	got := run.Normalized()
	if !strings.Contains(got, diff) {
		t.Fatalf("diff not verbatim:\n%s", got)
	}
}

func TestStateNormalizedJSONSorted(t *testing.T) {
	raw := `{"a_|-a_|-a_|-cmd.run": {"result": false, "changes": {"zeta": 1, "alpha": {"b": 2, "a": 1}},
		"comment": "boom", "__run_num__": 0}}`
	got := mustParse(t, raw).Normalized()
	if !strings.Contains(got, `{"alpha":{"a":1,"b":2},"zeta":1}`) {
		t.Fatalf("changes not compact sorted JSON:\n%s", got)
	}
}

func TestStateCleanRun(t *testing.T) {
	raw := `{
	  "a_|-a_|-a_|-cmd.run": {"result": true, "changes": {}, "comment": "ok", "__run_num__": 0},
	  "b_|-b_|-b_|-cmd.run": {"result": true, "changes": {}, "comment": "ok", "__run_num__": 1}
	}`
	run := mustParse(t, raw)
	if got := run.Normalized(); got != "clean (2 states)" {
		t.Fatalf("normalized = %q, want \"clean (2 states)\"", got)
	}
	if ok, changed, failed, pending := run.Counts(); ok != 2 || changed+failed+pending != 0 {
		t.Fatalf("counts = %d/%d/%d/%d, want a clean run", ok, changed, failed, pending)
	}
}

func TestStateSummary(t *testing.T) {
	run := mustParse(t, stateSample)
	run.Test = true
	if got := run.Summary(); got != "1 ok · 1 would change · 1 failed" {
		t.Fatalf("summary = %q", got)
	}
	clean := mustParse(t, `{"a_|-a_|-a_|-cmd.run": {"result": true, "changes": {}, "__run_num__": 0}}`)
	if got := clean.Summary(); got != "1 ok" {
		t.Fatalf("clean summary = %q", got)
	}
}

func TestStateRenderRows(t *testing.T) {
	run := mustParse(t, stateSample)
	run.Test = true
	rows := RenderRows(run)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	want := []string{"nginx.config", "nginx-config", "managed", "ok", "File is in the correct state"}
	for i, w := range want {
		if rows[0][i] != w {
			t.Errorf("row 0 cell %d = %q, want %q", i, rows[0][i], w)
		}
	}
	if rows[1][3] != "would change" {
		t.Errorf("pending result cell = %q", rows[1][3])
	}
	if rows[2][3] != "failed" {
		t.Errorf("failed result cell = %q", rows[2][3])
	}
}

func TestStateGroupingTwoHostsSameDiff(t *testing.T) {
	a := mustParse(t, stateSample)
	bRaw := strings.Replace(stateSample, `"duration": 12.3`, `"duration": 77.0`, 1)
	b := mustParse(t, bRaw)
	a.Test, b.Test = true, true
	g := group.NewGrouper(2, nil)
	g.Add("web-01", a.Normalized(), 0)
	g.Add("web-02", b.Normalized(), 0)
	sum := g.Summary()
	if len(sum.Groups) != 1 {
		t.Fatalf("groups = %d, want 1 (%+v)", len(sum.Groups), sum.Groups)
	}
	if len(sum.Groups[0].Hosts) != 2 {
		t.Fatalf("hosts = %v, want both", sum.Groups[0].Hosts)
	}
}

func TestStateGoldens(t *testing.T) {
	for _, name := range []string{"state-test-clean", "state-test-changes"} {
		name := name
		t.Run(name, func(t *testing.T) {
			p := filepath.Join("testdata", "salt", name+".json")
			b, err := os.ReadFile(p)
			if err != nil {
				if os.IsNotExist(err) {
					t.Skip("golden not captured")
				}
				t.Fatalf("read %s: %v", p, err)
			}
			if _, err := ParseStateReturn(json.RawMessage(b)); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
		})
	}
}
