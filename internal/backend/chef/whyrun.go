// chef-client through jumpr's own fan-out (SPEC §8.7): the why-run dry run and
// the convergent run, normalized and grouped so a fleet's worth of output
// collapses into the few answers that differ.
//
// `knife ssh '<q>' 'sudo chef-client'` is deliberately not used: jumpr runs
// the client itself on the targets, so safety, audit and policy stay uniform.

package chef

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/fanout"
	"jumpr/internal/group"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

const (
	// defaultClient is the client binary when the config names none.
	defaultClient = "chef-client"
	// cincClient is the Cinc distribution's binary name; the config selects it
	// with `chef.client: cinc-client`.
	cincClient = "cinc-client"
	// whyRunTimeout bounds one why-run fan-out (10 minutes, task 114).
	whyRunTimeout = 10 * time.Minute
	// clientRunTimeout bounds one chef-client run step. A convergent run of a
	// big run list is slow; the bus step budget is larger still.
	clientRunTimeout = 30 * time.Minute
	// runLockTimeout is how long chef-client waits for another run's lock.
	// Without it a second run blocks until its own timeout; with it the wait is
	// bounded and the lock failure is clear.
	runLockTimeout = "30"
	// whyRunOp is the error Op of the why-run fan-out and of its steps.
	whyRunOp = "chef.WhyRun"
	// clientRunOp is the error Op of a chef-client run step.
	clientRunOp = "chef.client.run"
	// lockMessage is what a run-lock failure is reported as, so an operator
	// reads "another run is in progress" instead of a wall of client output.
	lockMessage = "chef-client holds the run lock on this host: another run is in progress"
	// noSummaryText labels a host whose output carried no client summary.
	noSummaryText = "no summary"
)

// ClientOptions configures the chef-client fan-out. Client empty means
// "chef-client"; cinc-client is the Cinc distribution. Timeout zero means
// whyRunTimeout for a dry run and clientRunTimeout for a run.
type ClientOptions struct {
	Exec        transport.Executor
	Client      string
	Concurrency int
	Timeout     time.Duration
	Redactor    *redact.Redactor
}

// Client runs chef-client on hosts through the fan-out. It holds no state
// beyond its options and is safe for concurrent use.
type Client struct {
	exec    transport.Executor
	name    string
	conc    int
	timeout time.Duration
	red     *redact.Redactor
}

// NewClient returns a Client from o. An unknown client name is kept as typed:
// the command not existing is reported by the host, which is the honest answer.
func NewClient(o ClientOptions) *Client {
	name := strings.TrimSpace(o.Client)
	if name == "" {
		name = defaultClient
	}
	return &Client{
		exec:    o.Exec,
		name:    name,
		conc:    o.Concurrency,
		timeout: o.Timeout,
		red:     o.Redactor,
	}
}

// Name returns the client binary the fan-out runs.
func (c *Client) Name() string { return c.name }

// Configured reports whether the client can run at all.
func (c *Client) Configured() bool { return c != nil && c.exec != nil }

// WhyRunRow is one host's why-run summary: the resources the client said it
// would update, and whether the run itself failed.
type WhyRunRow struct {
	Host      resource.HostID
	Updated   int
	Total     int
	HasCounts bool // the output carried "n/m resources"
	Failed    bool
	Locked    bool // another run held the run lock
	Summary   string
	Output    string // normalized, redacted, no trailing newlines
}

// WhyRunReport is the grouped answer of one why-run fan-out.
type WhyRunReport struct {
	Total   int // hosts the run was started for
	Summary group.Summary
	Rows    []WhyRunRow // sorted by host; hosts whose transport failed are absent
}

// Headline renders the group counts, e.g. "3 identical".
func (r WhyRunReport) Headline() string { return r.Summary.Headline() }

// WouldChange is the number of hosts whose client reported at least one
// resource it would update.
func (r WhyRunReport) WouldChange() int {
	n := 0
	for _, row := range r.Rows {
		if row.Updated > 0 {
			n++
		}
	}
	return n
}

// Text renders the report for a plan preview: the counts, then every group's
// hosts and its representative output.
func (r WhyRunReport) Text() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s · %d %s · %d would change",
		hostCount(r.Total), len(r.Summary.Groups), plural(len(r.Summary.Groups), "group", "groups"),
		r.WouldChange()))
	if n := countErrors(r.Summary); n > 0 {
		b.WriteString(fmt.Sprintf(" · %d %s", n, plural(n, "error", "errors")))
	}
	for _, g := range r.Summary.Groups {
		b.WriteString(fmt.Sprintf("\n\n%s: %s\n", hostCount(len(g.Hosts)), hostNames(g.Hosts)))
		b.WriteString(strings.TrimRight(g.Output, "\n"))
	}
	return b.String()
}

// countErrors is the number of hosts whose transport failed.
func countErrors(s group.Summary) int {
	n := 0
	for _, l := range s.Errors {
		n += len(l)
	}
	return n
}

// hostNames joins host ids for display.
func hostNames(hosts []resource.HostID) string {
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, string(h))
	}
	return strings.Join(names, ", ")
}

// hostCount renders "1 host" / "12 hosts".
func hostCount(n int) string { return strconv.Itoa(n) + " " + plural(n, "host", "hosts") }

// plural picks the singular or plural noun for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// script builds the shell line the fan-out runs and the exact display line the
// plan shows. Every word goes through shq; the client name comes from the
// config, never from user input.
func (c *Client) script(whyRun bool) (script, display string) {
	args := []string{c.name}
	if whyRun {
		args = append(args, "--why-run")
	}
	args = append(args, "--no-color", "-l", "warn", "--run-lock-timeout", runLockTimeout)
	script = shq.Join(args...)
	return script, "sudo -n " + script
}

// WhyRun runs `sudo -n <client> --why-run --no-color -l warn --run-lock-timeout
// 30` on every host and groups the normalized output. Why-run is a dry run, but
// ruby_block resources that are not why-run safe still execute.
func (c *Client) WhyRun(ctx context.Context, hosts []resource.HostID) (WhyRunReport, error) {
	if !c.Configured() {
		return WhyRunReport{}, errs.New(errs.Config, whyRunOp, "no transport executor configured")
	}
	want := dedupeHosts(hosts)
	if len(want) == 0 {
		return WhyRunReport{}, errs.New(errs.Config, whyRunOp, "no hosts to target")
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = whyRunTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	script, _ := c.script(true)
	cmd := transport.Command{Script: script, Sudo: true, Timeout: timeout}
	g := group.NewGrouper(len(want), NormalizeClientOutput)
	var rows []WhyRunRow
	for hr := range fanout.Run(cctx, c.exec, want, cmd, fanout.Options{Concurrency: c.conc}) {
		if hr.Err != nil {
			g.AddError(hr.Host, hr.Err)
			continue
		}
		text := c.normalized(hr.Result)
		g.Add(hr.Host, text, hr.Result.RC)
		rows = append(rows, whyRunRow(hr.Host, text, hr.Result.RC))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Host < rows[j].Host })
	return WhyRunReport{Total: len(want), Summary: g.Summary(), Rows: rows}, nil
}

// whyRunRow folds one host's normalized output into the columns of the report.
func whyRunRow(host resource.HostID, text string, rc int) WhyRunRow {
	row := WhyRunRow{Host: host, Output: text, Locked: isRunLock(text)}
	if s, ok := ParseClientSummary(text); ok {
		row.Updated, row.Total, row.HasCounts, row.Failed = s.Updated, s.Total, s.HasCounts, s.Failed
		row.Summary = s.Line
	}
	if rc != 0 {
		row.Failed = true
	}
	return row
}

// normalized returns a host's combined output redacted, normalized and without
// trailing newlines. The redactor is applied before grouping, so a value that
// only one host printed cannot split a group.
func (c *Client) normalized(res transport.ExecResult) string {
	text := strings.TrimRight(string(res.Stdout), "\n")
	if se := strings.TrimRight(string(res.Stderr), "\n"); se != "" {
		if text != "" {
			text += "\n"
		}
		text += se
	}
	if c.red != nil {
		text = c.red.String(text)
	}
	return NormalizeClientOutput(text)
}

// Step runs the client on one host and returns the step result the bus records.
// A non-zero exit code is a failure, and a run-lock message is classified as
// one with a message an operator can act on.
func (c *Client) Step(ctx context.Context, target action.Target, whyRun bool) action.StepResult {
	op := clientRunOp
	if whyRun {
		op = whyRunOp
	}
	res := action.StepResult{Target: target}
	if !c.Configured() {
		res.RC = -1
		res.Err = errs.New(errs.Config, op, "no transport executor configured").WithHost(string(target.Host))
		return res
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = clientRunTimeout
		if whyRun {
			timeout = whyRunTimeout
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	script, _ := c.script(whyRun)
	out, err := c.exec.Exec(cctx, target.Host, transport.Command{Script: script, Sudo: true, Timeout: timeout})
	res.RC = out.RC
	res.Duration = out.Duration
	res.Output = c.normalized(out)
	if err != nil {
		res.Err = err
		return res
	}
	switch {
	case isRunLock(res.Output):
		res.Err = errs.New(errs.Remote, op, lockMessage).WithHost(string(target.Host))
	case out.RC != 0:
		msg := firstLine(res.Output)
		if msg == "" {
			msg = fmt.Sprintf("%s exited with code %d", c.name, out.RC)
		}
		res.Err = errs.New(errs.Remote, op, msg).WithHost(string(target.Host))
	}
	return res
}

// ClientSummary is the parsed summary line of one chef-client run.
type ClientSummary struct {
	Updated   int  // resources the client updated or would have updated
	Total     int  // resources in the resource collection
	HasCounts bool // the line carried "n/m resources"
	Failed    bool // the client failed
	Line      string
}

var (
	// summaryRE matches the closing line of a run:
	// "Chef Infra Client finished, 2/45 resources would have been updated".
	summaryRE = regexp.MustCompile(`Chef Infra Client (finished|failed)[,.]?\s+(\d+)/(\d+) resources (?:would have been updated|updated)`)
	// failedRE matches a failed run without counts:
	// "Chef Infra Client failed. 0 resources updated in 12 seconds".
	failedRE = regexp.MustCompile(`Chef Infra Client failed`)
	// tsRE matches the leading timestamp of a log line.
	tsRE = regexp.MustCompile(`\[\d{4}-\d\d-\d\d[T ][^\]]*\]`)
	// durRE matches a duration: "in 3.42 seconds", "took 12 ms".
	durRE = regexp.MustCompile(`\b\d+(?:\.\d+)?\s*(?:seconds?|minutes?|hours?|ms)\b`)
	// pidRE matches a process id in any of the forms the client prints.
	pidRE = regexp.MustCompile(`\b(?:[Pp][Ii][Dd])[ =:]+\d+\b`)
)

// ParseClientSummary reads the summary line of a client run. The last matching
// line wins: a why-run prints its summary at the end. It reports false when the
// output carried no summary at all.
func ParseClientSummary(out string) (ClientSummary, bool) {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if m := summaryRE.FindStringSubmatch(line); m != nil {
			updated, _ := strconv.Atoi(m[2])
			total, _ := strconv.Atoi(m[3])
			return ClientSummary{
				Updated:   updated,
				Total:     total,
				HasCounts: true,
				Failed:    m[1] == "failed",
				Line:      line,
			}, true
		}
		if failedRE.MatchString(line) {
			return ClientSummary{Failed: true, Line: line}, true
		}
	}
	return ClientSummary{}, false
}

// NormalizeClientOutput strips what differs between hosts for reasons that are
// not a drift signal: log timestamps, durations and process ids. Two hosts that
// converged identically then group into one.
func NormalizeClientOutput(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, line := range lines {
		if tsRE.MatchString(line) {
			// A stripped timestamp would leave the space that separated it.
			line = strings.TrimLeft(tsRE.ReplaceAllString(line, ""), " 	")
		}
		line = durRE.ReplaceAllString(line, "<dur>")
		line = pidRE.ReplaceAllString(line, "pid <pid>")
		lines[i] = strings.TrimRight(line, " 	")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// isRunLock reports whether the output is chef-client refusing to run because
// another run holds the lock.
func isRunLock(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "cannot concurrently run more than one") ||
		strings.Contains(low, "run lock") ||
		strings.Contains(low, "another chef-client") ||
		strings.Contains(low, "another cinc-client")
}
