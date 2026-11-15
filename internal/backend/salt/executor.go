package salt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
)

// Executor defaults. BUILD-PLAN §4.3 defines the Command contract; §8.6 the
// trade-off between SSH fan-out and the master.
const (
	// defaultExecTimeout bounds one command when neither the Command nor the
	// ExecOptions set a timeout (BUILD-PLAN §8: every remote call has one).
	defaultExecTimeout = 60 * time.Second
	// defaultBatchWindow is how long the dispatcher collects requests before
	// submitting them as one job.
	defaultBatchWindow = 100 * time.Millisecond
	// defaultBatchMax caps the number of requests in one micro-batch.
	defaultBatchMax = 200
	// defaultChunk is how many minion IDs go into one -L target.
	defaultChunk = 200
	// maxScriptBase64 is the largest base64 payload a single Salt command line
	// may carry (96 KiB of base64, i.e. about 72 KiB of script).
	maxScriptBase64 = 96 << 10
	// defaultExecMaxOutput is the per-stream cap from the Command contract.
	defaultExecMaxOutput = 2 << 20
	// batchSlack is how far the batch context outlives the command's own
	// timeout, so the job engine can report the minions that never answered.
	batchSlack = 5 * time.Second
	// execOp is the Op used by executor errors; runner.go already uses the
	// same op for Salt CLI failures.
	execOp = "salt.exec"
)

// lcAllPrefix is put in front of every script so the output does not depend on
// the minion's locale.
const lcAllPrefix = "export LC_ALL=C LANG=C; "

// ExecOptions tunes an Executor. Zero values mean the defaults above.
type ExecOptions struct {
	Chunk       int           // minion IDs per -L target; 0 = 200
	Timeout     time.Duration // command timeout; 0 = 60 s
	BatchWindow time.Duration // micro-batch collection window; 0 = 100 ms
	BatchMax    int           // micro-batch size; 0 = 200
}

// Executor implements transport.Executor and transport.BatchExecutor by
// running cmd.run_all on the minions through the master's job engine.
//
// The script never touches a layer of quoting: it is base64-encoded into the
// `cmd` kwarg, so quotes, substitutions, newlines and `a=b` words arrive on the
// minion exactly as written. Salt runs as root, so Command.Sudo is ignored and
// Command.Stdin is refused.
type Executor struct {
	runner  *Runner
	backend *Backend
	opt     ExecOptions
	master  resource.HostID

	// jobOpts are the job engine's tunables; tests shorten them.
	jobOpts jobOptions

	queue  chan *execRequest
	closed chan struct{}
	once   sync.Once
}

var (
	_ transport.Executor      = (*Executor)(nil)
	_ transport.BatchExecutor = (*Executor)(nil)
)

// execRequest is one queued Exec call waiting for its micro-batch.
type execRequest struct {
	host resource.HostID
	cmd  transport.Command
	ctx  context.Context
	ch   chan transport.HostResult // buffered (1): the dispatcher never blocks
}

// NewExecutor returns an Executor that reaches the minions through r and maps
// inventory hosts to minion IDs with b. It starts the micro-batch dispatcher;
// Close stops it.
func NewExecutor(r *Runner, b *Backend, o ExecOptions) *Executor {
	e := &Executor{
		runner:  r,
		backend: b,
		opt:     o,
		queue:   make(chan *execRequest),
		closed:  make(chan struct{}),
	}
	if b != nil {
		e.master = b.master
	}
	if e.master == "" && r != nil {
		e.master = r.master
	}
	go e.dispatch()
	return e
}

// Name returns the transport name shown in ExecResult.Via and the plan.
func (e *Executor) Name() string { return backendName }

// Close stops the dispatcher. Callers already queued are flushed with one last
// job; Exec after Close fails.
func (e *Executor) Close() { e.once.Do(func() { close(e.closed) }) }

// Exec runs cmd on one host. Requests that arrive within the batch window and
// carry the same script and timeout share one Salt job.
func (e *Executor) Exec(ctx context.Context, host resource.HostID, cmd transport.Command) (transport.ExecResult, error) {
	res := transport.ExecResult{Host: host, RC: -1, Via: e.via()}
	if err := e.validate(cmd); err != nil {
		return res, err.WithHost(string(host))
	}
	if _, ok := e.minion(host); !ok {
		return res, e.notMinion(host)
	}
	req := &execRequest{host: host, cmd: cmd, ctx: ctx, ch: make(chan transport.HostResult, 1)}
	select {
	case e.queue <- req:
	case <-ctx.Done():
		return res, canceled(host)
	case <-e.closed:
		return res, errs.New(errs.Internal, execOp, "salt executor is closed").WithHost(string(host))
	}
	select {
	case r := <-req.ch:
		return r.Result, r.Err
	case <-ctx.Done():
		return res, canceled(host)
	}
}

// ExecBatch runs cmd on every host as one Salt job per chunk of minion IDs.
// Every host yields exactly one result and the channel is closed.
func (e *Executor) ExecBatch(ctx context.Context, hosts []resource.HostID, cmd transport.Command) <-chan transport.HostResult {
	out := make(chan transport.HostResult, resultsBuffer)
	go func() {
		defer close(out)
		e.runBatch(ctx, out, dedupHosts(hosts), cmd)
	}()
	return out
}

// runBatch is the shared body of ExecBatch: resolve hosts, submit once, stream
// the per-minion returns back as per-host results.
func (e *Executor) runBatch(ctx context.Context, out chan<- transport.HostResult, hosts []resource.HostID, cmd transport.Command) {
	res := func(h resource.HostID) transport.ExecResult {
		return transport.ExecResult{Host: h, RC: -1, Via: e.via()}
	}
	if len(hosts) == 0 {
		return
	}
	if err := e.validate(cmd); err != nil {
		for _, h := range hosts {
			send(ctx, out, transport.HostResult{Host: h, Result: res(h), Err: err.WithHost(string(h))})
		}
		return
	}

	start := time.Now()
	minionHosts := make(map[string][]resource.HostID, len(hosts))
	skip := make(map[resource.HostID]bool, len(hosts))
	for _, h := range hosts {
		id, ok := e.minion(h)
		if !ok {
			skip[h] = true
			send(ctx, out, transport.HostResult{Host: h, Result: res(h), Err: e.notMinion(h)})
			continue
		}
		minionHosts[id] = append(minionHosts[id], h)
	}
	if len(minionHosts) == 0 {
		return
	}

	ids := make([]string, 0, len(minionHosts))
	for id := range minionHosts {
		ids = append(ids, id)
	}
	timeout := e.timeoutFor(cmd)
	call, cerr := e.call(cmd.Script, timeout)
	if cerr != nil {
		for _, h := range hosts {
			if skip[h] {
				continue
			}
			send(ctx, out, transport.HostResult{Host: h, Result: res(h), Err: cerr.WithHost(string(h))})
		}
		return
	}

	submit := Submit{
		Targets: CompileHosts(ids, e.chunk()),
		Fun:     call.Fun,
		Kwargs:  call.Kwargs,
		Expect:  ids,
		Timeout: timeout,
	}
	job, err := e.runner.start(ctx, submit, e.jobOpts)
	if err != nil {
		for _, hs := range minionHosts {
			for _, h := range hs {
				send(ctx, out, transport.HostResult{Host: h, Result: res(h), Err: withHost(err, h)})
			}
		}
		return
	}

	reported := make(map[resource.HostID]bool, len(hosts))
	for mr := range job.Results() {
		hs, ok := minionHosts[mr.Minion]
		if !ok {
			continue // a minion nobody asked for (Unexpected)
		}
		for _, h := range hs {
			result, err := e.mapResult(h, mr, cmd, time.Since(start))
			reported[h] = true
			send(ctx, out, transport.HostResult{Host: h, Result: result, Err: err})
		}
	}

	for _, h := range hosts {
		if reported[h] || skip[h] {
			continue
		}
		send(ctx, out, transport.HostResult{Host: h, Result: res(h), Err: e.missing(job, ctx, h)})
	}
}

// mapResult turns one minion's cmd.run_all return into an ExecResult.
func (e *Executor) mapResult(host resource.HostID, mr MinionResult, cmd transport.Command, dur time.Duration) (transport.ExecResult, error) {
	res := transport.ExecResult{Host: host, RC: -1, Duration: dur, Via: e.via()}
	if mr.NoReturn {
		return res, errs.New(errs.Timeout, execOp, "no return from minion").WithHost(string(host))
	}
	var text string
	if err := json.Unmarshal(mr.Return, &text); err == nil {
		// Salt answered with a string: the function is not available, the
		// minion refused the command, and so on.
		return res, errs.New(errs.Remote, execOp, firstLine(text)).WithHost(string(host))
	}
	var out struct {
		Pid     int    `json:"pid"`
		Retcode int    `json:"retcode"`
		Stdout  string `json:"stdout"`
		Stderr  string `json:"stderr"`
	}
	if err := json.Unmarshal(mr.Return, &out); err != nil {
		return res, errs.New(errs.Remote, execOp, snippet(mr.Return)).WithHost(string(host))
	}
	max := cmd.MaxOutput
	if max <= 0 {
		max = defaultExecMaxOutput
	}
	res.RC = out.Retcode
	res.Stdout, res.Truncated = capBytes([]byte(out.Stdout), max)
	var cut bool
	res.Stderr, cut = capBytes([]byte(out.Stderr), max)
	res.Truncated = res.Truncated || cut
	return res, nil
}

// missing explains a host that produced no result at all.
func (e *Executor) missing(job *Job, ctx context.Context, host resource.HostID) error {
	if err := job.Err(); err != nil {
		return withHost(err, host)
	}
	if ctx.Err() != nil {
		return canceled(host)
	}
	return errs.New(errs.Timeout, execOp, "no return from minion").WithHost(string(host))
}

// call builds the Salt call that carries script. The command line is always
// cmd.run_all with the base64 script in the cmd kwarg; shq quotes the kwarg
// word, so no layer between jumpr and the minion can split it.
func (e *Executor) call(script string, timeout time.Duration) (Call, *errs.Error) {
	payload := base64.StdEncoding.EncodeToString([]byte(lcAllPrefix + script))
	if len(payload) > maxScriptBase64 {
		return Call{}, errs.New(errs.Config, execOp, "script too large for the Salt transport")
	}
	return Call{
		Tool: toolSalt,
		Fun:  "cmd.run_all",
		Kwargs: map[string]string{
			"cmd":            "echo " + payload + " | base64 -d | sh",
			"python_shell":   "True",
			"ignore_retcode": "True",
			"timeout":        strconv.Itoa(seconds(timeout)),
		},
	}, nil
}

// validate rejects what the Salt transport cannot express.
func (e *Executor) validate(cmd transport.Command) *errs.Error {
	if len(cmd.Stdin) > 0 {
		return errs.New(errs.Config, execOp, "stdin is not supported via the Salt transport")
	}
	if _, err := e.call(cmd.Script, e.timeoutFor(cmd)); err != nil {
		return err
	}
	return nil
}

// minion maps a host onto its Salt minion ID.
func (e *Executor) minion(host resource.HostID) (string, bool) {
	if e.backend == nil {
		return "", false
	}
	return e.backend.HostMinion(host)
}

// notMinion is the error for a host Salt does not manage. It is returned
// without submitting anything.
func (e *Executor) notMinion(host resource.HostID) error {
	return errs.New(errs.Unreachable, execOp, "not a salt minion").WithHost(string(host))
}

// via is the ExecResult.Via value for this executor.
func (e *Executor) via() string {
	if e.master == "" {
		return backendName
	}
	return backendName + ":" + string(e.master)
}

// dispatch collects Exec requests into micro-batches and runs each group as
// one job. It never blocks on a caller: every request channel is buffered.
func (e *Executor) dispatch() {
	for {
		var first *execRequest
		select {
		case first = <-e.queue:
		case <-e.closed:
			return
		}
		batch := []*execRequest{first}
		timer := time.NewTimer(e.window())
		closing := false
	collect:
		for len(batch) < e.batchMax() {
			select {
			case req := <-e.queue:
				batch = append(batch, req)
			case <-timer.C:
				break collect
			case <-e.closed:
				closing = true
				break collect
			}
		}
		timer.Stop()
		e.runGroups(batch)
		if closing {
			return
		}
	}
}

// runGroups splits one micro-batch by (script, timeout) and runs each group as
// a single job, so a batch never mixes commands.
func (e *Executor) runGroups(batch []*execRequest) {
	groups := make(map[string][]*execRequest, 1)
	order := make([]string, 0, 1)
	for _, req := range batch {
		key := req.cmd.Script + "\x00" + req.cmd.Timeout.String()
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], req)
	}
	for _, key := range order {
		e.runGroup(groups[key])
	}
}

// runGroup runs one micro-batch group through the same path as ExecBatch.
func (e *Executor) runGroup(reqs []*execRequest) {
	byHost := make(map[resource.HostID]*execRequest, len(reqs))
	hosts := make([]resource.HostID, 0, len(reqs))
	for _, req := range reqs {
		if _, ok := byHost[req.host]; ok {
			continue
		}
		byHost[req.host] = req
		hosts = append(hosts, req.host)
	}
	cmd := reqs[0].cmd
	ctx, cancel := context.WithTimeout(context.Background(), e.timeoutFor(cmd)+batchSlack)
	defer cancel()
	for res := range e.ExecBatch(ctx, hosts, cmd) {
		if req := byHost[res.Host]; req != nil {
			deliver(req, res)
		}
	}
}

// deliver hands one result to its caller, dropping it when the caller is gone.
func deliver(req *execRequest, res transport.HostResult) {
	select {
	case <-req.ctx.Done():
	case req.ch <- res:
	}
}

// send hands one batch result on, giving up when the batch is canceled.
func send(ctx context.Context, out chan<- transport.HostResult, res transport.HostResult) {
	select {
	case out <- res:
	case <-ctx.Done():
	}
}

// canceled is the error a caller sees when its context ends.
func canceled(host resource.HostID) error {
	return errs.New(errs.Canceled, execOp, "canceled").WithHost(string(host))
}

// withHost adds host to a *errs.Error, leaving other error types alone.
func withHost(err error, host resource.HostID) error {
	if e, ok := err.(*errs.Error); ok {
		return e.WithHost(string(host))
	}
	return errs.Wrap(errs.Internal, execOp, err).WithHost(string(host))
}

// seconds rounds d up to whole seconds, the unit Salt's timeout kwarg takes.
func seconds(d time.Duration) int {
	if d <= 0 {
		return int(defaultExecTimeout.Seconds())
	}
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		return 1
	}
	return s
}

// capBytes cuts b to max bytes and reports whether it had to.
func capBytes(b []byte, max int) ([]byte, bool) {
	if max > 0 && len(b) > max {
		return b[:max], true
	}
	return b, false
}

// dedupHosts removes duplicate host IDs, keeping the first occurrence.
func dedupHosts(hosts []resource.HostID) []resource.HostID {
	if len(hosts) < 2 {
		return hosts
	}
	seen := make(map[resource.HostID]struct{}, len(hosts))
	out := make([]resource.HostID, 0, len(hosts))
	for _, h := range hosts {
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}

func (e *Executor) chunk() int {
	if e.opt.Chunk > 0 {
		return e.opt.Chunk
	}
	return defaultChunk
}

func (e *Executor) window() time.Duration {
	if e.opt.BatchWindow > 0 {
		return e.opt.BatchWindow
	}
	return defaultBatchWindow
}

func (e *Executor) batchMax() int {
	if e.opt.BatchMax > 0 {
		return e.opt.BatchMax
	}
	return defaultBatchMax
}

// timeoutFor resolves the timeout a command runs under.
func (e *Executor) timeoutFor(cmd transport.Command) time.Duration {
	if cmd.Timeout > 0 {
		return cmd.Timeout
	}
	if e.opt.Timeout > 0 {
		return e.opt.Timeout
	}
	return defaultExecTimeout
}

// mustMinion and hostKnown support the missing-result pass; both are cheap
// wrappers kept local to the executor.
