// Package errs defines the error taxonomy used across jumpr.
//
// Error is the only error type that crosses package boundaries. Every
// cross-package failure carries a Kind so the UI can label it and callers can
// branch on it; KindOf classifies context errors and unclassified failures too.
package errs

import (
	"context"
	"errors"
	"strings"
)

// Kind classifies a failure so the UI can label it and callers can branch on it.
type Kind string

const (
	Timeout      Kind = "timeout"           // deadline exceeded (connect, exec, HTTP, salt job)
	Canceled     Kind = "canceled"          // context canceled by the user or by shutdown
	Unreachable  Kind = "unreachable"       // DNS, refused, no route, bastion down
	Auth         Kind = "auth"              // SSH auth failed, HTTP 401/403, kube auth
	HostKey      Kind = "hostkey"           // unknown or mismatched host key
	Sudo         Kind = "sudo"              // `sudo -n` needs a password / not permitted
	NotAvailable Kind = "not-available"     // command or API missing on the remote (rc 127, 404)
	Remote       Kind = "remote"            // remote command ran and failed in a way the caller did not expect
	Parse        Kind = "parse"             // output did not parse
	Policy       Kind = "policy"            // denied by policy.yaml or read-only mode
	Interaction  Kind = "needs-interaction" // needs a prompt but no client is attached / prompt timed out
	Config       Kind = "config"            // invalid or missing configuration
	Protocol     Kind = "protocol"          // session-server protocol error or version mismatch
	Internal     Kind = "internal"          // bug; anything unclassified
)

// Error is the only error type that crosses package boundaries.
type Error struct {
	Kind Kind
	Op   string // "ssh.dial", "salt.lookup_jid", ...
	Host string // optional
	Msg  string // short, user-facing, already redacted
	Err  error  // wrapped cause, may be nil
}

// Error formats the populated parts as "<op>: <host>: <msg>: <cause>", skipping
// empty parts and their separators. When Msg is empty and there is no cause the
// kind is used as the message.
func (e *Error) Error() string {
	parts := make([]string, 0, 4)
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Host != "" {
		parts = append(parts, e.Host)
	}
	msg := e.Msg
	if msg == "" && e.Err == nil {
		msg = string(e.Kind)
	}
	if msg != "" {
		parts = append(parts, msg)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

// Unwrap returns the wrapped cause.
func (e *Error) Unwrap() error { return e.Err }

// New builds an Error.
func New(kind Kind, op, msg string) *Error {
	return &Error{Kind: kind, Op: op, Msg: msg}
}

// Wrap builds one around a cause.
func Wrap(kind Kind, op string, err error) *Error {
	return &Error{Kind: kind, Op: op, Err: err}
}

// WithHost returns a copy with Host set.
func (e *Error) WithHost(host string) *Error {
	cp := *e
	cp.Host = host
	return &cp
}

// KindOf returns the Kind of err. It unwraps; it maps context.DeadlineExceeded
// to Timeout and context.Canceled to Canceled; anything else is Internal.
// KindOf(nil) is "".
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Canceled
	default:
		return Internal
	}
}

// Is reports whether err classifies as kind.
func Is(err error, kind Kind) bool { return KindOf(err) == kind }
