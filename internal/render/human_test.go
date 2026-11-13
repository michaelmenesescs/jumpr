package render

import (
	"math"
	"testing"
	"time"
)

func TestInt(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{1512, "1,512"},
		{-1000, "-1,000"},
		{-1, "-1"},
		{1000000, "1,000,000"},
		{math.MaxInt64, "9,223,372,036,854,775,807"},
		{math.MinInt64, "-9,223,372,036,854,775,808"},
	}
	for _, tt := range tests {
		if got := Int(tt.in); got != tt.want {
			t.Errorf("Int(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{-512, "-512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
		{2 * 1024 * 1024 * 1024, "2.0 GiB"},
		{-1536, "-1.5 KiB"},
	}
	for _, tt := range tests {
		if got := Bytes(tt.in); got != tt.want {
			t.Errorf("Bytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{500 * time.Millisecond, "0s"},
		{12 * time.Second, "12s"},
		{59 * time.Second, "59s"},
		{38 * time.Minute, "38m"},
		{38*time.Minute + 12*time.Second, "38m 12s"},
		{2*time.Hour + 5*time.Minute, "2h 5m"},
		{2*time.Hour + 5*time.Minute + 3*time.Second, "2h 5m"},
		{41*24*time.Hour + 3*time.Hour, "41d 3h"},
		{41*24*time.Hour + 3*time.Hour + 20*time.Minute, "41d 3h"},
		{41 * 24 * time.Hour, "41d"},
		{-38 * time.Minute, "-38m"},
	}
	for _, tt := range tests {
		if got := Duration(tt.in); got != tt.want {
			t.Errorf("Duration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero time", time.Time{}, "—"},
		{"minutes", now.Add(-38 * time.Minute), "38m"},
		{"hours and minutes", now.Add(-2*time.Hour - 5*time.Minute), "2h 5m"},
		{"days and hours", now.Add(-(41*24*time.Hour + 3*time.Hour)), "41d 3h"},
		{"just now", now, "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Age(tt.t, now); got != tt.want {
				t.Fatalf("Age(%v, now) = %q, want %q", tt.t, got, tt.want)
			}
		})
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0.0%"},
		{3.24, "3.2%"},
		{9.5, "9.5%"},
		{10, "10%"},
		{71, "71%"},
		{71.4, "71%"},
		{100, "100%"},
		{-3.2, "-3.2%"},
		{-71, "-71%"},
	}
	for _, tt := range tests {
		if got := Percent(tt.in); got != tt.want {
			t.Errorf("Percent(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
