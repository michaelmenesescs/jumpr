package fuzzy

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		name      string
		pattern   string
		s         string
		ok        bool
		positions []int
	}{
		{name: "empty pattern", pattern: "", s: "anything", ok: true},
		{name: "empty pattern empty string", pattern: "", s: "", ok: true},
		{name: "exact", pattern: "abc", s: "abc", ok: true, positions: []int{0, 1, 2}},
		{name: "scattered", pattern: "abc", s: "aXbXc", ok: true, positions: []int{0, 2, 4}},
		{name: "no match", pattern: "abc", s: "ab", ok: false},
		{name: "no match order", pattern: "cba", s: "abc", ok: false},
		{name: "case folding", pattern: "ABC", s: "a-b-c", ok: true, positions: []int{0, 2, 4}},
		{name: "case folding lower pattern", pattern: "webdb", s: "WEB-DB-01", ok: true, positions: []int{0, 1, 2, 4, 5}},
		{name: "unicode rune indices", pattern: "世", s: "a世b", ok: true, positions: []int{1}},
		{name: "unicode dotless i matches itself", pattern: "ı", s: "ı", ok: true, positions: []int{0}},
		{name: "unicode dotless i does not match I", pattern: "ı", s: "I", ok: false},
		{name: "unicode I does not match dotless i", pattern: "I", s: "ı", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			score, positions, ok := Match(tc.pattern, tc.s)
			if ok != tc.ok {
				t.Fatalf("Match(%q, %q) ok = %v, want %v", tc.pattern, tc.s, ok, tc.ok)
			}
			if !ok {
				if score != 0 {
					t.Errorf("score = %d on no match, want 0", score)
				}
				if positions != nil {
					t.Errorf("positions = %v on no match, want nil", positions)
				}
				return
			}
			if tc.pattern == "" {
				if score != 0 || positions != nil {
					t.Errorf("empty pattern = (%d, %v), want (0, nil)", score, positions)
				}
				return
			}
			if len(positions) != len([]rune(tc.pattern)) {
				t.Fatalf("positions = %v, want one per pattern rune", positions)
			}
			if tc.positions != nil {
				if len(positions) != len(tc.positions) {
					t.Fatalf("positions = %v, want %v", positions, tc.positions)
				}
				for i := range positions {
					if positions[i] != tc.positions[i] {
						t.Fatalf("positions = %v, want %v", positions, tc.positions)
					}
				}
			}
		})
	}
}

func TestMatchRunePositionsFold(t *testing.T) {
	const pattern = "wbd"
	const s = "Web-DB-01"
	_, positions, ok := Match(pattern, s)
	if !ok {
		t.Fatalf("Match(%q, %q) did not match", pattern, s)
	}
	runes := []rune(s)
	p := []rune(pattern)
	for i, pos := range positions {
		if pos < 0 || pos >= len(runes) {
			t.Fatalf("position %d out of range", pos)
		}
		if unicode.ToLower(runes[pos]) != unicode.ToLower(p[i]) {
			t.Fatalf("position %d is %q, want %q", pos, runes[pos], p[i])
		}
	}
	for i := 1; i < len(positions); i++ {
		if positions[i] <= positions[i-1] {
			t.Fatalf("positions not strictly increasing: %v", positions)
		}
	}
}

func TestMatchContiguousBeatsScattered(t *testing.T) {
	contiguous, _, ok := Match("abc", "abc")
	if !ok {
		t.Fatal("contiguous did not match")
	}
	scattered, _, ok := Match("abc", "aXbXc")
	if !ok {
		t.Fatal("scattered did not match")
	}
	if contiguous <= scattered {
		t.Errorf("contiguous score %d <= scattered score %d", contiguous, scattered)
	}
}

func TestMatchContiguousBeatsGaps(t *testing.T) {
	tight, _, ok := Match("bcd", "abcd")
	if !ok {
		t.Fatal("tight did not match")
	}
	loose, _, ok := Match("bcd", "abXcd")
	if !ok {
		t.Fatal("loose did not match")
	}
	if tight <= loose {
		t.Errorf("tight score %d <= loose score %d", tight, loose)
	}
}

func TestMatchWordStartBonus(t *testing.T) {
	start, _, ok := Match("db", "web-db-01")
	if !ok {
		t.Fatal("word start did not match")
	}
	inner, _, ok := Match("db", "webd-b-01")
	if !ok {
		t.Fatal("inner did not match")
	}
	if start <= inner {
		t.Errorf("word-start score %d <= inner score %d", start, inner)
	}
}

func TestMatchOrderingProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(140))
	printable := func(n int) string {
		runes := make([]rune, n)
		for i := range runes {
			runes[i] = rune(33 + rng.Intn(94))
		}
		return string(runes)
	}
	for i := 0; i < 500; i++ {
		s := printable(1 + rng.Intn(40))
		var pattern string
		if rng.Intn(4) == 0 {
			pattern = printable(rng.Intn(6))
		} else {
			// Build a genuine subsequence so the match branch is exercised.
			runes := []rune(s)
			var b strings.Builder
			for j := 0; j < len(runes); j++ {
				if rng.Intn(3) == 0 {
					b.WriteRune(runes[j])
				}
			}
			pattern = b.String()
		}
		score, positions, ok := Match(pattern, s)
		if !ok {
			continue
		}
		if pattern == "" {
			if score != 0 || positions != nil {
				t.Fatalf("empty pattern returned (%d, %v)", score, positions)
			}
			continue
		}
		if len(positions) != len([]rune(pattern)) {
			t.Fatalf("Match(%q, %q): %d positions, want %d", pattern, s, len(positions), len([]rune(pattern)))
		}
		runes := []rune(s)
		p := []rune(pattern)
		prev := -1
		for k, pos := range positions {
			if pos <= prev {
				t.Fatalf("Match(%q, %q): positions %v not increasing", pattern, s, positions)
			}
			if pos >= len(runes) {
				t.Fatalf("Match(%q, %q): position %d out of range", pattern, s, pos)
			}
			if unicode.ToLower(runes[pos]) != unicode.ToLower(p[k]) {
				t.Fatalf("Match(%q, %q): position %d is %q, want %q", pattern, s, pos, runes[pos], p[k])
			}
			prev = pos
		}
	}
}

func TestRank(t *testing.T) {
	cands := []string{"web-ewr-001", "web-db-01", "db-01"}
	got := Rank("wbd", cands)
	// Neither web-ewr-001 nor db-01 has all three runes in order (no "d" in
	// the first, no "w" in the second), so web-db-01 is the only match. Its
	// word-start alignment on "db" is what makes it the winner.
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("Rank = %v, want [1] (web-db-01)", got)
	}
}

func TestRankWordStartWins(t *testing.T) {
	got := Rank("ab", []string{"aXbY", "a-b"})
	if len(got) != 2 || got[0] != 1 {
		t.Fatalf("Rank = %v, want the word-start match first", got)
	}
}

func TestRankStable(t *testing.T) {
	cands := []string{"host", "hosts", "hostx"}
	got := Rank("ho", cands)
	if len(got) != 3 {
		t.Fatalf("Rank = %v, want every candidate", got)
	}
	for i := range got {
		if got[i] != i {
			t.Fatalf("Rank = %v, want equal scores to keep input order", got)
		}
	}
}

func TestRankEmptyPattern(t *testing.T) {
	cands := []string{"b", "a", "c"}
	got := Rank("", cands)
	if len(got) != len(cands) {
		t.Fatalf("Rank = %v, want every index", got)
	}
	for i := range got {
		if got[i] != i {
			t.Fatalf("Rank = %v, want input order", got)
		}
	}
	if got := Rank("x", nil); got != nil {
		t.Fatalf("Rank over no candidates = %v, want nil", got)
	}
}

func TestRankDropsNonMatches(t *testing.T) {
	got := Rank("zzz", []string{"host", "zebra-zoo-zone"})
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("Rank = %v, want only the matching index", got)
	}
}
