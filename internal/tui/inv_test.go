package tui_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	teatest "github.com/charmbracelet/x/exp/teatest"

	"jumper/internal/backend"
	"jumper/internal/backend/fake"
	"jumper/internal/backend/linux"
	"jumper/internal/investigation"
	"jumper/internal/redact"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
	"jumper/internal/tui/views"
)

// invNow is the fixed clock of the investigation tests, so ids and timestamps
// do not depend on when the suite runs.
var invNow = time.Date(2026, 11, 12, 14, 2, 11, 0, time.FixedZone("EST", -5*60*60))

// invHarness is a root wired to a real FileLog over a temp knowledge root,
// the fake fleet plus two stub backends: one serving the host-overview kind
// that opened host views report, one serving the journal.
type invHarness struct {
	root *tui.Root
	env  *tui.Env
	log  *investigation.FileLog
	dir  string
}

// stubBackend serves fixed kinds and one streamed row set.
type stubBackend struct {
	name  string
	specs []resource.KindSpec
	rows  []resource.Resource
	host  resource.HostID
}

func (s *stubBackend) Name() string               { return s.name }
func (s *stubBackend) Kinds() []resource.KindSpec { return s.specs }

func (s *stubBackend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	ch := make(chan backend.Result, 1)
	go func() {
		defer close(ch)
		if len(s.rows) == 0 {
			ch <- backend.Result{Done: 1, Total: 1}
			return
		}
		ch <- backend.Result{Host: s.host, Items: s.rows, Done: 1, Total: 1}
	}()
	return ch, nil
}

func (s *stubBackend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	return resource.Resource{Ref: ref}, nil
}

func (s *stubBackend) Links(r resource.Resource) []resource.Link { return nil }

func (s *stubBackend) Health(ctx context.Context) backend.Health {
	return backend.Health{State: backend.HealthOK, Checked: invNow}
}

// overviewSpec is the host-overview kind the real Linux backend serves.
func overviewSpec() resource.KindSpec {
	return resource.KindSpec{
		Name:  linux.KindHostOverview,
		Title: "Host overview",
		Scope: resource.ScopeHost,
		Columns: []resource.Column{
			{ID: "host", Title: "HOST", Width: 0},
		},
	}
}

// journalSpec is the journal kind, with the columns the journal view renders.
func journalSpec() resource.KindSpec {
	return resource.KindSpec{
		Name:  linux.KindJournal,
		Title: "Journal",
		Scope: resource.ScopeHost,
		Columns: []resource.Column{
			{ID: "time", Title: "TIME", Width: 8, Kind: resource.ColTime},
			{ID: "source", Title: "SOURCE", Width: 10},
			{ID: "message", Title: "MESSAGE", Width: 0},
		},
	}
}

// journalRows are the two lines the stub journal serves.
func journalRows() []resource.Resource {
	msg := "bind() to 0.0.0.0:443 failed (98: Address already in use)"
	return []resource.Resource{
		{
			Ref: resource.Ref{Backend: "stub", Kind: linux.KindJournal, Host: "web-01", Name: "nginx"},
			Fields: map[string]any{
				"time":    invNow.Add(-3 * time.Minute),
				"host":    "web-01",
				"source":  "nginx",
				"message": msg,
			},
		},
		{
			Ref: resource.Ref{Backend: "stub", Kind: linux.KindJournal, Host: "web-01", Name: "haproxy"},
			Fields: map[string]any{
				"time":    invNow.Add(-2 * time.Minute),
				"host":    "web-01",
				"source":  "haproxy",
				"message": "stale listener on :443",
			},
		},
	}
}

// newInvHarness builds a root whose session has a real investigation recorder.
func newInvHarness(t *testing.T, q resource.Query) *invHarness {
	t.Helper()
	theme.SetProfile("none")
	dir := t.TempDir()
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}
	log := investigation.NewLog(dir, red, func() time.Time { return invNow })

	reg := backend.NewRegistry()
	for _, b := range []backend.Backend{
		fake.New(fake.Options{Now: func() time.Time { return invNow }}),
		&stubBackend{name: "stub-overview", specs: []resource.KindSpec{overviewSpec()}},
		&stubBackend{name: "stub-journal", specs: []resource.KindSpec{journalSpec()}, rows: journalRows(), host: "web-01"},
	} {
		if err := reg.Register(b); err != nil {
			t.Fatalf("register %s: %v", b.Name(), err)
		}
	}

	env := &tui.Env{
		Registry:  reg,
		Session:   &tui.SessionState{Name: "main"},
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       func() time.Time { return invNow },
		Factories: make(map[resource.KindName]tui.Factory),
		Recorder:  log,
		// The inventory lets `:hosts <selector>` resolve, including
		// `@investigation`, without a backend that serves hosts.
		Inventory: newTargetInventory(),
	}
	for kind, f := range views.Factories() {
		env.Factories[kind] = f
	}
	for _, spec := range reg.Kinds() {
		if env.Factories[spec.Name] == nil {
			env.Factories[spec.Name] = tableFactory
		}
	}
	env.Factories[linux.KindHostOverview] = func(e *tui.Env, q resource.Query) (tui.View, error) {
		host := ""
		if len(q.Hosts) > 0 {
			host = string(q.Hosts[0])
		}
		return views.NewText(e, "host overview", []string{"host overview " + host}), nil
	}

	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	if err := tui.RegisterMuxCommands(cmds); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	return &invHarness{root: tui.NewRoot(env, cmds, tableFactory, q, "ctrl+a"), env: env, log: log, dir: dir}
}

// openInvestigation types `:inv open nginx 443` and waits for the status line.
func (h *invHarness) openInvestigation(t *testing.T, tm *teatest.TestModel) string {
	t.Helper()
	tm.Type(":inv open nginx 443")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "inv:2026-11-12-nginx-443")
	return filepath.Join(h.dir, "investigations", "2026-11-12-nginx-443.md")
}

// sendKey sends one rune key.
func sendKey(tm *teatest.TestModel, key string) {
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
}

// waitFile polls path until it contains every want, then returns its content.
// The record is written by a tea.Cmd, so it can land a moment after the flash.
func waitFile(t *testing.T, path string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			last = string(data)
			ok := true
			for _, w := range want {
				if !strings.Contains(last, w) {
					ok = false
					break
				}
			}
			if ok {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s does not contain %q after 3s:\n%s", path, want, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFindings polls path until the ## Findings section contains sub.
func waitFindings(t *testing.T, path, sub string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for {
		if data, err := os.ReadFile(path); err == nil {
			last = string(data)
			if strings.Contains(findingsSection(last), sub) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("## Findings of %s never contained %q:\n%s", path, sub, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// findingsSection returns the text under ## Findings, without the resolution.
func findingsSection(file string) string {
	const heading = "## Findings"
	i := strings.Index(file, heading)
	if i < 0 {
		return ""
	}
	rest := file[i+len(heading):]
	if j := strings.Index(rest, "## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestInvestigationOpenShowsStatus(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")

	path := h.openInvestigation(t, tm)
	file := waitFile(t, path, "**open**", "# nginx 443")
	if !strings.Contains(file, "`nginx 443`") {
		t.Fatalf("open bullet has no title:\n%s", file)
	}
	if !strings.Contains(file, "## Timeline") {
		t.Fatalf("record has no timeline:\n%s", file)
	}

	screen := finish(t, tm)
	if !strings.Contains(screen, "inv:2026-11-12-nginx-443") {
		t.Fatalf("status line has no investigation:\n%s", screen)
	}

	// Closing clears the status line again.
	closed := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm2 := start(t, closed.root)
	waitFor(t, tm2, "web-ewr-007")
	closed.openInvestigation(t, tm2)
	tm2.Type(":inv close")
	tm2.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm2, "investigation closed")
	if screen := finish(t, tm2); strings.Contains(screen, "inv:2026-11-12-nginx-443") {
		t.Fatalf("status line still shows a closed investigation:\n%s", screen)
	}
}

func TestInvestigationRecordsHostOverview(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")
	path := h.openInvestigation(t, tm)

	tm.Type(":host-overview --host web-01")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "host overview web-01")

	file := waitFile(t, path, "**view** host overview web-01")
	if strings.Count(file, "**view**") != 1 {
		t.Fatalf("expected exactly one view entry:\n%s", file)
	}
}

func TestInvestigationNote(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")
	path := h.openInvestigation(t, tm)

	sendKey(tm, "n")
	waitFor(t, tm, "note:")
	tm.Type("haproxy holds :443")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	file := waitFile(t, path, "**note** haproxy holds :443")
	if !strings.Contains(file, "## Timeline") {
		t.Fatalf("note landed outside the timeline:\n%s", file)
	}
}

func TestInvestigationFindingFromTable(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")
	path := h.openInvestigation(t, tm)

	sendKey(tm, "F")
	waitFor(t, tm, "finding recorded")

	file := waitFindings(t, path, "web-ewr-0")
	if !strings.Contains(file, "**finding**") {
		t.Fatalf("no finding bullet in the timeline:\n%s", file)
	}
}

func TestInvestigationJournalEntryAndFinding(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")
	path := h.openInvestigation(t, tm)

	tm.Type(":journal --host web-01")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "bind() to 0.0.0.0:443 failed")

	file := waitFile(t, path, "**journal** all units on web-01 (2 lines in window)")
	if !strings.Contains(file, "**journal**") {
		t.Fatalf("journal entry missing:\n%s", file)
	}

	sendKey(tm, "F")
	waitFor(t, tm, "finding recorded")
	waitFindings(t, path, "stale listener on :443")
}

func TestTimelineShowsMarkdown(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")
	path := h.openInvestigation(t, tm)

	// The `:` form, so the timeline test does not race a modal prompt.
	tm.Type(":note bound after haproxy restart")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFile(t, path, "**note** bound after haproxy restart")

	sendKey(tm, "T")
	waitFor(t, tm, "## Timeline", "# nginx 443", "bound after haproxy restart")

	screen := finish(t, tm)
	for _, want := range []string{"## Timeline", "## Findings", "**open**", "**note** bound after haproxy restart"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("timeline screen is missing %q:\n%s", want, screen)
		}
	}
}

func TestInvestigationListShowsRecords(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")
	h.openInvestigation(t, tm)

	tm.Type(":inv")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "investigations", "open")

	screen := finish(t, tm)
	if !strings.Contains(screen, "2026-11-12-nginx-443") {
		t.Fatalf("list screen has no investigation id:\n%s", screen)
	}
}

func TestTimelineWithoutInvestigationShowsAuditPath(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")

	tm.Type(":timeline")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "no investigation is open", "audit")

	screen := finish(t, tm)
	if !strings.Contains(screen, "no investigation is open") {
		t.Fatalf("timeline without an investigation has no hint:\n%s", screen)
	}
}

func TestInvestigationResolvePrompt(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")
	path := h.openInvestigation(t, tm)

	tm.Type(":inv resolve")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "root cause:")
	tm.Type("stale haproxy bound :443")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	file := waitFile(t, path, "**resolve** stale haproxy bound :443", "status: resolved")
	if !strings.Contains(file, "## Resolution\nstale haproxy bound :443") {
		t.Fatalf("resolution section missing:\n%s", file)
	}
	cur, ok := h.log.Current()
	if !ok || cur.Status != "resolved" {
		t.Fatalf("Current() = %+v, %v; want resolved", cur, ok)
	}
}

func TestInvestigationSelectorResolvesTargets(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")

	// The investigation takes its hosts from the session targets, so make
	// them something distinguishable first.
	tm.Type(":hosts role=web dc=ewr")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets:504")

	path := h.openInvestigation(t, tm)
	waitFile(t, path, "on 504 hosts")

	tm.Type(":hosts dc=lax")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets:96")

	// @investigation goes back to the record's hosts, not the store's.
	tm.Type(":hosts @investigation")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets:504")

	screen := finish(t, tm)
	if !strings.Contains(screen, "targets:504") {
		t.Fatalf("status line is %q, want targets:504 from @investigation", screen)
	}
}

func TestTimelinePathFlashAndResolveKey(t *testing.T) {
	h := newInvHarness(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, h.root)
	waitFor(t, tm, "web-ewr-007")
	path := h.openInvestigation(t, tm)

	sendKey(tm, "T")
	waitFor(t, tm, "## Timeline")

	// o never opens $EDITOR from a view: it flashes the path. The flash is cut
	// to the terminal width, so only its head is asserted.
	sendKey(tm, "o")
	waitFor(t, tm, "investigations/2026")

	// R resolves through the same prompt as `:inv resolve`.
	sendKey(tm, "R")
	waitFor(t, tm, "root cause:")
	tm.Type("bound after restart")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFile(t, path, "**resolve** bound after restart", "status: resolved")
}
