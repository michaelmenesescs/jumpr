// salt-ssh awareness (SPEC §8.6, task 097).
//
// A Salt master may have a roster: a YAML file of hosts it can reach without a
// minion installed. jumpr reads it (over SSH, with sudo cat) and exposes the
// entries as the `salt-roster` inventory source. Execution through salt-ssh
// stays refused unless the user opted in with salt.allow_salt_ssh: true,
// because salt-ssh copies a runtime to the target.

package salt

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/shq"
)

const (
	// DefaultRosterPath is where a Salt master keeps its salt-ssh roster.
	DefaultRosterPath = "/etc/salt/roster"
	// SaltSSHWarning is what every plan that would use salt-ssh carries.
	SaltSSHWarning = "salt-ssh copies a runtime to /var/tmp on the target (not zero-footprint)"

	// saltSSHOp is the error Op of a salt-ssh call.
	saltSSHOp = "salt.salt-ssh"
	// rosterOp is the error Op of the roster read.
	rosterOp = "salt.roster"
	// rosterTimeout bounds the one cat that reads the roster.
	rosterTimeout = 30 * time.Second
	// saltSSHTool is the CLI salt-ssh installs beside salt.
	saltSSHTool = "salt-ssh"
	// saltSSHTimeout bounds one salt-ssh call.
	saltSSHTimeout = 120 * time.Second
)

// RosterEntry is one host of a salt-ssh roster.
type RosterEntry struct {
	ID   string // the roster key, which is the salt-ssh target name
	Host string // where to connect; empty means the ID itself
	User string
	Port int
}

// Address is what a connection would dial: the host when set, else the ID.
func (e RosterEntry) Address() string {
	if h := strings.TrimSpace(e.Host); h != "" {
		return h
	}
	return e.ID
}

// ParseRoster decodes a salt-ssh roster: `id: {host, user, port}` per entry.
// Entries that are not objects, or that name nothing dialable, are skipped;
// remote config is data and never fails the whole read.
func ParseRoster(b []byte) ([]RosterEntry, error) {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(b, &top); err != nil {
		return nil, errs.Wrap(errs.Parse, rosterOp, err)
	}
	ids := make([]string, 0, len(top))
	for id := range top {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	type rosterFields struct {
		Host string      `yaml:"host"`
		User string      `yaml:"user"`
		Port interface{} `yaml:"port"`
	}
	out := make([]RosterEntry, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		node := top[id]
		var f rosterFields
		if err := node.Decode(&f); err != nil {
			continue
		}
		entry := RosterEntry{
			ID:   id,
			Host: strings.TrimSpace(f.Host),
			User: strings.TrimSpace(f.User),
			Port: rosterPort(f.Port),
		}
		if entry.Host == "" && entry.User == "" && entry.Port == 0 {
			// A roster entry with nothing in it names nothing to dial.
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// rosterPort turns the port value into a number: YAML may give an int or a
// string. A missing or nonsensical port is 0, which means "salt-ssh default".
func rosterPort(v interface{}) int {
	switch p := v.(type) {
	case int:
		if p > 0 && p < 65536 {
			return p
		}
	case int64:
		if p > 0 && p < 65536 {
			return int(p)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return 0
}

// RosterPath is the roster file the master is read from.
func (b *Backend) RosterPath() string {
	if strings.TrimSpace(b.rosterPath) == "" {
		return DefaultRosterPath
	}
	return b.rosterPath
}

// Roster reads and parses the master's roster. A master without a roster is
// errs.NotAvailable, not an empty list: salt-ssh being unused is a fact worth
// showing.
func (b *Backend) Roster(ctx context.Context) ([]RosterEntry, error) {
	if b.runner == nil {
		return nil, errs.New(errs.Config, rosterOp, "salt backend not configured: no runner")
	}
	out, err := b.runner.ReadFile(ctx, b.RosterPath(), rosterTimeout)
	if err != nil {
		return nil, err
	}
	entries, err := ParseRoster(out)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errs.New(errs.NotAvailable, rosterOp,
			"the roster has no usable entries: "+b.RosterPath()).WithHost(string(b.master))
	}
	return entries, nil
}

// RosterReadable reports whether the master has a readable roster.
func (b *Backend) RosterReadable(ctx context.Context) bool {
	_, err := b.Roster(ctx)
	return err == nil
}

// SaltSSHAllowed reports whether execution through salt-ssh was opted in.
func (b *Backend) SaltSSHAllowed() bool { return b.allowSSH }

// SaltSSHCommand renders one salt-ssh call for minions. It is built with shq,
// so nothing in the ids or arguments can alter the command line.
func SaltSSHCommand(minions []string, fun string, args []string) (string, error) {
	ids := sortedMinions(minions)
	if len(ids) == 0 {
		return "", errs.New(errs.Config, saltSSHOp, "no hosts to target")
	}
	if fun == "" || !funRe.MatchString(fun) {
		return "", errs.New(errs.Config, saltSSHOp, fmt.Sprintf("invalid salt function %q", fun))
	}
	words := []string{saltSSHTool, "-L", strings.Join(ids, ","), fun}
	words = append(words, args...)
	return shq.Join(words...), nil
}

// SaltSSH runs one salt-ssh call through the same runner every other Salt call
// uses. It refuses unless salt.allow_salt_ssh is set: salt-ssh copies a runtime
// to /var/tmp on the target, which breaks the zero-footprint rule.
func (b *Backend) SaltSSH(ctx context.Context, minions []string, fun string, args []string) ([]byte, error) {
	if !b.allowSSH {
		return nil, errs.New(errs.Config, saltSSHOp,
			"salt-ssh is disabled; set salt.allow_salt_ssh: true to allow it")
	}
	if b.runner == nil {
		return nil, errs.New(errs.Config, saltSSHOp, "salt backend not configured: no runner")
	}
	line, err := SaltSSHCommand(minions, fun, args)
	if err != nil {
		return nil, err
	}
	return b.runner.exec(ctx, line, saltSSHTimeout)
}

// saltSSHPlanWarning is the warning list a salt-ssh plan carries.
func saltSSHPlanWarning() []string { return []string{SaltSSHWarning} }

// sshSteps plans one salt-ssh call per minion. Every step still runs through
// the bus: this only builds it. It refuses before anything is shown when
// salt-ssh was not opted in.
func (b *Backend) sshSteps(spec action.Spec, req action.Request) ([]action.Step, string, []string, error) {
	if !b.allowSSH {
		return nil, "", nil, errs.New(errs.Config, "salt.Plan",
			"salt-ssh is disabled; set salt.allow_salt_ssh: true to allow it")
	}
	call, _, _, err := b.callFor(spec, req.Params)
	if err != nil {
		return nil, "", nil, err
	}
	planned, err := b.resolveMinions(req.Targets)
	if err != nil {
		return nil, "", nil, err
	}
	steps := make([]action.Step, 0, len(planned))
	minions := make([]string, 0, len(planned))
	for _, p := range planned {
		display, derr := SaltSSHCommand([]string{p.minion}, call.Fun, call.Args)
		if derr != nil {
			return nil, "", nil, derr
		}
		pm := p
		stepCall := call
		steps = append(steps, action.Step{
			Target:  pm.target,
			Display: display,
			Run: func(ctx context.Context) action.StepResult {
				return b.runSSHStep(ctx, pm.minion, stepCall, pm.target)
			},
		})
		minions = append(minions, p.minion)
	}
	title := planTitle(spec, req.Params, minions) + " via salt-ssh"
	return steps, title, minions, nil
}

// runSSHStep runs one salt-ssh call for one minion.
func (b *Backend) runSSHStep(ctx context.Context, minion string, call Call, target action.Target) action.StepResult {
	res := action.StepResult{Target: target}
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, saltSSHTimeout)
	defer cancel()

	out, err := b.SaltSSH(cctx, []string{minion}, call.Fun, call.Args)
	res.Duration = time.Since(start)
	if err != nil {
		res.RC = 1
		res.Err = err
		res.Output = firstLine(err.Error())
		return res
	}
	res.Output = strings.TrimRight(string(out), "\n")
	return res
}

// ReadFile reads an absolute path on the master with `sudo -n cat`. It exists
// for files the Salt CLI does not expose, such as the roster.
func (r *Runner) ReadFile(ctx context.Context, path string, timeout time.Duration) ([]byte, error) {
	p := strings.TrimSpace(path)
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\n\r\x00") {
		return nil, errs.New(errs.Config, rosterOp, fmt.Sprintf("invalid file path %q", path))
	}
	if timeout <= 0 {
		timeout = rosterTimeout
	}
	out, err := r.exec(ctx, shq.Join(localSudo, localSudoFlag, "cat", p), timeout)
	if err != nil && errs.KindOf(err) == errs.Remote {
		// A file that is simply not there is not a remote failure: a master
		// without a roster is a normal master.
		return nil, errs.New(errs.NotAvailable, rosterOp, firstLine(err.Error()))
	}
	return out, err
}
