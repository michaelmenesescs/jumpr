package salt

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumper/internal/resource"
	"jumper/internal/transport/transporttest"
)

// jobsHistory is the answer of one jobs.list_jobs call. jidB is also in
// jobs.active, so it exercises the merge.
const jobsHistory = `{
  "20261112135501123456": {"Function": "cmd.run_all", "Arguments": ["uptime"], "Target": "web-01",
    "Target-type": "glob", "User": "sudo_sre", "StartTime": "2026, Nov 12 13:55:01.123456"},
  "20261112135501765432": {"Function": "state.apply", "Arguments": ["nginx"], "Target": ["web-01", "web-02"],
    "Target-type": "list", "User": "sudo_sre", "StartTime": "2026, Nov 12 13:55:01.765432"},
  "20261112121000000000": {"Function": "state.highstate", "Arguments": [], "Target": ["db-01"],
    "Target-type": "list", "User": "sudo_sre", "StartTime": "2026, Nov 12 12:10:00.000000"}
}`

// jobsActive is the answer of jobs.active: jidB is still running, and one
// internal job (jumper's own polling noise) is running too.
const jobsActive = `{
  "20261112135501765432": {"Function": "state.apply", "Arguments": ["nginx"], "Target": ["web-01", "web-02"],
    "Target-type": "list", "User": "sudo_sre", "Running": [{"web-02": 4321}], "Returned": ["web-01"]},
  "20261112140000000000": {"Function": "saltutil.find_job", "Arguments": ["20261112135501123456"],
    "Target": "*", "Target-type": "glob", "User": "sudo_sre", "Running": [{"web-01": 17}], "Returned": []}
}`

// newJobsBackend scripts one jobs.active and one jobs.list_jobs answer.
func newJobsBackend(t *testing.T, clk *testClock, active, history string) (*Backend, *transporttest.Fake) {
	t.Helper()
	f := transporttest.New().
		On("jobs.active", transporttest.Reply{Stdout: active}).
		On("jobs.list_jobs", transporttest.Reply{Stdout: history})
	b := New(Options{
		Runner:    NewRunner(f, master, RunnerOptions{}),
		Inventory: testInventory(),
		Master:    master,
		Now:       clk.now,
	})
	return b, f
}

// listJobsScript is the script of the jobs.list_jobs call, or "" when it was
// not run.
func listJobsScript(f *transporttest.Fake) string {
	for _, c := range f.Calls() {
		if strings.Contains(c.Command.Script, "jobs.list_jobs") {
			return c.Command.Script
		}
	}
	return ""
}

func TestKindJobsRunningFirst(t *testing.T) {
	clk := newClock()
	b, _ := newJobsBackend(t, clk, jobsActive, jobsHistory)

	rows := collectRows(t, b, resource.Query{Kind: KindJobs})
	if len(rows) != 3 {
		t.Fatalf("rows = %d (%v), want 3", len(rows), jobNames(rows))
	}
	if rows[0].Ref.Name != jidB {
		t.Errorf("first row = %s, want the running job %s", rows[0].Ref.Name, jidB)
	}
	if got := rows[0].Str("state"); got != JobRunning {
		t.Errorf("state = %q, want running", got)
	}
	if got := rows[0].Str("returned"); got != "1/2" {
		t.Errorf("returned = %q, want 1/2", got)
	}
	if got := rows[0].Str("target"); got != "web-01,web-02" {
		t.Errorf("target = %q, want web-01,web-02", got)
	}
	if rows[1].Ref.Name != jidA || rows[2].Ref.Name != "20261112121000000000" {
		t.Errorf("order = %v, want %s then the 12:10 job", jobNames(rows), jidA)
	}
	for _, r := range rows[1:] {
		if got := r.Str("state"); got != JobDone {
			t.Errorf("%s state = %q, want done", r.Ref.Name, got)
		}
		if got := r.Str("returned"); got != "" {
			t.Errorf("%s returned = %q, want blank for history", r.Ref.Name, got)
		}
	}
}

func TestKindJobsWindowed(t *testing.T) {
	t.Run("session window", func(t *testing.T) {
		clk := newClock()
		b, f := newJobsBackend(t, clk, `{}`, jobsHistory)
		q := resource.Query{Kind: KindJobs, Window: resource.TimeWindow{
			From: baseTime.Add(-2 * time.Hour), ToNow: true, Label: "-2h",
		}}
		collectRows(t, b, q)
		script := listJobsScript(f)
		for _, want := range []string{"start_time=2026-11-12 11:55:00", "end_time=2026-11-12 13:55:00"} {
			if !strings.Contains(script, want) {
				t.Errorf("script = %q, want %q", script, want)
			}
		}
	})

	t.Run("default last hour", func(t *testing.T) {
		clk := newClock()
		b, f := newJobsBackend(t, clk, `{}`, jobsHistory)
		collectRows(t, b, resource.Query{Kind: KindJobs})
		script := listJobsScript(f)
		for _, want := range []string{"start_time=2026-11-12 12:55:00", "end_time=2026-11-12 13:55:00"} {
			if !strings.Contains(script, want) {
				t.Errorf("script = %q, want %q", script, want)
			}
		}
	})
}

func TestKindJobsMinionFilter(t *testing.T) {
	active := `{
  "20261112135501765432": {"Function": "state.apply", "Target": ["web-02"], "Target-type": "list",
    "User": "sudo_sre", "Running": [{"web-02": 1}], "Returned": ["db-01"]}
}`
	clk := newClock()
	b, _ := newJobsBackend(t, clk, active, jobsHistory)

	cases := []struct {
		minion string
		want   []string
	}{
		{"web-02", []string{jidB}},
		{"web-01", []string{jidA}},
		{"db-01", []string{jidB, "20261112121000000000"}},
		{"", []string{jidB, jidA, "20261112121000000000"}},
	}
	for _, c := range cases {
		rows := collectRows(t, b, resource.Query{Kind: KindJobs, Args: map[string]string{"minion": c.minion}})
		got := jobNames(rows)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("minion %q rows = %v, want %v", c.minion, got, c.want)
		}
	}

	// fun is a function prefix filter.
	rows := collectRows(t, b, resource.Query{Kind: KindJobs, Args: map[string]string{"fun": "state."}})
	if got := jobNames(rows); strings.Join(got, ",") != strings.Join([]string{jidB, "20261112121000000000"}, ",") {
		t.Errorf("fun filter rows = %v", got)
	}
}

func TestKindJobsHideInternal(t *testing.T) {
	active := `{
  "20261112140000000000": {"Function": "saltutil.find_job", "Target": "*", "Target-type": "glob",
    "User": "sudo_sre", "Running": [{"web-01": 17}], "Returned": []},
  "20261112140100000000": {"Function": "runner.jobs.lookup_jid", "Target": "*", "Target-type": "glob",
    "User": "sudo_sre", "Running": [{"web-01": 18}], "Returned": []}
}`
	history := `{
  "20261112135501123456": {"Function": "runner.jobs.list_jobs", "Target": "*", "Target-type": "glob",
    "User": "sudo_sre", "StartTime": "2026, Nov 12 13:55:01.123456"},
  "20261112135501765432": {"Function": "saltutil.find_job", "Target": "*", "Target-type": "glob",
    "User": "sudo_sre", "StartTime": "2026, Nov 12 13:55:01.765432"},
  "20261112121000000000": {"Function": "test.ping", "Target": "*", "Target-type": "glob",
    "User": "sudo_sre", "StartTime": "2026, Nov 12 12:10:00.000000"}
}`
	clk := newClock()
	b, _ := newJobsBackend(t, clk, active, history)

	rows := collectRows(t, b, resource.Query{Kind: KindJobs})
	if got := jobNames(rows); strings.Join(got, ",") != "20261112121000000000" {
		t.Errorf("rows = %v, want only the operator's test.ping job", got)
	}
}

func TestKindJobsSpec(t *testing.T) {
	clk := newClock()
	b := New(Options{Master: master, Now: clk.now})
	var found bool
	for _, spec := range b.Kinds() {
		if spec.Name != KindJobs {
			continue
		}
		found = true
		if spec.Scope != resource.ScopeGlobal {
			t.Errorf("scope = %v, want global", spec.Scope)
		}
		for _, alias := range spec.Aliases {
			if _, ok := normalizeKind(resource.KindName(alias)); !ok {
				t.Errorf("alias %q does not normalize", alias)
			}
		}
		for _, id := range []string{"jid", "start", "state", "function", "target", "user", "returned"} {
			if _, ok := spec.Column(id); !ok {
				t.Errorf("kind has no %q column", id)
			}
		}
	}
	if !found {
		t.Fatal("kindSpecs has no salt-job kind")
	}
}

func TestKindJobsLinksOpenDetail(t *testing.T) {
	clk := newClock()
	b := New(Options{Master: master, Now: clk.now})
	links := b.Links(resource.Resource{Ref: resource.Ref{Backend: backendName, Kind: KindJobs, Name: jidA}})
	if len(links) != 1 || links[0].To.Kind != "salt-job-detail" || links[0].To.Args["jid"] != jidA {
		t.Fatalf("links = %+v, want the detail of %s", links, jidA)
	}
}

// jobNames is the row names of a jobs listing, in order.
func jobNames(rows []resource.Resource) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Ref.Name)
	}
	return out
}

// TestJobRowsContextCanceled checks the listing stops when the caller does.
func TestJobRowsContextCanceled(t *testing.T) {
	clk := newClock()
	b, _ := newJobsBackend(t, clk, `{}`, jobsHistory)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.List(ctx, resource.Query{Kind: KindJobs}); err != nil {
		t.Fatalf("List: %v", err)
	}
}
