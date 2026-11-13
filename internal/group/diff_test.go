package group

import (
	"strings"
	"testing"
)

func TestStripTimestamps(t *testing.T) {
	in := "a 2026-11-12T10:11:12Z b 2026-11-12 10:11:12.123+01:00 c Nov  3 04:05:06 d"
	want := "a <ts> b <ts> c <ts> d"
	if got := StripTimestamps(in); got != want {
		t.Fatalf("%q", got)
	}
}

func TestStripPIDs(t *testing.T) {
	if got := StripPIDs("x pid=123 sshd[4567]: y"); got != "x pid=<n> sshd[<n>]: y" {
		t.Fatalf("%q", got)
	}
	c := Chain(StripTimestamps, StripPIDs)
	if got := c("Nov 3 04:05:06 sshd[1]"); got != "<ts> sshd[<n>]" {
		t.Fatalf("%q", got)
	}
}

func TestUnifiedDiffEqualIsEmpty(t *testing.T) {
	if UnifiedDiff("a", "x\n", "b", "x\n") != "" {
		t.Fatal("not empty")
	}
}

func TestUnifiedDiffShape(t *testing.T) {
	d := UnifiedDiff("a", "1\n2\n3\n", "b", "1\nX\n3\n")
	for _, w := range []string{"--- a", "+++ b", "@@ ", "-2", "+X", " 1"} {
		if !strings.Contains(d, w) {
			t.Errorf("missing %q in %q", w, d)
		}
	}
}
