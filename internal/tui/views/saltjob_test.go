package views

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend"
	"jumpr/internal/backend/linux"
	"jumpr/internal/backend/salt"
	"jumpr/internal/errs"
	"jumpr/internal/investigation"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
)

// jobTestJID is the job every detail test opens.
const jobTestJID = "20261112135501123456"

// fakeJobBackend is a scripted SaltJobBackend.
type fakeJobBackend struct {
	mu        sync.Mutex
	info      salt.JobInfo
	active    bool
	lookups   int
	lookupErr error
	hosts     map[string]resource.HostID
}

func (f *fakeJobBackend) Lookup(ctx context.Context, jid string) (salt.JobInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.lookupErr != nil {
		return salt.JobInfo{}, f.lookupErr
	}
	return f.info, nil
}

func (f *fakeJobBackend) Active(ctx context.Context) (map[string]salt.ActiveJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]salt.ActiveJob{}
	if f.active {
		out[f.info.JID] = salt.ActiveJob{Function: f.info.Function}
	}
	return out, nil
}

func (f *fakeJobBackend) MinionHost(id string) (resource.HostID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.hosts[id]
	return h, ok
}

// lookupsSeen is the number of Lookup calls so far.
func (f *fakeJobBackend) lookupsSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

// setActive switches the running flag the fake reports.
func (f *fakeJobBackend) setActive(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = v
}

// jobReturn builds a cmd.run_all style return.
func jobReturn(stdout string, rc int) salt.MinionReturn {
	body, err := json.Marshal(map[string]any{"stdout": stdout, "stderr": "", "retcode": rc})
	if err != nil {
		panic(err)
	}
	return salt.MinionReturn{Return: body, Retcode: rc, Success: rc == 0}
}

// jobInfo builds a finished job over minions ms with the given results.
func jobInfo(fun string, ms []string, results map[string]salt.MinionReturn) salt.JobInfo {
	return salt.JobInfo{
		JID:       jobTestJID,
		Function:  fun,
		Arguments: []json.RawMessage{json.RawMessage(`"sleep 1"`)},
		Target:    json.RawMessage(`["web-01","web-02"]`),
		User:      "sudo_sre",
		Start:     fixedNow.Add(-10 * time.Minute),
		Minions:   ms,
		Result:    results,
	}
}

// saltJobEnv is a colourless environment with a redactor.
func saltJobEnv(t *testing.T) *tui.Env {
	t.Helper()
	env := testEnv()
	r, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}
	env.Redact = r
	return env
}

// stepJob feeds one message to the view and returns it with the next command.
func stepJob(t *testing.T, j *SaltJob, msg tea.Msg) (*SaltJob, tea.Cmd) {
	t.Helper()
	view, cmd := j.Update(msg)
	next, ok := view.(*SaltJob)
	if !ok {
		t.Fatalf("Update returned %T, want *SaltJob", view)
	}
	return next, cmd
}

// loadJob runs Init and the first load command.
func loadJob(t *testing.T, j *SaltJob) (*SaltJob, tea.Cmd) {
	t.Helper()
	cmd := j.Init()
	if cmd == nil {
		t.Fatal("Init returned no command")
	}
	return stepJob(t, j, cmd())
}

// TestSaltJobDetailGroups is the SPEC W8 case: 44 minions, 41 identical, 2
// differing, 1 that never returned.
func TestSaltJobDetailGroups(t *testing.T) {
	results := make(map[string]salt.MinionReturn, 43)
	minions := make([]string, 0, 44)
	for i := 0; i < 44; i++ {
		id := fmt.Sprintf("web-%02d", i)
		minions = append(minions, id)
		switch i {
		case 41:
			results[id] = jobReturn("bad one", 1)
		case 42:
			results[id] = jobReturn("bad two", 1)
		case 43:
			// never returned
		default:
			results[id] = jobReturn("ok", 0)
		}
	}
	f := &fakeJobBackend{info: jobInfo("cmd.run_all", minions, results)}
	j := NewSaltJob(saltJobEnv(t), f, jobTestJID)
	j, _ = loadJob(t, j)

	if got := j.sum.Headline(); !strings.Contains(got, "41 identical · 2 differ · 1 errors") {
		t.Fatalf("headline = %q", got)
	}
	got := j.headline()
	if want := "41 identical · 2 differ · 1 no return"; got != want {
		t.Errorf("banner headline = %q, want %q", got, want)
	}
	out := j.Render(120, 24)
	if !strings.Contains(out, "41 identical · 2 differ · 1 no return") {
		t.Errorf("render = %q, want the W8 headline", out)
	}
	if strings.Contains(out, " errors") {
		t.Errorf("render = %q, want \"no return\" instead of \"errors\"", out)
	}
	if len(j.sum.Groups) != 3 {
		t.Errorf("groups = %d, want 3", len(j.sum.Groups))
	}
	if !strings.Contains(strings.Join(j.CopyLines(), "\n"), "no return") {
		t.Errorf("copy lines = %v, want the no return bucket", j.CopyLines())
	}
}

// TestSaltJobDetailRepollsWhileActive checks the 2 s re-poll of a running job.
func TestSaltJobDetailRepollsWhileActive(t *testing.T) {
	ms := []string{"web-01", "web-02"}
	f := &fakeJobBackend{
		info:   jobInfo("state.apply", ms, map[string]salt.MinionReturn{"web-01": jobReturn("ok", 0)}),
		active: true,
	}
	j := NewSaltJob(saltJobEnv(t), f, jobTestJID)
	j.poll = time.Millisecond

	j, poll := loadJob(t, j)
	if poll == nil {
		t.Fatal("a running job asks for no follow-up poll")
	}
	if f.lookupsSeen() != 1 {
		t.Fatalf("lookups = %d, want 1", f.lookupsSeen())
	}
	if !j.active {
		t.Fatal("view does not know the job is running")
	}
	if got := j.AutoRefresh(); got != saltJobAutoRefresh {
		t.Errorf("AutoRefresh = %v, want %v", got, saltJobAutoRefresh)
	}

	// The tick leads to another read.
	j, load := stepJob(t, j, poll())
	if load == nil {
		t.Fatal("tick returned no load command")
	}
	j, poll = stepJob(t, j, load())
	if f.lookupsSeen() != 2 {
		t.Fatalf("lookups = %d, want 2 after the poll", f.lookupsSeen())
	}
	if poll == nil {
		t.Fatal("still-running job stopped polling")
	}

	// Once the master stops reporting the job the polling stops.
	f.setActive(false)
	j, load = stepJob(t, j, poll())
	j, next := stepJob(t, j, load())
	if next != nil {
		t.Errorf("finished job still polls")
	}
	if got := j.AutoRefresh(); got != 0 {
		t.Errorf("AutoRefresh = %v, want 0 for a finished job", got)
	}
	if f.lookupsSeen() != 3 {
		t.Errorf("lookups = %d, want 3", f.lookupsSeen())
	}
}

// loadJobMsg runs one load command and returns its message.
func loadJobMsg(t *testing.T, j *SaltJob) tea.Msg {
	t.Helper()
	cmd := j.load()
	if cmd == nil {
		t.Fatal("load returned no command")
	}
	return cmd()
}

// TestSaltJobDetailPivotToHost walks group → member → host, and the journal
// pivot of a member.
func TestSaltJobDetailPivotToHost(t *testing.T) {
	ms := []string{"web-01", "web-02"}
	results := map[string]salt.MinionReturn{
		"web-01": jobReturn("ok", 0),
		"web-02": jobReturn("ok", 0),
	}
	f := &fakeJobBackend{
		info:  jobInfo("cmd.run_all", ms, results),
		hosts: map[string]resource.HostID{"web-01": "web-01.example.internal"},
	}
	j := NewSaltJob(saltJobEnv(t), f, jobTestJID)
	j, _ = loadJob(t, j)

	// enter on the majority group lists its minions
	j, cmd := stepJob(t, j, keyMsg("enter"))
	if cmd != nil {
		t.Fatalf("opening the members produced a command: %v", cmd)
	}
	if !j.members {
		t.Fatal("enter did not open the member list")
	}
	if out := j.Render(120, 20); !strings.Contains(out, "web-01") || !strings.Contains(out, "web-02") {
		t.Errorf("member list = %q, want both minions", out)
	}

	// enter on web-01 opens its host overview
	_, cmd = stepJob(t, j, keyMsg("enter"))
	if cmd == nil {
		t.Fatal("enter on a member produced no command")
	}
	qmsg, ok := cmd().(tui.OpenQueryMsg)
	if !ok {
		t.Fatalf("command returned %T, want OpenQueryMsg", cmd())
	}
	if qmsg.Query.Kind != linux.KindHostOverview {
		t.Errorf("kind = %q, want host overview", qmsg.Query.Kind)
	}
	if len(qmsg.Query.Hosts) != 1 || qmsg.Query.Hosts[0] != "web-01.example.internal" {
		t.Errorf("hosts = %v, want web-01.example.internal", qmsg.Query.Hosts)
	}

	// j on a member opens that host's journal around the job
	j, cmd = stepJob(t, j, keyMsg("j"))
	if cmd == nil {
		t.Fatal("j produced no command")
	}
	jmsg, ok := cmd().(tui.OpenQueryMsg)
	if !ok {
		t.Fatalf("journal command returned %T, want OpenQueryMsg", cmd())
	}
	if jmsg.Query.Kind != linux.KindJournal {
		t.Errorf("kind = %q, want journal", jmsg.Query.Kind)
	}
	start := f.info.Start
	if want := start.Add(-time.Minute); !jmsg.Query.Window.From.Equal(want) {
		t.Errorf("window from = %v, want %v", jmsg.Query.Window.From, want)
	}
	if want := start.Add(5 * time.Minute); !jmsg.Query.Window.To.Equal(want) {
		t.Errorf("window to = %v, want %v", jmsg.Query.Window.To, want)
	}

	// esc goes back to the groups, then out of the view
	j, cmd = stepJob(t, j, keyMsg("esc"))
	if j.members {
		t.Fatal("esc did not leave the member list")
	}
	if cmd != nil {
		t.Fatalf("esc from the members produced %v, want nothing", cmd)
	}
	_, cmd = stepJob(t, j, keyMsg("esc"))
	if cmd == nil {
		t.Fatal("esc on the groups produced no command")
	}
	if _, ok := cmd().(tui.PopViewMsg); !ok {
		t.Errorf("esc returned %T, want PopViewMsg", cmd())
	}
}

// TestSaltJobDetailRedactsArguments checks a secret in the arguments never
// reaches the screen.
func TestSaltJobDetailRedactsArguments(t *testing.T) {
	const token = "Bearer supersecrettokenvalue123456"
	info := jobInfo("cmd.run", []string{"web-01"}, map[string]salt.MinionReturn{
		"web-01": jobReturn("ok", 0),
	})
	info.Arguments = []json.RawMessage{json.RawMessage(`"curl -H \"Authorization: ` + token + `\" https://example.internal"`)}
	f := &fakeJobBackend{info: info}
	j := NewSaltJob(saltJobEnv(t), f, jobTestJID)
	j, _ = loadJob(t, j)

	out := j.Render(160, 20)
	if strings.Contains(out, "supersecrettokenvalue123456") {
		t.Errorf("render leaks the token: %q", out)
	}
	if !strings.Contains(out, "‹redacted:") {
		t.Errorf("render = %q, want a redaction marker", out)
	}
	if strings.Contains(strings.Join(j.CopyLines(), "\n"), "supersecrettokenvalue123456") {
		t.Errorf("copy lines leak the token: %v", j.CopyLines())
	}
}

// fakeRecorder is an investigation timeline writer.
type fakeRecorder struct {
	mu      sync.Mutex
	open    bool
	entries []investigation.Entry
}

func (r *fakeRecorder) Append(e investigation.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return nil
}

func (r *fakeRecorder) Current() (investigation.Meta, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return investigation.Meta{ID: "2026-11-12-api-5xx"}, r.open
}

func (r *fakeRecorder) seen() []investigation.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]investigation.Entry(nil), r.entries...)
}

// TestSaltJobDetailRecordsViewEntry checks the investigation gets one view
// entry, and only while an investigation is open.
func TestSaltJobDetailRecordsViewEntry(t *testing.T) {
	ms := []string{"web-01"}
	info := jobInfo("state.apply", ms, map[string]salt.MinionReturn{"web-01": jobReturn("ok", 0)})

	rec := &fakeRecorder{open: true}
	env := saltJobEnv(t)
	env.Recorder = rec
	j := NewSaltJob(env, &fakeJobBackend{info: info}, jobTestJID)
	j, _ = loadJob(t, j)

	got := rec.seen()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].Kind != investigation.EntryView {
		t.Errorf("kind = %q, want view", got[0].Kind)
	}
	if want := "salt job " + jobTestJID + " state.apply"; got[0].Text != want {
		t.Errorf("text = %q, want %q", got[0].Text, want)
	}

	// A second load does not record again.
	_, _ = stepJob(t, j, loadJobMsg(t, j))
	if n := len(rec.seen()); n != 1 {
		t.Errorf("entries = %d, want 1 after a re-poll", n)
	}

	// Without an open investigation nothing is written.
	closed := &fakeRecorder{}
	env2 := saltJobEnv(t)
	env2.Recorder = closed
	j2 := NewSaltJob(env2, &fakeJobBackend{info: info}, jobTestJID)
	j2, _ = loadJob(t, j2)
	if n := len(closed.seen()); n != 0 {
		t.Errorf("entries = %d, want none while no investigation is open", n)
	}
}

// jobStubBackend is a Backend that also serves jobs, for the factory tests.
type jobStubBackend struct {
	fakeJobBackend
	spec resource.KindSpec
}

func (b *jobStubBackend) Name() string               { return "salt" }
func (b *jobStubBackend) Kinds() []resource.KindSpec { return []resource.KindSpec{b.spec} }

func (b *jobStubBackend) List(context.Context, resource.Query) (<-chan backend.Result, error) {
	return nil, errs.New(errs.Config, "stub", "no list")
}

func (b *jobStubBackend) Get(context.Context, resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, errs.New(errs.NotAvailable, "stub", "no get")
}

func (b *jobStubBackend) Links(resource.Resource) []resource.Link { return nil }

func (b *jobStubBackend) Health(context.Context) backend.Health {
	return backend.Health{State: backend.HealthOK}
}

// jobsTestSpec is the salt-job KindSpec the stub serves.
func jobsTestSpec() resource.KindSpec {
	return resource.KindSpec{
		Name:    salt.KindJobs,
		Aliases: []string{"jobs", "job"},
		Title:   "Salt jobs",
		Scope:   resource.ScopeGlobal,
		Columns: []resource.Column{
			{ID: "jid", Title: "JID"},
			{ID: "state", Title: "STATE"},
		},
	}
}

// TestSaltJobFactoryOpensDetail checks the factory registration and the
// command line of the detail view.
func TestSaltJobFactoryOpensDetail(t *testing.T) {
	stub := &jobStubBackend{spec: jobsTestSpec()}
	reg := backend.NewRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatalf("Register: %v", err)
	}
	env := saltJobEnv(t)
	env.Registry = reg

	factory, ok := Factories()[KindSaltJobDetail]
	if !ok {
		t.Fatal("no factory for salt-job-detail")
	}
	v, err := factory(env, resource.Query{Kind: KindSaltJobDetail, Args: map[string]string{"jid": jobTestJID}})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	job, ok := v.(*SaltJob)
	if !ok {
		t.Fatalf("factory returned %T, want *SaltJob", v)
	}
	if job.jid != jobTestJID {
		t.Errorf("jid = %q, want %q", job.jid, jobTestJID)
	}
	if got, want := job.CommandLine(), "salt-job-detail --arg jid="+jobTestJID; got != want {
		t.Errorf("CommandLine = %q, want %q", got, want)
	}

	if _, err := factory(env, resource.Query{Kind: KindSaltJobDetail}); !errs.Is(err, errs.Config) {
		t.Errorf("factory without a jid = %v, want a config error", err)
	}

	// The jobs list is the table with its auto-refresh hook.
	listFactory, ok := Factories()[salt.KindJobs]
	if !ok {
		t.Fatal("no factory for salt-jobs")
	}
	lv, err := listFactory(env, resource.Query{Kind: salt.KindJobs})
	if err != nil {
		t.Fatalf("jobs factory: %v", err)
	}
	tbl, ok := lv.(*jobsTable)
	if !ok {
		t.Fatalf("jobs factory returned %T, want *jobsTable", lv)
	}
	if got := tbl.AutoRefresh(); got != 0 {
		t.Errorf("AutoRefresh on an empty table = %v, want 0", got)
	}
	tbl.Table.rows = []resource.Resource{{Fields: map[string]any{"state": salt.JobRunning}}}
	if got := tbl.AutoRefresh(); got != saltJobAutoRefresh {
		t.Errorf("AutoRefresh with a running job = %v, want %v", got, saltJobAutoRefresh)
	}
}
