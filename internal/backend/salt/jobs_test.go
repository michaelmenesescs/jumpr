package salt

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
)

const (
	jidA = "20261112135501123456"
	jidB = "20261112135501765432"
)

// wireResult and wireJob mirror jobs.list_job's JSON so the tests build the
// master's answers the way Salt writes them.
type wireResult struct {
	Return  json.RawMessage `json:"return"`
	Retcode int             `json:"retcode"`
	Success bool            `json:"success"`
}

type wireJob struct {
	JID        string                `json:"jid"`
	Function   string                `json:"Function"`
	Arguments  []string              `json:"Arguments"`
	Target     any                   `json:"Target"`
	TargetType string                `json:"Target-type"`
	User       string                `json:"User"`
	StartTime  string                `json:"StartTime"`
	Minions    []string              `json:"Minions"`
	Result     map[string]wireResult `json:"Result,omitempty"`
}

// minion is one scripted entry of a job's Result map. Return is a raw JSON
// literal.
type minion struct {
	ID      string
	Return  string
	Retcode int
	Success bool
}

// jobReply renders a jobs.list_job answer.
func jobReply(t *testing.T, jid string, minions []string, results ...minion) transporttest.Reply {
	t.Helper()
	w := wireJob{
		JID:        jid,
		Function:   "cmd.run_all",
		Arguments:  []string{"uptime"},
		Target:     []string{"web-01", "web-02"},
		TargetType: "list",
		User:       "sudo_sre",
		StartTime:  "2026, Nov 12 13:55:01.123456",
		Minions:    minions,
	}
	if len(results) > 0 {
		w.Result = make(map[string]wireResult, len(results))
		for _, r := range results {
			w.Result[r.ID] = wireResult{Return: json.RawMessage(r.Return), Retcode: r.Retcode, Success: r.Success}
		}
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	return transporttest.Reply{Stdout: string(b) + "\n"}
}

// okMinion is a minion that ran the function successfully.
func okMinion(id, out string) minion {
	return minion{ID: id, Return: `{"retcode": 0, "stdout": ` + quoteJSON(out) + `}`, Retcode: 0, Success: true}
}

// quoteJSON renders s as a JSON string literal.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// scriptedMaster is a fake Salt master: submissions hand out jids in order and
// jobs.list_job is answered by onLook(poll number per jid).
type scriptedMaster struct {
	mu      sync.Mutex
	jids    []string
	next    int
	polls   map[string]int
	submits int
	onLook  func(jid string, poll int) transporttest.Reply
	fake    *transporttest.Fake
}

func newScriptedMaster(jids []string, onLook func(jid string, poll int) transporttest.Reply) *scriptedMaster {
	m := &scriptedMaster{jids: jids, polls: map[string]int{}, onLook: onLook}
	m.fake = transporttest.New().OnFunc(m.reply)
	return m
}

func (m *scriptedMaster) reply(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case strings.Contains(cmd.Script, "--async"):
		m.submits++
		if m.next >= len(m.jids) {
			return transporttest.Reply{RC: 1, Stderr: "Error: no job id scripted"}, true
		}
		jid := m.jids[m.next]
		m.next++
		return transporttest.Reply{Stdout: "Executed command with job ID: " + jid + "\n"}, true
	case strings.Contains(cmd.Script, "jobs.list_job"):
		fields := strings.Fields(cmd.Script)
		jid := fields[len(fields)-1]
		n := m.polls[jid]
		m.polls[jid]++
		return m.onLook(jid, n), true
	}
	return transporttest.Reply{}, false
}

func (m *scriptedMaster) pollCount(jid string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.polls[jid]
}

func (m *scriptedMaster) submitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.submits
}

// fastOptions keeps the poll loop at test speed.
func fastOptions() jobOptions {
	return jobOptions{
		first:   2 * time.Millisecond,
		poll:    5 * time.Millisecond,
		timeout: 2 * time.Second,
		grace:   20 * time.Millisecond,
	}
}

// collect drains the job's results and fails if it does not finish in time.
func collectJobs(t *testing.T, j *Job, wait time.Duration) []MinionResult {
	t.Helper()
	var out []MinionResult
	deadline := time.After(wait)
	for {
		select {
		case r, ok := <-j.Results():
			if !ok {
				return out
			}
			out = append(out, r)
		case <-deadline:
			t.Fatalf("job did not finish within %v (got %d results)", wait, len(out))
		}
	}
}

func submitList(t *testing.T, m *scriptedMaster, expr, fun string, expect []string, opt jobOptions) *Job {
	t.Helper()
	r := NewRunner(m.fake, master, RunnerOptions{})
	j, err := r.start(context.Background(), Submit{
		Targets: []Target{{Type: "list", Expr: expr}},
		Fun:     fun,
		Expect:  expect,
	}, opt)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return j
}

func resultByMinion(t *testing.T, got []MinionResult, minionID string) MinionResult {
	t.Helper()
	for _, r := range got {
		if r.Minion == minionID {
			return r
		}
	}
	t.Fatalf("no result for %q in %+v", minionID, got)
	return MinionResult{}
}

func TestJobStartStreamsEachMinionOnce(t *testing.T) {
	m := newScriptedMaster([]string{jidA}, func(jid string, poll int) transporttest.Reply {
		switch poll {
		case 0: // job cache has nothing yet
			return jobReply(t, jid, nil)
		case 1:
			return jobReply(t, jid, []string{"web-01"}, okMinion("web-01", "ok"))
		default:
			return jobReply(t, jid, []string{"web-01", "web-02"},
				okMinion("web-01", "ok"), okMinion("web-02", "also ok"))
		}
	})
	j := submitList(t, m, "web-01,web-02", "cmd.run_all", nil, fastOptions())

	if got := j.JIDs(); len(got) != 1 || got[0] != jidA {
		t.Fatalf("JIDs = %v, want [%s]", got, jidA)
	}
	got := collectJobs(t, j, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("results = %d (%+v), want 2 emitted exactly once each", len(got), got)
	}
	for _, id := range []string{"web-01", "web-02"} {
		r := resultByMinion(t, got, id)
		if r.NoReturn || r.Unexpected || !r.Success || r.JID != jidA {
			t.Errorf("%s = %+v, want a plain success from %s", id, r, jidA)
		}
		if !strings.Contains(string(r.Return), "retcode") {
			t.Errorf("%s return = %s, want the raw return object", id, r.Return)
		}
	}
	if exp := j.Expected(); len(exp) != 2 {
		t.Errorf("Expected = %v, want both minions", exp)
	}
	if err := j.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
	if m.submitCount() != 1 {
		t.Errorf("submits = %d, want 1", m.submitCount())
	}
}

func TestJobNoReturnAfterTimeout(t *testing.T) {
	m := newScriptedMaster([]string{jidA}, func(jid string, poll int) transporttest.Reply {
		return jobReply(t, jid, []string{"web-01", "web-02"}, okMinion("web-01", "ok"))
	})
	opt := fastOptions()
	opt.timeout = 40 * time.Millisecond
	j := submitList(t, m, "web-01,web-02", "cmd.run_all", []string{"web-01", "web-02"}, opt)

	got := collectJobs(t, j, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("results = %d (%+v), want one per expected minion", len(got), got)
	}
	if r := resultByMinion(t, got, "web-01"); r.NoReturn || !r.Success {
		t.Errorf("web-01 = %+v, want its real result", r)
	}
	r := resultByMinion(t, got, "web-02")
	if !r.NoReturn {
		t.Errorf("web-02 = %+v, want NoReturn", r)
	}
	if r.JID != jidA {
		t.Errorf("web-02 JID = %q, want %q", r.JID, jidA)
	}
	if err := j.Err(); err != nil {
		t.Errorf("Err = %v, want nil: a timeout is not a failure", err)
	}
}

func TestJobChunkedTargetsMerge(t *testing.T) {
	m := newScriptedMaster([]string{jidA, jidB}, func(jid string, poll int) transporttest.Reply {
		switch jid {
		case jidA:
			return jobReply(t, jid, []string{"web-01"}, okMinion("web-01", "a"))
		default:
			return jobReply(t, jid, []string{"web-02"}, okMinion("web-02", "b"))
		}
	})
	r := NewRunner(m.fake, master, RunnerOptions{})
	j, err := r.start(context.Background(), Submit{
		Targets: []Target{{Type: "list", Expr: "web-01"}, {Type: "list", Expr: "web-02"}},
		Fun:     "cmd.run_all",
		Expect:  []string{"web-01", "web-02"},
	}, fastOptions())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := j.JIDs(); len(got) != 2 || got[0] != jidA || got[1] != jidB {
		t.Fatalf("JIDs = %v, want [%s %s]", got, jidA, jidB)
	}
	got := collectJobs(t, j, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("results = %d (%+v), want both chunks on one stream", len(got), got)
	}
	if a := resultByMinion(t, got, "web-01"); a.JID != jidA {
		t.Errorf("web-01 JID = %q, want %q", a.JID, jidA)
	}
	if b := resultByMinion(t, got, "web-02"); b.JID != jidB {
		t.Errorf("web-02 JID = %q, want %q", b.JID, jidB)
	}
	if m.submitCount() != 2 {
		t.Errorf("submits = %d, want one per target", m.submitCount())
	}
}

func TestJobUnexpectedMinionFlagged(t *testing.T) {
	m := newScriptedMaster([]string{jidA}, func(jid string, poll int) transporttest.Reply {
		return jobReply(t, jid, []string{"web-01", "web-02"},
			okMinion("web-01", "ok"), okMinion("web-02", "surprise"))
	})
	j := submitList(t, m, "web-01", "cmd.run_all", []string{"web-01"}, fastOptions())

	got := collectJobs(t, j, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("results = %d (%+v), want both", len(got), got)
	}
	if r := resultByMinion(t, got, "web-01"); r.Unexpected {
		t.Errorf("web-01 = %+v, want it expected", r)
	}
	if r := resultByMinion(t, got, "web-02"); !r.Unexpected {
		t.Errorf("web-02 = %+v, want Unexpected", r)
	}
}

func TestJobCancelStopsPolling(t *testing.T) {
	m := newScriptedMaster([]string{jidA}, func(jid string, poll int) transporttest.Reply {
		return jobReply(t, jid, []string{"web-01"})
	})
	opt := fastOptions()
	opt.timeout = 5 * time.Second
	j := submitList(t, m, "web-01", "cmd.run_all", []string{"web-01", "web-02"}, opt)

	deadline := time.Now().Add(time.Second)
	for m.pollCount(jidA) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	j.Cancel()
	select {
	case <-j.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop the poll loop")
	}
	got, open := 0, true
	for open {
		select {
		case _, ok := <-j.Results():
			if ok {
				got++
			}
			open = ok
		default:
			open = false
		}
	}
	before := m.pollCount(jidA)
	time.Sleep(30 * time.Millisecond)
	if after := m.pollCount(jidA); after != before {
		t.Errorf("polls after cancel = %d, want the %d from the cancel", after, before)
	}
	if got != 0 {
		t.Errorf("results = %d, want none: nothing had returned", got)
	}
}

func TestJobPollErrorsRetriedThenGiveUp(t *testing.T) {
	boom := errs.New(errs.Unreachable, "ssh.exec", "connection lost").WithHost(master)
	m := newScriptedMaster([]string{jidA}, func(jid string, poll int) transporttest.Reply {
		return transporttest.Reply{Err: boom}
	})
	j := submitList(t, m, "web-01,web-02", "cmd.run_all", []string{"web-01", "web-02"}, fastOptions())

	got := collectJobs(t, j, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("results = %d (%+v), want NoReturn for both", len(got), got)
	}
	for _, r := range got {
		if !r.NoReturn {
			t.Errorf("%s = %+v, want NoReturn", r.Minion, r)
		}
	}
	if err := j.Err(); !errors.Is(err, boom) {
		t.Errorf("Err = %v, want the poll error", err)
	}
	if n := m.pollCount(jidA); n != maxPollFailures {
		t.Errorf("polls = %d, want %d retries before giving up", n, maxPollFailures)
	}
}

func TestJobEmptyMinionsDoesNotFinishEarly(t *testing.T) {
	m := newScriptedMaster([]string{jidA}, func(jid string, poll int) transporttest.Reply {
		if poll < 3 { // the master has not written the results yet
			return jobReply(t, jid, nil)
		}
		return jobReply(t, jid, []string{"web-01"}, okMinion("web-01", "late"))
	})
	j := submitList(t, m, "web-01", "cmd.run_all", nil, fastOptions())

	got := collectJobs(t, j, 2*time.Second)
	if len(got) != 1 || got[0].Minion != "web-01" || got[0].NoReturn {
		t.Fatalf("results = %+v, want the late web-01 result", got)
	}
	if n := m.pollCount(jidA); n < 4 {
		t.Errorf("polls = %d, want the loop to keep polling through empty answers", n)
	}
	if exp := j.Expected(); len(exp) != 1 || exp[0] != "web-01" {
		t.Errorf("Expected = %v, want web-01", exp)
	}
}

func TestJobSubmitErrorsKeepWorkingChunks(t *testing.T) {
	var mu sync.Mutex
	n := 0
	fake := transporttest.New().OnFunc(func(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if strings.Contains(cmd.Script, "jobs.list_job") {
			return jobReply(t, jidA, []string{"web-01"}, okMinion("web-01", "ok")), true
		}
		if !strings.Contains(cmd.Script, "--async") {
			return transporttest.Reply{}, false
		}
		mu.Lock()
		n++
		second := n == 2
		mu.Unlock()
		if second {
			return transporttest.Reply{RC: 1, Stderr: "Error: target returns empty"}, true
		}
		return transporttest.Reply{Stdout: "Executed command with job ID: " + jidA + "\n"}, true
	})
	r := NewRunner(fake, master, RunnerOptions{})
	j, err := r.start(context.Background(), Submit{
		Targets: []Target{{Type: "list", Expr: "web-01"}, {Type: "list", Expr: "web-02"}},
		Fun:     "cmd.run_all",
		Expect:  []string{"web-01"},
	}, fastOptions())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(j.SubmitErrors) != 1 {
		t.Fatalf("SubmitErrors = %v, want one", j.SubmitErrors)
	}
	if got := j.JIDs(); len(got) != 1 {
		t.Fatalf("JIDs = %v, want the submitted chunk only", got)
	}
	got := collectJobs(t, j, 2*time.Second)
	if len(got) != 1 || got[0].Minion != "web-01" || got[0].NoReturn || !got[0].Success {
		t.Fatalf("results = %+v, want the surviving chunk's web-01 result", got)
	}
}

func TestJobSubmitAllChunksFail(t *testing.T) {
	fake := transporttest.New().On("--async", transporttest.Reply{RC: 127, Stderr: "sh: salt: command not found\n"})
	r := NewRunner(fake, master, RunnerOptions{})
	j, err := r.Start(context.Background(), Submit{
		Targets: []Target{{Type: "list", Expr: "web-01"}},
		Fun:     "cmd.run_all",
	})
	if err == nil {
		t.Fatalf("Start = %+v, want the submission error", j)
	}
	if !errs.Is(err, errs.NotAvailable) {
		t.Errorf("kind = %q, want not-available (%v)", errs.KindOf(err), err)
	}
	if j != nil {
		t.Errorf("job = %+v, want nil", j)
	}
}

func TestParseListJobTargetStringOrList(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{"string target", `"web-*"`},
		{"list target", `["web-01", "web-02"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := `{"jid": "` + jidA + `", "Function": "cmd.run_all", "Arguments": ["cmd=uptime"],
  "Target": ` + tt.target + `, "Target-type": "list", "User": "sudo_sre",
  "StartTime": "2026, Nov 12 13:55:01.123456", "Minions": ["web-01", "web-02"],
  "Result": {"web-01": {"return": {"pid": 1, "retcode": 0, "stdout": "ok", "stderr": ""},
             "retcode": 0, "success": true}}}`
			info, err := parseListJob([]byte(raw))
			if err != nil {
				t.Fatalf("parseListJob: %v", err)
			}
			if info.JID != jidA || info.Function != "cmd.run_all" {
				t.Errorf("jid/function = %q/%q", info.JID, info.Function)
			}
			if info.TargetType != "list" || info.User != "sudo_sre" {
				t.Errorf("target-type/user = %q/%q", info.TargetType, info.User)
			}
			if string(info.Target) != tt.target {
				t.Errorf("Target = %s, want %s", info.Target, tt.target)
			}
			if len(info.Arguments) != 1 || string(info.Arguments[0]) != `"cmd=uptime"` {
				t.Errorf("Arguments = %v", info.Arguments)
			}
			want := time.Date(2026, time.November, 12, 13, 55, 1, 123456000, time.Local)
			if !info.Start.Equal(want) {
				t.Errorf("Start = %v, want %v", info.Start, want)
			}
			if len(info.Minions) != 2 || info.Minions[1] != "web-02" {
				t.Errorf("Minions = %v", info.Minions)
			}
			mr, ok := info.Result["web-01"]
			if !ok {
				t.Fatalf("Result = %v, want web-01", info.Result)
			}
			if mr.Retcode != 0 || !mr.Success {
				t.Errorf("minion return = %+v", mr)
			}
			if !strings.Contains(string(mr.Return), `"stdout": "ok"`) {
				t.Errorf("Return = %s", mr.Return)
			}
		})
	}
}

func TestParseListJobUnknown(t *testing.T) {
	for _, raw := range []string{"{}", `{"Error": "Job 20260101000000000000 not found"}`} {
		if _, err := parseListJob([]byte(raw)); !errs.Is(err, errs.NotAvailable) {
			t.Errorf("parseListJob(%s) kind = %q, want not-available (%v)", raw, errs.KindOf(err), err)
		}
	}
}

func TestLookupUnknownJob(t *testing.T) {
	fake := transporttest.New().On("jobs.list_job", transporttest.Reply{Stdout: "{}\n"})
	r := NewRunner(fake, master, RunnerOptions{})

	if _, err := r.Lookup(context.Background(), jidA); !errs.Is(err, errs.NotAvailable) {
		t.Fatalf("kind = %q, want not-available (%v)", errs.KindOf(err), err)
	}
	if _, err := r.Lookup(context.Background(), "not-a-jid"); !errs.Is(err, errs.Config) {
		t.Fatalf("kind = %q, want config (%v)", errs.KindOf(err), err)
	}
	if n := len(fake.Calls()); n != 1 {
		t.Errorf("executor calls = %d, want 1: an invalid jid must not be sent", n)
	}
}

func TestLookupFillsJID(t *testing.T) {
	fake := transporttest.New().On("jobs.list_job", transporttest.Reply{
		Stdout: `{"Function": "test.ping", "Minions": ["web-01"], "Result": {"web-01": {"return": true, "retcode": 0, "success": true}}}` + "\n",
	})
	r := NewRunner(fake, master, RunnerOptions{})

	info, err := r.Lookup(context.Background(), jidA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info.JID != jidA {
		t.Errorf("JID = %q, want the requested %q", info.JID, jidA)
	}
	call := fake.Calls()[0].Command.Script
	if !strings.Contains(call, "jobs.list_job "+jidA) {
		t.Errorf("script = %q, want jobs.list_job %s", call, jidA)
	}
}

func TestActiveJobs(t *testing.T) {
	fake := transporttest.New().On("jobs.active", transporttest.Reply{Stdout: `{
  "` + jidA + `": {"Function": "state.apply", "Arguments": ["common"], "Target": ["web-01", "web-02"],
    "Target-type": "list", "User": "sudo_sre", "Running": [{"web-01": 4321}], "Returned": ["web-02"]}}` + "\n"})
	r := NewRunner(fake, master, RunnerOptions{})

	got, err := r.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	a, ok := got[jidA]
	if !ok {
		t.Fatalf("Active = %v, want %s", got, jidA)
	}
	if a.Function != "state.apply" || a.TargetType != "list" || a.User != "sudo_sre" {
		t.Errorf("active job = %+v", a)
	}
	if len(a.Running) != 1 || a.Running[0]["web-01"] != 4321 {
		t.Errorf("Running = %v", a.Running)
	}
	if len(a.Returned) != 1 || a.Returned[0] != "web-02" {
		t.Errorf("Returned = %v", a.Returned)
	}
}

func TestListJobsWindowKwargs(t *testing.T) {
	now := time.Date(2026, time.November, 12, 14, 0, 0, 0, time.Local)
	body := `{
  "20261112130000000000": {"Function": "test.ping", "Target": "*", "Target-type": "glob", "User": "sudo_sre", "StartTime": "2026, Nov 12 13:00:00.000000"},
  "20261112135501123456": {"Function": "cmd.run_all", "Target": "web-01", "Target-type": "glob", "User": "sudo_sre", "StartTime": "2026, Nov 12 13:55:01.123456"},
  "20261112120000000000": {"Function": "state.apply", "Target": "web-02", "Target-type": "glob", "User": "sudo_sre", "StartTime": "2026, Nov 12 12:00:00.000000"}}` + "\n"
	fake := transporttest.New().On("jobs.list_jobs", transporttest.Reply{Stdout: body})
	r := NewRunner(fake, master, RunnerOptions{})

	w, err := resource.ParseTimeWindow("-1h", now)
	if err != nil {
		t.Fatalf("ParseTimeWindow: %v", err)
	}
	got, err := r.ListJobs(context.Background(), w, now)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("jobs = %d, want 3", len(got))
	}
	newest := got[0]
	if newest.JID != jidA || newest.Function != "cmd.run_all" {
		t.Errorf("first job = %+v, want the newest (%s)", newest, jidA)
	}
	if got[2].JID != "20261112120000000000" {
		t.Errorf("last job = %+v, want the oldest", got[2])
	}
	if got[0].Start.IsZero() || !got[0].Start.Equal(time.Date(2026, time.November, 12, 13, 55, 1, 123456000, time.Local)) {
		t.Errorf("Start = %v", got[0].Start)
	}

	script := fake.Calls()[0].Command.Script
	for _, want := range []string{"jobs.list_jobs", "start_time=2026-11-12 13:00:00", "end_time=2026-11-12 14:00:00"} {
		if !strings.Contains(script, want) {
			t.Errorf("script = %q, want it to contain %q", script, want)
		}
	}
}

func TestListJobsEmptyWindowHasNoKwargs(t *testing.T) {
	fake := transporttest.New().On("jobs.list_jobs", transporttest.Reply{Stdout: "{}\n"})
	r := NewRunner(fake, master, RunnerOptions{})

	got, err := r.ListJobs(context.Background(), resource.TimeWindow{}, time.Now())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("jobs = %v, want none", got)
	}
	if script := fake.Calls()[0].Command.Script; strings.Contains(script, "start_time") {
		t.Errorf("script = %q, want no window kwargs", script)
	}
}

func TestJIDTime(t *testing.T) {
	want := time.Date(2026, time.November, 12, 13, 55, 1, 123456000, time.Local)
	got, ok := JIDTime(jidA)
	if !ok {
		t.Fatalf("JIDTime(%q) not ok", jidA)
	}
	if !got.Equal(want) {
		t.Errorf("JIDTime = %v, want %v", got, want)
	}
	for _, bad := range []string{"", "2026", jidA + "0", "2026111213550112345x"} {
		if _, ok := JIDTime(bad); ok {
			t.Errorf("JIDTime(%q) ok, want not ok", bad)
		}
	}
}
