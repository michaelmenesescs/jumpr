package render

import (
	"math"
	"testing"
)

func TestSpark(t *testing.T) {
	nan := math.NaN()
	tests := []struct {
		name  string
		vals  []float64
		width int
		want  string
	}{
		{"empty pads to width", nil, 5, "     "},
		{"zero width", []float64{1, 2}, 0, ""},
		{"negative width", []float64{1, 2}, -3, ""},
		{"single peak", []float64{1}, 1, "█"},
		{"linear ramp", []float64{0, 1}, 2, "▁█"},
		{"zero peak renders lowest", []float64{0, 0, 0}, 3, "▁▁▁"},
		{"all negative is flat", []float64{-5, -1}, 2, "▁▁"},
		{"short series left-pads", []float64{1}, 4, "   █"},
		{"NaN renders as space", []float64{nan, 1}, 2, " █"},
		{"downsample averages buckets", []float64{0, 0, 8, 8}, 2, "▁█"},
		{"downsample drops NaN when averaging", []float64{nan, 8, 0, 8}, 2, "█▄"},
		{"exact width untouched", []float64{0, 4, 8}, 3, "▁▄█"},
		{"values above peak stay at top", []float64{1, 1}, 2, "██"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Spark(tt.vals, tt.width)
			if got != tt.want {
				t.Fatalf("Spark(%v, %d) = %q, want %q", tt.vals, tt.width, got, tt.want)
			}
			if tt.width > 0 {
				if w := Width(got); w != tt.width {
					t.Fatalf("Width(Spark(%v, %d)) = %d, want %d", tt.vals, tt.width, w, tt.width)
				}
			}
		})
	}
}

func TestSparkWidthProperty(t *testing.T) {
	cases := [][]float64{
		nil,
		{},
		{0},
		{1, 2, 3},
		{math.NaN(), math.NaN()},
		{math.NaN(), 1, math.NaN(), 4},
		{-1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		make([]float64, 100),
	}
	for _, vals := range cases {
		for width := 1; width <= 20; width++ {
			got := Spark(vals, width)
			if w := Width(got); w != width {
				t.Fatalf("Width(Spark(%v, %d)) = %d, want %d (%q)", vals, width, w, width, got)
			}
		}
	}
}

func TestBar(t *testing.T) {
	tests := []struct {
		name     string
		fraction float64
		width    int
		want     string
	}{
		{"empty is all dots", 0, 4, "····"},
		{"half", 0.5, 4, "██··"},
		{"full", 1, 4, "████"},
		{"over one clamps", 2, 4, "████"},
		{"negative clamps to empty", -0.5, 3, "···"},
		{"NaN is empty", math.NaN(), 2, "··"},
		{"rounds to nearest cell", 0.3, 3, "█··"},
		{"zero width", 1, 0, ""},
		{"single full cell", 1, 1, "█"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Bar(tt.fraction, tt.width)
			if got != tt.want {
				t.Fatalf("Bar(%v, %d) = %q, want %q", tt.fraction, tt.width, got, tt.want)
			}
			if tt.width > 0 {
				if w := Width(got); w != tt.width {
					t.Fatalf("Width(Bar(%v, %d)) = %d, want %d", tt.fraction, tt.width, w, tt.width)
				}
			}
		})
	}
}
