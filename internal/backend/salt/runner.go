// Package salt drives the Salt command line tools (salt, salt-run, salt-key)
// on the master over any transport.Executor and decodes their JSON output.
//
// Nothing here knows about jobs, targeting logic or specific Salt functions:
// it builds correct command lines with internal/shq, runs them, and maps the
// failure modes of the CLI onto jumpr's error kinds.
package salt

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

// defaultTimeout bounds one Salt CLI call when neither the runner nor the call
// sets a timeout. BUILD-PLAN §8: every remote call has a timeout.
const defaultTimeout = 30 * time.Second

// Tool names accepted in Call.Tool.
const (
	toolSalt = "salt"
	toolRun  = "salt-run"
	toolKey  = "salt-key"
)

var (
	funRe     = regexp.MustCompile(`^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)*$`)
	kwargRe   = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	keywordRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

// targetFlags maps a Target.Type to the Salt flag that introduces it. A glob
// target is the salt default and takes no flag.
var targetFlags = map[string]string{
	"glob":      "",
	"list":      "-L",
	"compound":  "-C",
	"grain":     "-G",
	"pcre":      "-E",
	"nodegroup": "-N",
}

// RunnerOptions configures a Runner. Timeout 0 means defaultTimeout; Log is
// optional and only receives call metadata, never arguments or output.
type RunnerOptions struct {
	Sudo    bool
	Timeout time.Duration
	Log     *slog.Logger
}

// Target is one Salt target expression. Type is glob, list, compound, grain,
// pcre or nodegroup.
type Target struct {
	Type string
	Expr string
}

// Call is one Salt CLI invocation. Kwargs are rendered as key=value words;
// positional Args must never look like keyword arguments.
type Call struct {
	Tool    string
	Target  *Target
	Fun     string
	Args    []string
	Kwargs  map[string]string
	Flags   []string
	Timeout time.Duration
	// Sensitive keeps a call's arguments and output out of the log. Every
	// pillar.* call is sensitive by function name alone (see hidden); any
	// other call may say so explicitly.
	Sensitive bool
}

// hidden reports whether the call's arguments and output must stay out of the
// log: an explicit Sensitive, or a pillar.* function.
func (c Call) hidden() bool { return c.Sensitive || HiddenFunction(c.Fun) }

// CommandLine renders the call as a single shell line. The word order is
// fixed: tool, JSON output flags, extra flags, target flag and expression,
// function, positional arguments, sorted keyword arguments.
func (c Call) CommandLine() (string, error) {
	switch c.Tool {
	case toolSalt, toolRun, toolKey:
	default:
		return "", errs.New(errs.Config, "salt.command", fmt.Sprintf("unknown salt tool %q", c.Tool))
	}
	if c.Fun != "" && !funRe.MatchString(c.Fun) {
		return "", errs.New(errs.Config, "salt.command", fmt.Sprintf("invalid salt function %q", c.Fun))
	}
	for _, a := range c.Args {
		if keywordRe.MatchString(a) {
			return "", errs.New(errs.Config, "salt.command",
				fmt.Sprintf("positional argument %q looks like a keyword argument; use Kwargs", a))
		}
	}
	keys := make([]string, 0, len(c.Kwargs))
	for k := range c.Kwargs {
		if !kwargRe.MatchString(k) {
			return "", errs.New(errs.Config, "salt.command", fmt.Sprintf("invalid keyword argument name %q", k))
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	words := []string{c.Tool, "--out=json", "--out-indent=-1", "--no-color"}
	words = append(words, c.Flags...)

	// salt-run and salt-key take no target; a target set by mistake is ignored.
	if c.Tool == toolSalt && c.Target != nil {
		flag, ok := targetFlags[c.Target.Type]
		if !ok {
			return "", errs.New(errs.Config, "salt.command", fmt.Sprintf("unknown target type %q", c.Target.Type))
		}
		if c.Target.Expr == "" {
			return "", errs.New(errs.Config, "salt.command", "empty target expression")
		}
		if flag != "" {
			words = append(words, flag)
		}
		words = append(words, c.Target.Expr)
	}
	if c.Fun != "" {
		words = append(words, c.Fun)
	}
	words = append(words, c.Args...)
	for _, k := range keys {
		words = append(words, k+"="+c.Kwargs[k])
	}
	return shq.Join(words...), nil
}

// Runner runs Salt CLI calls on the master.
type Runner struct {
	ex      transport.Executor
	master  resource.HostID
	sudo    bool
	timeout time.Duration
	log     *slog.Logger
}

// NewRunner returns a Runner that executes on master. A nil executor makes
// every call fail with errs.Internal rather than panicking.
func NewRunner(ex transport.Executor, master resource.HostID, o RunnerOptions) *Runner {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Runner{ex: ex, master: master, sudo: o.Sudo, timeout: timeout, log: o.Log}
}

// Run executes the call on the master and returns its stdout. A non-zero exit
// code with stdout on it is returned as data: Salt exits non-zero when any
// minion fails, and that partial output is what the callers need.
//
// A sensitive call (see Call.Sensitive) puts nothing about itself in the log:
// no function, no arguments, no output — only the tool and the host, marked
// as sensitive.
func (r *Runner) Run(ctx context.Context, c Call) ([]byte, error) {
	line, err := c.CommandLine()
	if err != nil {
		return nil, err
	}
	sensitive := c.hidden()
	if r.log != nil {
		if sensitive {
			r.log.Debug("salt call", "tool", c.Tool, "host", string(r.master), "sensitive", true)
		} else {
			r.log.Debug("salt call", "tool", c.Tool, "fun", c.Fun, "host", string(r.master),
				"args", len(c.Args), "kwargs", len(c.Kwargs))
		}
	}
	out, err := r.exec(ctx, line, r.timeoutFor(c))
	if r.log != nil && !sensitive {
		r.log.Debug("salt call done", "tool", c.Tool, "host", string(r.master), "stdout_bytes", len(out))
	}
	return out, err
}

// Version returns the master's Salt version as a "3007.1"-style string.
func (r *Runner) Version(ctx context.Context) (string, error) {
	out, err := r.exec(ctx, shq.Join(toolSalt, "--version"), r.timeout)
	if err != nil {
		return "", err
	}
	if m := versionRe.FindStringSubmatch(string(out)); m != nil {
		return m[1], nil
	}
	return "", errs.New(errs.Parse, "salt.version", "unexpected salt --version output").WithHost(string(r.master))
}

var versionRe = regexp.MustCompile(`\b(\d+\.\d+(?:\.\d+)?)\b`)

func (r *Runner) timeoutFor(c Call) time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return r.timeout
}

// exec runs one already-built shell line on the master and applies the Salt
// CLI's failure rules.
func (r *Runner) exec(ctx context.Context, line string, timeout time.Duration) ([]byte, error) {
	if r.ex == nil {
		return nil, errs.New(errs.Internal, "salt.exec", "no transport executor").WithHost(string(r.master))
	}
	cmd := transport.Command{Script: line, Sudo: r.sudo, Timeout: timeout}
	res, err := r.ex.Exec(ctx, r.master, cmd)
	if err != nil {
		return nil, err
	}
	if res.RC == 0 || len(res.Stdout) > 0 {
		return res.Stdout, nil
	}
	return nil, r.classify(res)
}

// classify maps a failed call with no output on stdout onto an error kind.
func (r *Runner) classify(res transport.ExecResult) *errs.Error {
	se := string(res.Stderr)
	var e *errs.Error
	switch {
	case strings.Contains(se, "sudo:"):
		e = errs.New(errs.Sudo, "salt.exec", firstLine(se))
	case strings.Contains(se, "command not found") || res.RC == 127:
		e = errs.New(errs.NotAvailable, "salt.exec", firstLine(se))
	case strings.Contains(se, "Salt request timed out") || strings.Contains(se, "The salt master could not be contacted"):
		e = errs.New(errs.Unreachable, "salt.exec", firstLine(se))
	default:
		msg := firstLine(se)
		if msg == "" {
			msg = "salt exited with code " + strconv.Itoa(res.RC)
		}
		e = errs.New(errs.Remote, "salt.exec", msg)
	}
	return e.WithHost(string(r.master))
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
