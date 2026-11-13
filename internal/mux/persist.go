package mux

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/errs"
)

// SessionVersion is the only session file version this build writes and reads.
// A file with any other version is a configuration error: the caller starts a
// fresh session and tells the user, it never guesses at an older layout.
const SessionVersion = 1

// SessionFile is what survives a restart: the session name, the windows with
// their layouts and pane contents, and the command history. It is JSON, small
// and written to <cache>/sessions/<name>.json.
type SessionFile struct {
	Version int          `json:"version"`
	Name    string       `json:"name"`
	Active  int          `json:"active"`
	Windows []WindowFile `json:"windows"`
	History []string     `json:"history"`
	SavedAt time.Time    `json:"saved_at"`
}

// WindowFile is one saved window. Focus is the pane that had the focus; Panes
// holds the state of every pane of the layout, keyed by its pane id.
type WindowFile struct {
	Name   string                  `json:"name"`
	Layout *Layout                 `json:"layout"`
	Focus  PaneID                  `json:"focus"`
	Panes  map[PaneID]ContentState `json:"panes"`
}

// Snapshot describes m as a SessionFile. history is the command bar's history,
// oldest first. A nil model (or a session with no windows) yields an empty,
// valid file.
func Snapshot(m *Model, history []string, now time.Time) SessionFile {
	f := SessionFile{
		Version: SessionVersion,
		History: append([]string(nil), history...),
		SavedAt: now,
		Active:  -1,
	}
	if m == nil || m.sess == nil {
		return f
	}
	f.Name = m.sess.Name
	f.Active = m.sess.Active
	for _, w := range m.sess.Windows {
		if w == nil {
			continue
		}
		wf := WindowFile{Name: w.Name, Layout: w.Layout, Focus: w.Focus}
		if w.Layout != nil {
			wf.Panes = make(map[PaneID]ContentState, w.Layout.Count())
			for _, id := range w.Layout.Panes() {
				var st ContentState
				if c, ok := m.panes[id]; ok && c != nil {
					st = c.State()
				}
				wf.Panes[id] = st
			}
		}
		f.Windows = append(f.Windows, wf)
	}
	return f
}

// Save writes f to path atomically: the data goes to path+".tmp" (mode 0600)
// and is renamed over path, so a crash mid-write leaves the previous file
// whole. The directory is created with 0700.
func Save(path string, f SessionFile) error {
	const op = "mux.Save"
	if path == "" {
		return errs.New(errs.Config, op, "no session path")
	}
	if f.Version == 0 {
		f.Version = SessionVersion
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return errs.Wrap(errs.Internal, op, err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return errs.Wrap(errs.Config, op, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return errs.Wrap(errs.Config, op, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Leave no half-written file behind for the next Load to trip over.
		_ = os.Remove(tmp)
		return errs.Wrap(errs.Config, op, err)
	}
	return nil
}

// Load reads a session file. A missing file returns the os.ErrNotExist it got
// from the filesystem, so the caller can tell "nothing saved yet" from "this
// file is broken": corrupt JSON and a wrong version are errs.Config, and the
// caller starts fresh.
func Load(path string) (SessionFile, error) {
	const op = "mux.Load"
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SessionFile{}, err
		}
		return SessionFile{}, errs.Wrap(errs.Config, op, err)
	}
	var f SessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		return SessionFile{}, errs.Wrap(errs.Config, op, err)
	}
	if f.Version != SessionVersion {
		return SessionFile{}, errs.New(errs.Config, op,
			fmt.Sprintf("unsupported session file version %d (want %d)", f.Version, SessionVersion))
	}
	return f, nil
}

// Restore rebuilds a session from f. Pane ids are kept as saved and contents
// are recreated through factory with their saved state, so a view pane comes
// back with the same stack. Counters continue after the highest restored id.
// A pane whose content cannot be created falls back to the default content and
// the failure is returned as a warning: the model is still usable, and a nil
// model is returned only when the file itself is impossible to open.
func Restore(f SessionFile, factory Factory, prefixKey string, styles Styles) (*Model, tea.Cmd, error) {
	const op = "mux.Restore"
	if len(f.Windows) == 0 {
		return nil, nil, errs.New(errs.Config, op, "session file has no windows")
	}
	s := NewSession(f.Name)
	for i, wf := range f.Windows {
		if wf.Layout == nil || wf.Layout.Count() == 0 {
			return nil, nil, errs.New(errs.Config, op, fmt.Sprintf("window %d has no layout", i))
		}
	}
	maxPane := 0
	for _, wf := range f.Windows {
		panes := wf.Layout.Panes()
		focus := wf.Focus
		if !wf.Layout.Contains(focus) {
			focus = panes[0]
		}
		w := &Window{
			ID:       WindowID(len(s.Windows)),
			Name:     wf.Name,
			Layout:   wf.Layout,
			Focus:    focus,
			Last:     focus,
			AutoName: wf.Name == "",
		}
		s.Windows = append(s.Windows, w)
		s.layoutMode[w.ID] = -1
		for _, id := range panes {
			if int(id) > maxPane {
				maxPane = int(id)
			}
		}
	}
	s.Active = f.Active
	if s.Active < 0 || s.Active >= len(s.Windows) {
		s.Active = 0
	}
	s.nextPane = PaneID(maxPane + 1)
	if s.nextPane < 1 {
		s.nextPane = 1
	}
	s.nextWindow = WindowID(len(s.Windows))

	m := NewModel(s, factory, prefixKey, styles)
	active := s.Windows[s.Active]
	var cmds []tea.Cmd
	var warns []string
	for wi, w := range s.Windows {
		for _, id := range w.Layout.Panes() {
			st := f.Windows[wi].Panes[id]
			var c Content
			var cmd tea.Cmd
			if factory != nil {
				c, cmd = factory(id, st)
			}
			if c == nil {
				warns = append(warns, fmt.Sprintf("pane %d: content could not be created, using the default", id))
				if factory != nil {
					c, cmd = factory(id, ContentState{})
				}
			}
			if c == nil {
				return nil, nil, errs.New(errs.Config, op,
					fmt.Sprintf("pane %d: no content and no default content", id))
			}
			c.SetFocus(w == active && id == w.Focus)
			m.panes[id] = c
			cmds = append(cmds, cmd)
			if ic := c.Init(); ic != nil {
				cmds = append(cmds, ic)
			}
		}
	}
	var warn error
	if len(warns) > 0 {
		warn = errs.New(errs.Config, op, strings.Join(warns, "; "))
	}
	return m, batch(cmds), warn
}

// batch returns one command for cmds, dropping the nils so an all-nil batch is
// nil rather than a BatchMsg full of holes.
func batch(cmds []tea.Cmd) tea.Cmd {
	live := cmds[:0]
	for _, c := range cmds {
		if c != nil {
			live = append(live, c)
		}
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	}
	return tea.Batch(live...)
}
