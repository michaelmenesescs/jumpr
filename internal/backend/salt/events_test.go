package salt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
)

const (
	eventJobNewLine = `salt/job/20261113010101123456/new	{"fun": "state.apply", "arg": ["nginx"], "tgt": "web-01", "jid": "20261113010101123456", "user": "root", "_stamp": "2026-11-13T01:01:01.123456"}`
	eventJobRetLine = `salt/job/20261113010101123456/ret/web-01	{"fun": "state.apply", "jid": "20261113010101123456", "id": "web-01", "retcode": 0, "success": true, "_stamp": "2026-11-13T01:01:04.654321"}`
)

// eventBackend is a Salt backend over a scripted transport.
func eventBackend(ex transport.Executor) *Backend {
	return New(Options{
		Runner: NewRunner(ex, master, RunnerOptions{Sudo: true}),
		Master: master,
	})
}

// drainEvents reads ch to its end and returns every event.
func drainEvents(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatal("the event stream did not end")
		}
	}
}

func TestEventsKinds(t *testing.T) {
	tests := []struct {
		tag  string
		want string
	}{
		{"salt/job/20261113010101123456/new", EventJobNew},
		{"salt/job/20261113010101123456/ret/web-01", EventJobRet},
		{"salt/auth", EventAuth},
		{"salt/auth/web-01", EventAuth},
		{"minion_start", EventMinionStart},
		{"salt/minion/web-01/start", EventMinionStart},
		{"salt/beacon/web-01/disk/load", EventBeacon},
		{"salt/key", EventKey},
		{"salt/run/20261113010101123456/new", EventRunner},
		{"salt/unknown/web-01/thing", EventOther},
		{"", EventOther},
	}
	for _, tc := range tests {
		if got := EventKind(tc.tag); got != tc.want {
			t.Errorf("EventKind(%q) = %q, want %q", tc.tag, got, tc.want)
		}
	}
}

func TestEventsParseLine(t *testing.T) {
	ev, ok := ParseEventLine([]byte(eventJobRetLine))
	if !ok {
		t.Fatal("the job return line did not parse")
	}
	if ev.Tag != "salt/job/20261113010101123456/ret/web-01" {
		t.Errorf("Tag = %q", ev.Tag)
	}
	if ev.Kind != EventJobRet {
		t.Errorf("Kind = %q, want %q", ev.Kind, EventJobRet)
	}
	if ev.Minion != "web-01" {
		t.Errorf("Minion = %q, want web-01", ev.Minion)
	}
	if ev.JID != "20261113010101123456" {
		t.Errorf("JID = %q", ev.JID)
	}
	if ev.Fun != "state.apply" {
		t.Errorf("Fun = %q, want state.apply", ev.Fun)
	}
	want := time.Date(2026, 11, 13, 1, 1, 4, 654321000, time.UTC)
	if !ev.Time.Equal(want) {
		t.Errorf("Time = %v, want %v", ev.Time, want)
	}
	if len(ev.Data) == 0 || !strings.Contains(string(ev.Data), `"retcode": 0`) {
		t.Errorf("Data = %s, want the raw payload", ev.Data)
	}
}

func TestEventsParseLineSpaceSeparator(t *testing.T) {
	line := "salt/job/20261113010324555556/new {\"fun\": \"test.ping\", \"jid\": \"20261113010324555556\", \"_stamp\": \"2026-11-13T01:03:24.000000\"}"
	ev, ok := ParseEventLine([]byte(line))
	if !ok {
		t.Fatal("a single-space separator was not tolerated")
	}
	if ev.Kind != EventJobNew || ev.Fun != "test.ping" {
		t.Errorf("event = %+v", ev)
	}
}

func TestEventsParseLineSkipsOddLines(t *testing.T) {
	for _, line := range []string{
		"",
		"just-a-tag-without-a-payload",
		"salt/job/1/new\t{not json",
		"salt/beacon/web-02/load/1\t[1, 2, 3]",
		"salt/job/1/new\t\"just a string\"",
	} {
		if _, ok := ParseEventLine([]byte(line)); ok {
			t.Errorf("line %q parsed, want it skipped", line)
		}
	}
}

// TestEventsGolden parses the captured sample: every real line becomes an
// event, the odd ones are skipped.
func TestEventsGolden(t *testing.T) {
	path := filepath.Join("..", "..", "..", "testdata", "salt", "event-sample.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("golden not captured")
		}
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	var events []Event
	for _, line := range lines {
		if ev, ok := ParseEventLine([]byte(line)); ok {
			events = append(events, ev)
		}
	}
	if len(lines) != 16 {
		t.Fatalf("golden has %d lines, want 16", len(lines))
	}
	if len(events) != 13 {
		t.Fatalf("parsed %d events, want 13: %+v", len(events), events)
	}
	kinds := map[string]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
		if ev.Tag == "" {
			t.Errorf("event without a tag: %+v", ev)
		}
	}
	want := map[string]int{
		EventJobNew: 2, EventJobRet: 3, EventAuth: 1, EventMinionStart: 2,
		EventBeacon: 1, EventKey: 1, EventRunner: 2, EventOther: 1,
	}
	for kind, n := range want {
		if kinds[kind] != n {
			t.Errorf("kind %s = %d, want %d (all: %v)", kind, kinds[kind], n, kinds)
		}
	}
	// The pillar return is identified by its function, which the view hides.
	var pillar *Event
	for i := range events {
		if events[i].Fun == "pillar.items" {
			pillar = &events[i]
		}
	}
	if pillar == nil {
		t.Fatal("the golden has no pillar return")
	}
	if !HiddenFunction(pillar.Fun) {
		t.Errorf("pillar.items is not treated as hidden")
	}
}

func TestEventsStream(t *testing.T) {
	ex := transporttest.New().On("state.event", transporttest.Reply{
		Lines: []string{eventJobNewLine, eventJobRetLine, "not an event"},
	})
	b := eventBackend(ex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := b.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	events := drainEvents(t, ch)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (%+v)", len(events), events)
	}
	if events[0].Kind != EventJobNew || events[1].Kind != EventJobRet {
		t.Errorf("kinds = %q, %q", events[0].Kind, events[1].Kind)
	}

	calls := ex.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Host != master {
		t.Errorf("host = %q, want %q", calls[0].Host, master)
	}
	if got, want := calls[0].Command.Script, "salt-run state.event pretty=False"; got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
	if !calls[0].Command.Sudo {
		t.Error("the event stream must run with sudo")
	}
}

// TestEventsOneStreamPerSession: two panes subscribe to the one stream the
// master's connection budget allows.
func TestEventsOneStreamPerSession(t *testing.T) {
	ex := transporttest.New().On("state.event", transporttest.Reply{
		Lines: []string{eventJobNewLine},
		Delay: 100 * time.Millisecond,
	})
	b := eventBackend(ex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, err := b.Events(ctx)
	if err != nil {
		t.Fatalf("first Events: %v", err)
	}
	second, err := b.Events(ctx)
	if err != nil {
		t.Fatalf("second Events: %v", err)
	}
	if n := len(drainEvents(t, first)); n != 1 {
		t.Errorf("first pane got %d events, want 1", n)
	}
	if n := len(drainEvents(t, second)); n != 1 {
		t.Errorf("second pane got %d events, want 1", n)
	}
	if calls := ex.Calls(); len(calls) != 1 {
		t.Fatalf("started %d streams, want 1 (one per session)", len(calls))
	}
}

// TestEventsReconnectAfterEnd: when the stream ends (a master restart) a new
// subscription starts a new one.
func TestEventsReconnectAfterEnd(t *testing.T) {
	ex := transporttest.New().On("state.event", transporttest.Reply{
		Lines: []string{eventJobNewLine},
	})
	b := eventBackend(ex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, err := b.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	drainEvents(t, first)

	second, err := b.Events(ctx)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if n := len(drainEvents(t, second)); n != 1 {
		t.Errorf("reconnected pane got %d events, want 1", n)
	}
	if calls := ex.Calls(); len(calls) != 2 {
		t.Fatalf("started %d streams, want 2", len(calls))
	}
}

func TestEventsNotConfigured(t *testing.T) {
	if _, err := New(Options{}).Events(context.Background()); !errs.Is(err, errs.Config) {
		t.Fatalf("Events without a master = %v, want a config error", err)
	}
	// A runner whose executor cannot stream means no events either.
	b := New(Options{
		Runner: NewRunner(execOnly{}, master, RunnerOptions{}),
		Master: master,
	})
	if _, err := b.Events(context.Background()); !errs.Is(err, errs.Config) {
		t.Fatalf("Events without a streamer = %v, want a config error", err)
	}
}

// execOnly is an Executor that is not a Streamer.
type execOnly struct{}

func (execOnly) Name() string { return "exec-only" }

func (execOnly) Exec(context.Context, resource.HostID, transport.Command) (transport.ExecResult, error) {
	return transport.ExecResult{}, nil
}

// TestEventsOwnRunner: jumpr's own polling runners are recognisable, so the
// screen can hide them by default.
func TestEventsOwnRunner(t *testing.T) {
	for _, fun := range []string{"manage.status", "manage.versions", "cache.grains", "cache.mine", "jobs.active", "jobs.list_job", "jobs.list_jobs"} {
		if !OwnRunner(fun) {
			t.Errorf("OwnRunner(%q) = false, want true", fun)
		}
	}
	for _, fun := range []string{"", "state.apply", "pillar.items"} {
		if OwnRunner(fun) {
			t.Errorf("OwnRunner(%q) = true, want false", fun)
		}
	}
}
