package views

import (
	"context"
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend"
	"jumpr/internal/backend/fake"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
)

// watchStub is a backend that can also be watched: the test feeds the events
// channel and the table has to fold them into its rows.
type watchStub struct {
	backend.Backend
	events   chan backend.Event
	watchErr error
	watched  chan resource.Query // one Query per Watch call
}

// Watch hands the table the event channel the test controls.
func (w *watchStub) Watch(ctx context.Context, q resource.Query) (<-chan backend.Event, error) {
	if w.watchErr != nil {
		return nil, w.watchErr
	}
	if w.watched != nil {
		w.watched <- q
	}
	return w.events, nil
}

// newWatchTable builds a table over the fake host list, watchable when
// watchable is set, with a stub watcher behind it.
func newWatchTable(t *testing.T, watchable bool) (*Table, *watchStub) {
	t.Helper()
	fb := fake.New(fake.Options{})
	spec := hostKind(t, fb)
	spec.Watchable = watchable
	stub := &watchStub{
		Backend: fb,
		events:  make(chan backend.Event, 16),
		watched: make(chan resource.Query, 4),
	}
	return NewTable(testEnv(), stub, spec, resource.Query{Kind: "host"}), stub
}

// drainList drives the initial listing by hand and returns the command that
// follows it: the watch subscription, or nil when the table does not watch.
func drainList(t *testing.T, tbl *Table) (*Table, tea.Cmd) {
	t.Helper()
	cmd := tbl.Init()
	for i := 0; cmd != nil; i++ {
		if i > 5000 {
			t.Fatal("listing did not close")
		}
		msg := cmd()
		view, next := tbl.Update(msg)
		var ok bool
		if tbl, ok = view.(*Table); !ok {
			t.Fatalf("Update returned %T, want *Table", view)
		}
		if batch, isBatch := msg.(tui.ListBatchMsg); isBatch && batch.Closed {
			return tbl, next
		}
		cmd = next
	}
	return tbl, nil
}

// rowsByName indexes the table's rows by name.
func rowsByName(t *Table) map[string]resource.Resource {
	out := make(map[string]resource.Resource, len(t.rows))
	for _, r := range t.rows {
		out[r.Ref.Name] = r
	}
	return out
}

// TestTableWatchAppliesEvents: once the listing closes, a Watchable kind with
// a watching backend gets add, modify and delete events folded into the rows.
func TestTableWatchAppliesEvents(t *testing.T) {
	tbl, stub := newWatchTable(t, true)
	tbl, cmd := drainList(t, tbl)
	if cmd == nil {
		t.Fatal("no watch command after the listing closed")
	}
	before := len(tbl.rows)
	if before == 0 {
		t.Fatal("the listing produced no rows")
	}

	// The subscription asked for the table's own query.
	select {
	case q := <-stub.watched:
		if q.Kind != "host" {
			t.Errorf("Watch query kind = %q, want host", q.Kind)
		}
	default:
		t.Error("the backend was never asked to watch")
	}

	added := instantiate("stub-host-added")
	added.Fields["load"] = 0.5
	stub.events <- backend.Event{Type: backend.EventAdded, Resource: added}
	view, next := tbl.Update(cmd())
	tbl = view.(*Table)
	if next == nil {
		t.Fatal("the table stopped asking for events while the channel was open")
	}
	if got := len(tbl.rows); got != before+1 {
		t.Fatalf("rows = %d after an add, want %d", got, before+1)
	}
	if _, ok := rowsByName(tbl)["stub-host-added"]; !ok {
		t.Error("the added host is not in the rows")
	}

	// A modify replaces the row in place instead of adding a second one.
	modified := added
	modified.Fields = map[string]any{"load": 9.5}
	stub.events <- backend.Event{Type: backend.EventModified, Resource: modified}
	view, next = tbl.Update(next())
	tbl = view.(*Table)
	if got := len(tbl.rows); got != before+1 {
		t.Fatalf("rows = %d after a modify, want %d", got, before+1)
	}
	if got := rowsByName(tbl)["stub-host-added"].Fields["load"]; got != 9.5 {
		t.Errorf("load = %v, want the modified value", got)
	}

	// A delete drops the row.
	stub.events <- backend.Event{Type: backend.EventDeleted, Resource: modified}
	view, next = tbl.Update(next())
	tbl = view.(*Table)
	if got := len(tbl.rows); got != before {
		t.Fatalf("rows = %d after a delete, want %d", got, before)
	}
	if _, ok := rowsByName(tbl)["stub-host-added"]; ok {
		t.Error("the deleted host is still in the rows")
	}

	// Closing the channel ends the watch without touching the rows.
	close(stub.events)
	view, next = tbl.Update(next())
	tbl = view.(*Table)
	if next != nil {
		t.Error("the table kept watching a closed channel")
	}
	if got := len(tbl.rows); got != before {
		t.Errorf("rows = %d after the watch closed, want %d", got, before)
	}
}

// TestTableWatchStaleSeqIgnored: a batch from an earlier listing is dropped,
// so a refresh never mixes two streams.
func TestTableWatchStaleSeqIgnored(t *testing.T) {
	tbl, _ := newWatchTable(t, true)
	tbl, cmd := drainList(t, tbl)
	if cmd == nil {
		t.Fatal("no watch command after the listing closed")
	}
	before := len(tbl.rows)
	msg := watchBatchMsg{
		seq:    tbl.wseq - 1,
		events: []backend.Event{{Type: backend.EventAdded, Resource: instantiate("ghost-host")}},
	}
	view, next := tbl.Update(msg)
	tbl = view.(*Table)
	if next != nil {
		t.Error("a stale watch batch must not schedule another read")
	}
	if got := len(tbl.rows); got != before {
		t.Errorf("rows = %d, want %d: a stale batch must be ignored", got, before)
	}
}

// TestTableWatchNotStarted: a kind that is not Watchable, or a backend that is
// not a Watcher, leaves the table alone after the listing.
func TestTableWatchNotStarted(t *testing.T) {
	t.Run("kind not watchable", func(t *testing.T) {
		tbl, stub := newWatchTable(t, false)
		_, cmd := drainList(t, tbl)
		if cmd != nil {
			t.Error("a kind that is not Watchable must not be watched")
		}
		select {
		case <-stub.watched:
			t.Error("the backend was asked to watch a kind that is not Watchable")
		default:
		}
	})
	t.Run("backend not a watcher", func(t *testing.T) {
		fb := fake.New(fake.Options{})
		spec := hostKind(t, fb)
		spec.Watchable = true
		tbl := NewTable(testEnv(), fb, spec, resource.Query{Kind: "host"})
		_, cmd := drainList(t, tbl)
		if cmd != nil {
			t.Error("a backend without Watch must not be watched")
		}
	})
}

// TestTableWatchErrorFlashes: a subscription that fails shows a warning and
// keeps the rows the listing produced.
func TestTableWatchErrorFlashes(t *testing.T) {
	fb := fake.New(fake.Options{})
	spec := hostKind(t, fb)
	spec.Watchable = true
	stub := &watchStub{Backend: fb, events: make(chan backend.Event), watchErr: errors.New("forbidden")}
	tbl := NewTable(testEnv(), stub, spec, resource.Query{Kind: "host"})
	tbl, cmd := drainList(t, tbl)
	if cmd == nil {
		t.Fatal("no command after the listing closed")
	}
	msg := cmd()
	flash, ok := msg.(tui.FlashMsg)
	if !ok {
		t.Fatalf("msg = %T, want a FlashMsg", msg)
	}
	if flash.Level != tui.FlashWarn {
		t.Errorf("flash level = %v, want warn", flash.Level)
	}
	if len(tbl.rows) == 0 {
		t.Error("the rows of the listing must survive a watch failure")
	}
}
