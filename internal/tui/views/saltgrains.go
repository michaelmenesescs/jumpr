package views

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/salt"
	"jumper/internal/errs"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// The grains explorer and the mine browser (SPEC §8.6 "Grains" and "Mine").
//
// Both screens read the master's cache, never the minions: only `:grains
// --live` asks a minion for its grains, and only to prove the cache stale.
// Nothing here blocks: every read runs in a tea.Cmd with a deadline.
const (
	// KindGrains is the factory key of the grains explorer. It is a view
	// kind, not a backend kind.
	KindGrains resource.KindName = "salt-grains"
	// KindMine is the factory key of the mine browser: the salt backend
	// serves the salt-mine kind and this view draws it.
	KindMine resource.KindName = salt.KindMine

	// grainsLoadTimeout bounds one grains read.
	grainsLoadTimeout = 60 * time.Second
	// grainsFleetTimeout bounds the fleet-wide cache read a distribution or a
	// target-set needs.
	grainsFleetTimeout = 90 * time.Second
	// mineLoadTimeout bounds one mine cache read.
	mineLoadTimeout = 90 * time.Second
	// mineValueTimeout bounds the one full-value read behind `enter`.
	mineValueTimeout = 45 * time.Second

	// grainsDistKey shows the distribution of the selected grain; `r` is
	// refresh, `/` is filter.
	grainsDistKey = "L"
)

// GrainsBackend is what the grains explorer needs from the Salt backend. The
// salt package implements it; tests use a fake.
type GrainsBackend interface {
	// MinionGrains reads one minion's grains from the master's cache.
	MinionGrains(ctx context.Context, minion string) (salt.Grains, error)
	// AllGrains reads the whole cache, for a distribution across the fleet.
	AllGrains(ctx context.Context) (map[string]salt.Grains, error)
	// LiveGrains runs grains.items on the minion itself.
	LiveGrains(ctx context.Context, minion string) (salt.Grains, error)
}

// MineBackend is what the mine browser needs from the Salt backend.
type MineBackend interface {
	// Mine reads the mine cache of the fleet.
	Mine(ctx context.Context) ([]salt.MineEntry, error)
	// MineValue reads one value in full, for the dialog that asks for it.
	MineValue(ctx context.Context, minion, function string) (string, error)
}

// minionHostMapper is the part of the Salt backend that maps a minion onto an
// inventory host. A backend without it addresses the minion by its own id.
type minionHostMapper interface {
	MinionHost(id string) (resource.HostID, bool)
}

// grainsMode is what the keyboard is doing.
type grainsMode int

const (
	grainsIdle grainsMode = iota
	grainsFilter
)

// grainsRow is one grain of one minion as the view shows it.
type grainsRow struct {
	Key   string
	Value string
	// Was is the cached value when the live read differs from it.
	Was   string
	Stale bool
}

// grainsLoadMsg is one grains read, tagged with Seq.
type grainsLoadMsg struct {
	Seq    int
	Grains salt.Grains
	Cached salt.Grains
	Live   bool
	Err    error
}

// grainsFleetMsg is the fleet-wide cache read: the answer to `L` (Target "")
// or to `enter` on one value.
type grainsFleetMsg struct {
	Key    string
	Target string
	All    map[string]salt.Grains
	Err    error
}

// SaltGrains is `:grains <minion>` (SPEC §8.6 "Grains"): every grain of one
// minion from the master's cache, filterable, with the fleet-wide distribution
// of one grain on demand. It is a tui.View.
type SaltGrains struct {
	env    *tui.Env
	b      GrainsBackend
	minion string
	// live is `:grains --live`: read grains.items on the minion and diff it
	// against the cache.
	live   bool
	keys   *keymap.Map
	glyphs render.GlyphSet
	theme  *theme.Theme

	base context.Context

	seq     int
	cancel  context.CancelFunc
	grains  salt.Grains
	cached  salt.Grains
	diffs   map[string]salt.GrainDiff
	all     map[string]salt.Grains // the fleet cache, once a distribution or a target set needed it
	rows    []grainsRow
	filter  string
	mode    grainsMode
	input   textinput.Model
	loading bool
	loadErr error
	cursor  int

	w, h int
}

var _ tui.View = (*SaltGrains)(nil)

// NewSaltGrains builds the grains screen of one minion. live reads the minion
// instead of the cache.
func NewSaltGrains(env *tui.Env, b GrainsBackend, minion string, live bool) *SaltGrains {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	glyphs, _ := render.Glyphs("unicode")
	if env != nil && env.Glyphs.Name != "" {
		glyphs = env.Glyphs
	}
	th := theme.Dark()
	if env != nil && env.Theme != nil {
		th = env.Theme
	}
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	return &SaltGrains{
		env:    env,
		b:      b,
		minion: strings.TrimSpace(minion),
		live:   live,
		keys:   keys,
		glyphs: glyphs,
		theme:  th,
		input:  in,
	}
}

// Init starts the grains read.
func (s *SaltGrains) Init() tea.Cmd { return s.load() }

// SetContext makes every read derive from ctx, so closing the pane stops it.
func (s *SaltGrains) SetContext(ctx context.Context) {
	if ctx != nil {
		s.base = ctx
	}
}

// Close cancels a read in flight.
func (s *SaltGrains) Close() error {
	s.stop()
	return nil
}

// now returns the clock the environment handed in.
func (s *SaltGrains) now() time.Time {
	if s.env != nil && s.env.Now != nil {
		return s.env.Now()
	}
	return time.Now()
}

// baseCtx is the context every read derives from.
func (s *SaltGrains) baseCtx() context.Context {
	if s.base != nil {
		return s.base
	}
	return context.Background()
}

// stop cancels a read in flight.
func (s *SaltGrains) stop() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// load abandons a read in flight and asks for the minion's grains: the cache
// when the view is in cache mode, the minion itself (plus the cache, for the
// diff) in live mode.
func (s *SaltGrains) load() tea.Cmd {
	s.stop()
	s.seq++
	seq := s.seq
	s.grains = nil
	s.cached = nil
	s.diffs = nil
	s.rows = nil
	s.loadErr = nil
	s.loading = true
	s.cursor = 0
	if s.b == nil {
		s.loading = false
		s.loadErr = errs.New(errs.Config, "views.saltgrains", "no salt backend configured")
		return nil
	}
	if s.minion == "" {
		s.loading = false
		s.loadErr = errs.New(errs.Config, "views.saltgrains", "grains need a minion")
		return nil
	}
	b, minion, live := s.b, s.minion, s.live
	ctx, cancel := context.WithCancel(s.baseCtx())
	s.cancel = cancel
	return func() tea.Msg {
		cctx, ccancel := context.WithTimeout(ctx, grainsLoadTimeout)
		defer ccancel()
		if !live {
			g, err := b.MinionGrains(cctx, minion)
			return grainsLoadMsg{Seq: seq, Grains: g, Err: err}
		}
		cached, err := b.MinionGrains(cctx, minion)
		if err != nil {
			return grainsLoadMsg{Seq: seq, Err: err}
		}
		g, err := b.LiveGrains(cctx, minion)
		return grainsLoadMsg{Seq: seq, Grains: g, Cached: cached, Live: true, Err: err}
	}
}

// Update folds reads and routes keys. A superseded read is dropped.
func (s *SaltGrains) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case grainsLoadMsg:
		if m.Seq != s.seq {
			return s, nil
		}
		return s, s.applyLoad(m)
	case grainsFleetMsg:
		return s, s.applyFleet(m)
	case tui.RefreshRequestMsg:
		return s, s.load()
	case tea.KeyMsg:
		return s.handleKey(m)
	}
	return s, nil
}

// applyLoad keeps the grains, or the reason there are none, and rebuilds the
// rows. In live mode it also records which keys the cache has wrong.
func (s *SaltGrains) applyLoad(m grainsLoadMsg) tea.Cmd {
	s.loading = false
	if m.Err != nil {
		s.loadErr = m.Err
		s.rows = nil
		return nil
	}
	s.grains = m.Grains
	s.cached = m.Cached
	s.diffs = nil
	if m.Live {
		s.diffs = make(map[string]salt.GrainDiff)
		for _, d := range salt.DiffGrains(m.Cached, m.Grains) {
			s.diffs[d.Key] = d
		}
	}
	s.rebuild()
	return nil
}

// rebuild turns the grains into one row per key, sorted by key.
func (s *SaltGrains) rebuild() {
	keys := make([]string, 0, len(s.grains))
	for k := range s.grains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	needle := strings.ToLower(strings.TrimSpace(s.filter))
	s.rows = make([]grainsRow, 0, len(keys))
	for _, k := range keys {
		if needle != "" && !strings.Contains(strings.ToLower(k), needle) &&
			!strings.Contains(strings.ToLower(salt.GrainText(s.grains[k])), needle) {
			continue
		}
		row := grainsRow{Key: k, Value: salt.GrainText(s.grains[k])}
		if d, ok := s.diffs[k]; ok {
			row.Stale = true
			row.Was = d.Cached
			if row.Was == "" {
				row.Was = "(not cached)"
			}
			if row.Value == "" {
				row.Value = "(gone)"
			}
		}
		s.rows = append(s.rows, row)
	}
	s.clampCursor()
}

// setFilter installs the key/value filter and rebuilds the rows.
func (s *SaltGrains) setFilter(text string) {
	s.filter = text
	s.rebuild()
}

// clampCursor keeps the cursor inside the rows.
func (s *SaltGrains) clampCursor() {
	last := len(s.rows) - 1
	if last < 0 {
		s.cursor = 0
		return
	}
	if s.cursor > last {
		s.cursor = last
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
}

// move moves the cursor by delta.
func (s *SaltGrains) move(delta int) {
	s.cursor += delta
	s.clampCursor()
}

// selectedRow returns the row under the cursor.
func (s *SaltGrains) selectedRow() (grainsRow, bool) {
	if s.cursor < 0 || s.cursor >= len(s.rows) {
		return grainsRow{}, false
	}
	return s.rows[s.cursor], true
}

// handleKey routes one key press.
func (s *SaltGrains) handleKey(m tea.KeyMsg) (tui.View, tea.Cmd) {
	if s.mode != grainsIdle {
		return s, s.inputKey(m)
	}
	key := m.String()
	switch {
	case s.keys.Filter.Matches(key):
		return s, s.beginFilter()
	case s.keys.Up.Matches(key):
		s.move(-1)
		return s, nil
	case s.keys.Down.Matches(key):
		s.move(1)
		return s, nil
	case s.keys.PageUp.Matches(key):
		s.move(-max(1, s.h-3))
		return s, nil
	case s.keys.PageDown.Matches(key):
		s.move(max(1, s.h-3))
		return s, nil
	case s.keys.Top.Matches(key):
		s.cursor = 0
		return s, nil
	case s.keys.Bottom.Matches(key):
		s.cursor = len(s.rows) - 1
		s.clampCursor()
		return s, nil
	case s.keys.Open.Matches(key):
		return s, s.rowTarget()
	case s.keys.Refresh.Matches(key):
		return s, s.load()
	case s.keys.Back.Matches(key):
		if s.filter != "" {
			s.setFilter("")
			return s, nil
		}
		return s, tui.Pop()
	}
	if key == grainsDistKey {
		return s, s.distribution()
	}
	return s, nil
}

// beginFilter opens the key/value filter.
func (s *SaltGrains) beginFilter() tea.Cmd {
	s.mode = grainsFilter
	s.input.SetValue(s.filter)
	s.input.CursorEnd()
	s.input.Focus()
	return nil
}

// inputKey edits the filter. enter keeps it, esc drops it.
func (s *SaltGrains) inputKey(m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "enter":
		s.mode = grainsIdle
		s.input.Blur()
		s.setFilter(strings.TrimSpace(s.input.Value()))
		return nil
	case "esc":
		s.mode = grainsIdle
		s.input.Blur()
		s.setFilter("")
		return nil
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(m)
	s.setFilter(s.input.Value())
	return cmd
}

// rowTarget makes the hosts that report the selected row's value the session
// targets.
func (s *SaltGrains) rowTarget() tea.Cmd {
	row, ok := s.selectedRow()
	if !ok {
		return tui.Flash(tui.FlashInfo, "select a grain")
	}
	if row.Value == "" {
		return tui.Flash(tui.FlashInfo, "%s has no scalar value to target", row.Key)
	}
	return s.withFleet(row.Key, row.Value)
}

// distribution shows how often each value of the selected grain appears across
// the fleet.
func (s *SaltGrains) distribution() tea.Cmd {
	row, ok := s.selectedRow()
	if !ok {
		return tui.Flash(tui.FlashInfo, "select a grain")
	}
	return s.withFleet(row.Key, "")
}

// withFleet reads the fleet cache when the view does not have it yet, then
// either sets targets to the minions holding target or opens the distribution
// dialog (target "").
func (s *SaltGrains) withFleet(key, target string) tea.Cmd {
	if s.all != nil {
		if target != "" {
			return s.setTargets(key, target)
		}
		return s.openDistribution(key)
	}
	b, base := s.b, s.baseCtx()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, grainsFleetTimeout)
		defer cancel()
		all, err := b.AllGrains(ctx)
		return grainsFleetMsg{Key: key, Target: target, All: all, Err: err}
	}
}

// applyFleet keeps the fleet cache and finishes the action that asked for it.
func (s *SaltGrains) applyFleet(m grainsFleetMsg) tea.Cmd {
	if m.Err != nil {
		return tui.Flash(tui.FlashError, "grains: %v", m.Err)
	}
	s.all = m.All
	if m.Target != "" {
		return s.setTargets(m.Key, m.Target)
	}
	return s.openDistribution(m.Key)
}

// setTargets makes the hosts whose grain key holds value the session targets.
func (s *SaltGrains) setTargets(key, value string) tea.Cmd {
	minions := salt.MinionsWithGrain(s.all, key, value)
	if len(minions) == 0 {
		return tui.Flash(tui.FlashInfo, "no minion reports %s=%s", key, value)
	}
	set := targets.FromHosts(key+"="+value, s.hostsOf(minions))
	text := fmt.Sprintf("targets: %s=%s → %s", key, value, countWithNoun(len(set.Hosts), "host"))
	return tea.Batch(
		tui.Flash(tui.FlashOK, "%s", text),
		func() tea.Msg { return tui.TargetsChangedMsg{Set: set} },
	)
}

// hostsOf maps minions onto inventory hosts. A minion the inventory does not
// know is addressed by its own id, so a target set is never silently empty.
func (s *SaltGrains) hostsOf(minions []string) []resource.HostID {
	mapper, _ := s.b.(minionHostMapper)
	hosts := make([]resource.HostID, 0, len(minions))
	for _, id := range minions {
		if mapper != nil {
			if h, ok := mapper.MinionHost(id); ok {
				hosts = append(hosts, h)
				continue
			}
		}
		hosts = append(hosts, resource.NormalizeHostID(id))
	}
	return hosts
}

// openDistribution asks the root to show the distribution dialog of one grain.
func (s *SaltGrains) openDistribution(key string) tea.Cmd {
	values := salt.GrainDistribution(s.all, key)
	if len(values) == 0 {
		return tui.Flash(tui.FlashInfo, "no minion reports a value for %s", key)
	}
	title := fmt.Sprintf("grain distribution %s · %s", key, countWithNoun(len(values), "value"))
	dialog := newGrainsDistDialog(s.theme, s.glyphs, title, values, func(value string) tea.Cmd {
		return tea.Batch(tui.CloseDialog(), s.setTargets(key, value))
	})
	return func() tea.Msg { return tui.OpenDialogMsg{Dialog: dialog} }
}

// Render draws the banner, the header and the visible rows, exactly w × h.
func (s *SaltGrains) Render(w, h int) string {
	s.w, s.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := []string{s.banner(w)}
	if h > 1 {
		lines = append(lines, s.header(w))
		if visible := h - 2; visible > 0 {
			start := 0
			if s.cursor >= visible {
				start = s.cursor - visible + 1
			}
			for i := start; i < start+visible && i < len(s.rows); i++ {
				lines = append(lines, s.rowLine(i, w))
			}
			for i := len(s.rows); i < start+visible; i++ {
				lines = append(lines, render.PadRight("", w))
			}
		}
	}
	return render.FitWith(s.glyphs, strings.Join(lines, "\n"), w, h)
}

// banner is the summary line: the minion, the key count, the mode and, while
// it is open, the filter prompt.
func (s *SaltGrains) banner(w int) string {
	var text string
	switch {
	case s.mode == grainsFilter:
		text = "filter> " + s.input.View()
	case s.loadErr != nil:
		text = "error: " + s.loadErr.Error()
	case s.loading:
		text = "reading the grains of " + s.minion + " " + string(s.glyphs.Ellipsis)
	default:
		source := "cache"
		if s.live {
			source = "live"
		}
		text = fmt.Sprintf("grains %s · %s %s · %s", s.minion, countWithNoun(len(s.rows), "key"), source, s.staleNote())
		if s.filter != "" {
			text += " · filter: " + s.filter
		}
	}
	text = render.Truncate(text, w)
	if s.theme != nil {
		if s.loadErr != nil {
			text = s.theme.Flash.Error.Render(text)
		} else {
			text = s.theme.Dim.Render(text)
		}
	}
	return render.PadRight(text, w)
}

// staleNote is what the banner says about the cache after a live read.
func (s *SaltGrains) staleNote() string {
	if !s.live {
		return "never touches the minion"
	}
	if len(s.diffs) == 0 {
		return "cache up to date"
	}
	return fmt.Sprintf("cache stale: %s differ", countWithNoun(len(s.diffs), "key"))
}

// header is the column title row.
func (s *SaltGrains) header(w int) string {
	line := render.Truncate("GRAIN  VALUE", w)
	if s.theme != nil {
		line = s.theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// rowLine renders one grain row, styled by its staleness and selection.
func (s *SaltGrains) rowLine(i int, w int) string {
	row := s.rows[i]
	line := render.PadRight(render.Truncate(grainsRowText(row), w), w)
	if s.theme == nil {
		return line
	}
	if i == s.cursor {
		return s.theme.Selected.Render(line)
	}
	if row.Stale {
		return s.theme.Warn.Render(line)
	}
	return line
}

// rowText is the plain text of one row: "role  web", and in live mode the
// cached value when it differs.
func grainsRowText(row grainsRow) string {
	text := row.Key + "  " + row.Value
	if row.Stale {
		text += "  ‹was " + row.Was + "›"
	}
	return text
}

// Crumb is "grains <minion>".
func (s *SaltGrains) Crumb() string {
	if s.live {
		return "grains --live " + s.minion
	}
	return "grains " + s.minion
}

// Hints are the keys the screen answers.
func (s *SaltGrains) Hints() []tui.KeyHint {
	if s.mode != grainsIdle {
		return []tui.KeyHint{{Key: "enter", Text: "apply"}, {Key: "esc", Text: "cancel"}}
	}
	return []tui.KeyHint{
		{Key: firstKey(s.keys.Open), Text: "target hosts with this value"},
		{Key: grainsDistKey, Text: "distribution"},
		{Key: firstKey(s.keys.Filter), Text: "filter"},
		{Key: firstKey(s.keys.Refresh), Text: "reload"},
		{Key: firstKey(s.keys.Back), Text: "back"},
	}
}

// Subject is the minion the grains belong to.
func (s *SaltGrains) Subject() tui.Subject {
	return tui.Subject{
		Hosts: []resource.HostID{resource.HostID(s.minion)},
		Kind:  salt.KindMinion,
		Ref:   resource.Ref{Backend: "salt", Kind: salt.KindMinion, Name: s.minion},
	}
}

// Capturing is true while the filter owns the keyboard.
func (s *SaltGrains) Capturing() bool { return s.mode != grainsIdle }

// CommandLine is the `:` command line that reopens this screen.
func (s *SaltGrains) CommandLine() string {
	if s.live {
		return "grains --live " + s.minion
	}
	return "grains " + s.minion
}

// CopyLines returns the plain text of the screen: keys and values as shown.
func (s *SaltGrains) CopyLines() []string {
	head := fmt.Sprintf("grains %s: %d keys", s.minion, len(s.rows))
	if s.live {
		head = fmt.Sprintf("grains --live %s: %d keys, %d differ from the cache",
			s.minion, len(s.rows), len(s.diffs))
	}
	if s.loadErr != nil {
		head = "grains " + s.minion + ": error: " + s.loadErr.Error()
	}
	out := []string{head}
	for _, row := range s.rows {
		out = append(out, grainsRowText(row))
	}
	if s.filter != "" {
		out = append(out, "filter: "+s.filter)
	}
	return out
}

// grainsDistDialog lists the distinct values of one grain and how many minions
// report each. `enter` on a value makes those minions the session targets.
type grainsDistDialog struct {
	t      *theme.Theme
	glyphs render.GlyphSet
	title  string
	values []salt.GrainValue
	choose func(value string) tea.Cmd
	cursor int
	top    int
	h      int
}

var _ tui.Dialog = (*grainsDistDialog)(nil)

// newGrainsDistDialog builds the distribution box. choose runs on `enter` with
// the selected value; nil makes `enter` do nothing.
func newGrainsDistDialog(t *theme.Theme, glyphs render.GlyphSet, title string, values []salt.GrainValue,
	choose func(value string) tea.Cmd) *grainsDistDialog {
	if glyphs.Name == "" {
		glyphs, _ = render.Glyphs("unicode")
	}
	return &grainsDistDialog{t: t, glyphs: glyphs, title: title, values: values, choose: choose}
}

// Title is the box's border title.
func (d *grainsDistDialog) Title() string { return d.title }

// Update moves the cursor, sets targets or closes the box.
func (d *grainsDistDialog) Update(msg tea.Msg) (tui.Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	switch k.String() {
	case "esc", "q":
		return d, tui.CloseDialog()
	case "enter":
		if d.cursor < 0 || d.cursor >= len(d.values) || d.choose == nil {
			return d, nil
		}
		return d, d.choose(d.values[d.cursor].Value)
	case "up", "k":
		d.cursor--
	case "down", "j":
		d.cursor++
	case "pgup":
		d.cursor -= max(1, d.page())
	case "pgdown":
		d.cursor += max(1, d.page())
	case "home", "g":
		d.cursor = 0
	case "end", "G":
		d.cursor = len(d.values) - 1
	}
	d.clamp()
	return d, nil
}

// page is the visible row count, one line of which is the footer.
func (d *grainsDistDialog) page() int {
	if d.h-1 < 1 {
		return 1
	}
	return d.h - 1
}

// clamp keeps the cursor and the scroll window inside the values.
func (d *grainsDistDialog) clamp() {
	if len(d.values) == 0 {
		d.cursor, d.top = 0, 0
		return
	}
	if d.cursor < 0 {
		d.cursor = 0
	}
	if d.cursor >= len(d.values) {
		d.cursor = len(d.values) - 1
	}
	if d.top > d.cursor {
		d.top = d.cursor
	}
	if page := d.page(); d.cursor >= d.top+page {
		d.top = d.cursor - page + 1
	}
	if d.top < 0 {
		d.top = 0
	}
}

// View renders the visible values and the footer, exactly maxW cells wide.
func (d *grainsDistDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	d.h = maxH
	d.clamp()
	rows := maxH - 1
	if rows < 1 {
		rows = 1
	}
	lines := make([]string, 0, rows+1)
	for i := 0; i < rows; i++ {
		idx := d.top + i
		if idx >= len(d.values) {
			lines = append(lines, "")
			continue
		}
		line := d.valueLine(d.values[idx], maxW, idx == d.cursor)
		lines = append(lines, line)
	}
	footer := "enter set targets · esc close"
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
	}
	lines = append(lines, footer)
	for i := range lines {
		lines[i] = render.PadRight(render.TruncateWith(d.glyphs, lines[i], maxW), maxW)
	}
	return strings.Join(lines, "\n")
}

// valueLine is the plain text of one value row: the value and its count.
func (d *grainsDistDialog) valueLine(v salt.GrainValue, maxW int, selected bool) string {
	count := fmt.Sprintf("%d", v.Count)
	text := render.Truncate(v.Value, max(0, maxW-len(count)-2)) + "  " + count
	text = render.PadRight(text, maxW)
	if d.t == nil {
		return text
	}
	if selected {
		return d.t.Selected.Render(text)
	}
	return text
}

// SaltMine is `:mine` (SPEC §8.6 "Mine"): the mine functions and cached values
// per minion, one row each, with the full value one key press away. It is a
// tui.View.
type SaltMine struct {
	env    *tui.Env
	b      MineBackend
	keys   *keymap.Map
	glyphs render.GlyphSet
	theme  *theme.Theme

	base context.Context

	seq     int
	cancel  context.CancelFunc
	entries []salt.MineEntry
	rows    []salt.MineEntry
	filter  string
	mode    grainsMode
	input   textinput.Model
	loading bool
	loadErr error
	cursor  int

	w, h int
}

var _ tui.View = (*SaltMine)(nil)

// mineValueMsg is one full mine value, on its way to the dialog.
type mineValueMsg struct {
	Minion   string
	Function string
	Value    string
	Err      error
}

// NewSaltMine builds the mine browser.
func NewSaltMine(env *tui.Env, b MineBackend) *SaltMine {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	glyphs, _ := render.Glyphs("unicode")
	if env != nil && env.Glyphs.Name != "" {
		glyphs = env.Glyphs
	}
	th := theme.Dark()
	if env != nil && env.Theme != nil {
		th = env.Theme
	}
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	return &SaltMine{env: env, b: b, keys: keys, glyphs: glyphs, theme: th, input: in}
}

// mineLoadMsg is one mine cache read, tagged with Seq.
type mineLoadMsg struct {
	Seq     int
	Entries []salt.MineEntry
	Err     error
}

// Init starts the mine read.
func (m *SaltMine) Init() tea.Cmd { return m.load() }

// SetContext makes every read derive from ctx.
func (m *SaltMine) SetContext(ctx context.Context) {
	if ctx != nil {
		m.base = ctx
	}
}

// Close cancels a read in flight.
func (m *SaltMine) Close() error {
	m.stop()
	return nil
}

// baseCtx is the context every read derives from.
func (m *SaltMine) baseCtx() context.Context {
	if m.base != nil {
		return m.base
	}
	return context.Background()
}

// stop cancels a read in flight.
func (m *SaltMine) stop() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// load abandons a read in flight and asks for the mine cache.
func (m *SaltMine) load() tea.Cmd {
	m.stop()
	m.seq++
	seq := m.seq
	m.entries = nil
	m.rows = nil
	m.loadErr = nil
	m.loading = true
	m.cursor = 0
	if m.b == nil {
		m.loading = false
		m.loadErr = errs.New(errs.Config, "views.saltmine", "no salt backend configured")
		return nil
	}
	b := m.b
	ctx, cancel := context.WithCancel(m.baseCtx())
	m.cancel = cancel
	return func() tea.Msg {
		cctx, ccancel := context.WithTimeout(ctx, mineLoadTimeout)
		defer ccancel()
		entries, err := b.Mine(cctx)
		return mineLoadMsg{Seq: seq, Entries: entries, Err: err}
	}
}

// Update folds reads and full values, and routes keys.
func (m *SaltMine) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch v := msg.(type) {
	case mineLoadMsg:
		if v.Seq != m.seq {
			return m, nil
		}
		return m, m.applyLoad(v)
	case mineValueMsg:
		return m, m.applyValue(v)
	case tui.RefreshRequestMsg:
		return m, m.load()
	case tea.KeyMsg:
		return m.handleKey(v)
	}
	return m, nil
}

// applyLoad keeps the entries and rebuilds the visible rows.
func (m *SaltMine) applyLoad(v mineLoadMsg) tea.Cmd {
	m.loading = false
	if v.Err != nil {
		m.loadErr = v.Err
		m.rows = nil
		return nil
	}
	m.entries = v.Entries
	m.rebuild()
	return nil
}

// rebuild applies the filter to the entries.
func (m *SaltMine) rebuild() {
	needle := strings.ToLower(strings.TrimSpace(m.filter))
	m.rows = make([]salt.MineEntry, 0, len(m.entries))
	for _, e := range m.entries {
		if needle != "" &&
			!strings.Contains(strings.ToLower(e.Minion+" "+e.Function+" "+e.Value), needle) {
			continue
		}
		m.rows = append(m.rows, e)
	}
	m.clampCursor()
}

// clampCursor keeps the cursor inside the rows.
func (m *SaltMine) clampCursor() {
	last := len(m.rows) - 1
	if last < 0 {
		m.cursor = 0
		return
	}
	if m.cursor > last {
		m.cursor = last
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// handleKey routes one key press.
func (m *SaltMine) handleKey(msg tea.KeyMsg) (tui.View, tea.Cmd) {
	if m.mode != grainsIdle {
		return m, m.inputKey(msg)
	}
	key := msg.String()
	switch {
	case m.keys.Filter.Matches(key):
		return m, m.beginFilter()
	case m.keys.Up.Matches(key):
		m.cursor--
		m.clampCursor()
		return m, nil
	case m.keys.Down.Matches(key):
		m.cursor++
		m.clampCursor()
		return m, nil
	case m.keys.PageUp.Matches(key):
		m.cursor -= max(1, m.h-3)
		m.clampCursor()
		return m, nil
	case m.keys.PageDown.Matches(key):
		m.cursor += max(1, m.h-3)
		m.clampCursor()
		return m, nil
	case m.keys.Top.Matches(key):
		m.cursor = 0
		return m, nil
	case m.keys.Bottom.Matches(key):
		m.cursor = len(m.rows) - 1
		m.clampCursor()
		return m, nil
	case m.keys.Open.Matches(key):
		return m, m.openValue()
	case m.keys.Refresh.Matches(key):
		return m, m.load()
	case m.keys.Back.Matches(key):
		if m.filter != "" {
			m.setFilter("")
			return m, nil
		}
		return m, tui.Pop()
	}
	return m, nil
}

// beginFilter opens the free-text filter.
func (m *SaltMine) beginFilter() tea.Cmd {
	m.mode = grainsFilter
	m.input.SetValue(m.filter)
	m.input.CursorEnd()
	m.input.Focus()
	return nil
}

// inputKey edits the filter. enter keeps it, esc drops it.
func (m *SaltMine) inputKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		m.mode = grainsIdle
		m.input.Blur()
		m.setFilter(strings.TrimSpace(m.input.Value()))
		return nil
	case "esc":
		m.mode = grainsIdle
		m.input.Blur()
		m.setFilter("")
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.setFilter(m.input.Value())
	return cmd
}

// setFilter installs the filter text and rebuilds the rows.
func (m *SaltMine) setFilter(text string) {
	m.filter = text
	m.rebuild()
}

// openValue reads the full value of the selected row.
func (m *SaltMine) openValue() tea.Cmd {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return tui.Flash(tui.FlashInfo, "no mine values")
	}
	entry := m.rows[m.cursor]
	b, base := m.b, m.baseCtx()
	if b == nil {
		return tui.Flash(tui.FlashError, "salt is not configured")
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, mineValueTimeout)
		defer cancel()
		value, err := b.MineValue(ctx, entry.Minion, entry.Function)
		return mineValueMsg{Minion: entry.Minion, Function: entry.Function, Value: value, Err: err}
	}
}

// applyValue shows the full value in a dialog. The value lives in the dialog
// and nowhere else: the view keeps the row, never the text.
func (m *SaltMine) applyValue(v mineValueMsg) tea.Cmd {
	if v.Err != nil {
		return tui.Flash(tui.FlashError, "mine %s: %v", v.Function, v.Err)
	}
	title := fmt.Sprintf("mine %s · %s", v.Function, v.Minion)
	dialog := newPillarValueDialog(m.theme, title, v.Value)
	return func() tea.Msg { return tui.OpenDialogMsg{Dialog: dialog} }
}

// Render draws the banner, the header and the visible rows, exactly w × h.
func (m *SaltMine) Render(w, h int) string {
	m.w, m.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := []string{m.banner(w)}
	if h > 1 {
		lines = append(lines, m.header(w))
		if visible := h - 2; visible > 0 {
			start := 0
			if m.cursor >= visible {
				start = m.cursor - visible + 1
			}
			for i := start; i < start+visible && i < len(m.rows); i++ {
				lines = append(lines, m.rowLine(i, w))
			}
			for i := len(m.rows); i < start+visible; i++ {
				lines = append(lines, render.PadRight("", w))
			}
		}
	}
	return render.FitWith(m.glyphs, strings.Join(lines, "\n"), w, h)
}

// banner is the summary line: the row count, the minion count and the filter.
func (m *SaltMine) banner(w int) string {
	var text string
	switch {
	case m.mode == grainsFilter:
		text = "filter> " + m.input.View()
	case m.loadErr != nil:
		text = "error: " + m.loadErr.Error()
	case m.loading:
		text = "reading the mine cache " + string(m.glyphs.Ellipsis)
	default:
		text = fmt.Sprintf("mine · %s · %s", countWithNoun(len(m.rows), "value"), countWithNoun(len(m.minions()), "minion"))
		if m.filter != "" {
			text += " · filter: " + m.filter
		}
	}
	text = render.Truncate(text, w)
	if m.theme != nil {
		if m.loadErr != nil {
			text = m.theme.Flash.Error.Render(text)
		} else {
			text = m.theme.Dim.Render(text)
		}
	}
	return render.PadRight(text, w)
}

// minions lists the distinct minions of the visible rows, sorted.
func (m *SaltMine) minions() []string {
	seen := make(map[string]bool, len(m.rows))
	out := make([]string, 0, len(m.rows))
	for _, e := range m.rows {
		if seen[e.Minion] {
			continue
		}
		seen[e.Minion] = true
		out = append(out, e.Minion)
	}
	sort.Strings(out)
	return out
}

// header is the column title row.
func (m *SaltMine) header(w int) string {
	line := render.Truncate("MINION  FUNCTION  VALUE  SIZE", w)
	if m.theme != nil {
		line = m.theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// rowLine renders one mine row, styled by selection.
func (m *SaltMine) rowLine(i int, w int) string {
	line := render.PadRight(render.Truncate(mineRowText(m.rows[i]), w), w)
	if m.theme == nil {
		return line
	}
	if i == m.cursor {
		return m.theme.Selected.Render(line)
	}
	return line
}

// mineRowText is the plain text of one mine row.
func mineRowText(e salt.MineEntry) string {
	return fmt.Sprintf("%s  %s  %s  %s", e.Minion, e.Function, e.Value, render.Bytes(e.Size))
}

// Crumb is "mine (<n>)".
func (m *SaltMine) Crumb() string { return fmt.Sprintf("mine (%d)", len(m.rows)) }

// Hints are the keys the screen answers.
func (m *SaltMine) Hints() []tui.KeyHint {
	if m.mode != grainsIdle {
		return []tui.KeyHint{{Key: "enter", Text: "apply"}, {Key: "esc", Text: "cancel"}}
	}
	return []tui.KeyHint{
		{Key: firstKey(m.keys.Open), Text: "full value"},
		{Key: firstKey(m.keys.Filter), Text: "filter"},
		{Key: firstKey(m.keys.Refresh), Text: "refresh"},
		{Key: firstKey(m.keys.Back), Text: "back"},
	}
}

// Subject is the minion of the selected row.
func (m *SaltMine) Subject() tui.Subject {
	subj := tui.Subject{Kind: salt.KindMine}
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return subj
	}
	entry := m.rows[m.cursor]
	subj.Ref = resource.Ref{Backend: "salt", Kind: salt.KindMine, Namespace: entry.Minion, Name: entry.Function}
	subj.Hosts = []resource.HostID{resource.HostID(entry.Minion)}
	return subj
}

// Capturing is true while the filter owns the keyboard.
func (m *SaltMine) Capturing() bool { return m.mode != grainsIdle }

// CommandLine is the `:` command line that reopens this screen.
func (m *SaltMine) CommandLine() string { return "mine" }

// CopyLines returns the plain text of the screen.
func (m *SaltMine) CopyLines() []string {
	head := fmt.Sprintf("mine: %d values on %d minions", len(m.rows), len(m.minions()))
	if m.loadErr != nil {
		head = "mine: error: " + m.loadErr.Error()
	}
	out := []string{head}
	for _, e := range m.rows {
		out = append(out, mineRowText(e))
	}
	if m.filter != "" {
		out = append(out, "filter: "+m.filter)
	}
	return out
}

// grainsFactory builds the grains explorer of the query's minion and mode.
func grainsFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.grainsFactory", "no backends configured")
	}
	minion := strings.TrimSpace(q.Args["minion"])
	if minion == "" {
		return nil, errs.New(errs.Config, "views.grainsFactory", "grains need a minion")
	}
	b, ok := saltBackend(env, q)
	if !ok {
		return nil, errs.New(errs.Config, "views.grainsFactory", "no salt backend registered")
	}
	gb, ok := b.(GrainsBackend)
	if !ok {
		return nil, errs.New(errs.Config, "views.grainsFactory", "backend cannot read grains")
	}
	return NewSaltGrains(env, gb, minion, q.Args["live"] == "1"), nil
}

// mineFactory builds the mine browser.
func mineFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.mineFactory", "no backends configured")
	}
	b, ok := saltBackend(env, q)
	if !ok {
		return nil, errs.New(errs.Config, "views.mineFactory", "no salt backend registered")
	}
	mb, ok := b.(MineBackend)
	if !ok {
		return nil, errs.New(errs.Config, "views.mineFactory", "backend cannot read the mine cache")
	}
	v := NewSaltMine(env, mb)
	if filter := strings.TrimSpace(q.Args["filter"]); filter != "" {
		v.setFilter(filter)
	}
	return v, nil
}

// saltBackend is the backend a Salt view reads: the query's, else "salt".
func saltBackend(env *tui.Env, q resource.Query) (backend.Backend, bool) {
	name := strings.TrimSpace(q.Backend)
	if name == "" {
		name = "salt"
	}
	return env.Registry.Get(name)
}

// countWithNoun renders "14 minions": the noun agreeing with n.
func countWithNoun(n int, noun string) string {
	return fmt.Sprintf("%d %s", n, pluralWord(n, noun))
}
