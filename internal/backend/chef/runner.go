// Package chef reads a Chef server through the `knife` command line tool on a
// workstation, over any transport.Executor (SPEC §8.7).
//
// Nothing here talks to the Chef server directly and nothing is installed on a
// node: every read is one `knife … -F json` line run on the workstation the
// config names. The package builds the command lines with internal/shq, decodes
// the JSON tolerantly and maps knife's failure modes onto jumpr's error kinds.
package chef

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

const (
	// defaultTimeout bounds one knife call when neither the runner nor the
	// call sets a timeout. BUILD-PLAN §8: every remote call has a timeout.
	defaultTimeout = 30 * time.Second
	// defaultKnife is the command name when the config leaves it empty.
	defaultKnife = "knife"
)

var (
	// nodeNameRE matches a Chef node, role or environment name. It is
	// deliberately strict: the name becomes one word of a command line.
	nodeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	// attrNameRE matches a node attribute name passed to `knife search -a`.
	attrNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

// RunnerOptions configures a Runner. Knife empty means "knife"; Timeout zero
// means defaultTimeout. Log is optional and only receives call metadata, never
// output.
type RunnerOptions struct {
	Knife   string
	Sudo    bool
	Timeout time.Duration
	Log     *slog.Logger
}

// Runner runs knife on the Chef workstation.
type Runner struct {
	ex      transport.Executor
	ws      resource.HostID
	knife   string
	sudo    bool
	timeout time.Duration
	log     *slog.Logger
}

// NewRunner returns a Runner that executes on workstation. A nil executor makes
// every call fail with errs.Internal rather than panicking.
func NewRunner(ex transport.Executor, workstation resource.HostID, o RunnerOptions) *Runner {
	knife := strings.TrimSpace(o.Knife)
	if knife == "" {
		knife = defaultKnife
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Runner{
		ex:      ex,
		ws:      resource.NormalizeHostID(string(workstation)),
		knife:   knife,
		sudo:    o.Sudo,
		timeout: timeout,
		log:     o.Log,
	}
}

// Workstation returns the host the runner executes on.
func (r *Runner) Workstation() resource.HostID { return r.ws }

// CommandLine renders the call as a single shell line: the knife command, the
// arguments, then the JSON output flags. Every word is quoted by shq.
func (r *Runner) CommandLine(args ...string) (string, error) {
	for _, a := range args {
		if strings.TrimSpace(a) == "" {
			return "", errs.New(errs.Config, "chef.command", "empty knife argument")
		}
		if strings.ContainsRune(a, 0) {
			return "", errs.New(errs.Config, "chef.command", "knife argument contains a NUL byte")
		}
	}
	words := make([]string, 0, len(args)+3)
	words = append(words, r.knife)
	words = append(words, args...)
	words = append(words, "-F", "json")
	return shq.Join(words...), nil
}

// Run executes the call on the workstation and returns its stdout. A non-zero
// exit code is classified into an error kind; output on stdout with rc 0 is
// data.
func (r *Runner) Run(ctx context.Context, args ...string) ([]byte, error) {
	line, err := r.CommandLine(args...)
	if err != nil {
		return nil, err
	}
	if r.log != nil {
		r.log.Debug("chef call", "host", string(r.ws), "args", len(args), "sudo", r.sudo)
	}
	out, err := r.exec(ctx, line, r.timeout)
	if r.log != nil {
		r.log.Debug("chef call done", "host", string(r.ws), "stdout_bytes", len(out))
	}
	return out, err
}

// RunShell runs one already-built shell line on the workstation with the
// runner's timeout and sudo setting. It is for the few knife commands whose
// output is not JSON (knife --version), and the capture tool's use.
func (r *Runner) RunShell(ctx context.Context, line string) ([]byte, error) {
	return r.exec(ctx, line, r.timeout)
}

// RunJSON runs the call and decodes its JSON output into v.
func (r *Runner) RunJSON(ctx context.Context, v any, args ...string) error {
	out, err := r.Run(ctx, args...)
	if err != nil {
		return err
	}
	return DecodeInto(out, v)
}

// exec runs one already-built shell line on the workstation and applies knife's
// failure rules.
func (r *Runner) exec(ctx context.Context, line string, timeout time.Duration) ([]byte, error) {
	if r.ex == nil {
		return nil, errs.New(errs.Internal, "chef.exec", "no transport executor").WithHost(string(r.ws))
	}
	cmd := transport.Command{Script: line, Sudo: r.sudo, Timeout: timeout}
	res, err := r.ex.Exec(ctx, r.ws, cmd)
	if err != nil {
		return nil, err
	}
	if res.RC == 0 {
		return res.Stdout, nil
	}
	return nil, r.classify(res)
}

// classify maps a failed knife call onto an error kind (BUILD-PLAN §7 plus the
// Chef server's own failure shapes).
func (r *Runner) classify(res transport.ExecResult) *errs.Error {
	se := string(res.Stderr)
	low := strings.ToLower(se)
	var e *errs.Error
	switch {
	case strings.Contains(low, "sudo:"):
		e = errs.New(errs.Sudo, "chef.exec", firstLine(se))
	case res.RC == 127 || strings.Contains(low, "command not found") || strings.Contains(low, "no such file or directory"):
		e = errs.New(errs.NotAvailable, "chef.exec", firstLine(se))
	case strings.Contains(se, "401") || strings.Contains(low, "failed to authenticate") ||
		strings.Contains(low, "unable to authenticate"):
		e = errs.New(errs.Auth, "chef.exec", firstLine(se))
	case strings.Contains(low, "connection refused") || strings.Contains(low, "no route to host") ||
		strings.Contains(low, "no such host") || strings.Contains(low, "connection reset"):
		e = errs.New(errs.Unreachable, "chef.exec", firstLine(se))
	case strings.Contains(se, "Cannot find") || strings.Contains(low, "cannot load such file"):
		e = errs.New(errs.NotAvailable, "chef.exec", firstLine(se))
	default:
		msg := firstLine(se)
		if msg == "" {
			msg = "knife exited with code " + strconv.Itoa(res.RC)
		}
		e = errs.New(errs.Remote, "chef.exec", msg)
	}
	return e.WithHost(string(r.ws))
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// validName reports whether s is a usable node, role or environment name.
func validName(s string) bool { return nodeNameRE.MatchString(s) }

// validAttr reports whether s is a usable attribute name for `knife search -a`.
func validAttr(s string) bool { return attrNameRE.MatchString(s) }

// nameError builds the Config error for an invalid name.
func nameError(op, what, name string) *errs.Error {
	return errs.New(errs.Config, op, fmt.Sprintf("invalid %s name %q", what, name))
}
