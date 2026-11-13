package resource

import (
	"strings"
	"testing"
	"time"

	"sitrep/internal/errs"
)

func testNow() time.Time {
	return time.Date(2026, 11, 12, 15, 30, 0, 0, time.UTC)
}

func TestParseTimeWindowRelative(t *testing.T) {
	now := testNow()
	tests := []struct {
		input string
		want  time.Duration
	}{
		{input: "-30m", want: 30 * time.Minute},
		{input: "-1h", want: time.Hour},
		{input: "-2d", want: 48 * time.Hour},
		{input: "-45s", want: 45 * time.Second},
		{input: "-3w", want: 21 * 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			w, err := ParseTimeWindow(tt.input, now)
			if err != nil {
				t.Fatalf("ParseTimeWindow(%q): %v", tt.input, err)
			}
			if !w.ToNow {
				t.Errorf("ToNow = false, want true")
			}
			if got, want := w.From, now.Add(-tt.want); !got.Equal(want) {
				t.Errorf("From = %v, want %v", got, want)
			}
			if w.Label != tt.input {
				t.Errorf("Label = %q, want %q", w.Label, tt.input)
			}
			if w.IsZero() {
				t.Errorf("IsZero = true, want false")
			}
			later := now.Add(5 * time.Minute)
			from, to := w.Bounds(later)
			if !from.Equal(w.From) || !to.Equal(later) {
				t.Errorf("Bounds = (%v, %v), want (%v, %v)", from, to, w.From, later)
			}
			if got := w.String(); got != tt.input {
				t.Errorf("String() = %q, want %q", got, tt.input)
			}
		})
	}
}

func TestParseTimeWindowRanges(t *testing.T) {
	now := testNow()
	loc := now.Location()
	tests := []struct {
		name     string
		input    string
		wantFrom time.Time
		wantTo   time.Time
	}{
		{
			name:     "date and clock",
			input:    "2026-11-12 13:00..14:00",
			wantFrom: time.Date(2026, 11, 12, 13, 0, 0, 0, loc),
			wantTo:   time.Date(2026, 11, 12, 14, 0, 0, 0, loc),
		},
		{
			name:     "clock to now",
			input:    "13:00..now",
			wantFrom: time.Date(2026, 11, 12, 13, 0, 0, 0, loc),
			wantTo:   now,
		},
		{
			name:     "relative to relative",
			input:    "-2h..-1h",
			wantFrom: now.Add(-2 * time.Hour),
			wantTo:   now.Add(-time.Hour),
		},
		{
			name:     "relative to now",
			input:    "-2h..now",
			wantFrom: now.Add(-2 * time.Hour),
			wantTo:   now,
		},
		{
			name:     "RFC 3339",
			input:    "2026-11-12T13:00:00Z..2026-11-12T14:00:00Z",
			wantFrom: time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC),
			wantTo:   time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC),
		},
		{
			name:     "ISO without seconds or zone",
			input:    "2026-11-12T13:00..2026-11-12T14:00",
			wantFrom: time.Date(2026, 11, 12, 13, 0, 0, 0, loc),
			wantTo:   time.Date(2026, 11, 12, 14, 0, 0, 0, loc),
		},
		{
			name:     "date and clock on both sides",
			input:    "2026-11-11 22:00..2026-11-12 02:00",
			wantFrom: time.Date(2026, 11, 11, 22, 0, 0, 0, loc),
			wantTo:   time.Date(2026, 11, 12, 2, 0, 0, 0, loc),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := ParseTimeWindow(tt.input, now)
			if err != nil {
				t.Fatalf("ParseTimeWindow(%q): %v", tt.input, err)
			}
			if w.ToNow {
				t.Errorf("ToNow = true, want false")
			}
			if !w.From.Equal(tt.wantFrom) {
				t.Errorf("From = %v, want %v", w.From, tt.wantFrom)
			}
			if !w.To.Equal(tt.wantTo) {
				t.Errorf("To = %v, want %v", w.To, tt.wantTo)
			}
			if w.Label != tt.input {
				t.Errorf("Label = %q, want %q", w.Label, tt.input)
			}
			from, to := w.Bounds(now)
			if !from.Equal(tt.wantFrom) || !to.Equal(tt.wantTo) {
				t.Errorf("Bounds = (%v, %v), want (%v, %v)", from, to, tt.wantFrom, tt.wantTo)
			}
		})
	}
}

func TestParseTimeWindowTrimmedLabel(t *testing.T) {
	now := testNow()
	w, err := ParseTimeWindow("  -30m  ", now)
	if err != nil {
		t.Fatalf("ParseTimeWindow: %v", err)
	}
	if w.Label != "-30m" {
		t.Errorf("Label = %q, want %q", w.Label, "-30m")
	}
}

func TestParseTimeWindowErrors(t *testing.T) {
	now := testNow()
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "garbage", input: "yesterday"},
		{name: "bad unit", input: "-2y"},
		{name: "missing number", input: "-h"},
		{name: "reversed range", input: "14:00..13:00"},
		{name: "equal range", input: "13:00..13:00"},
		{name: "bad start", input: "nope..now"},
		{name: "bad end", input: "-1h..nope"},
		{name: "missing end", input: "-1h.."},
		{name: "missing start", input: "..now"},
		{name: "too many parts", input: "-1h..now..later"},
		{name: "invalid date", input: "2026-13-40 13:00..14:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseTimeWindow(tt.input, now)
			if err == nil {
				t.Fatalf("ParseTimeWindow(%q) = nil error, want error", tt.input)
			}
			if !errs.Is(err, errs.Config) {
				t.Errorf("kind = %q, want config", errs.KindOf(err))
			}
			if tt.input != "" && !strings.Contains(err.Error(), strings.TrimSpace(tt.input)) {
				t.Errorf("error %q does not name input %q", err, tt.input)
			}
		})
	}
}

func TestTimeWindowZero(t *testing.T) {
	var w TimeWindow
	if !w.IsZero() {
		t.Errorf("zero window IsZero = false")
	}
	if got, want := w.String(), "-"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	from, to := w.Bounds(testNow())
	if !from.IsZero() || !to.IsZero() {
		t.Errorf("Bounds on zero = (%v, %v), want zero times", from, to)
	}
}

func TestTimeWindowRFC3339KeepsOffset(t *testing.T) {
	now := testNow()
	w, err := ParseTimeWindow("2026-11-12T13:00:00-05:00..2026-11-12T14:00:00-05:00", now)
	if err != nil {
		t.Fatalf("ParseTimeWindow: %v", err)
	}
	_, offset := w.From.Zone()
	if offset != -5*60*60 {
		t.Errorf("From offset = %d, want -18000", offset)
	}
}
