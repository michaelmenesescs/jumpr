package shq

import (
	"strings"

	"jumpr/internal/errs"
)

const maxMsg = 200

var sudoPatterns = []string{
	"sudo: a password is required",
	"sudo: a terminal is required",
	"is not in the sudoers file",
}

// Classify maps a remote exit code and stderr to an *errs.Error, or nil when
// nothing matched. Sudo patterns only apply when sudo is true.
func Classify(rc int, stderr string, sudo bool) error {
	low := strings.ToLower(stderr)
	if sudo {
		if p := firstMatch(low, sudoPatterns); p != "" {
			return newErr(errs.Sudo, stderr, p)
		}
	}
	if rc == 127 || strings.Contains(low, "command not found") {
		return newErr(errs.NotAvailable, stderr, "command not found")
	}
	if rc == 126 {
		return newErr(errs.NotAvailable, stderr, "permission denied")
	}
	return nil
}

func firstMatch(low string, pats []string) string {
	for _, p := range pats {
		if strings.Contains(low, p) {
			return p
		}
	}
	return ""
}

// newErr uses the first stderr line containing pat as the message, falling
// back to the first non-empty line, then to pat itself.
func newErr(kind errs.Kind, stderr, pat string) error {
	msg := ""
	first := ""
	for _, ln := range strings.Split(stderr, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if first == "" {
			first = ln
		}
		if strings.Contains(strings.ToLower(ln), pat) {
			msg = ln
			break
		}
	}
	if msg == "" {
		msg = first
	}
	if msg == "" {
		msg = pat
	}
	if r := []rune(msg); len(r) > maxMsg {
		msg = string(r[:maxMsg])
	}
	return &errs.Error{Kind: kind, Op: "remote.exec", Msg: msg}
}
