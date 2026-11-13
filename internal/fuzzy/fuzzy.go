// Package fuzzy implements fzf-style case-insensitive subsequence matching and
// ranking. It is pure Go with no dependencies and no external processes: the
// table filter and the command bar call it directly.
package fuzzy

import (
	"sort"
	"unicode"
)

// Scoring weights. Contiguous runs beat scattered letters, a match at a word
// start (_, -, ., /, space, camelCase boundary or the first rune) is strongly
// favoured, every skipped rune costs, and starting later in the string costs a
// small leading penalty.
const (
	wordStartBonus     = 16
	contiguousBonus    = 8
	gapPenalty         = 1
	firstOffsetPenalty = 3
)

// unset marks a DP cell no match can reach.
const unset = -1 << 30

// Match reports whether pattern is a case-insensitive subsequence of s and, if
// so, the score of the best alignment and the rune indices into s of one
// matched rune per pattern rune, strictly increasing. An empty pattern matches
// everything with score 0 and no positions.
func Match(pattern, s string) (score int, positions []int, ok bool) {
	p := []rune(pattern)
	if len(p) == 0 {
		return 0, nil, true
	}
	r := []rune(s)
	if len(p) > len(r) {
		return 0, nil, false
	}

	lp := make([]rune, len(p))
	for i, c := range p {
		lp[i] = unicode.ToLower(c)
	}
	lr := make([]rune, len(r))
	for j, c := range r {
		lr[j] = unicode.ToLower(c)
	}

	// dp[i*n+j] is the best score matching p[0..i] with p[i] at r[j];
	// prev stores the position of p[i-1] for that alignment.
	n := len(r)
	dp := make([]int, len(p)*n)
	prev := make([]int, len(p)*n)
	for i := range dp {
		dp[i] = unset
		prev[i] = -1
	}

	for i := 0; i < len(p); i++ {
		for j := 0; j < n; j++ {
			if lp[i] != lr[j] {
				continue
			}
			bonus := 0
			if isWordStart(r, j) {
				bonus = wordStartBonus
			}
			if i == 0 {
				sc := bonus
				if j > 0 {
					sc -= firstOffsetPenalty
				}
				dp[j] = sc
				continue
			}
			base := (i - 1) * n
			best, bestK := unset, -1
			for k := 0; k < j; k++ {
				if dp[base+k] == unset {
					continue
				}
				sc := dp[base+k] + bonus
				if j == k+1 {
					sc += contiguousBonus
				} else {
					sc -= (j - k - 1) * gapPenalty
				}
				if sc > best {
					best, bestK = sc, k
				}
			}
			if bestK >= 0 {
				dp[i*n+j] = best
				prev[i*n+j] = bestK
			}
		}
	}

	last := (len(p) - 1) * n
	bestJ, bestScore := -1, unset
	for j := 0; j < n; j++ {
		if dp[last+j] > bestScore {
			bestScore, bestJ = dp[last+j], j
		}
	}
	if bestJ < 0 {
		return 0, nil, false
	}

	positions = make([]int, len(p))
	j := bestJ
	for i := len(p) - 1; i >= 0; i-- {
		positions[i] = j
		j = prev[i*n+j]
	}
	return bestScore, positions, true
}

// Rank returns the indices of the candidates that match pattern, best first.
// Ties keep the input order (stable); non-matching candidates are dropped and
// an empty pattern returns every index in order.
func Rank(pattern string, candidates []string) []int {
	if len(candidates) == 0 {
		return nil
	}
	type entry struct {
		idx   int
		score int
	}
	entries := make([]entry, 0, len(candidates))
	for i, c := range candidates {
		score, _, ok := Match(pattern, c)
		if !ok {
			continue
		}
		entries = append(entries, entry{idx: i, score: score})
	}
	sort.SliceStable(entries, func(a, b int) bool {
		return entries[a].score > entries[b].score
	})
	out := make([]int, len(entries))
	for i, e := range entries {
		out[i] = e.idx
	}
	return out
}

// isWordStart reports whether r[j] begins a word: the first rune, a rune after
// one of _-./ or a space, or an upper-case rune after a lower-case one.
func isWordStart(r []rune, j int) bool {
	if j <= 0 {
		return true
	}
	switch r[j-1] {
	case '_', '-', '.', '/', ' ':
		return true
	}
	return unicode.IsLower(r[j-1]) && unicode.IsUpper(r[j])
}
