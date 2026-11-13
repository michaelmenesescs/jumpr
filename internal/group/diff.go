package group

import (
	"regexp"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

var (
	reISO  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	reSys  = regexp.MustCompile(`\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) +\d{1,2} \d{2}:\d{2}:\d{2}\b`)
	rePID  = regexp.MustCompile(`\bpid=\d+`)
	reBrkt = regexp.MustCompile(`\[\d+\]`)
)

// StripTimestamps replaces ISO-8601 and "Mon DD HH:MM:SS" timestamps with "<ts>".
func StripTimestamps(s string) string {
	s = reISO.ReplaceAllString(s, "<ts>")
	return reSys.ReplaceAllString(s, "<ts>")
}

// StripPIDs rewrites "pid=123" to "pid=<n>" and "[123]" to "[<n>]".
func StripPIDs(s string) string {
	s = rePID.ReplaceAllString(s, "pid=<n>")
	return reBrkt.ReplaceAllString(s, "[<n>]")
}

// Chain composes normalizers, applied left to right.
func Chain(fs ...func(string) string) func(string) string {
	return func(s string) string {
		for _, f := range fs {
			if f != nil {
				s = f(s)
			}
		}
		return s
	}
}

// UnifiedDiff returns a unified diff of two texts with 3 lines of context. Empty string when equal.
func UnifiedDiff(aName, a, bName, b string) string {
	if a == b {
		return ""
	}
	out, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(a),
		B:        difflib.SplitLines(b),
		FromFile: aName,
		ToFile:   bName,
		Context:  3,
	})
	if err != nil {
		return ""
	}
	return strings.TrimRight(out, "\n") + "\n"
}
