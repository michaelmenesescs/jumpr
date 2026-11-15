// salt-call local mode (SPEC §8.6, task 097).
//
// When the master is unreachable or a user is not allowed to drive it, one
// minion can answer for itself: `sudo -n salt-call --local grains.items` reads
// local grains without contacting the master, and `sudo -n salt-call
// state.apply test=True` asks the minion's own master for a dry run. Both run
// over the normal SSH executor, on the host itself, and nothing is installed
// or written on the node.

package salt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

const (
	// localOp is the error Op of every salt-call local read.
	localOp = "salt.local"
	// localGrainsTimeout bounds one local grains read; --local never leaves
	// the minion.
	localGrainsTimeout = 30 * time.Second
	// localStateTimeout bounds one local state.apply test=True, which contacts
	// the master from the minion (task 097: 120 s).
	localStateTimeout = 120 * time.Second
	// localProgram is the client a minion ships for itself.
	localProgram = "salt-call"
	// localSudo is the sudo prefix every local call uses: -n refuses to prompt,
	// so a missing sudo rule fails fast instead of hanging a pane.
	localSudo, localSudoFlag = "sudo", "-n"
)

// LocalGrainsCommand is the shell line of a local grains read, built with shq
// so no user input can alter it.
func LocalGrainsCommand() string {
	return shq.Join(localSudo, localSudoFlag, localProgram, "--local", "--out=json", "grains.items")
}

// LocalStateCommand is the shell line of a local state.apply dry run. An empty
// sls is the highstate; the name is validated by the caller.
func LocalStateCommand(sls string) string {
	words := []string{localSudo, localSudoFlag, localProgram, "--out=json", "state.apply"}
	if sls = strings.TrimSpace(sls); sls != "" {
		words = append(words, sls)
	}
	words = append(words, "test=True")
	return shq.Join(words...)
}

// LocalAvailable reports whether the backend can reach a minion directly.
func (b *Backend) LocalAvailable() bool { return b.ex != nil }

// UseLocalFallback reports whether the host overview's Salt section should fall
// back to salt-call local mode: the host itself is reachable and the master is
// down.
func (b *Backend) UseLocalFallback(ctx context.Context) bool {
	if !b.LocalAvailable() {
		return false
	}
	return b.Health(ctx).State == backend.HealthDown
}

// ParseLocal unwraps salt-call's envelope: every salt-call --out=json answer is
// {"local": …}. The inner value is returned as raw JSON.
func ParseLocal(b []byte) (json.RawMessage, error) {
	obj, err := DecodeObject(b)
	if err != nil {
		return nil, err
	}
	raw, ok := obj["local"]
	if !ok {
		return nil, errs.New(errs.Parse, localOp, "salt-call output has no local key")
	}
	return raw, nil
}

// ParseLocalGrains decodes a local grains.items answer.
func ParseLocalGrains(b []byte) (Grains, error) {
	raw, err := ParseLocal(b)
	if err != nil {
		if errs.Is(err, errs.Parse) {
			return nil, errs.New(errs.Parse, localOp, "salt-call did not return grains: "+snippet(b))
		}
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errs.New(errs.Parse, localOp, "grains.items did not return an object")
	}
	var g Grains
	if err := json.Unmarshal(trimmed, &g); err != nil {
		return nil, errs.New(errs.Parse, localOp, "grains.items did not return an object")
	}
	if g == nil {
		g = Grains{}
	}
	return g, nil
}

// ParseLocalState decodes a local state.apply test=True answer into the same
// rows the dry-run view shows.
func ParseLocalState(b []byte) (StateRun, error) {
	raw, err := ParseLocal(b)
	if err != nil {
		if errs.Is(err, errs.Parse) {
			return StateRun{Test: true}, errs.New(errs.Parse, localOp, "salt-call did not return a state run: "+snippet(b))
		}
		return StateRun{Test: true}, err
	}
	run, err := ParseStateReturn(raw)
	if err != nil {
		return StateRun{Test: true}, err
	}
	run.Test = true
	return run, nil
}

// LocalGrains reads one host's grains with `salt-call --local` on the host
// itself.
func (b *Backend) LocalGrains(ctx context.Context, host resource.HostID) (Grains, error) {
	out, err := b.localRun(ctx, host, LocalGrainsCommand(), localGrainsTimeout)
	if err != nil {
		return nil, err
	}
	g, err := ParseLocalGrains(out)
	if err != nil {
		return nil, withLocalHost(err, host)
	}
	return g, nil
}

// LocalState runs `salt-call state.apply test=True` on one host. sls empty
// means the highstate. The call contacts the master from the minion, so it is
// given the longer local timeout.
func (b *Backend) LocalState(ctx context.Context, host resource.HostID, sls string) (StateRun, error) {
	sls = strings.TrimSpace(sls)
	if sls != "" && !slsRe.MatchString(sls) {
		return StateRun{Test: true}, errs.New(errs.Config, localOp, fmt.Sprintf("invalid sls %q", sls))
	}
	out, err := b.localRun(ctx, host, LocalStateCommand(sls), localStateTimeout)
	if err != nil {
		return StateRun{Test: true}, err
	}
	run, err := ParseLocalState(out)
	if err != nil {
		return StateRun{Test: true}, withLocalHost(err, host)
	}
	return run, nil
}

// localRun executes one already-built salt-call line on host over the SSH
// executor and returns its stdout. Non-zero exits carrying stdout are data, as
// with every Salt CLI.
func (b *Backend) localRun(ctx context.Context, host resource.HostID, script string, timeout time.Duration) ([]byte, error) {
	if b.ex == nil {
		return nil, errs.New(errs.Config, localOp, "salt-call local mode needs an SSH executor").WithHost(string(host))
	}
	if strings.TrimSpace(string(host)) == "" {
		return nil, errs.New(errs.Config, localOp, "salt-call local mode needs a host")
	}
	res, err := b.ex.Exec(ctx, host, transport.Command{Script: script, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	if res.RC == 0 || len(res.Stdout) > 0 {
		return res.Stdout, nil
	}
	return nil, localError(host, res)
}

// localError maps a failed salt-call with no stdout onto an error kind.
func localError(host resource.HostID, res transport.ExecResult) *errs.Error {
	stderr := string(res.Stderr)
	var e *errs.Error
	switch {
	case strings.Contains(stderr, "sudo:"):
		e = errs.New(errs.Sudo, localOp, firstLine(stderr))
	case strings.Contains(stderr, "command not found") || res.RC == 127:
		e = errs.New(errs.NotAvailable, localOp, firstLine(stderr))
	case strings.Contains(stderr, "The salt master could not be contacted") ||
		strings.Contains(stderr, "Salt request timed out"):
		e = errs.New(errs.Unreachable, localOp, firstLine(stderr))
	default:
		msg := firstLine(stderr)
		if msg == "" {
			msg = fmt.Sprintf("salt-call exited with code %d", res.RC)
		}
		e = errs.New(errs.Remote, localOp, msg)
	}
	return e.WithHost(string(host))
}

// withLocalHost adds the host to an error that crossed a package boundary.
func withLocalHost(err error, host resource.HostID) error {
	if e, ok := err.(*errs.Error); ok {
		return e.WithHost(string(host))
	}
	return errs.Wrap(errs.Parse, localOp, err).WithHost(string(host))
}
