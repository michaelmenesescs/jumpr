package linux

import (
	"context"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/fanout"
	"jumpr/internal/probe"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
)

// Section IDs of the dependency and timer batches.
const (
	secDeps       = "deps"
	secDepsRev    = "deps-reverse"
	secTimers     = "timers"
	timerShowID   = "show"
	timerShowNext = "NextElapseUSecRealtime"
	timerShowLast = "LastTriggerUSec"
)

// Read-only remote commands of this file.
const (
	// cmdTimers lists every timer, including the ones that never ran.
	cmdTimers = "systemctl list-timers --all --no-pager --no-legend"
)

// Op names used on the errors built here.
const depsOp = "linux.deps"

// timerStampLayouts are the timestamp shapes systemctl prints in the timer
// table and in `systemctl show` (the day name is locale-dependent but always a
// single word, so the second layout covers it).
var timerStampLayouts = []string{
	"Mon 2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05 MST",
}

// depGlyphs are the tree-drawing characters `systemctl list-dependencies`
// prints without --plain; they are not part of a unit name.
const depGlyphs = "│├└─┬┴┼|`- "

// depStateGlyphs are the optional state marks systemd puts before a unit.
const depStateGlyphs = "●○×✗*"

// DepNode is one line of `systemctl list-dependencies --plain`. The root unit is
// depth 0 and every two leading spaces add one level.
type DepNode struct {
	Unit  string
	Depth int
}

// ParseDependencies parses `systemctl list-dependencies <u> --plain --no-pager`
// and the same command with --reverse. Indentation (two spaces per level) gives
// the depth; a leading state glyph (●, ○, ×) and any tree-drawing leftovers are
// dropped. Blank output yields an empty slice; nothing here can fail.
func ParseDependencies(out string) []DepNode {
	var nodes []DepNode
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		spaces := 0
		for spaces < len(line) && line[spaces] == ' ' {
			spaces++
		}
		unit := strings.TrimSpace(strings.TrimLeft(line[spaces:], depStateGlyphs))
		unit = strings.TrimSpace(strings.TrimLeft(unit, depGlyphs))
		if unit == "" {
			continue
		}
		nodes = append(nodes, DepNode{Unit: unit, Depth: spaces / 2})
	}
	return nodes
}

// Dependencies is one unit's dependency tree in both directions: what it pulls
// in and what pulls it in.
type Dependencies struct {
	Forward []DepNode
	Reverse []DepNode
}

// depSections is the dependency batch: the forward tree and the reverse tree in
// one round trip. Every command is assembled with shq, and the unit name is
// validated before it is used.
func depSections(unit string) []probe.Section {
	return []probe.Section{
		{ID: secDeps, Cmd: shq.Join("systemctl", "list-dependencies", unit, "--plain", "--no-pager")},
		{ID: secDepsRev, Cmd: shq.Join("systemctl", "list-dependencies", unit, "--plain", "--no-pager", "--reverse")},
	}
}

// UnitDependencies runs the dependency batch for one unit on one host. It is one
// round trip; a unit that does not exist is errs.NotAvailable.
func (b *Backend) UnitDependencies(ctx context.Context, host resource.HostID, unit string) (Dependencies, error) {
	host = resource.NormalizeHostID(string(host))
	if host == "" {
		return Dependencies{}, errs.New(errs.Config, depsOp, "dependencies need a host")
	}
	unit = strings.TrimSpace(unit)
	if !shq.ValidUnitName(unit) {
		return Dependencies{}, errs.New(errs.Config, depsOp, "invalid unit name "+strconv.Quote(unit)).WithHost(string(host))
	}
	if b.exec == nil {
		return Dependencies{}, errs.New(errs.Config, depsOp, "no transport executor configured").WithHost(string(host))
	}
	batch, err := probe.NewBatch(b.newNonce(), depSections(unit)...)
	if err != nil {
		return Dependencies{}, err
	}

	cctx, cancel := context.WithTimeout(ctx, b.timeoutDuration())
	defer cancel()
	res, err := b.exec.Exec(cctx, host, batch.Command(b.timeoutDuration()))
	if err != nil {
		return Dependencies{}, err
	}
	sec := batch.Split(res.Stdout, res.Stderr)
	for _, id := range []string{secDeps, secDepsRev} {
		if e := sectionErr(host, id, sec[id], false); e != nil {
			if errs.KindOf(e) == errs.Remote && sec[id].RC == unitRCNoSuchUnit {
				return Dependencies{}, errs.New(errs.NotAvailable, id, "no such unit").WithHost(string(host))
			}
			return Dependencies{}, e
		}
	}
	return Dependencies{
		Forward: ParseDependencies(sec[secDeps].Stdout),
		Reverse: ParseDependencies(sec[secDepsRev].Stdout),
	}, nil
}

// Timer is one row of `systemctl list-timers --all --no-pager --no-legend`.
// Next and Last are the zero time when systemd printed "n/a" or "-"; Never marks
// a timer that has no next elapse at all.
type Timer struct {
	Name      string
	Next      time.Time
	Left      time.Duration
	Last      time.Time
	Passed    time.Duration
	Activates string
	Never     bool
}

// ParseTimers parses `systemctl list-timers --all --no-pager --no-legend`.
// The layout differs between systemd versions, so the two last fields (the timer
// and the unit it activates) are taken from the right and the rest is split on
// the "left" and "ago" markers; columns are never counted from the left.
func ParseTimers(out string) []Timer {
	var timers []Timer
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		t := Timer{Name: fields[len(fields)-2], Activates: fields[len(fields)-1]}
		// A timer unit always carries a dot; anything else is a truncated or
		// foreign line, which is skipped rather than guessed at.
		if t.Name == "" || !strings.Contains(t.Name, ".") {
			continue
		}
		fillTimer(&t, fields[:len(fields)-2])
		timers = append(timers, t)
	}
	return timers
}

// fillTimer fills the four time columns from the tokens left of the timer and
// activates names.
func fillTimer(t *Timer, tok []string) {
	leftAt, agoAt := -1, -1
	for i, s := range tok {
		switch s {
		case "left":
			if leftAt < 0 {
				leftAt = i
			}
		case "ago":
			if agoAt < 0 {
				agoAt = i
			}
		}
	}
	// Where the "next / left" pair ends: the "left" marker, else the "ago" one.
	headEnd := len(tok)
	if leftAt >= 0 {
		headEnd = leftAt
	} else if agoAt >= 0 {
		headEnd = agoAt
	}

	if leftAt >= 0 {
		// The columns are: NEXT LEFT left LAST PASSED ago. Without a "left"
		// marker the version printed no middle column at all, so the only
		// timestamp on the line is the last run.
		if ts, at, n, ok := firstTimerStamp(tok, 0, leftAt); ok {
			t.Next = ts
			t.Left = parseTimerSpan(tok[at+n : leftAt])
		} else {
			// "n/a" (or "-") says the timer has no next elapse; anything else
			// is a layout we could not read, which the enrichment pass retries.
			t.Never = timerNever(tok[:leftAt])
		}
		if ts, at, n, ok := firstTimerStamp(tok, leftAt+1, len(tok)); ok {
			t.Last = ts
			end := len(tok)
			if agoAt > at+n {
				end = agoAt
			}
			t.Passed = parseTimerSpan(tok[at+n : end])
		}
		return
	}

	// No "left" column at all: the next elapse is unknown, and only an explicit
	// n/a (or a dash) means the timer never fires again.
	t.Never = timerNever(tok)
	if ts, at, n, ok := firstTimerStamp(tok, 0, headEnd); ok {
		t.Last = ts
		end := len(tok)
		if agoAt > at+n {
			end = agoAt
		}
		t.Passed = parseTimerSpan(tok[at+n : end])
	}
}

// timerNever reports whether the tokens say the timer has no next elapse.
func timerNever(tok []string) bool {
	for _, s := range tok {
		switch strings.ToLower(s) {
		case "n/a", "-", "never", "[not", "set]":
			return true
		}
	}
	return false
}

// firstTimerStamp finds the first timestamp in tok[from:to] and reports the
// token index it starts at and how many tokens it spans.
func firstTimerStamp(tok []string, from, to int) (time.Time, int, int, bool) {
	for i := from; i < to; i++ {
		for _, layout := range timerStampLayouts {
			n := len(strings.Fields(layout))
			if i+n > to {
				continue
			}
			if ts, ok := parseTimerStampAt(tok[i : i+n]); ok {
				return ts, i, n, true
			}
		}
	}
	return time.Time{}, 0, 0, false
}

// parseTimerStampAt parses one timestamp made of the given tokens.
func parseTimerStampAt(tok []string) (time.Time, bool) {
	s := strings.Join(tok, " ")
	for _, layout := range timerStampLayouts {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// timerSpanUnits maps the unit words systemd uses in a timespan onto durations.
var timerSpanUnits = map[string]time.Duration{
	"us": time.Microsecond, "usec": time.Microsecond, "usecs": time.Microsecond,
	"ms": time.Millisecond, "msec": time.Millisecond, "msecs": time.Millisecond,
	"s": time.Second, "sec": time.Second, "secs": time.Second,
	"second": time.Second, "seconds": time.Second,
	"min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	"w": 7 * 24 * time.Hour, "week": 7 * 24 * time.Hour, "weeks": 7 * 24 * time.Hour,
	"month": 30 * 24 * time.Hour, "months": 30 * 24 * time.Hour,
	"y": 365 * 24 * time.Hour, "year": 365 * 24 * time.Hour, "years": 365 * 24 * time.Hour,
}

// parseTimerSpan turns the tokens of one timespan ("1 day 2h", "21min", "6
// days") into a duration. "n/a", "-" and unknowns add nothing.
func parseTimerSpan(tok []string) time.Duration {
	var total time.Duration
	for i := 0; i < len(tok); i++ {
		s := strings.ToLower(strings.Trim(strings.TrimSpace(tok[i]), ","))
		// systemd separates a bare number from its unit ("6 days"); a token
		// that is only a number takes the next token as its unit.
		if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 && i+1 < len(tok) {
			if unit, ok := timerSpanUnits[strings.ToLower(strings.Trim(tok[i+1], ","))]; ok {
				total += time.Duration(n * float64(unit))
				i++
				continue
			}
		}
		total += parseTimerToken(s)
	}
	return total
}

// parseTimerToken turns one timespan token ("21min") into a duration.
func parseTimerToken(s string) time.Duration {
	s = strings.ToLower(strings.Trim(strings.TrimSpace(s), ","))
	switch s {
	case "", "n/a", "-", "left", "ago":
		return 0
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || n <= 0 {
		return 0
	}
	unit, ok := timerSpanUnits[s[i:]]
	if !ok {
		return 0
	}
	return time.Duration(n * float64(unit))
}

// timerSections is the timer listing batch. It is one section, but going through
// probe gives the exit code and the stderr classification for free.
func timerSections() []probe.Section {
	return []probe.Section{{ID: secTimers, Cmd: cmdTimers}}
}

// timers runs the timer listing for one host and enriches the rows whose text
// layout did not parse with `systemctl show`. It is at most two round trips.
func (b *Backend) timers(ctx context.Context, host resource.HostID) ([]Timer, error) {
	batch, err := probe.NewBatch(b.newNonce(), timerSections()...)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, b.timeoutDuration())
	defer cancel()
	res, err := b.exec.Exec(cctx, host, batch.Command(b.timeoutDuration()))
	if err != nil {
		return nil, err
	}
	sec := batch.Split(res.Stdout, res.Stderr)
	if e := sectionErr(host, secTimers, sec[secTimers], false); e != nil {
		return nil, e
	}
	timers := ParseTimers(sec[secTimers].Stdout)
	b.enrichTimers(cctx, host, timers)
	return timers, nil
}

// enrichTimers fills the next elapse of the timers whose line did not carry a
// parseable timestamp, and the last run when that failed too. It runs one
// `systemctl show` batch only when something is missing.
func (b *Backend) enrichTimers(ctx context.Context, host resource.HostID, timers []Timer) {
	var idx []int
	var sections []probe.Section
	for i, t := range timers {
		if !t.Next.IsZero() || t.Never {
			continue
		}
		if !shq.ValidUnitName(t.Name) {
			continue
		}
		sections = append(sections, probe.Section{
			ID:  timerShowID + strconv.Itoa(len(sections)),
			Cmd: shq.Join("systemctl", "show", t.Name, "--no-pager", "-p", timerShowNext, "-p", timerShowLast),
		})
		idx = append(idx, i)
	}
	if len(sections) == 0 {
		return
	}
	batch, err := probe.NewBatch(b.newNonce(), sections...)
	if err != nil {
		return
	}
	res, err := b.exec.Exec(ctx, host, batch.Command(b.timeoutDuration()))
	if err != nil {
		return
	}
	sec := batch.Split(res.Stdout, res.Stderr)
	for n, i := range idx {
		sr := sec[timerShowID+strconv.Itoa(n)]
		if sr.Err != nil {
			continue
		}
		next, last := ParseTimerShowTimes(sr.Stdout)
		if !next.IsZero() {
			timers[i].Next = next
			timers[i].Never = false
		}
		if timers[i].Last.IsZero() && !last.IsZero() {
			timers[i].Last = last
		}
	}
}

// ParseTimerShowTimes reads NextElapseUSecRealtime and LastTriggerUSec from a
// `systemctl show` reply. systemctl prints them as timestamps by default and as
// microseconds since the epoch with --timestamp=usec; both are accepted, and
// "n/a", "[not set]" and 0 are the zero time.
func ParseTimerShowTimes(out string) (next, last time.Time) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case timerShowNext:
			next = timerShowTime(val)
		case timerShowLast:
			last = timerShowTime(val)
		}
	}
	return next, last
}

// timerShowTime parses one `systemctl show` time value.
func timerShowTime(s string) time.Time {
	s = strings.TrimSpace(s)
	switch s {
	case "", "n/a", "[not set]", "0":
		return time.Time{}
	}
	tok := strings.Fields(s)
	for _, layout := range timerStampLayouts {
		if len(strings.Fields(layout)) != len(tok) {
			continue
		}
		if ts, ok := parseTimerStampAt(tok); ok {
			return ts
		}
	}
	us, err := strconv.ParseInt(s, 10, 64)
	if err != nil || us <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(us).UTC()
}

// timerRows turns one host's timers into resources.
func (b *Backend) timerRows(host resource.HostID, timers []Timer) []resource.Resource {
	out := make([]resource.Resource, 0, len(timers))
	for _, t := range timers {
		labels := b.labels(host)
		out = append(out, resource.Resource{
			Ref: resource.Ref{Backend: backendName, Kind: KindTimer, Host: host, Name: t.Name},
			Fields: map[string]any{
				"host":      string(host),
				"timer":     t.Name,
				"next":      t.Next,
				"left":      t.Left,
				"last":      t.Last,
				"passed":    t.Passed,
				"activates": t.Activates,
			},
			Labels:    labels,
			Status:    resource.StatusUnknown,
			Raw:       t,
			FetchedAt: b.now(),
		})
	}
	return out
}

// timer is Get for one timer: one listing (plus the enrichment round trip) and
// the matching row.
func (b *Backend) timer(ctx context.Context, host resource.HostID, name string) (resource.Resource, error) {
	timers, err := b.timers(ctx, host)
	if err != nil {
		return resource.Resource{}, err
	}
	for _, row := range b.timerRows(host, timers) {
		if row.Ref.Name == name {
			return row, nil
		}
	}
	return resource.Resource{}, errs.New(errs.NotAvailable, "linux.Get",
		"no timer "+strconv.Quote(name)).WithHost(string(host))
}

// listTimers runs the timer listing once per host through the fan-out and emits
// the parsed rows. Enrichment happens per host, inside its own result.
func (b *Backend) listTimers(ctx context.Context, out chan<- backend.Result, hosts []resource.HostID, q resource.Query) {
	defer close(out)
	batch, err := probe.NewBatch(b.newNonce(), timerSections()...)
	if err != nil {
		for _, h := range hosts {
			select {
			case <-ctx.Done():
				return
			case out <- backend.Result{Host: h, Err: err}:
			}
		}
		return
	}
	want := strings.TrimSpace(q.Args["timer"])
	total, done := len(hosts), 0
	for hr := range fanout.Run(ctx, b.exec, hosts, batch.Command(b.timeoutDuration()), fanout.Options{Concurrency: b.concurrency}) {
		done++
		res := backend.Result{Host: hr.Host, Done: done, Total: total}
		switch {
		case hr.Err != nil:
			b.record(true)
			res.Err = hr.Err
		default:
			sec := batch.Split(hr.Result.Stdout, hr.Result.Stderr)
			if e := sectionErr(hr.Host, secTimers, sec[secTimers], false); e != nil {
				b.record(false)
				res.Err = e
			} else {
				b.record(false)
				timers := ParseTimers(sec[secTimers].Stdout)
				b.enrichTimers(ctx, hr.Host, timers)
				if want != "" {
					kept := timers[:0]
					for _, t := range timers {
						if t.Name == want {
							kept = append(kept, t)
						}
					}
					timers = kept
				}
				res.Items = b.timerRows(hr.Host, timers)
			}
		}
		select {
		case <-ctx.Done():
			return
		case out <- res:
		}
	}
}
