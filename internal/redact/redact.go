// Package redact removes secrets from text before it is stored or shown.
//
// Every record, log line and error message passes through a Redactor first
// (BUILD-PLAN §7). The built-in patterns cover the shapes that actually leak
// in operations output — authorization headers, bearer tokens, AWS keys,
// private key blocks, password/secret/token assignments, credentials embedded
// in URLs, JWTs and cookies — and user-supplied patterns can be added on top.
//
// Redaction is idempotent: running a Redactor over its own output changes
// nothing. A Redactor is immutable after New, so it is safe for concurrent
// use.
package redact

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"jumper/internal/errs"
)

const (
	openMark  = "‹redacted:"
	closeMark = "›"
)

// rule is one pattern and the text that replaces what it matches. frags is a
// pre-filter: lower-case ASCII fragments of which at least one must occur for
// the pattern to match at all. No fragments means the rule is always run.
type rule struct {
	re    *regexp.Regexp
	repl  string
	frags []string
}

// Redactor applies an ordered list of secret patterns to text.
//
// Built-in patterns that can cross a line boundary run first over the whole
// text, then the single-line patterns run line by line, then the user patterns
// run last over everything. A Redactor is immutable after New.
type Redactor struct {
	whole []rule // multi-line built-ins
	lines []rule // single-line built-ins
	user  []rule // extra patterns from config
}

// marker renders the placeholder for a named pattern.
func marker(name string) string { return openMark + name + closeMark }

// New builds a Redactor from the built-in patterns plus extra user regexps.
// A pattern that does not compile is an errs.Config error naming its position.
// A nil or empty extra list is valid.
func New(extra []string) (*Redactor, error) {
	whole, lines := builtinRules()
	r := &Redactor{whole: whole, lines: lines}
	for i, pat := range extra {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, errs.New(errs.Config, "redact.new",
				fmt.Sprintf("pattern %d: %v", i+1, err))
		}
		// No pre-filter: a user pattern can be anything at all, so it runs
		// over the whole text.
		r.user = append(r.user, rule{re: re, repl: marker(fmt.Sprintf("extra:%d", i+1))})
	}
	return r, nil
}

// builtinRules compiles the built-in patterns in application order: broad
// shapes first (key blocks, headers, whole lines) so a narrower rule never
// leaves half a secret behind.
func builtinRules() (whole, lines []rule) {
	whole = []rule{
		// Private key blocks, complete then unterminated.
		{re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), repl: marker("private-key")},
		{re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*`), repl: marker("private-key")},
	}
	lines = []rule{
		// Authorization headers keep their scheme, lose the credential.
		{re: regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`), repl: "${1}" + marker("bearer"), frags: []string{"authorization"}},
		{re: regexp.MustCompile(`(?i)(authorization:\s*basic\s+)\S+`), repl: "${1}" + marker("basic"), frags: []string{"authorization"}},
		// Bare bearer tokens in logs and proxy captures.
		{re: regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{16,}`), repl: "${1}" + marker("bearer"), frags: []string{"bearer"}},
		// AWS credentials.
		{re: regexp.MustCompile(`(?i)(aws_secret_access_key\s*[=:]\s*)\S+`), repl: "${1}" + marker("aws-secret"), frags: []string{"aws_secret_access_key"}},
		{re: regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), repl: marker("aws-key"), frags: []string{"akia", "asia"}},
		// JSON Web Tokens.
		{re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), repl: marker("jwt"), frags: []string{"eyj"}},
		// user:password@ in any URL.
		{re: regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`), repl: "://" + marker("url-cred") + "@", frags: []string{"://"}},
		// Whole cookie lines, headers or not.
		{re: regexp.MustCompile(`(?i)((set-)?cookie:\s*)[^\r\n]+`), repl: "${1}" + marker("cookie"), frags: []string{"cookie:"}},
		// key = value assignments; the key and separator stay readable.
		{re: regexp.MustCompile(`(?i)\b(pass(word|wd)?|secret|token|api[_-]?key)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`), repl: "${1}${3}" + marker("password"), frags: []string{"pass", "secret", "token", "api"}},
	}
	return whole, lines
}

// String returns s with every secret replaced by a ‹redacted:name› marker.
func (r *Redactor) String(s string) string {
	if r == nil || s == "" {
		return s
	}
	for _, ru := range r.whole {
		s = ru.re.ReplaceAllString(s, ru.repl)
	}
	if strings.Contains(s, "\n") {
		lines := strings.Split(s, "\n")
		for i, ln := range lines {
			lines[i] = r.redactLine(ln)
		}
		s = strings.Join(lines, "\n")
	} else {
		s = r.redactLine(s)
	}
	for _, ru := range r.user {
		s = ru.re.ReplaceAllString(s, ru.repl)
	}
	return s
}

// redactLine applies the single-line patterns to one line. The pre-filter keeps
// the cost proportional to the lines that can actually contain a secret: logs
// are mostly noise, and running the full pattern set over every megabyte of
// them costs far more than the redaction itself.
func (r *Redactor) redactLine(ln string) string {
	if ln == "" {
		return ln
	}
	// Non-ASCII text is always processed in full: case folding outside ASCII
	// can make the ASCII pre-filter miss a case-insensitive match.
	plain := isASCII(ln)
	for _, ru := range r.lines {
		if plain && !trigger(ln, ru.frags) {
			continue
		}
		ln = ru.re.ReplaceAllString(ln, ru.repl)
	}
	return ln
}

// trigger reports whether an ASCII line may contain a match for any of the
// rule's lower-case fragments. A rule without fragments always passes.
func trigger(ln string, frags []string) bool {
	if len(frags) == 0 {
		return true
	}
	for _, f := range frags {
		if containsFold(ln, f) {
			return true
		}
	}
	return false
}

// containsFold is strings.Contains with ASCII case folding and no allocation.
func containsFold(s, frag string) bool {
	if len(frag) > len(s) {
		return false
	}
	for i := 0; i+len(frag) <= len(s); i++ {
		j := 0
		for j < len(frag) && lowerASCII(s[i+j]) == frag[j] {
			j++
		}
		if j == len(frag) {
			return true
		}
	}
	return false
}

// lowerASCII folds one ASCII letter to lower case.
func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// isASCII reports whether s is plain ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// Bytes is String for byte slices.
func (r *Redactor) Bytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	return []byte(r.String(string(b)))
}

// Cap redacts s, then cuts the result on a line boundary so it fits in
// maxBytes, appending "… (N more lines)" for the rest. Text that already fits
// is returned unchanged apart from redaction.
func (r *Redactor) Cap(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	out := r.String(s)
	if len(out) <= maxBytes {
		return out
	}
	lines := strings.Split(out, "\n")
	keep := 0
	for i := 1; i <= len(lines); i++ {
		body := strings.Join(lines[:i], "\n")
		suffix := moreSuffix(len(lines) - i)
		if len(body)+len(suffix) > maxBytes {
			break
		}
		keep = i
	}
	res := strings.Join(lines[:keep], "\n") + moreSuffix(len(lines)-keep)
	if keep == 0 {
		res = strings.TrimPrefix(res, "\n")
	}
	return cutBytes(res, maxBytes)
}

// moreSuffix renders the omitted-lines notice.
func moreSuffix(n int) string {
	return fmt.Sprintf("\n… (%d more lines)", n)
}

// cutBytes truncates s to at most maxBytes without splitting a rune.
func cutBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}
