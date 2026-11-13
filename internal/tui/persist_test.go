package tui_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	teatest "github.com/charmbracelet/x/exp/teatest"

	"jumper/internal/mux"
	"jumper/internal/resource"
)

// sessionPath is where a test's session file lives: always the test's own temp
// directory, never the real cache.
func sessionPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sessions", "main.json")
}

// TestAutosaveAfterSplit proves the 1 s autosave: after a split the session
// file appears on its own, without a quit, and describes two panes.
func TestAutosaveAfterSplit(t *testing.T) {
	path := sessionPath(t)
	root := newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	root.SetSessionPath(path)
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")

	sendPrefix(t, tm, "%")
	waitFor(t, tm, "2:host")

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session file did not appear within 2 s of the split")
		}
		time.Sleep(20 * time.Millisecond)
	}

	f, err := mux.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Version != 1 || f.Name != "main" {
		t.Fatalf("session file = %+v, want version 1 named main", f)
	}
	if len(f.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(f.Windows))
	}
	if n := len(f.Windows[0].Layout.Panes()); n != 2 {
		t.Fatalf("panes = %d, want 2", n)
	}
	for _, st := range f.Windows[0].Panes {
		if st.Type != "view" || len(st.Commands) == 0 {
			t.Fatalf("pane state = %+v, want a restorable view stack", st)
		}
	}
	tm.Quit()
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestRestoreRebuildsViews restores a session file and checks that the windows,
// the split, the focus and both view stacks come back as saved.
func TestRestoreRebuildsViews(t *testing.T) {
	path := sessionPath(t)
	layout := mux.NewLayout(1)
	if err := layout.SplitH(1, 2); err != nil {
		t.Fatalf("split layout: %v", err)
	}
	saved := mux.SessionFile{
		Version: 1,
		Name:    "main",
		Active:  0,
		Windows: []mux.WindowFile{{
			Name:   "work",
			Layout: layout,
			Focus:  2,
			Panes: map[mux.PaneID]mux.ContentState{
				1: {Type: "view", Commands: []string{"host"}},
				2: {Type: "view", Commands: []string{"host", "alerts"}},
			},
		}},
		History: []string{"alerts"},
		SavedAt: time.Now(),
	}
	if err := mux.Save(path, saved); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := mux.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	root := newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(4)})
	root.SetSessionPath(path)
	if err := root.RestoreSession(loaded); err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	if _, cmd := root.Update(tea.WindowSizeMsg{Width: 120, Height: 30}); cmd != nil {
		_ = cmd
	}

	screen := root.View()
	if !strings.Contains(screen, "0:work") {
		t.Fatalf("status line does not show the saved window:\n%s", screen)
	}
	if !strings.Contains(screen, "1:host") {
		t.Fatalf("first pane was not rebuilt as hosts:\n%s", screen)
	}
	if !strings.Contains(screen, "2:alert") {
		t.Fatalf("second pane was not rebuilt on its saved stack:\n%s", screen)
	}
	if !strings.Contains(screen, "›") {
		t.Fatalf("the second pane's stack has one entry, want two (host › alerts):\n%s", screen)
	}

	// Pane ids are kept and both stacks survive a round trip.
	back := root.Snapshot()
	if len(back.Windows) != 1 || back.Windows[0].Name != "work" {
		t.Fatalf("snapshot windows = %+v", back.Windows)
	}
	ids := make([]int, 0, len(back.Windows[0].Panes))
	for id := range back.Windows[0].Panes {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("pane ids = %v, want [1 2]", ids)
	}
	if got, want := back.Windows[0].Panes[2].Commands, []string{"host", "alert"}; !equalStrings(got, want) {
		t.Fatalf("second pane commands = %q, want %q", got, want)
	}
	if got, want := back.Windows[0].Focus, mux.PaneID(2); got != want {
		t.Fatalf("focus = %d, want %d", got, want)
	}
}

// TestQuitSavesSessionOnce proves the quit-time save app performs after the
// program stops: the layout reached the disk without waiting for a tick.
func TestQuitSavesSessionOnce(t *testing.T) {
	path := sessionPath(t)
	root := newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	root.SetSessionPath(path)
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")
	sendPrefix(t, tm, "%")
	waitFor(t, tm, "2:host")
	tm.Quit()
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	if err := root.SaveSession(); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	f, err := mux.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(f.Windows[0].Layout.Panes()); n != 2 {
		t.Fatalf("panes after the quit save = %d, want 2", n)
	}
}

// equalStrings compares two string slices without pulling in reflect.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
