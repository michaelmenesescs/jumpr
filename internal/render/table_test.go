package render

import (
	"strings"
	"testing"
)

func TestColumnWidths(t *testing.T) {
	tests := []struct {
		name  string
		cols  []TableColumn
		total int
		want  []int
	}{
		{
			name:  "all fixed fit",
			cols:  []TableColumn{{Width: 6}, {Width: 4, AlignRight: true}},
			total: 11,
			want:  []int{6, 4},
		},
		{
			name:  "even split",
			cols:  []TableColumn{{Width: 10}, {Width: 0}, {Width: 0}},
			total: 30,
			want:  []int{10, 9, 9},
		},
		{
			name:  "leftover to last flexible",
			cols:  []TableColumn{{Width: 10}, {Width: 0}, {Width: 0}},
			total: 29,
			want:  []int{10, 8, 9},
		},
		{
			name:  "flexible minimum",
			cols:  []TableColumn{{Width: 0}, {Width: 0}, {Width: 0}},
			total: 13,
			want:  []int{4, 4, 4},
		},
		{
			name:  "shrink fixed from the right",
			cols:  []TableColumn{{Width: 10}, {Width: 10}, {Width: 10}},
			total: 20,
			want:  []int{10, 5, 3},
		},
		{
			name:  "shrink stops at minimum",
			cols:  []TableColumn{{Width: 10}, {Width: 10}},
			total: 5,
			want:  []int{3, 3},
		},
		{
			name:  "fixed before flexible",
			cols:  []TableColumn{{Width: 20}, {Width: 0}},
			total: 30,
			want:  []int{20, 9},
		},
		{
			name:  "no columns",
			cols:  nil,
			total: 40,
			want:  []int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ColumnWidths(tt.cols, tt.total)
			if len(got) != len(tt.want) {
				t.Fatalf("ColumnWidths(%v, %d) = %v, want %v", tt.cols, tt.total, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("ColumnWidths(%v, %d) = %v, want %v", tt.cols, tt.total, got, tt.want)
				}
			}
		})
	}
}

func TestTableRow(t *testing.T) {
	cols := []TableColumn{{Title: "HOST", Width: 6}, {Title: "LOAD", Width: 4, AlignRight: true}}
	widths := ColumnWidths(cols, 11)
	if len(widths) != 2 || widths[0] != 6 || widths[1] != 4 {
		t.Fatalf("ColumnWidths = %v, want [6 4]", widths)
	}

	t.Run("left and right alignment", func(t *testing.T) {
		got := Row([]Cell{{Text: "web-1"}, {Text: "7"}}, widths, cols)
		if want := "web-1     7"; got != want {
			t.Fatalf("Row = %q, want %q", got, want)
		}
		if w := Width(got); w != 11 {
			t.Fatalf("Row width = %d, want 11", w)
		}
	})

	t.Run("truncates to the column", func(t *testing.T) {
		got := Row([]Cell{{Text: "abcdefgh"}, {Text: "4.12"}}, widths, cols)
		if want := "abcde… 4.12"; got != want {
			t.Fatalf("Row = %q, want %q", got, want)
		}
	})

	t.Run("wide runes", func(t *testing.T) {
		got := Row([]Cell{{Text: "日本語"}, {Text: "1"}}, widths, cols)
		if w := Width(got); w != 11 {
			t.Fatalf("Row(%q) width = %d, want 11", got, w)
		}
	})

	t.Run("style after padding", func(t *testing.T) {
		style := func(s string) string { return "\x1b[7m" + s + "\x1b[0m" }
		got := Row([]Cell{{Text: "web-1", Style: style}, {Text: "7"}}, widths, cols)
		if want := "\x1b[7mweb-1 \x1b[0m    7"; got != want {
			t.Fatalf("Row = %q, want %q", got, want)
		}
		if w := Width(got); w != 11 {
			t.Fatalf("styled row width = %d, want 11", w)
		}
	})

	t.Run("ansi text in a cell", func(t *testing.T) {
		got := Row([]Cell{{Text: "\x1b[31mabcdef\x1b[0m"}, {Text: "7"}}, widths, cols)
		if w := Width(got); w != 11 {
			t.Fatalf("Row(%q) width = %d, want 11", got, w)
		}
	})

	t.Run("missing and extra cells", func(t *testing.T) {
		got := Row([]Cell{{Text: "web-1"}, {Text: "7"}, {Text: "extra"}}, widths, cols)
		if want := "web-1     7"; got != want {
			t.Fatalf("Row = %q, want %q", got, want)
		}
		got = Row([]Cell{{Text: "web-1"}}, widths, cols)
		if want := "web-1      "; got != want {
			t.Fatalf("Row = %q, want %q", got, want)
		}
	})

	t.Run("no widths", func(t *testing.T) {
		if got := Row(nil, nil, nil); got != "" {
			t.Fatalf("Row with no widths = %q, want empty", got)
		}
	})
}

func TestTableHeader(t *testing.T) {
	cols := []TableColumn{{Title: "HOST", Width: 6}, {Title: "LOAD", Width: 4, AlignRight: true}}
	widths := []int{6, 4}
	got := Header(cols, widths)
	if want := "HOST   LOAD"; got != want {
		t.Fatalf("Header = %q, want %q", got, want)
	}
	if w := Width(got); w != 11 {
		t.Fatalf("Header width = %d, want 11", w)
	}

	long := []TableColumn{{Title: "HOSTNAME", Width: 4}}
	got = Header(long, []int{4})
	if w := Width(got); w != 4 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated Header = %q (width %d), want a 4-cell ellipsis", got, w)
	}
}
