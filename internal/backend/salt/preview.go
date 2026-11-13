package salt

import (
	"context"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

const (
	// previewOp is the error Op of PreviewTarget.
	previewOp = "salt.preview-target"
	// pingOp is the error Op of TestPing.
	pingOp = "salt.ping"
	// previewTimeout bounds one `--preview-target` call.
	previewTimeout = 30 * time.Second
	// pingTimeout bounds one test.ping job submission.
	pingTimeout = time.Minute
)

// PreviewTarget resolves a native target to the minion IDs Salt would match. It
// runs `salt … --preview-target test.ping` on the master, which resolves the
// target without executing anything on the minions; the function argument is
// always passed because some Salt versions need one.
func (b *Backend) PreviewTarget(ctx context.Context, t Target) ([]string, error) {
	if b.runner == nil {
		return nil, errs.New(errs.Config, previewOp, "salt backend not configured: no runner")
	}
	if t.Type == "" || t.Expr == "" {
		return nil, errs.New(errs.Config, previewOp, "empty target")
	}
	cctx, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()

	target := t
	out, err := b.runner.Run(cctx, Call{
		Tool:    toolSalt,
		Target:  &target,
		Fun:     "test.ping",
		Flags:   []string{"--preview-target"},
		Timeout: previewTimeout,
	})
	if err != nil {
		return nil, err
	}
	return ParsePreview(out)
}

// CompileSet is the targeting plan for a resolved target set: the compiled
// targets the write would submit and the hosts Salt does not manage. It is the
// `:salt target` answer. HostMinion may run salt-key once to learn the accepted
// keys, so callers run this off the UI goroutine.
func (b *Backend) CompileSet(hosts []resource.HostID, sel resource.Selector) (Compiled, []resource.HostID) {
	minions := make([]string, 0, len(hosts))
	var unmapped []resource.HostID
	for _, h := range hosts {
		id, ok := b.HostMinion(h)
		if !ok {
			unmapped = append(unmapped, h)
			continue
		}
		minions = append(minions, id)
	}
	return Compile(sel, minions, b.grainKeys(), defaultChunk), unmapped
}

// TestPing runs test.ping on minions as one Salt job and returns the first job
// id, which is what the job detail view opens. The job keeps running on the
// minions; polling is the job detail's business.
func (b *Backend) TestPing(ctx context.Context, minions []string) (string, error) {
	if b.runner == nil {
		return "", errs.New(errs.Config, pingOp, "salt backend not configured: no runner")
	}
	ids := sortedMinions(minions)
	if len(ids) == 0 {
		return "", errs.New(errs.Config, pingOp, "no minions to target")
	}
	cctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	job, err := b.runner.Start(cctx, Submit{
		Targets: CompileHosts(ids, defaultChunk),
		Fun:     "test.ping",
		Expect:  ids,
		Timeout: pingTimeout,
	})
	if err != nil {
		return "", err
	}
	defer job.Cancel()
	jids := job.JIDs()
	if len(jids) == 0 {
		return "", errs.New(errs.Remote, pingOp, "the master returned no job id")
	}
	return jids[0], nil
}
