package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/redact"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
)

// doctorLabelWidth is the column width of the check name; statusWidth fits
// "needs-privilege", the longest status a check reports.
const (
	doctorStatusWidth = 15
	doctorLabelWidth  = 12
)

// Doctor runs one diagnostic subcommand and returns the process exit code. It
// writes every line to out, so nothing in this package touches a real terminal.
//
// Arguments are positional, with flags already stripped by cmd/jumper:
//
//	jumper doctor ssh <host>
func Doctor(ctx context.Context, o Options, args []string, out io.Writer) int {
	if out == nil {
		out = io.Discard
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) < 1 || args[0] != "ssh" {
		fmt.Fprintf(out, "usage: jumper doctor ssh <host> [--config-dir DIR]\n")
		return 2
	}
	if len(args) != 2 {
		fmt.Fprintf(out, "usage: jumper doctor ssh <host> [--config-dir DIR]\n")
		return 2
	}
	return doctorSSH(ctx, o, args[1], out)
}

// doctorReport writes one aligned status line per check.
type doctorReport struct {
	out io.Writer
	red *redact.Redactor
}

// status writes a line whose status is not just ok/FAIL.
func (r *doctorReport) status(status, label, detail string) {
	fmt.Fprintf(r.out, "%-*s  %-*s  %s\n", doctorStatusWidth, status, doctorLabelWidth, label, detail)
}

// ok writes a passing check.
func (r *doctorReport) ok(label, detail string) { r.status("ok", label, detail) }

// fail writes a failing check naming the error kind and its message, redacted.
func (r *doctorReport) fail(label string, err error) {
	msg := ""
	if err != nil {
		msg = r.redact(err.Error())
	}
	if k := errs.KindOf(err); k != "" {
		msg = string(k) + ": " + msg
	}
	r.status("FAIL", label, msg)
}

// failMsg writes a failing check with a plain detail (no error value).
func (r *doctorReport) failMsg(label, msg string) { r.status("FAIL", label, r.redact(msg)) }

// redact strips secrets from text before it is shown (BUILD-PLAN §7).
func (r *doctorReport) redact(s string) string {
	if r.red == nil {
		return s
	}
	return r.red.String(s)
}

// doctorSSH proves that SSH works against one host: the resolved chain, the
// dial, the host key, a round trip, sudo and the remote OS. It exits 0 when the
// dial and the echo round trip succeed, 1 otherwise.
func doctorSSH(ctx context.Context, o Options, alias string, out io.Writer) int {
	red, err := redact.New(nil)
	if err != nil {
		fmt.Fprintf(out, "FAIL  doctor         %v\n", err)
		return 1
	}
	r := &doctorReport{out: out, red: red}

	paths := config.ResolvePaths(o.Home, o.ConfigDir)
	log := newLogger(paths.CacheDir, o.Debug)

	cfg, err := config.Load(filepath.Join(paths.ConfigDir, "config.yaml"), o.Home)
	if err != nil {
		r.fail("config", err)
		return 1
	}
	stack, err := buildSSH(cfg, o, log)
	if err != nil {
		r.fail("config", err)
		return 1
	}
	defer func() { _ = stack.Close() }()

	chain, err := stack.Resolver.Chain(alias)
	if err != nil {
		r.fail("resolved", err)
		return 1
	}
	spec := chain[len(chain)-1]
	host := resource.HostID(alias)
	if len(chain) > 1 {
		hops := make([]string, 0, len(chain)-1)
		for _, h := range chain[:len(chain)-1] {
			hops = append(hops, h.Alias)
		}
		r.ok("resolved", fmt.Sprintf("%s via %s", alias, strings.Join(hops, " -> ")))
	} else {
		r.ok("resolved", alias)
	}

	start := time.Now()
	lease, err := stack.Pool.Client(ctx, host)
	elapsed := time.Since(start)
	if err != nil {
		r.fail("dial + auth", err)
		r.failMsg("host key", "not checked: the dial failed")
		r.failMsg("echo ok", "not run: the dial failed")
		r.failMsg("sudo -n true", "not run: the dial failed")
		r.failMsg("uname -sr", "not run: the dial failed")
		r.failMsg("os-release", "not run: the dial failed")
		return 1
	}
	lease.Release()
	r.ok("dial + auth", ms(elapsed))
	r.ok("host key", "accepted ("+strictMode(spec.StrictHostKeyChecking)+")")

	echoOK := doctorEcho(ctx, stack.Transport, host, r)
	doctorSudo(ctx, stack.Transport, host, r)
	doctorRun(ctx, stack.Transport, host, r, "uname -sr", "uname -sr")
	doctorRun(ctx, stack.Transport, host, r, "os-release",
		`. /etc/os-release 2>/dev/null && printf '%s\n' "$PRETTY_NAME"`)

	if !echoOK {
		return 1
	}
	return 0
}

// doctorEcho runs the round trip that decides the exit code.
func doctorEcho(ctx context.Context, ex transport.Executor, host resource.HostID, r *doctorReport) bool {
	res, err := ex.Exec(ctx, host, transport.Command{Script: "echo ok"})
	if err != nil {
		r.fail("echo ok", err)
		return false
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "ok" || res.RC != 0 {
		r.failMsg("echo ok", fmt.Sprintf("unexpected output %q (rc %d)", got, res.RC))
		return false
	}
	r.ok("echo ok", ms(res.Duration))
	return true
}

// doctorSudo reports whether `sudo -n true` works. A password requirement is
// reported as needs-privilege, not as a failure: the transport works, the login
// simply is not passwordlessly privileged.
func doctorSudo(ctx context.Context, ex transport.Executor, host resource.HostID, r *doctorReport) {
	res, err := ex.Exec(ctx, host, transport.Command{Script: "sudo -n true"})
	if err == nil && res.RC == 0 {
		r.ok("sudo -n true", "ok")
		return
	}
	cause := err
	if cause == nil {
		// The command ran and failed: classify its stderr, which is only done
		// for sudo failures.
		cause = shq.Classify(res.RC, string(res.Stderr), true)
	}
	if errs.KindOf(cause) == errs.Sudo {
		r.status("needs-privilege", "sudo -n true", "passwordless sudo is not available")
		return
	}
	if err != nil {
		r.fail("sudo -n true", err)
		return
	}
	r.failMsg("sudo -n true", fmt.Sprintf("rc %d: %s", res.RC, strings.TrimSpace(string(res.Stderr))))
}

// doctorRun runs one read-only probe and prints its first output line under
// label. It never changes the exit code.
func doctorRun(ctx context.Context, ex transport.Executor, host resource.HostID, r *doctorReport, label, script string) {
	res, err := ex.Exec(ctx, host, transport.Command{Script: script})
	if err != nil {
		r.fail(label, err)
		return
	}
	if res.RC != 0 {
		r.failMsg(label, fmt.Sprintf("rc %d: %s", res.RC, strings.TrimSpace(string(res.Stderr))))
		return
	}
	line := strings.TrimSpace(string(res.Stdout))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		line = "(empty)"
	}
	r.ok(label, line)
}

// strictMode names the effective host-key checking mode of the resolved host.
func strictMode(mode string) string {
	if mode = strings.ToLower(strings.TrimSpace(mode)); mode == "" {
		return "ask"
	}
	return mode
}

// ms formats a duration for the report: milliseconds below a second, tenths
// above.
func ms(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}
