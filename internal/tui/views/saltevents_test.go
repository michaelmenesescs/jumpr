package views

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/linux"
	"jumpr/internal/backend/salt"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
)

// ---- fixtures ----

// fakeEventsSource is the master's bus without a master: each Events call
// returns a fresh channel the test fills.
type fakeEventsSource struct {
	mu     sync.Mutex
	ch     chan salt.Event
	starts int
	err    error
	hosts  map[string]resource.HostID
}

func newFakeEventsSource() *fakeEventsSource {
	return &fakeEventsSource{hosts: map[string]resource.HostID{
		"web-01": resource.HostID("web-01.example.internal"),
		"web-02": resource.HostID("web-02.example.internal"),
	}}
}

func (f *fakeEventsSource) Events(_ context.Context) (<-chan salt.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.err != nil {
		return nil, f.err
	}
	f.ch = make(chan salt.Event, 256)
	return f.ch, nil
}

func (f *fakeEventsSource) push(events ...salt.Event) {
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	if ch == nil {
		panic("push before Events")
	}
	for _, ev := range events {
		ch <- ev
	}
}

func (f *fakeEventsSource) end() {
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (f *fakeEventsSource) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

// MinionHost maps a minion onto its inventory host.
func (f *fakeEventsSource) MinionHost(id string) (resource.HostID, bool) {
	h, ok := f.hosts[id]
	return h, ok
}

// viewEvent parses one event line with the real parser, so the fixtures are the
// master's own output shape.
func viewEvent(t *testing.T, tag, payload string) salt.Event {
	t.Helper()
	ev, ok := salt.ParseEventLine([]byte(tag + "\t" + payload))
	if !ok {
		t.Fatalf("fixture %s %s did not parse", tag, payload)
	}
	return ev
}

// saltEventsEnv is the environment the event screen runs in.
func saltEventsEnv() *tui.Env {
	return &tui.Env{
		Session: &tui.SessionState{Name: "test"},
		Now:     func() time.Time { return time.Unix(1730000000, 0) },
	}
}

// newSaltEventsView builds the view and returns the command that awaits its
// first batch.
func newSaltEventsView(t *testing.T) (*SaltEvents, *fakeEventsSource, tea.Cmd) {
	t.Helper()
	src := newFakeEventsSource()
	v := NewSaltEvents(saltEventsEnv(), src)
	start := v.Init()
	if start == nil {
		t.Fatal("Init returned no command")
	}
	_, pending := v.Update(start())
	if pending == nil {
		t.Fatal("the started stream did not ask for a batch")
	}
	return v, src, pending
}

// pumpSaltEvents pushes events and delivers them to the view, returning the
// command that awaits the next batch.
func pumpSaltEvents(t *testing.T, v *SaltEvents, src *fakeEventsSource, pending tea.Cmd, events ...salt.Event) tea.Cmd {
	t.Helper()
	src.push(events...)
	_, next := v.Update(pending())
	if next == nil {
		t.Fatal("the stream stopped asking for batches")
	}
	return next
}

// pressSaltEvents sends one key press to the view.
func pressSaltEvents(t *testing.T, v *SaltEvents, key string) tea.Cmd {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case " ":
		msg = tea.KeyMsg{Type: tea.KeySpace}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	_, cmd := v.Update(msg)
	return cmd
}

// jobNew, jobRetOK, jobRetFail and the rest are the master's event shapes.
func jobNew(t *testing.T, jid, fun, minion string) salt.Event {
	t.Helper()
	return viewEvent(t, "salt/job/"+jid+"/new",
		`{"fun": "`+fun+`", "jid": "`+jid+`", "tgt": "`+minion+`", "_stamp": "2026-11-13T01:01:01.000000"}`)
}

func jobRet(t *testing.T, jid, fun, minion string, retcode int) salt.Event {
	t.Helper()
	rc := "0"
	if retcode != 0 {
		rc = "1"
	}
	return viewEvent(t, "salt/job/"+jid+"/ret/"+minion,
		`{"fun": "`+fun+`", "jid": "`+jid+`", "id": "`+minion+`", "retcode": `+rc+`, "_stamp": "2026-11-13T01:01:04.000000"}`)
}

// ---- tests ----

func TestSaltEventsFollowsTheNewestEvent(t *testing.T) {
	v, src, pending := newSaltEventsView(t)
	pending = pumpSaltEvents(t, v, src, pending,
		jobNew(t, "20261113010101123456", "state.apply", "web-01"),
		jobRet(t, "20261113010101123456", "state.apply", "web-01", 0),
	)
	if len(v.view) != 2 {
		t.Fatalf("rows = %d, want 2", len(v.view))
	}
	if v.cursor != 1 {
		t.Errorf("cursor = %d, want the newest row", v.cursor)
	}
	screen := v.Render(100, 10)
	for _, want := range []string{"web-01", "state.apply ret ok", "job-ret", "TIME"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q:\n%s", want, screen)
		}
	}
}

func TestSaltEventsSummaryOutcomes(t *testing.T) {
	v, src, pending := newSaltEventsView(t)
	pending = pumpSaltEvents(t, v, src, pending,
		jobRet(t, "20261113010101987654", "state.apply", "web-02", 1),
		viewEvent(t, "salt/auth", `{"act": "pend", "id": "web-03"}`),
		viewEvent(t, "salt/key", `{"act": "accept", "id": "web-03"}`),
	)
	_ = pending
	if got := saltEventSummary(v.view[0]); got != "state.apply ret rc=1" {
		t.Errorf("summary = %q, want %q", got, "state.apply ret rc=1")
	}
	if got := saltEventSummary(v.view[1]); got != "auth pend" {
		t.Errorf("summary = %q, want %q", got, "auth pend")
	}
	if got := saltEventSummary(v.view[2]); got != "key accepted" {
		t.Errorf("summary = %q, want %q", got, "key accepted")
	}
}

func TestSaltEventsKindToggles(t *testing.T) {
	v, src, pending := newSaltEventsView(t)
	pending = pumpSaltEvents(t, v, src, pending,
		jobNew(t, "20261113010101123456", "state.apply", "web-01"),
		viewEvent(t, "salt/auth", `{"act": "pend", "id": "web-03"}`),
		viewEvent(t, "salt/beacon/web-01/disk/load", `{"id": "web-01"}`),
	)
	// 1 hides job-new.
	pressSaltEvents(t, v, "1")
	if len(v.view) != 2 {
		t.Fatalf("rows after hiding job-new = %d, want 2", len(v.view))
	}
	for _, ev := range v.view {
		if ev.Kind == salt.EventJobNew {
			t.Error("a job-new event is still visible")
		}
	}
	// 1 again shows it.
	pressSaltEvents(t, v, "1")
	if len(v.view) != 3 {
		t.Fatalf("rows after showing job-new = %d, want 3", len(v.view))
	}
	// A filter is applied on top of the kind toggles.
	v.setFilter("beacon")
	if len(v.view) != 1 || v.view[0].Kind != salt.EventBeacon {
		t.Fatalf("filtered rows = %+v, want the beacon only", v.view)
	}
	v.setFilter("")
	_ = pending
}

func TestSaltEventsHidesOwnPollingRunners(t *testing.T) {
	v, src, pending := newSaltEventsView(t)
	pending = pumpSaltEvents(t, v, src, pending,
		viewEvent(t, "salt/run/20261113010320123456/new", `{"fun": "manage.status", "jid": "20261113010320123456"}`),
		viewEvent(t, "salt/run/20261113010321987654/new", `{"fun": "state.orchestrate", "jid": "20261113010321987654"}`),
		jobNew(t, "20261113010101123456", "state.apply", "web-01"),
	)
	// jumpr's own polling runner is hidden by default.
	if len(v.view) != 2 {
		t.Fatalf("rows = %d, want 2 (%+v)", len(v.view), v.view)
	}
	for _, ev := range v.view {
		if ev.Kind == salt.EventRunner && ev.Fun == "manage.status" {
			t.Error("jumpr's own runner event is visible")
		}
	}
	// h shows them.
	pressSaltEvents(t, v, "h")
	if len(v.view) != 3 {
		t.Fatalf("rows after h = %d, want 3", len(v.view))
	}
	_ = pending
}

func TestSaltEventsFilterByText(t *testing.T) {
	v, src, pending := newSaltEventsView(t)
	pending = pumpSaltEvents(t, v, src, pending,
		jobRet(t, "20261113010101123456", "state.apply", "web-01", 0),
		jobRet(t, "20261113010101987654", "state.apply", "web-02", 1),
	)
	// "/" then "web-02" then enter.
	pressSaltEvents(t, v, "/")
	for _, r := range "web-02" {
		pressSaltEvents(t, v, string(r))
	}
	pressSaltEvents(t, v, "enter")
	if len(v.view) != 1 || v.view[0].Minion != "web-02" {
		t.Fatalf("filtered rows = %+v, want web-02", v.view)
	}
	if v.Capturing() {
		t.Error("enter did not close the filter prompt")
	}
	// a "~" filter is a regex
	v.setFilter("~web-0[12]")
	if len(v.view) != 2 {
		t.Errorf("regex filter rows = %d, want 2", len(v.view))
	}
	// esc clears it
	v.setFilter("~[")
	if len(v.view) != 0 {
		t.Errorf("a bad regex showed %d rows, want 0", len(v.view))
	}
	pressSaltEvents(t, v, "esc")
	if v.filter != "" || len(v.view) != 2 {
		t.Errorf("filter = %q, rows = %d, want cleared and 2", v.filter, len(v.view))
	}
	_ = pending
}

func TestSaltEventsPauseKeepsBuffering(t *testing.T) {
	v, src, pending := newSaltEventsView(t)
	pending = pumpSaltEvents(t, v, src, pending,
		jobNew(t, "20261113010101123456", "state.apply", "web-01"),
		jobNew(t, "20261113010101987654", "state.apply", "web-02"),
	)
	if v.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", v.cursor)
	}
	pressSaltEvents(t, v, " ")
	if !v.paused {
		t.Fatal("space did not pause the screen")
	}
	pending = pumpSaltEvents(t, v, src, pending,
		jobRet(t, "20261113010101123456", "state.apply", "web-01", 0),
		jobRet(t, "20261113010101987654", "state.apply", "web-02", 1),
		jobNew(t, "20261113010101555555", "test.ping", "web-01"),
	)
	if v.cursor != 1 {
		t.Errorf("cursor = %d while paused, want it to stay at 1", v.cursor)
	}
	if len(v.events) != 5 || len(v.view) != 5 {
		t.Fatalf("buffer = %d, rows = %d, want 5 and 5", len(v.events), len(v.view))
	}
	if !strings.Contains(v.Render(100, 10), saltEventsPaused) {
		t.Error("the banner does not say the screen is paused")
	}
	// Resuming follows the newest event.
	pressSaltEvents(t, v, " ")
	if v.paused || v.cursor != 4 {
		t.Errorf("after resume: paused = %v, cursor = %d, want false and 4", v.paused, v.cursor)
	}
}

func TestSaltEventsBufferCap(t *testing.T) {
	v, _, _ := newSaltEventsView(t)
	events := make([]salt.Event, saltEventsMax+10)
	for i := range events {
		events[i] = salt.Event{Tag: "salt/unknown/x", Kind: salt.EventOther}
	}
	v.add(events)
	if len(v.events) != saltEventsMax {
		t.Fatalf("buffer = %d, want %d", len(v.events), saltEventsMax)
	}
	if v.cursor < 0 || v.cursor >= len(v.view) {
		t.Errorf("cursor = %d is outside the %d rows", v.cursor, len(v.view))
	}
}

func TestSaltEventsEndedThenReconnect(t *testing.T) {
	v, src, pending := newSaltEventsView(t)
	pending = pumpSaltEvents(t, v, src, pending,
		jobNew(t, "20261113010101123456", "state.apply", "web-01"),
	)
	src.end()
	_, next := v.Update(pending())
	if next != nil {
		t.Error("a closed stream still asked for a batch")
	}
	if !v.ended {
		t.Fatal("the view did not notice the stream ended")
	}
	screen := v.Render(100, 10)
	if !strings.Contains(screen, "stream ended — r to reconnect") {
		t.Errorf("screen does not offer a reconnect:\n%s", screen)
	}
	// r reconnects.
	cmd := pressSaltEvents(t, v, "r")
	if cmd == nil {
		t.Fatal("r did not start a reconnect")
	}
	if _, next := v.Update(cmd()); next == nil {
		t.Fatal("the reconnected stream did not ask for a batch")
	}
	if src.startCount() != 2 {
		t.Errorf("starts = %d, want 2", src.startCount())
	}
	if v.ended {
		t.Error("the view still shows the ended state after reconnecting")
	}
}

func TestSaltEventsEnterPivots(t *testing.T) {
	v, src, pending := newSaltEventsView(t)
	pending = pumpSaltEvents(t, v, src, pending,
		jobRet(t, "20261113010101123456", "state.apply", "web-01", 0),
	)
	cmd := pressSaltEvents(t, v, "enter")
	if cmd == nil {
		t.Fatal("enter on a job event did nothing")
	}
	msg, ok := cmd().(tui.OpenQueryMsg)
	if !ok {
		t.Fatalf("enter emitted %T, want OpenQueryMsg", msg)
	}
	if msg.Query.Kind != KindSaltJobDetail {
		t.Errorf("kind = %q, want %q", msg.Query.Kind, KindSaltJobDetail)
	}
	if msg.Query.Args["jid"] != "20261113010101123456" {
		t.Errorf("jid = %q", msg.Query.Args["jid"])
	}

	// A minion event without a job id opens the host.
	pending = pumpSaltEvents(t, v, src, pending,
		viewEvent(t, "salt/minion/web-01/start", `{"id": "web-01"}`),
	)
	cmd = pressSaltEvents(t, v, "enter")
	if cmd == nil {
		t.Fatal("enter on a minion event did nothing")
	}
	msg, ok = cmd().(tui.OpenQueryMsg)
	if !ok {
		t.Fatalf("enter emitted %T, want OpenQueryMsg", msg)
	}
	if msg.Query.Kind != linux.KindHostOverview {
		t.Errorf("kind = %q, want %q", msg.Query.Kind, linux.KindHostOverview)
	}
	if len(msg.Query.Hosts) != 1 || msg.Query.Hosts[0] != "web-01.example.internal" {
		t.Errorf("hosts = %v, want web-01.example.internal", msg.Query.Hosts)
	}
}

func TestSaltEventsPillarReturnIsHidden(t *testing.T) {
	v, src, pending := newSaltEventsView(t)
	pending = pumpSaltEvents(t, v, src, pending,
		viewEvent(t, "salt/job/20261113010322555555/ret/web-01",
			`{"fun": "pillar.items", "jid": "20261113010322555555", "id": "web-01", "return": {"db:password": "s3cr3t"}, "retcode": 0, "_stamp": "2026-11-13T01:03:25.000000"}`),
	)
	_ = pending
	if got := saltEventSummary(v.view[0]); got != "pillar.items ret hidden" {
		t.Errorf("summary = %q, want %q", got, "pillar.items ret hidden")
	}
	screen := v.Render(120, 10)
	for _, leaked := range []string{"s3cr3t", "db:password"} {
		if strings.Contains(screen, leaked) {
			t.Errorf("the screen shows the pillar value %q", leaked)
		}
	}
	for _, line := range v.CopyLines() {
		if strings.Contains(line, "s3cr3t") {
			t.Errorf("copy mode carries the pillar value: %q", line)
		}
	}
}

func TestSaltEventsFactoryAndCommand(t *testing.T) {
	f, ok := Factories()[KindSaltEvents]
	if !ok {
		t.Fatal("Factories() has no salt-events view")
	}
	if _, err := f(saltEventsEnv(), resource.Query{Kind: KindSaltEvents}); err == nil {
		t.Error("the factory built a view without a backend")
	}
}
