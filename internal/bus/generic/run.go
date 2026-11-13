// Package generic plans the ad-hoc command behind `:run`: one shell script,
// verbatim user text, executed on every target through the action bus. Every
// `:run` is a write by definition (SPEC §8.1), so there is no read-only
// detection here and no dry run.
package generic

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

// Spec IDs, parameter names and defaults of the generic planner.
const (
	// SpecRun is the ad-hoc command spec ID.
	SpecRun = "generic.run"
	// ParamCommand is the shell text the user typed.
	ParamCommand = "command"
	// ParamSudo selects whether the command runs as root.
	ParamSudo = "sudo"
	// ParamVia names the transport in execs.
	ParamVia = "via"

	// sudoYes and sudoNo are the accepted values of ParamSudo.
	sudoYes = "yes"
	sudoNo  = "no"

	// defaultVia is used when neither the request nor the planner names a
	// transport.
	defaultVia = "ssh"

	// stepTimeout bounds one target's command. `:run` is an operator's
	// one-liner, not a long-running job, so the §8 exec budget applies.
	stepTimeout = 30 * time.Second
)

// rootTransportPrefix is the executor name prefix of transports that run as
// root on every node (Salt). A plan through one of them warns the user.
const rootTransportPrefix = "salt"

// Planner implements action.Planner for `generic.run`.
type Planner struct {
	execs      map[string]transport.Executor
	defaultVia string
}

var _ action.Planner = (*Planner)(nil)

// New builds the planner over the transports it may use. defaultVia is the
// transport a request without `via` runs through; empty means "ssh".
func New(execs map[string]transport.Executor, defaultVia string) *Planner {
	cp := make(map[string]transport.Executor, len(execs))
	for name, ex := range execs {
		cp[name] = ex
	}
	return &Planner{execs: cp, defaultVia: strings.TrimSpace(defaultVia)}
}

// spec describes the one write this planner offers.
func (p *Planner) spec() action.Spec {
	via := p.defaultVia
	if via == "" {
		via = defaultVia
	}
	return action.Spec{
		ID:    SpecRun,
		Title: "run command",
		Risk:  action.RiskDestructive,
		Params: []action.Param{
			{Name: ParamCommand, Title: "command", Required: true},
			{Name: ParamSudo, Title: "sudo", Default: sudoNo, Choices: []string{sudoYes, sudoNo}},
			{Name: ParamVia, Title: "via", Default: via, Choices: transportsOf(p.execs)},
		},
	}
}

// Actions offers `run command` for any non-empty selection: an ad-hoc command
// is not specific to a kind.
func (p *Planner) Actions(selection []resource.Resource) []action.Spec {
	if len(selection) == 0 {
		return nil
	}
	return []action.Spec{p.spec()}
}

// Plan resolves req into one step per target. Display is the command exactly as
// typed, prefixed with `sudo -n ` when sudo; the script is the command itself.
// Plan does no I/O and produces no preview.
func (p *Planner) Plan(_ context.Context, req action.Request) (action.Plan, error) {
	if req.SpecID != SpecRun {
		return action.Plan{}, errs.New(errs.Config, "generic.Plan", fmt.Sprintf("unknown spec %q", req.SpecID))
	}
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, "generic.Plan", "no targets")
	}

	command, err := commandOf(req.Params)
	if err != nil {
		return action.Plan{}, err
	}
	sudo, err := sudoOf(req.Params)
	if err != nil {
		return action.Plan{}, err
	}
	via, ex, err := p.executorFor(req.Params)
	if err != nil {
		return action.Plan{}, err
	}

	display := command
	if sudo {
		display = "sudo -n " + command
	}
	plan := action.Plan{
		Spec:    p.spec(),
		Request: req,
		Title:   fmt.Sprintf("run %s on %s", previewCommand(command), hostCount(len(req.Targets))),
		Steps:   make([]action.Step, 0, len(req.Targets)),
		Via:     ex.Name(),
	}
	if plan.Via == "" {
		plan.Via = via
	}
	if isRootTransport(plan.Via) {
		plan.Warnings = append(plan.Warnings, "runs as root on every target")
	}
	for _, t := range req.Targets {
		host, herr := targetHost(t)
		if herr != nil {
			return action.Plan{}, herr
		}
		target := t
		target.Host = host
		plan.Steps = append(plan.Steps, action.Step{
			Target:  target,
			Display: display,
			Run: func(ctx context.Context) action.StepResult {
				return runStep(ctx, ex, target, command, sudo)
			},
		})
	}
	return plan, nil
}

// commandOf reads and validates the command parameter. The text is kept
// verbatim: it is the user's shell line, quoted once by the executor.
func commandOf(params map[string]string) (string, error) {
	command := strings.TrimSpace(params[ParamCommand])
	if command == "" {
		return "", errs.New(errs.Config, "generic.Plan", "command is empty")
	}
	return command, nil
}

// sudoOf reads the sudo parameter: yes or no, default no.
func sudoOf(params map[string]string) (bool, error) {
	switch v := strings.ToLower(strings.TrimSpace(params[ParamSudo])); v {
	case "", sudoNo:
		return false, nil
	case sudoYes:
		return true, nil
	default:
		return false, errs.New(errs.Config, "generic.Plan", fmt.Sprintf("sudo must be yes or no, not %q", v))
	}
}

// executorFor picks the transport of the request and returns its executor.
func (p *Planner) executorFor(params map[string]string) (string, transport.Executor, error) {
	via := strings.TrimSpace(params[ParamVia])
	if via == "" {
		via = p.defaultVia
	}
	if via == "" {
		via = defaultVia
	}
	ex, ok := p.execs[via]
	if !ok || ex == nil {
		return "", nil, errs.New(errs.Config, "generic.Plan", fmt.Sprintf("no transport %q configured", via))
	}
	return via, ex, nil
}

// runStep executes the command on one target. A non-zero exit code is a failed
// step: the result carries the code and the combined output, and only a
// transport-level failure sets Err.
func runStep(ctx context.Context, ex transport.Executor, target action.Target, command string, sudo bool) action.StepResult {
	res := action.StepResult{Target: target}
	cctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	out, err := ex.Exec(cctx, target.Host, transport.Command{
		Script:  command,
		Sudo:    sudo,
		Timeout: stepTimeout,
	})
	res.RC = out.RC
	res.Output = string(out.Stdout) + string(out.Stderr)
	res.Duration = out.Duration
	if err != nil {
		res.Err = err
	}
	return res
}

// targetHost resolves the host of a target.
func targetHost(t action.Target) (resource.HostID, error) {
	if h := resource.NormalizeHostID(string(t.Host)); h != "" {
		return h, nil
	}
	return "", errs.New(errs.Config, "generic.Plan", "target needs a host")
}

// isRootTransport reports whether a transport runs as root on every node.
func isRootTransport(name string) bool {
	return name == rootTransportPrefix || strings.HasPrefix(name, rootTransportPrefix+":")
}

// transportsOf lists the configured transport names for the form's choices.
func transportsOf(execs map[string]transport.Executor) []string {
	names := make([]string, 0, len(execs))
	for name := range execs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// previewCommand shortens a multi-line command for the plan title.
func previewCommand(command string) string {
	line := command
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	const maxTitleCommand = 60
	if len(line) > maxTitleCommand {
		line = line[:maxTitleCommand-1] + "…"
	}
	return line
}

// hostCount renders "1 host" / "12 hosts".
func hostCount(n int) string {
	if n == 1 {
		return "1 host"
	}
	return strconv.Itoa(n) + " hosts"
}
