package views

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/linux"
	"jumpr/internal/backend/salt"
	"jumpr/internal/errs"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// KindSaltEvents is the factory key of the event screen (task 096). It is a
// view kind, not a backend kind: the salt backend serves salt-minion.
const KindSaltEvents resource.KindName = "salt-events"

const (
	// saltEventsMax caps the buffer the screen keeps. A paused screen keeps
	// filling it: the newest saltEventsMax events are kept and the oldest fall
	// out.
	saltEventsMax = 10000
	// saltEventsBatch is the most events one batch carries.
	saltEventsBatch = 64
	// The fixed columns; the summary takes the rest of the width.
	saltEventsTimeWidth   = 12 // "15:04:05.000"
	saltEventsKindWidth   = 12 // "minion-start"
	saltEventsMinionWidth = 18
	// saltEventsPaused is the banner line of a paused screen.
	saltEventsPaused = "paused"
	// saltEventsEnded is the banner line of a stream that ended by itself.
	saltEventsEnded = "stream ended — r to reconnect"
)

// SaltEventsSource is what the event screen needs from the Salt backend.
// *salt.Backend implements it.
type SaltEventsSource interface {
	// Events subscribes to the master's event bus.
	Events(ctx context.Context) (<-chan salt.Event, error)
	// MinionHost maps a minion ID onto its inventory host.
	MinionHost(id string) (resource.HostID, bool)
}

// saltEventsMode is what the keyboard is doing.
type saltEventsMode int

const (
	saltEventsIdle saltEventsMode = iota
	saltEventsFilter
)

// saltEventsStartMsg is the started stream, or the error that kept it from
// starting.
type saltEventsStartMsg struct {
	Seq int
	Ch  <-chan salt.Event
	Err error
}

// saltEventsBatchMsg is one batch of the stream. Closed means the stream ended
// (a master restart) or the pane was closed.
type saltEventsBatchMsg struct {
	Seq    int
	Events []salt.Event
	Closed bool
}

// SaltEvents is `:events` (SPEC §8.6 "Events"): the master's event bus live in
// a pane. It follows the newest event, filters by text, toggles kinds, hides
// jumpr's own polling runners and pauses without losing what arrives. It is a
// tui.View; the stream runs in a tea.Cmd and is tied to the pane's context.
type SaltEvents struct {
	env    *tui.Env
	src    SaltEventsSource
	keys   *keymap.Map
	glyphs render.GlyphSet
	theme  *theme.Theme

	// base is the pane's context; the stream derives from it.
	base context.Context

	seq    int
	cancel context.CancelFunc
	ch     <-chan salt.Event

	events []salt.Event
	view   []salt.Event
	cursor int
	// follow pins the cursor to the newest event; `space` pauses it.
	follow bool
	paused bool
	// hidden are the kind toggles of `1`–`6`, indexed like
	// saltEventsToggleKinds. true means the kind is hidden.
	hidden [6]bool
	// hideOwn hides jumpr's own polling runner events; `h` toggles it.
	hideOwn bool
	// ended is true when the stream closed by itself, so the screen can offer
	// a reconnect.
	ended bool
	// loading is true while the stream is being started.
	loading bool
	loadErr error

	mode      saltEventsMode
	input     textinput.Model
	filter    string
	filterRe  *regexp.Regexp
	filterBad bool

	w, h int
}

var _ tui.View = (*SaltEvents)(nil)

// NewSaltEvents builds the event screen over src.
func NewSaltEvents(env *tui.Env, src SaltEventsSource) *SaltEvents {
	keys := keymap.Default()
	glyphs, _ := render.Glyphs("unicode")
	th := theme.Dark()
	if env != nil {
		if env.Keys != nil {
			keys = env.Keys
		}
		if env.Glyphs.Name != "" {
			glyphs = env.Glyphs
		}
		if env.Theme != nil {
			th = env.Theme
		}
	}
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	return &SaltEvents{
		env:     env,
		src:     src,
		keys:    keys,
		glyphs:  glyphs,
		theme:   th,
		follow:  true,
		hideOwn: true,
		input:   in,
	}
}

// Init starts the event stream.
func (s *SaltEvents) Init() tea.Cmd { return s.start() }

// SetContext makes the stream derive from ctx, so closing the pane stops it.
func (s *SaltEvents) SetContext(ctx context.Context) {
	if ctx != nil {
		s.base = ctx
	}
}

// Close stops the stream. The stack calls it when the view is popped.
func (s *SaltEvents) Close() error {
	s.stop()
	return nil
}

// baseCtx is the context the stream derives from.
func (s *SaltEvents) baseCtx() context.Context {
	if s.base != nil {
		return s.base
	}
	return context.Background()
}

// stop cancels a stream in flight.
func (s *SaltEvents) stop() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.ch = nil
	s.loading = false
}

// start abandons a stream in flight and subscribes again. `r` calls it too,
// which is the reconnect after "stream ended".
func (s *SaltEvents) start() tea.Cmd {
	s.stop()
	s.seq++
	seq := s.seq
	s.ended = false
	s.loadErr = nil
	s.loading = true
	if s.src == nil {
		s.loading = false
		s.loadErr = errs.New(errs.Config, "views.saltevents", "no salt backend configured")
		return nil
	}
	src, base := s.src, s.baseCtx()
	ctx, cancel := context.WithCancel(base)
	s.cancel = cancel
	return func() tea.Msg {
		ch, err := src.Events(ctx)
		return saltEventsStartMsg{Seq: seq, Ch: ch, Err: err}
	}
}

// nextBatch asks for the next batch of the stream.
func (s *SaltEvents) nextBatch() tea.Cmd {
	ch, seq := s.ch, s.seq
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		events, closed := tui.NextBatch(ch, saltEventsBatch)
		return saltEventsBatchMsg{Seq: seq, Events: events, Closed: closed}
	}
}

// Update folds started streams and batches, and routes keys. A batch of a
// superseded stream is dropped.
func (s *SaltEvents) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case saltEventsStartMsg:
		return s, s.applyStart(m)
	case saltEventsBatchMsg:
		return s, s.applyBatch(m)
	case tea.KeyMsg:
		return s.handleKey(m)
	}
	return s, nil
}

// applyStart keeps the stream, or the reason it could not start, and asks for
// its first batch.
func (s *SaltEvents) applyStart(m saltEventsStartMsg) tea.Cmd {
	if m.Seq != s.seq {
		return nil
	}
	s.loading = false
	if m.Err != nil {
		s.loadErr = m.Err
		s.ch = nil
		return nil
	}
	s.ch = m.Ch
	s.loadErr = nil
	return s.nextBatch()
}

// applyBatch appends one batch and asks for the next one. A closed stream is
// not an error: the view says so and offers `r`.
func (s *SaltEvents) applyBatch(m saltEventsBatchMsg) tea.Cmd {
	if m.Seq != s.seq {
		return nil
	}
	if len(m.Events) > 0 {
		s.add(m.Events)
	}
	if m.Closed {
		s.ch = nil
		s.loading = false
		s.ended = true
		return nil
	}
	return s.nextBatch()
}

// add appends events, keeps the newest saltEventsMax and rebuilds the rows. A
// following screen keeps the cursor on the newest event; a paused one does not
// move, though the buffer keeps growing.
func (s *SaltEvents) add(events []salt.Event) {
	s.events = append(s.events, events...)
	if extra := len(s.events) - saltEventsMax; extra > 0 {
		kept := make([]salt.Event, saltEventsMax)
		copy(kept, s.events[extra:])
		s.events = kept
		s.cursor -= extra
		if s.cursor < 0 {
			s.cursor = 0
		}
	}
	s.rebuild()
	if s.follow && !s.paused && len(s.view) > 0 {
		s.cursor = len(s.view) - 1
	}
}

// saltEventsToggleKinds is the kind order of the `1`–`6` keys. Runner events
// have no key (they are `h`'s business) and other events are always shown.
func saltEventsToggleKinds() [6]string {
	return [6]string{
		salt.EventJobNew, salt.EventJobRet, salt.EventAuth,
		salt.EventMinionStart, salt.EventBeacon, salt.EventKey,
	}
}

// kindHidden reports whether a kind is switched off.
func (s *SaltEvents) kindHidden(kind string) bool {
	for i, k := range saltEventsToggleKinds() {
		if k == kind {
			return s.hidden[i]
		}
	}
	return false
}

// visible reports whether ev passes the kind toggles and the text filter.
func (s *SaltEvents) visible(ev salt.Event) bool {
	if s.kindHidden(ev.Kind) {
		return false
	}
	if ev.Kind == salt.EventRunner && s.hideOwn && salt.OwnRunner(ev.Fun) {
		return false
	}
	return s.matchText(ev)
}

// matchText applies the `/` filter to an event: a case-insensitive substring of
// its tag, kind, minion, function and summary, or a regex after "~".
func (s *SaltEvents) matchText(ev salt.Event) bool {
	if s.filter == "" {
		return true
	}
	if s.filterBad {
		return false
	}
	text := saltEventText(ev)
	if s.filterRe != nil {
		return s.filterRe.MatchString(text)
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(s.filter))
}

// saltEventText is the plain text a filter matches on.
func saltEventText(ev salt.Event) string {
	return strings.Join([]string{ev.Tag, ev.Kind, ev.Minion, ev.JID, ev.Fun, saltEventSummary(ev)}, " ")
}

// rebuild rebuilds the visible rows from the buffer.
func (s *SaltEvents) rebuild() {
	out := make([]salt.Event, 0, len(s.events))
	for _, ev := range s.events {
		if s.visible(ev) {
			out = append(out, ev)
		}
	}
	s.view = out
	s.clampCursor()
}

// setFilter installs the filter text; a "~" prefix makes it a regex.
func (s *SaltEvents) setFilter(text string) {
	s.filter = text
	s.filterBad = false
	s.filterRe = nil
	if pat, ok := strings.CutPrefix(text, "~"); ok {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			s.filterBad = true
		} else {
			s.filterRe = re
		}
	}
	s.rebuild()
}

// clampCursor keeps the cursor inside the rows.
func (s *SaltEvents) clampCursor() {
	last := len(s.view) - 1
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

// move moves the cursor by delta and stops following.
func (s *SaltEvents) move(delta int) {
	s.cursor += delta
	s.clampCursor()
	s.follow = false
}

// pageSize is one screen of rows.
func (s *SaltEvents) pageSize() int {
	if n := s.h - 2; n > 1 {
		return n
	}
	return 1
}

// selected is the event under the cursor.
func (s *SaltEvents) selected() (salt.Event, bool) {
	if s.cursor < 0 || s.cursor >= len(s.view) {
		return salt.Event{}, false
	}
	return s.view[s.cursor], true
}

// handleKey routes one key press.
func (s *SaltEvents) handleKey(m tea.KeyMsg) (tui.View, tea.Cmd) {
	if s.mode == saltEventsFilter {
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
		s.move(-s.pageSize())
		return s, nil
	case s.keys.PageDown.Matches(key):
		s.move(s.pageSize())
		return s, nil
	case s.keys.Top.Matches(key):
		s.cursor = 0
		s.follow = false
		return s, nil
	case s.keys.Bottom.Matches(key):
		if len(s.view) > 0 {
			s.cursor = len(s.view) - 1
		}
		s.follow = true
		return s, nil
	case s.keys.Open.Matches(key):
		return s, s.open()
	case s.keys.Refresh.Matches(key):
		return s, s.start()
	case s.keys.Back.Matches(key):
		if s.filter != "" {
			s.setFilter("")
			return s, nil
		}
		return s, tui.Pop()
	}
	switch key {
	case " ":
		return s, s.togglePause()
	case "h":
		s.hideOwn = !s.hideOwn
		s.rebuild()
		return s, nil
	}
	if len(key) == 1 && key[0] >= '1' && key[0] <= '6' {
		i := int(key[0] - '1')
		s.hidden[i] = !s.hidden[i]
		s.rebuild()
		return s, nil
	}
	return s, nil
}

// togglePause freezes the view without stopping the stream, and resumes on the
// newest event.
func (s *SaltEvents) togglePause() tea.Cmd {
	s.paused = !s.paused
	if s.paused {
		s.follow = false
		return tui.Flash(tui.FlashInfo, "paused: the buffer keeps filling")
	}
	s.follow = true
	if len(s.view) > 0 {
		s.cursor = len(s.view) - 1
	}
	return tui.Flash(tui.FlashInfo, "following")
}

// beginFilter opens the text filter.
func (s *SaltEvents) beginFilter() tea.Cmd {
	s.mode = saltEventsFilter
	s.input.SetValue(s.filter)
	s.input.CursorEnd()
	s.input.Focus()
	return nil
}

// inputKey edits the filter. enter keeps it, esc drops it.
func (s *SaltEvents) inputKey(m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "enter":
		s.mode = saltEventsIdle
		s.input.Blur()
		s.setFilter(strings.TrimSpace(s.input.Value()))
		return nil
	case "esc":
		s.mode = saltEventsIdle
		s.input.Blur()
		s.setFilter("")
		return nil
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(m)
	s.setFilter(s.input.Value())
	return cmd
}

// open drills in: an event with a job id opens the job detail (task 072), an
// event with a minion opens its host.
func (s *SaltEvents) open() tea.Cmd {
	ev, ok := s.selected()
	if !ok {
		return tui.Flash(tui.FlashInfo, "no events")
	}
	if ev.JID != "" && (ev.Kind == salt.EventJobNew || ev.Kind == salt.EventJobRet) {
		return tui.OpenQuery(resource.Query{Kind: KindSaltJobDetail, Args: map[string]string{"jid": ev.JID}})
	}
	if ev.Minion != "" {
		if host, ok := s.minionHost(ev.Minion); ok {
			return tui.OpenQuery(resource.Query{Kind: linux.KindHostOverview, Hosts: []resource.HostID{host}})
		}
		return tui.Flash(tui.FlashWarn, "minion %s is not a known host", ev.Minion)
	}
	return tui.Flash(tui.FlashInfo, "this event names no job or minion")
}

// minionHost resolves a minion through the backend, never panicking on a nil
// source.
func (s *SaltEvents) minionHost(minion string) (resource.HostID, bool) {
	if s.src == nil || minion == "" {
		return "", false
	}
	return s.src.MinionHost(minion)
}

// Render draws the banner, the header and the visible rows, exactly w × h.
func (s *SaltEvents) Render(w, h int) string {
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
			for i := start; i < start+visible && i < len(s.view); i++ {
				lines = append(lines, s.rowLine(i, w))
			}
			for i := len(s.view); i < start+visible; i++ {
				lines = append(lines, render.PadRight("", w))
			}
		}
	}
	return render.FitWith(s.glyphs, strings.Join(lines, "\n"), w, h)
}

// banner is the summary line: the counts, the pause and filter state, or why
// there is no stream.
func (s *SaltEvents) banner(w int) string {
	var text string
	switch {
	case s.mode == saltEventsFilter:
		text = "filter> " + s.input.View()
	case s.loadErr != nil:
		text = "error: " + s.loadErr.Error()
	case s.ended:
		text = saltEventsEnded
	case s.loading:
		text = "subscribing to the event bus " + string(s.glyphs.Ellipsis)
	default:
		text = fmt.Sprintf("events · %s shown · %s buffered",
			render.Int(int64(len(s.view))), render.Int(int64(len(s.events))))
		if s.paused {
			text += " · " + saltEventsPaused
		}
		if s.filter != "" {
			text += " · /filter: " + s.filter
		}
		if s.hideOwn {
			text += " · own runners hidden"
		}
	}
	text = render.Truncate(text, w)
	if s.theme != nil {
		switch {
		case s.loadErr != nil || s.ended:
			text = s.theme.Flash.Error.Render(text)
		default:
			text = s.theme.Dim.Render(text)
		}
	}
	return render.PadRight(text, w)
}

// header is the column title row.
func (s *SaltEvents) header(w int) string {
	line := "TIME" + strings.Repeat(" ", max(0, saltEventsTimeWidth-len("TIME"))) +
		"  " + "KIND" + strings.Repeat(" ", max(0, saltEventsKindWidth-len("KIND"))) +
		"  " + "MINION" + strings.Repeat(" ", max(0, saltEventsMinionWidth-len("MINION"))) +
		"  SUMMARY"
	line = render.Truncate(line, w)
	if s.theme != nil {
		line = s.theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// rowLine renders one event: time, coloured kind, minion and summary.
func (s *SaltEvents) rowLine(i int, w int) string {
	ev := s.view[i]
	plain := saltEventsColumns(ev)
	line := plain
	if i == s.cursor {
		if s.theme != nil {
			return s.theme.Selected.Render(render.PadRight(render.Truncate(line, w), w))
		}
		return render.PadRight(render.Truncate(line, w), w)
	}
	if s.theme != nil && ev.Kind != "" {
		coloured := saltEventsColumnsStyled(s.theme, ev)
		return render.PadRight(render.Truncate(coloured, w), w)
	}
	return render.PadRight(render.Truncate(line, w), w)
}

// saltEventsColumns is one row as plain text, the columns padded.
func saltEventsColumns(ev salt.Event) string {
	return saltEventTimeText(ev.Time) + strings.Repeat(" ", max(0, saltEventsTimeWidth-len(saltEventTimeText(ev.Time)))) +
		"  " + ev.Kind + strings.Repeat(" ", max(0, saltEventsKindWidth-len(ev.Kind))) +
		"  " + ev.Minion + strings.Repeat(" ", max(0, saltEventsMinionWidth-len(ev.Minion))) +
		"  " + saltEventSummary(ev)
}

// saltEventsColumnsStyled is one row with the kind coloured by its severity.
func saltEventsColumnsStyled(th *theme.Theme, ev salt.Event) string {
	kind := th.Base.Render(ev.Kind)
	switch ev.Kind {
	case salt.EventJobNew:
		kind = th.Accent.Render(ev.Kind)
	case salt.EventJobRet:
		if saltEventFailed(ev) {
			kind = th.Crit.Render(ev.Kind)
		} else {
			kind = th.OK.Render(ev.Kind)
		}
	case salt.EventAuth, salt.EventKey:
		kind = th.Warn.Render(ev.Kind)
	case salt.EventMinionStart:
		kind = th.OK.Render(ev.Kind)
	case salt.EventRunner:
		kind = th.Dim.Render(ev.Kind)
	case salt.EventOther:
		kind = th.Faint.Render(ev.Kind)
	}
	return saltEventTimeText(ev.Time) + strings.Repeat(" ", max(0, saltEventsTimeWidth-len(saltEventTimeText(ev.Time)))) +
		"  " + kind + strings.Repeat(" ", max(0, saltEventsKindWidth-len(ev.Kind))) +
		"  " + ev.Minion + strings.Repeat(" ", max(0, saltEventsMinionWidth-len(ev.Minion))) +
		"  " + saltEventSummary(ev)
}

// saltEventTimeText is the time column; a missing stamp is shown as unknown.
func saltEventTimeText(t time.Time) string {
	if t.IsZero() {
		return "--:--:--.---"
	}
	return t.Format("15:04:05.000")
}

// saltEventSummary is the gist of an event: "state.apply ret ok",
// "state.apply ret rc=1", "auth pend". A pillar.* return never shows its
// payload, as in task 093.
func saltEventSummary(ev salt.Event) string {
	switch ev.Kind {
	case salt.EventJobNew:
		return joinNonEmpty(ev.Fun, "new")
	case salt.EventJobRet:
		fun := ev.Fun
		if fun == "" {
			fun = "job"
		}
		if salt.HiddenFunction(fun) {
			return fun + " ret hidden"
		}
		return fun + " ret " + saltEventOutcome(ev)
	case salt.EventAuth:
		return "auth " + saltEventAct(ev, "pend")
	case salt.EventKey:
		return "key " + saltEventAct(ev, "change")
	case salt.EventMinionStart:
		return "minion started"
	case salt.EventBeacon:
		if name := beaconName(ev.Tag); name != "" {
			return name + " beacon"
		}
		return "beacon"
	case salt.EventRunner:
		return joinNonEmpty(ev.Fun, "runner")
	}
	if ev.Tag != "" {
		return ev.Tag
	}
	return "event"
}

// joinNonEmpty joins the non-empty parts with a space.
func joinNonEmpty(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return strings.Join(out, " ")
}

// saltEventOutcome is a job return's outcome: "ok", "rc=N" or "fail".
func saltEventOutcome(ev salt.Event) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ev.Data, &fields); err != nil {
		return "ret"
	}
	if rc, ok := intField(fields, "retcode"); ok {
		if rc != 0 {
			return fmt.Sprintf("rc=%d", rc)
		}
		return "ok"
	}
	// A minion's own return may carry the retcode in a nested object.
	if raw, ok := fields["return"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err == nil {
			if rc, ok := intField(inner, "retcode"); ok {
				if rc != 0 {
					return fmt.Sprintf("rc=%d", rc)
				}
				return "ok"
			}
		}
	}
	if raw, ok := fields["success"]; ok {
		var success bool
		if err := json.Unmarshal(raw, &success); err == nil && !success {
			return "fail"
		}
	}
	return "ok"
}

// saltEventFailed reports whether a job return failed.
func saltEventFailed(ev salt.Event) bool {
	outcome := saltEventOutcome(ev)
	return outcome != "ok" && outcome != "ret"
}

// saltEventAct is the payload's action ("pend", "accept", "reject", …),
// normalised to a word, or def when the payload has none.
func saltEventAct(ev salt.Event, def string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ev.Data, &fields); err != nil {
		return def
	}
	raw, ok := fields["act"]
	if !ok {
		return def
	}
	var act string
	if err := json.Unmarshal(raw, &act); err != nil {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(act)) {
	case "":
		return def
	case "acc", "accept", "accepted":
		return "accepted"
	case "rej", "reject", "rejected":
		return "rejected"
	case "den", "deny", "denied":
		return "denied"
	case "del", "delete", "deleted":
		return "deleted"
	default:
		return strings.ToLower(strings.TrimSpace(act))
	}
}

// beaconName is the beacon's name from salt/beacon/<minion>/<name>/….
func beaconName(tag string) string {
	rest, ok := strings.CutPrefix(tag, "salt/beacon/")
	if !ok {
		return ""
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// intField reads one integer field of a payload.
func intField(fields map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := fields[key]
	if !ok {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

// Crumb is "events (n)".
func (s *SaltEvents) Crumb() string {
	return fmt.Sprintf("events (%d)", len(s.view))
}

// Hints are the keys the screen answers.
func (s *SaltEvents) Hints() []tui.KeyHint {
	if s.mode == saltEventsFilter {
		return []tui.KeyHint{{Key: "enter", Text: "apply"}, {Key: "esc", Text: "cancel"}}
	}
	pause := "pause"
	if s.paused {
		pause = "resume"
	}
	own := "own runners"
	if s.hideOwn {
		own = "show own runners"
	}
	return []tui.KeyHint{
		{Key: firstKey(s.keys.Open), Text: "open"},
		{Key: "space", Text: pause},
		{Key: "1-6", Text: "kinds"},
		{Key: "h", Text: own},
		{Key: firstKey(s.keys.Filter), Text: "filter"},
		{Key: firstKey(s.keys.Refresh), Text: "reconnect"},
		{Key: firstKey(s.keys.Back), Text: "back"},
	}
}

// Subject is the selected event's minion, when it names one.
func (s *SaltEvents) Subject() tui.Subject {
	ev, ok := s.selected()
	if !ok {
		return tui.Subject{}
	}
	if ev.Minion == "" {
		return tui.Subject{}
	}
	return tui.Subject{
		Hosts: []resource.HostID{resource.HostID(ev.Minion)},
		Kind:  salt.KindMinion,
		Ref:   resource.Ref{Backend: "salt", Kind: salt.KindMinion, Name: ev.Minion},
	}
}

// Capturing is true while the filter owns the keyboard.
func (s *SaltEvents) Capturing() bool { return s.mode != saltEventsIdle }

// CommandLine is the `:` command line that reopens this screen.
func (s *SaltEvents) CommandLine() string { return "events" }

// CopyLines returns the plain text of the screen.
func (s *SaltEvents) CopyLines() []string {
	head := fmt.Sprintf("events: %d shown, %d buffered", len(s.view), len(s.events))
	if s.ended {
		head = "events: " + saltEventsEnded
	}
	out := []string{head}
	for _, ev := range s.view {
		out = append(out, saltEventsColumns(ev))
	}
	if s.filter != "" {
		out = append(out, "filter: "+s.filter)
	}
	return out
}

// saltEventsFactory builds the event screen. The salt backend is looked up by
// registration name, so the factory never needs the concrete type.
func saltEventsFactory(env *tui.Env, _ resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.saltEventsFactory", "no backends configured")
	}
	name := "salt"
	b, ok := env.Registry.Get(name)
	if !ok {
		return nil, errs.New(errs.Config, "views.saltEventsFactory", "no salt backend registered")
	}
	src, ok := b.(SaltEventsSource)
	if !ok {
		return nil, errs.New(errs.Config, "views.saltEventsFactory", "backend cannot stream events")
	}
	return NewSaltEvents(env, src), nil
}
