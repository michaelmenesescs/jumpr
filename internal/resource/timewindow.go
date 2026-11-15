package resource

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
)

// TimeWindow is the investigation time range every view respects.
type TimeWindow struct {
	From  time.Time
	To    time.Time // ignored when ToNow
	ToNow bool
	Label string // what the user typed, e.g. "-1h"
}

// relativePattern matches a relative offset such as "-30m" or "-2d".
var relativePattern = regexp.MustCompile(`^-([0-9]+)([smhdw])$`)

// ParseTimeWindow accepts "-30m", "-1h", "-2d", "2026-11-12 13:00..14:00",
// "2026-11-12T13:00:00Z..now". now is injected for tests.
func ParseTimeWindow(s string, now time.Time) (TimeWindow, error) {
	label := strings.TrimSpace(s)
	if label == "" {
		return TimeWindow{}, windowError(label, "empty time window")
	}

	if m := relativePattern.FindStringSubmatch(label); m != nil {
		d, err := relativeDuration(m[1], m[2])
		if err != nil {
			return TimeWindow{}, windowError(label, err.Error())
		}
		return TimeWindow{From: now.Add(-d), To: now, ToNow: true, Label: label}, nil
	}

	parts := strings.Split(label, "..")
	if len(parts) != 2 {
		return TimeWindow{}, windowError(label, `expected a relative offset like "-30m" or a range "A..B"`)
	}
	from, err := parseWindowBound(strings.TrimSpace(parts[0]), now)
	if err != nil {
		return TimeWindow{}, windowError(label, "start: "+err.Error())
	}
	to, err := parseWindowBound(strings.TrimSpace(parts[1]), now)
	if err != nil {
		return TimeWindow{}, windowError(label, "end: "+err.Error())
	}
	if !from.Before(to) {
		return TimeWindow{}, windowError(label, "start must be before end")
	}
	return TimeWindow{From: from, To: to, Label: label}, nil
}

// Bounds returns the resolved interval; the end is now when ToNow is set.
func (w TimeWindow) Bounds(now time.Time) (from, to time.Time) {
	if w.ToNow {
		return w.From, now
	}
	return w.From, w.To
}

// IsZero reports whether the window was never set.
func (w TimeWindow) IsZero() bool {
	return !w.ToNow && w.From.IsZero() && w.To.IsZero()
}

// String returns the label the user typed, or "-" for the zero window.
func (w TimeWindow) String() string {
	if w.IsZero() {
		return "-"
	}
	return w.Label
}

// parseWindowBound resolves one side of a range: "now", a relative offset,
// RFC 3339, "2006-01-02 15:04", "2006-01-02T15:04" or "15:04" (today, in
// now's location).
func parseWindowBound(v string, now time.Time) (time.Time, error) {
	switch v {
	case "":
		return time.Time{}, fmt.Errorf("missing value")
	case "now":
		return now, nil
	}
	if m := relativePattern.FindStringSubmatch(v); m != nil {
		d, err := relativeDuration(m[1], m[2])
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", v, now.Location()); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, now.Location()); err == nil {
		return t, nil
	}
	if hhmm, err := time.Parse("15:04", v); err == nil {
		y, mo, d := now.Date()
		return time.Date(y, mo, d, hhmm.Hour(), hhmm.Minute(), 0, 0, now.Location()), nil
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", v)
}

// relativeDuration converts the number and unit of a relative offset.
func relativeDuration(number, unit string) (time.Duration, error) {
	n, err := strconv.Atoi(number)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", number)
	}
	var unitDur time.Duration
	switch unit {
	case "s":
		unitDur = time.Second
	case "m":
		unitDur = time.Minute
	case "h":
		unitDur = time.Hour
	case "d":
		unitDur = 24 * time.Hour
	case "w":
		unitDur = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("invalid unit %q", unit)
	}
	return time.Duration(n) * unitDur, nil
}

func windowError(label, why string) error {
	return errs.New(errs.Config, "resource.ParseTimeWindow", fmt.Sprintf("bad time window %q: %s", label, why))
}
