package views

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/linux"
	"jumper/internal/resource"
	"jumper/internal/tui"
)

// journalRow builds one journal resource row.
func journalRow(at time.Time, pri string, status resource.Status, source, msg string) resource.Resource {
	return resource.Resource{
		Ref: resource.Ref{Backend: "linux", Kind: linux.KindJournal, Host: "web-01", Name: source + "@" + at.Format("150405")},
		Fields: map[string]any{
			"host":    "web-01",
			"time":    at,
			"pri":     pri,
			"source":  source,
			"message": msg,
		},
		Labels: map[string]string{"host": "web-01"},
		Status: status,
	}
}

// fakeJournalBackend serves scripted rows and a scripted follow stream.
type fakeJournalBackend struct {
	mu        sync.Mutex
	rows      []resource.Resource
	queries   []resource.Query
	follows   []linux.JournalOptions
	followCh  chan linux.Entry
	followErr error
}

var _ backend.Backend = (*fakeJournalBackend)(nil)

func (f *fakeJournalBackend) Name() string { return "linux" }

func (f *fakeJournalBackend) Kinds() []resource.KindSpec {
	return linux.New(linux.Options{}).Kinds()
}

func (f *fakeJournalBackend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	rows := append([]resource.Resource(nil), f.rows...)
	f.mu.Unlock()

	host := resource.HostID("")
	if len(q.Hosts) > 0 {
		host = q.Hosts[0]
	}
	out := make(chan backend.Result, 1)
	out <- backend.Result{Host: host, Items: rows, Done: 1, Total: 1}
	close(out)
	return out, nil
}

func (f *fakeJournalBackend) Get(context.Context, resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, nil
}

func (f *fakeJournalBackend) Links(resource.Resource) []resource.Link { return nil }

func (f *fakeJournalBackend) Health(context.Context) backend.Health {
	return backend.Health{}
}

func (f *fakeJournalBackend) FollowJournal(ctx context.Context, host resource.HostID, o linux.JournalOptions) (<-chan linux.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.follows = append(f.follows, o)
	if f.followErr != nil {
		return nil, f.followErr
	}
	return f.followCh, nil
}

func (f *fakeJournalBackend) lastQuery() resource.Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		return resource.Query{}
	}
	return f.queries[len(f.queries)-1]
}

// journalFixture builds a loaded journal view over two scripted entries.
func journalFixture(t *testing.T) (*Journal, *fakeJournalBackend) {
	t.Helper()
	base := fixedNow.Add(-time.Hour)
	fb := &fakeJournalBackend{rows: []resource.Resource{
		journalRow(base.Add(10*time.Minute), "err", resource.StatusCrit, "nginx", "upstream timed out"),
		journalRow(base.Add(20*time.Minute), "info", resource.StatusOK, "nginx", "started worker"),
	}}
	j := NewJournal(testEnv(), fb, resource.Query{
		Kind:   linux.KindJournal,
		Hosts:  []resource.HostID{"web-01"},
		Args:   map[string]string{"unit": "nginx.service"},
		Window: resource.TimeWindow{From: base, ToNow: true, Label: "-1h"},
	})
	return drainJournal(t, j), fb
}

// drainJournal drives Init and every follow-up ListBatch command by hand.
func drainJournal(t *testing.T, j *Journal) *Journal {
	t.Helper()
	cmd := j.Init()
	for i := 0; cmd != nil; i++ {
		if i > 5000 {
			t.Fatal("stream did not close")
		}
		view, next := j.Update(cmd())
		var ok bool
		if j, ok = view.(*Journal); !ok {
			t.Fatalf("Update returned %T, want *Journal", view)
		}
		cmd = next
	}
	return j
}

// pressJournal sends one key to the view.
func pressJournal(t *testing.T, j *Journal, msg tea.KeyMsg) (*Journal, tea.Cmd) {
	t.Helper()
	view, cmd := j.Update(msg)
	next, ok := view.(*Journal)
	if !ok {
		t.Fatalf("Update returned %T, want *Journal", view)
	}
	return next, cmd
}

// runeKey is one printable key.
func runeKey(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestJournalRendersPriorities(t *testing.T) {
	j, _ := journalFixture(t)

	if len(j.rows) != 2 {
		t.Fatalf("loaded %d rows, want 2", len(j.rows))
	}
	if j.cursor != 1 {
		t.Errorf("cursor = %d, want the last line (1)", j.cursor)
	}
	out := j.Render(100, 10)
	for _, want := range []string{"TIME", "PRI", "SOURCE", "MESSAGE", "err", "info", "nginx", "upstream timed out", "started worker"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "\n"); got != 9 {
		t.Errorf("render has %d newlines, want 9 (exactly 10 lines)", got)
	}
	banner := strings.SplitN(out, "\n", 2)[0]
	for _, want := range []string{"2 lines", "-1h → now", "-u nginx.service"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner %q missing %q", banner, want)
		}
	}

	// The banner reports the maximum priority once it is narrowed.
	j.maxPri = 4
	j.q.Args["priority"] = "4"
	if b := j.Render(100, 10); !strings.Contains(b, "p≤4") {
		t.Errorf("banner missing p≤4: %q", b)
	}
}

func TestJournalFollowAppends(t *testing.T) {
	j, fb := journalFixture(t)
	fb.followCh = make(chan linux.Entry, 4)

	base := fixedNow
	fb.followCh <- linux.Entry{Time: base.Add(time.Minute), Priority: 3, Source: "nginx", Message: "new error"}
	fb.followCh <- linux.Entry{Time: base.Add(2 * time.Minute), Priority: 6, Source: "nginx", Message: "new info"}

	var cmd tea.Cmd
	j, cmd = pressJournal(t, j, runeKey("w"))
	if cmd == nil {
		t.Fatal("w returned no command")
	}
	if !j.following {
		t.Fatal("following is false after w")
	}
	if len(fb.follows) != 1 {
		t.Fatalf("FollowJournal called %d times, want 1", len(fb.follows))
	}
	if o := fb.follows[0]; o.Unit != "nginx.service" || o.MaxPriority != 7 || !o.Follow {
		t.Errorf("follow options = %+v", o)
	}

	// The batch is bounded by NextBatch's 50 ms window; one entry is enough.
	view, next := j.Update(cmd())
	var ok bool
	if j, ok = view.(*Journal); !ok {
		t.Fatalf("Update returned %T", view)
	}
	if next == nil {
		t.Fatal("open follow stream returned no next batch command")
	}
	if len(j.rows) != 4 {
		t.Fatalf("rows = %d after follow, want 4", len(j.rows))
	}
	if j.cursor != len(j.view)-1 {
		t.Errorf("cursor = %d, want pinned to the end (%d)", j.cursor, len(j.view)-1)
	}
	if out := j.Render(100, 10); !strings.Contains(out, "following") {
		t.Errorf("banner missing the following badge: %q", strings.SplitN(out, "\n", 2)[0])
	}

	// Turning follow off cancels it.
	j, cmd = pressJournal(t, j, runeKey("w"))
	if j.following {
		t.Fatal("following is still true after w")
	}
	if cmd == nil {
		t.Fatal("turning follow off should flash")
	}
	_ = j.Close()
}

func TestJournalFollowNeedsSingleHost(t *testing.T) {
	fb := &fakeJournalBackend{}
	j := NewJournal(testEnv(), fb, resource.Query{Kind: linux.KindJournal, Hosts: []resource.HostID{"a", "b"}})
	if _, cmd := pressJournal(t, j, runeKey("w")); cmd == nil {
		t.Fatal("w over two hosts should flash a warning")
	}
	if len(fb.follows) != 0 {
		t.Fatal("follow started for more than one host")
	}
}

func TestJournalFilter(t *testing.T) {
	j, _ := journalFixture(t)

	j, _ = pressJournal(t, j, runeKey("/"))
	if !j.Capturing() {
		t.Fatal("filter does not capture after /")
	}
	for _, r := range "timed" {
		j, _ = pressJournal(t, j, runeKey(string(r)))
	}
	j, _ = pressJournal(t, j, tea.KeyMsg{Type: tea.KeyEnter})
	if j.Capturing() {
		t.Fatal("filter still capturing after enter")
	}
	if len(j.view) != 1 {
		t.Fatalf("filtered rows = %d, want 1", len(j.view))
	}
	out := j.Render(100, 10)
	if !strings.Contains(out, "upstream timed out") || strings.Contains(out, "started worker") {
		t.Errorf("filter did not narrow the rows:\n%s", out)
	}

	// ~ makes the filter a regex over source + message.
	j, _ = pressJournal(t, j, runeKey("/"))
	j, _ = pressJournal(t, j, tea.KeyMsg{Type: tea.KeyCtrlU})
	for _, r := range "~^nginx.*start" {
		j, _ = pressJournal(t, j, runeKey(string(r)))
	}
	j, _ = pressJournal(t, j, tea.KeyMsg{Type: tea.KeyEnter})
	if len(j.view) != 1 {
		t.Fatalf("regex rows = %d, want 1", len(j.view))
	}
	if !strings.Contains(j.Render(100, 10), "started worker") {
		t.Error("regex filter kept the wrong row")
	}
}

func TestJournalReloadsOnWindow(t *testing.T) {
	j, fb := journalFixture(t)
	if len(fb.queries) != 1 {
		t.Fatalf("initial List calls = %d, want 1", len(fb.queries))
	}

	newWindow := resource.TimeWindow{From: fixedNow.Add(-2 * time.Hour), ToNow: true, Label: "-2h"}
	view, cmd := j.Update(tui.WindowChangedMsg{Window: newWindow})
	var ok bool
	if j, ok = view.(*Journal); !ok {
		t.Fatalf("Update returned %T", view)
	}
	if cmd == nil {
		t.Fatal("window change did not reload")
	}
	j = drainJournalCmd(t, j, cmd)
	if len(fb.queries) != 2 {
		t.Fatalf("List calls = %d after window change, want 2", len(fb.queries))
	}
	if q := fb.lastQuery(); q.Window.Label != "-2h" {
		t.Errorf("reloaded window label = %q, want -2h", q.Window.Label)
	}
	if b := strings.SplitN(j.Render(100, 10), "\n", 2)[0]; !strings.Contains(b, "-2h → now") {
		t.Errorf("banner = %q, want the new window", b)
	}
}

// drainJournalCmd finishes a stream already started by Update.
func drainJournalCmd(t *testing.T, j *Journal, cmd tea.Cmd) *Journal {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i > 5000 {
			t.Fatal("stream did not close")
		}
		view, next := j.Update(cmd())
		var ok bool
		if j, ok = view.(*Journal); !ok {
			t.Fatalf("Update returned %T", view)
		}
		cmd = next
	}
	return j
}

func TestJournalPriorityKeyReloads(t *testing.T) {
	j, fb := journalFixture(t)
	var cmd tea.Cmd
	j, cmd = pressJournal(t, j, runeKey("4"))
	if cmd == nil {
		t.Fatal("4 did not reload")
	}
	j = drainJournalCmd(t, j, cmd)
	if j.maxPri != 4 {
		t.Errorf("maxPri = %d, want 4", j.maxPri)
	}
	if q := fb.lastQuery(); q.Args["priority"] != "4" {
		t.Errorf("reloaded priority arg = %q, want 4", q.Args["priority"])
	}
	if b := strings.SplitN(j.Render(100, 10), "\n", 2)[0]; !strings.Contains(b, "p≤4") {
		t.Errorf("banner = %q, want p≤4", b)
	}
}

func TestJournalCommandLine(t *testing.T) {
	j, _ := journalFixture(t)
	want := "journal --host web-01 --arg unit=nginx.service"
	if got := j.CommandLine(); got != want {
		t.Errorf("CommandLine = %q, want %q", got, want)
	}
}

func TestJournalCopyLines(t *testing.T) {
	j, _ := journalFixture(t)
	lines := j.CopyLines()
	if len(lines) != 3 {
		t.Fatalf("CopyLines = %d lines, want header + 2", len(lines))
	}
	if !strings.Contains(lines[1], "upstream timed out") {
		t.Errorf("first row = %q", lines[1])
	}
}
