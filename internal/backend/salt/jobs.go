package salt

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

// Job engine defaults. BUILD-PLAN §8: a Salt job times out after 60 s and the
// master is polled without hammering it.
const (
	// defaultJobPoll is the poll interval once the job is under way.
	defaultJobPoll = 2 * time.Second
	// defaultFirstPoll is the delay before the first poll: the master needs a
	// moment to write a fresh job into its cache.
	defaultFirstPoll = 500 * time.Millisecond
	// defaultJobTimeout bounds a whole job, measured from submit.
	defaultJobTimeout = 60 * time.Second
	// defaultFinishGrace is how long a job may look finished before it really
	// is, so a lagging job cache cannot cut off minions that have not
	// registered yet. It is skipped when the caller supplied Submit.Expect.
	defaultFinishGrace = 3 * time.Second
	// maxPollFailures is how many consecutive failing polls are retried before
	// the job gives up.
	maxPollFailures = 3
	// resultsBuffer is the results channel buffer (BUILD-PLAN §8: 64).
	resultsBuffer = 64
)

// Submit is one async Salt job request. Expect names the minion ids the caller
// believes will match; it may be empty, in which case the expected set comes
// from the master alone.
type Submit struct {
	Targets []Target
	Fun     string
	Args    []string
	Kwargs  map[string]string
	Expect  []string
	Timeout time.Duration
}

// MinionResult is one minion's outcome. NoReturn marks a minion that never
// reported, Unexpected a minion that reported although it was not in Expect.
type MinionResult struct {
	Minion     string
	Return     json.RawMessage
	Retcode    int
	Success    bool
	NoReturn   bool
	Unexpected bool
	JID        string
}

// jobOptions are the tunables of one job. The zero value means "use the
// defaults"; tests inject short intervals instead of waiting seconds.
type jobOptions struct {
	poll    time.Duration
	first   time.Duration
	timeout time.Duration
	grace   time.Duration
	now     func() time.Time
}

// withDefaults fills unset options.
func (o jobOptions) withDefaults() jobOptions {
	if o.poll <= 0 {
		o.poll = defaultJobPoll
	}
	if o.first <= 0 {
		o.first = defaultFirstPoll
	}
	if o.timeout <= 0 {
		o.timeout = defaultJobTimeout
	}
	if o.grace <= 0 {
		o.grace = defaultFinishGrace
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// Job is a submitted Salt job. Results stream one minion at a time and the
// channel is closed by the poll goroutine when the job finishes, times out or
// is canceled. Cancel stops polling only: the job keeps running on the minions
// (killing it is an explicit action).
type Job struct {
	runner *Runner
	opt    jobOptions
	submit Submit

	jids []string

	// SubmitErrors holds one error per target chunk whose submission failed.
	// Chunks that were already submitted keep running and streaming.
	SubmitErrors []error

	results chan MinionResult
	cancel  context.CancelFunc
	done    chan struct{}

	mu       sync.Mutex
	expected []string
	err      error
}

// Start submits s as one async Salt job per target and starts polling. A chunk
// that fails to submit is reported in Job.SubmitErrors and does not cancel the
// chunks already submitted; when every chunk fails, the error is returned.
func (r *Runner) Start(ctx context.Context, s Submit) (*Job, error) {
	return r.start(ctx, s, jobOptions{})
}

// start is Start with injectable intervals and clock.
func (r *Runner) start(ctx context.Context, s Submit, o jobOptions) (*Job, error) {
	o = o.withDefaults()
	if len(s.Targets) == 0 {
		return nil, errs.New(errs.Config, "salt.job", "no targets")
	}
	if s.Fun == "" {
		return nil, errs.New(errs.Config, "salt.job", "no function")
	}
	jctx, cancel := context.WithCancel(ctx)
	j := &Job{
		runner:  r,
		opt:     o,
		submit:  s,
		results: make(chan MinionResult, resultsBuffer),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	j.mergeExpected(s.Expect)
	start := o.now()
	var firstErr error
	seen := map[string]bool{}
	for _, t := range s.Targets {
		target := t
		out, err := r.Run(jctx, Call{
			Tool:   toolSalt,
			Target: &target,
			Fun:    s.Fun,
			Args:   s.Args,
			Kwargs: s.Kwargs,
			Flags:  []string{"--async"},
		})
		if err == nil {
			var jid string
			if jid, err = ParseJID(out); err == nil {
				if !seen[jid] {
					seen[jid] = true
					j.jids = append(j.jids, jid)
				}
				continue
			}
		}
		if firstErr == nil {
			firstErr = err
		}
		j.SubmitErrors = append(j.SubmitErrors, err)
	}
	if len(j.jids) == 0 {
		cancel()
		return nil, firstErr
	}
	go j.poll(jctx, start)
	return j, nil
}

// JIDs returns the job ids the submission produced, in submission order.
func (j *Job) JIDs() []string {
	return append([]string(nil), j.jids...)
}

// Results returns the stream of per-minion results, closed when the job
// finishes.
func (j *Job) Results() <-chan MinionResult { return j.results }

// Done is closed when the poll goroutine has finished and Results is closed.
func (j *Job) Done() <-chan struct{} { return j.done }

// Expected returns the expected minion set: the union of what the caller
// passed in Submit.Expect and the Minions the master has reported so far.
func (j *Job) Expected() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.expected...)
}

// Err returns the error that stopped the job early, if any.
func (j *Job) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// Cancel stops polling and closes the results channel. The Salt job itself
// keeps running on the minions.
func (j *Job) Cancel() { j.cancel() }

// mergeExpected adds minions to the expected set, keeping it sorted.
func (j *Job) mergeExpected(minions []string) {
	if len(minions) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	have := make(map[string]bool, len(j.expected))
	for _, m := range j.expected {
		have[m] = true
	}
	added := false
	for _, m := range minions {
		if m == "" || have[m] {
			continue
		}
		have[m] = true
		j.expected = append(j.expected, m)
		added = true
	}
	if added {
		sort.Strings(j.expected)
	}
}

// setErr records the first error that ended the job.
func (j *Job) setErr(err error) {
	j.mu.Lock()
	if j.err == nil {
		j.err = err
	}
	j.mu.Unlock()
}

// poll runs the poll loop for one job: it emits every minion's result exactly
// once and closes the results channel when the job is done.
func (j *Job) poll(ctx context.Context, start time.Time) {
	defer close(j.done)
	defer close(j.results)

	emitted := make(map[string]bool)  // jid + "\x00" + minion
	reported := make(map[string]bool) // minion that produced a result
	minionJID := make(map[string]string)
	failures := 0
	delay := j.opt.first

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = j.opt.poll

		if j.opt.now().Sub(start) >= j.opt.timeout {
			j.emitMissing(ctx, reported, minionJID)
			return
		}

		var pollErr error
		for _, jid := range j.jids {
			info, err := j.runner.Lookup(ctx, jid)
			if err != nil {
				pollErr = err
				continue
			}
			j.mergeExpected(info.Minions)
			for _, m := range info.Minions {
				if _, ok := minionJID[m]; !ok {
					minionJID[m] = jid
				}
			}
			for minion, mr := range info.Result {
				key := jid + "\x00" + minion
				if emitted[key] {
					continue
				}
				if !j.emit(ctx, j.result(jid, minion, mr)) {
					return
				}
				emitted[key] = true
				reported[minion] = true
				if _, ok := minionJID[minion]; !ok {
					minionJID[minion] = jid
				}
			}
		}

		if pollErr != nil {
			failures++
			if failures >= maxPollFailures {
				j.setErr(pollErr)
				j.emitMissing(ctx, reported, minionJID)
				return
			}
		} else {
			failures = 0
		}

		exp := j.Expected()
		if len(exp) == 0 || !allReported(exp, reported) {
			continue
		}
		// Everyone answered. Finish now when the caller supplied the expected
		// set, otherwise only after the grace period, so a lagging job cache
		// cannot cut the job short.
		if len(j.submit.Expect) > 0 || j.opt.now().Sub(start) >= j.opt.grace {
			return
		}
	}
}

// result builds the MinionResult for one (jid, minion) pair.
func (j *Job) result(jid, minion string, mr MinionReturn) MinionResult {
	return MinionResult{
		Minion:     minion,
		Return:     mr.Return,
		Retcode:    mr.Retcode,
		Success:    mr.Success,
		NoReturn:   NoReturn(mr.Return),
		Unexpected: len(j.submit.Expect) > 0 && !contains(j.submit.Expect, minion),
		JID:        jid,
	}
}

// emitMissing reports NoReturn for every expected minion that never answered.
func (j *Job) emitMissing(ctx context.Context, reported map[string]bool, minionJID map[string]string) {
	j.mu.Lock()
	exp := append([]string(nil), j.expected...)
	j.mu.Unlock()
	if len(exp) == 0 {
		return
	}
	fallback := ""
	if len(j.jids) > 0 {
		fallback = j.jids[0]
	}
	for _, m := range exp {
		if reported[m] {
			continue
		}
		jid := minionJID[m]
		if jid == "" {
			jid = fallback
		}
		reported[m] = true
		if !j.emit(ctx, MinionResult{Minion: m, NoReturn: true, JID: jid,
			Unexpected: len(j.submit.Expect) > 0 && !contains(j.submit.Expect, m)}) {
			return
		}
	}
}

// emit sends one result, giving up when the job is canceled.
func (j *Job) emit(ctx context.Context, m MinionResult) bool {
	select {
	case j.results <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

// allReported reports whether every minion in exp has produced a result.
func allReported(exp []string, reported map[string]bool) bool {
	for _, m := range exp {
		if !reported[m] {
			return false
		}
	}
	return true
}

// contains reports whether list holds v.
func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// Lookup returns one job as the master recorded it. An unknown job id is
// errs.NotAvailable.
func (r *Runner) Lookup(ctx context.Context, jid string) (JobInfo, error) {
	if !validJID(jid) {
		return JobInfo{}, errs.New(errs.Config, opListJob, fmt.Sprintf("invalid job id %q", jid))
	}
	out, err := r.Run(ctx, Call{Tool: toolRun, Fun: "jobs.list_job", Args: []string{jid}})
	if err != nil {
		return JobInfo{}, err
	}
	info, err := parseListJob(out)
	if err != nil {
		return JobInfo{}, err
	}
	if info.JID == "" {
		info.JID = jid
	}
	return info, nil
}

// Active returns every job still running on the master, keyed by job id.
func (r *Runner) Active(ctx context.Context) (map[string]ActiveJob, error) {
	out, err := r.Run(ctx, Call{Tool: toolRun, Fun: "jobs.active"})
	if err != nil {
		return nil, err
	}
	return parseActive(out)
}

// ListJobs returns the jobs whose start time falls inside w, newest first.
// A zero window asks the master for everything it still has.
func (r *Runner) ListJobs(ctx context.Context, w resource.TimeWindow, now time.Time) ([]JobSummary, error) {
	call := Call{Tool: toolRun, Fun: "jobs.list_jobs"}
	if !w.IsZero() {
		from, to := w.Bounds(now)
		call.Kwargs = make(map[string]string, 2)
		if !from.IsZero() {
			call.Kwargs["start_time"] = from.Format(listJobsTimeLayout)
		}
		if !to.IsZero() {
			call.Kwargs["end_time"] = to.Format(listJobsTimeLayout)
		}
	}
	out, err := r.Run(ctx, call)
	if err != nil {
		return nil, err
	}
	return parseListJobs(out)
}

// listJobsTimeLayout is the format jobs.list_jobs takes its window in.
const listJobsTimeLayout = "2006-01-02 15:04:05"
