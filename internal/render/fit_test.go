package render

import (
	"math/rand"
	"strings"
	"testing"
)

func TestWidth(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"ascii", "hello", 5},
		{"spaces", "a b", 3},
		{"wide runes", "日本語", 6},
		{"mixed", "a日b", 4},
		{"ansi colour", "\x1b[31mred\x1b[0m", 3},
		{"ansi bold wide", "\x1b[1m日本\x1b[0m", 4},
		{"reset only", "\x1b[0m", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Width(tt.in); got != tt.want {
				t.Fatalf("Width(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	ansiRed := "\x1b[31mhello\x1b[0m"
	tests := []struct {
		name     string
		in       string
		w        int
		want     string
		exact    bool // want an exact string (else only width/suffix checks)
		wantW    int
		hasReset bool
	}{
		{name: "fits", in: "hello", w: 10, want: "hello", exact: true, wantW: 5},
		{name: "exact fit", in: "hello", w: 5, want: "hello", exact: true, wantW: 5},
		{name: "cut ascii", in: "hello", w: 4, want: "hel…", exact: true, wantW: 4},
		{name: "cut to one", in: "hello", w: 1, want: "…", exact: true, wantW: 1},
		{name: "zero", in: "hello", w: 0, want: "", exact: true, wantW: 0},
		{name: "negative", in: "hello", w: -3, want: "", exact: true, wantW: 0},
		{name: "wide fits", in: "日本語", w: 6, want: "日本語", exact: true, wantW: 6},
		{name: "wide cut", in: "日本語", w: 5, want: "日本…", exact: true, wantW: 5},
		{name: "wide boundary", in: "日本語", w: 4, want: "日…", exact: true, wantW: 3},
		{name: "ansi fits", in: ansiRed, w: 5, hasReset: false, wantW: 5},
		{name: "ansi cut", in: ansiRed, w: 4, wantW: 4, hasReset: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Truncate(tt.in, tt.w)
			if tt.exact && got != tt.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tt.in, tt.w, got, tt.want)
			}
			if w := Width(got); w != tt.wantW {
				t.Fatalf("Truncate(%q, %d) has width %d, want %d (%q)", tt.in, tt.w, w, tt.wantW, got)
			}
			if Width(got) > tt.w && tt.w > 0 {
				t.Fatalf("Truncate(%q, %d) wider than %d cells", tt.in, tt.w, tt.w)
			}
			if tt.hasReset && !strings.HasSuffix(got, "\x1b[0m") {
				t.Fatalf("truncated coloured string %q is not reset", got)
			}
		})
	}
}

func TestPad(t *testing.T) {
	tests := []struct {
		name string
		in   string
		w    int
		pad  func(string, int) string
		want string
	}{
		{name: "right pads", in: "abc", w: 5, pad: PadRight, want: "abc  "},
		{name: "right exact", in: "abcde", w: 5, pad: PadRight, want: "abcde"},
		{name: "right truncates", in: "abcdef", w: 4, pad: PadRight, want: "abc…"},
		{name: "right zero", in: "abc", w: 0, pad: PadRight, want: ""},
		{name: "right wide", in: "日本語", w: 8, pad: PadRight, want: "日本語  "},
		{name: "right wide boundary", in: "日本語", w: 5, pad: PadRight, want: "日本…"},
		{name: "left pads", in: "abc", w: 5, pad: PadLeft, want: "  abc"},
		{name: "left truncates", in: "abcdef", w: 4, pad: PadLeft, want: "abc…"},
		{name: "left negative", in: "abc", w: -1, pad: PadLeft, want: ""},
		{name: "left wide", in: "日本語", w: 8, pad: PadLeft, want: "  日本語"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.pad(tt.in, tt.w)
			if got != tt.want {
				t.Fatalf("%q (w=%d) = %q, want %q", tt.in, tt.w, got, tt.want)
			}
			if w := Width(got); w != tt.w && tt.w > 0 {
				t.Fatalf("%q (w=%d) has width %d", tt.in, tt.w, w)
			}
		})
	}
}

func TestFit(t *testing.T) {
	tests := []struct {
		name string
		in   string
		w, h int
		want string
	}{
		{name: "pads short", in: "a\nbb", w: 4, h: 3, want: "a   \nbb  \n    "},
		{name: "cuts extra lines", in: "a\nb\nc\nd", w: 2, h: 2, want: "a \nb "},
		{name: "empty input", in: "", w: 3, h: 2, want: "   \n   "},
		{name: "truncates long line", in: "abcdef", w: 4, h: 1, want: "abc…"},
		{name: "wide rune", in: "日", w: 1, h: 1, want: "…"},
		{name: "ansi kept", in: "\x1b[31mred\x1b[0m", w: 3, h: 1, want: "\x1b[31mred\x1b[0m"},
		{name: "zero width", in: "abc", w: 0, h: 2, want: ""},
		{name: "zero height", in: "abc", w: 4, h: 0, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Fit(tt.in, tt.w, tt.h)
			if got != tt.want {
				t.Fatalf("Fit(%q, %d, %d) = %q, want %q", tt.in, tt.w, tt.h, got, tt.want)
			}
		})
	}
}

// TestFitProperty checks the invariant every view relies on: for arbitrary
// input, Fit returns exactly h lines of exactly w cells.
func TestFitProperty(t *testing.T) {
	tokens := []string{"a", "B", "z", "0", " ", "日", "本", "語", "→", "\x1b[31m", "\x1b[0m", "\x1b[7m"}
	rng := rand.New(rand.NewSource(20261112))

	for i := 0; i < 500; i++ {
		n := rng.Intn(40)
		var b strings.Builder
		for j := 0; j < n; j++ {
			b.WriteString(tokens[rng.Intn(len(tokens))])
		}
		s := b.String()
		w := rng.Intn(20) + 1
		h := rng.Intn(8) + 1

		got := Fit(s, w, h)
		if c := strings.Count(got, "\n"); c != h-1 {
			t.Fatalf("Fit(%q, %d, %d) has %d newlines, want %d", s, w, h, c, h-1)
		}
		lines := strings.Split(got, "\n")
		if len(lines) != h {
			t.Fatalf("Fit(%q, %d, %d) has %d lines, want %d", s, w, h, len(lines), h)
		}
		for k, line := range lines {
			if lw := Width(line); lw != w {
				t.Fatalf("Fit(%q, %d, %d) line %d = %q has width %d, want %d", s, w, h, k, line, lw, w)
			}
		}
		if strings.HasSuffix(got, "\n") {
			t.Fatalf("Fit(%q, %d, %d) ends with a newline", s, w, h)
		}
	}
}
