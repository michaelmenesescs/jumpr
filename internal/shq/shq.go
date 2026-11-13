// Package shq is the only place remote command lines are assembled. Every
// remote shell line is built with Quote, Join and Wrap; user input never
// reaches a command line unquoted or unvalidated.
package shq

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	safeRe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	unitRe = regexp.MustCompile(`^[A-Za-z0-9@:._\-]+$`)
)

// Quote returns s as a single POSIX shell word. NUL bytes are stripped because
// they cannot be passed in an argument.
func Quote(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if s == "" {
		return "''"
	}
	if safeRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Join quotes each argument and joins them with single spaces.
func Join(args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// Wrap returns the exact line sent to the remote. The script is prefixed with
// a locale reset so parsers see stable output; with sudo the shell runs under
// `sudo -n` so a password prompt fails instead of hanging.
func Wrap(script string, sudo bool) string {
	line := "sh -c " + Quote("export LC_ALL=C LANG=C; "+script)
	if sudo {
		return "sudo -n -- " + line
	}
	return line
}

// ValidUnitName reports whether s is safe to use as a systemd unit name.
func ValidUnitName(s string) bool {
	return len(s) <= 256 && unitRe.MatchString(s)
}

// ValidSignal reports whether s is a permitted signal name or a number 1-64.
func ValidSignal(s string) bool {
	switch s {
	case "TERM", "KILL", "HUP", "INT", "USR1", "USR2", "QUIT", "STOP", "CONT":
		return true
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 64
}
