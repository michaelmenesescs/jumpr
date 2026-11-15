package mux

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/errs"
)

// stateStub is a Content whose saved state is fixed, so a round trip can be
// compared field by field.
type stateStub struct {
	stub
	st ContentState
}

func (s *stateStub) State() ContentState { return s.st }

// persistFactory records every state it is asked to build and remembers which
// panes it refused to build, so Restore's fallback can be observed.
type persistFactory struct {
	states   map[PaneID]ContentState
	refuse   map[PaneID]bool
	built    []PaneID
	defaults int
}

func newPersistFactory(refuse ...PaneID) *persistFactory {
	f := &persistFactory{states: map[PaneID]ContentState{}, refuse: map[PaneID]bool{}}
	for _, id := range refuse {
		f.refuse[id] = true
	}
	return f
}

func (f *persistFactory) make(id PaneID, st ContentState) (Content, tea.Cmd) {
	isDefault := st.Type == "" && st.Host == "" && len(st.Commands) == 0
	if isDefault {
		f.defaults++
	}
	// A refused pane cannot be built from its saved state; the default pane
	// for the same id still works.
	if f.refuse[id] && !isDefault {
		return nil, nil
	}
	f.states[id] = st
	f.built = append(f.built, id)
	c := &stateStub{st: st}
	c.stub.id = id
	return c, nil
}

// persistModel builds a two-pane session in one window, each pane with a
// distinctive state.
func persistModel(t *testing.T, f Factory) *Model {
	t.Helper()
	s := NewSession("main")
	_, first := s.NewWindow("left")
	second, err := s.Split(true)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	m := NewModel(s, f, "ctrl+a", Styles{})
	m.panes[first] = &stateStub{st: ContentState{Type: "view", Commands: []string{"host"}}}
	m.panes[second] = &stateStub{st: ContentState{Type: "view", Commands: []string{"host", "alerts"}}}
	return m
}

func TestPersistSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "main.json")
	now := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)

	m := persistModel(t, nil)
	saved := Snapshot(m, []string{"hosts dc=ewr", "alerts"}, now)
	if saved.Version != 1 {
		t.Fatalf("Version = %d, want 1", saved.Version)
	}
	if err := Save(path, saved); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, saved)
	}
	if got.Name != "main" || got.Active != 0 || len(got.Windows) != 1 {
		t.Fatalf("unexpected session header: %+v", got)
	}
	if !got.SavedAt.Equal(now) {
		t.Fatalf("SavedAt = %v, want %v", got.SavedAt, now)
	}
	if want := []string{"hosts dc=ewr", "alerts"}; !reflect.DeepEqual(got.History, want) {
		t.Fatalf("History = %q, want %q", got.History, want)
	}
	w := got.Windows[0]
	if w.Name != "left" || w.Focus != got.Windows[0].Layout.Panes()[1] {
		t.Fatalf("unexpected window: %+v", w)
	}
	if len(w.Panes) != 2 {
		t.Fatalf("Panes has %d entries, want 2", len(w.Panes))
	}
	for _, st := range w.Panes {
		if st.Type != "view" {
			t.Fatalf("pane state %+v, want a view", st)
		}
	}

	// The history is copied, not aliased: changing it later cannot resave it.
	saved.History[0] = "changed"
	if got.History[0] != "hosts dc=ewr" {
		t.Fatal("Snapshot and Load share the history slice with the caller")
	}
}

func TestPersistSnapshotPaneStateComesFromContent(t *testing.T) {
	m := persistModel(t, nil)
	f := Snapshot(m, nil, time.Now())
	if len(f.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(f.Windows))
	}
	panes := f.Windows[0].Layout.Panes()
	second := f.Windows[0].Panes[panes[1]]
	if want := []string{"host", "alerts"}; !reflect.DeepEqual(second.Commands, want) {
		t.Fatalf("second pane commands = %q, want %q", second.Commands, want)
	}
}

func TestPersistRestoreKeepsLayoutAndFocus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.json")
	src := persistModel(t, nil)
	if err := Save(path, Snapshot(src, []string{"alerts"}, time.Now())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	fac := newPersistFactory()
	m, cmd, err := Restore(f, fac.make, "ctrl+a", Styles{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if m == nil {
		t.Fatal("Restore returned a nil model")
	}
	if cmd != nil {
		_ = cmd() // the batch must run without panicking
	}
	for id, c := range m.Contents() {
		s, ok := c.(*stateStub)
		if !ok {
			t.Fatalf("pane %d has content %T, want *stateStub", id, c)
		}
		if s.initRuns == 0 {
			t.Fatalf("pane %d was never initialised", id)
		}
	}
	sess := m.Session()
	if len(sess.Windows) != 1 || sess.Active != 0 {
		t.Fatalf("session = %+v, want one active window", sess)
	}
	srcPanes := src.Session().Windows[0].Layout.Panes()
	gotPanes := sess.Windows[0].Layout.Panes()
	if !reflect.DeepEqual(gotPanes, srcPanes) {
		t.Fatalf("panes = %v, want %v (ids must be kept)", gotPanes, srcPanes)
	}
	if got, want := sess.Windows[0].Focus, src.Session().Windows[0].Focus; got != want {
		t.Fatalf("focus = %d, want %d", got, want)
	}
	if got, want := sess.Windows[0].Name, "left"; got != want {
		t.Fatalf("window name = %q, want %q", got, want)
	}

	// The saved state reached the factory, and the contents are focused right.
	if want := []string{"host", "alerts"}; !reflect.DeepEqual(fac.states[srcPanes[1]].Commands, want) {
		t.Fatalf("factory state = %q, want %q", fac.states[srcPanes[1]].Commands, want)
	}
	if _, c := m.Focused(); c == nil {
		t.Fatal("no focused content after restore")
	} else if cs, ok := c.(*stateStub); !ok || !reflect.DeepEqual(cs.st.Commands, []string{"host", "alerts"}) {
		t.Fatalf("focused content = %+v, want the second pane's state", c)
	}

	// Counters continue after the highest restored id.
	id, _ := m.AddPane(ContentState{}, "h")
	if id <= srcPanes[1] {
		t.Fatalf("new pane id = %d, want more than %d", id, srcPanes[1])
	}
	if got := m.Session().Windows[0].Layout.Count(); got != 3 {
		t.Fatalf("pane count after split = %d, want 3", got)
	}
	// Two windows means window ids continued too.
	_, wid := m.Session().NewWindow("next")
	if wid >= 1000 {
		t.Fatalf("unexpected pane id %d", wid)
	}
	if got := m.Session().Windows[len(m.Session().Windows)-1].ID; got != WindowID(1) {
		t.Fatalf("new window id = %d, want 1", got)
	}
}

// TestPersistRestoreWarnsOnBadPane checks that a pane the factory cannot build
// falls back to the default content and reports a warning, instead of failing
// the whole restore.
func TestPersistRestoreWarnsOnBadPane(t *testing.T) {
	src := persistModel(t, nil)
	panes := src.Session().Windows[0].Layout.Panes()
	f := Snapshot(src, nil, time.Now())

	fac := newPersistFactory(panes[1])
	m, _, err := Restore(f, fac.make, "ctrl+a", Styles{})
	if err == nil {
		t.Fatal("Restore hid a pane it could not create")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Fatalf("warning kind = %q, want %q", kind, errs.Config)
	}
	if m == nil || m.Contents()[panes[1]] == nil {
		t.Fatal("the pane did not fall back to the default content")
	}
	if fac.defaults == 0 {
		t.Fatal("the factory was never asked for the default content")
	}
}

func TestPersistCorruptFileIsConfigError(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing", func(t *testing.T) {
		_, err := Load(filepath.Join(dir, "nope.json"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Load(missing) = %v, want os.ErrNotExist", err)
		}
	})

	t.Run("corrupt json", func(t *testing.T) {
		path := filepath.Join(dir, "corrupt.json")
		if err := os.WriteFile(path, []byte(`{"version":1,"windows":[`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatal("Load accepted broken JSON")
		}
		if kind := errs.KindOf(err); kind != errs.Config {
			t.Fatalf("kind = %q, want %q (err %v)", kind, errs.Config, err)
		}
	})

	t.Run("wrong version", func(t *testing.T) {
		path := filepath.Join(dir, "old.json")
		data, err := json.Marshal(map[string]any{"version": 7, "name": "main"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err = Load(path)
		if err == nil {
			t.Fatal("Load accepted an unknown version")
		}
		if kind := errs.KindOf(err); kind != errs.Config {
			t.Fatalf("kind = %q, want %q", kind, errs.Config)
		}
	})

	t.Run("not json at all", func(t *testing.T) {
		path := filepath.Join(dir, "garbage.json")
		if err := os.WriteFile(path, []byte("\x00\x01not json"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := Load(path)
		if kind := errs.KindOf(err); kind != errs.Config {
			t.Fatalf("kind = %q, want %q", kind, errs.Config)
		}
	})
}

func TestPersistSaveIsAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	path := filepath.Join(dir, "main.json")
	f := Snapshot(persistModel(t, nil), nil, time.Now())

	if err := Save(path, f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file left behind: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir mode = %o, want 700", perm)
	}

	// A failed write must leave the previous file untouched: block the temp
	// path with a directory so the rename cannot happen.
	if err := os.MkdirAll(path+".tmp", 0o700); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	f.Name = "different"
	if err := Save(path, f); err == nil {
		t.Fatal("Save reported success with an unusable temp path")
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after a failed save: %v", err)
	}
	if got.Name != "main" {
		t.Fatalf("the old file was replaced by a failed save: name = %q", got.Name)
	}
}
