package bus

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"jumper/internal/action"
	"jumper/internal/errs"
)

// run is the live rollout behind the frozen Run interface. It is the only
// place in the repository that calls action.Step.Run.
type run struct {
	bus      *bus
	proposal Proposal
	ctx      context.Context
	events   chan Event

	contCh    chan struct{} // Continue, buffered 1
	abortCh   chan struct{} // closed once by Abort
	abortOnce sync.Once
	aborted   atomic.Bool

	// emitMu serialises sends and the coalescing drop, so the run can emit
	// from several batch goroutines without blocking on a slow consumer.
	emitMu sync.Mutex

	mu      sync.Mutex
	results []action.StepResult
	done    bool
}

// newRun builds an unfinished run for p on the bus's root context.
func newRun(b *bus, p Proposal) *run {
	return &run{
		bus:      b,
		proposal: p,
		ctx:      b.ctx,
		events:   make(chan Event, eventBuffer),
		contCh:   make(chan struct{}, 1),
		abortCh:  make(chan struct{}),
		results:  make([]action.StepResult, len(p.Plan.Steps)),
	}
}

// Events returns the event stream, closed after the final event.
func (r *run) Events() <-chan Event { return r.events }

// Continue leaves PhasePaused. Calls at any other phase, and repeated calls,
// are harmless: the leave is dropped when nobody is waiting for it.
func (r *run) Continue() {
	select {
	case r.contCh <- struct{}{}:
	default:
	}
}

// Abort stops the rollout before the next batch. Steps already in flight
// finish. Repeated calls are harmless.
func (r *run) Abort() {
	r.aborted.Store(true)
	r.abortOnce.Do(func() { close(r.abortCh) })
}

// Proposal returns the proposal being executed.
func (r *run) Proposal() Proposal { return r.proposal }

// Results returns a snapshot of the results so far, with output redacted and
// capped the same way the recorder sees it.
func (r *run) Results() []action.StepResult {
	return r.bus.redacted(r.rawResults())
}

// finished reports whether the rollout reached its final event.
func (r *run) finished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// loop runs the rollout: canary, pause, batches, summary.
func (r *run) loop() {
	defer close(r.events)

	start := r.bus.now()
	steps := r.proposal.Plan.Steps
	canary, batches := Batches(steps, r.proposal.Rollout)
	total := len(steps)

	var okN, failN, skipN int
	idx := 0

	if len(canary) > 0 {
		r.emit(Event{Phase: PhaseCanary})
	}
	for i := range canary {
		res := r.exec(canary[i])
		r.store(i, res)
		r.emit(Event{Phase: PhaseCanary, Result: &res})
		if stepFailed(res) {
			failN++
		} else {
			okN++
		}
	}
	idx = len(canary)

	if failN > 0 {
		skipN = total - idx
		r.finish(PhaseStopped, Summary{OK: okN, Failed: failN, Skipped: skipN}, start)
		return
	}

	// A canary that passed hands over to the batches; the rollout stays
	// paused until Continue or Abort, for as long as it takes.
	if len(canary) > 0 && len(batches) > 0 && !r.stopping() {
		r.emit(Event{Phase: PhasePaused})
		select {
		case <-r.contCh:
		case <-r.abortCh:
		case <-r.ctx.Done():
		}
	}

	if r.stopping() && idx < total {
		skipN = total - idx
		r.finish(PhaseAborted, Summary{OK: okN, Failed: failN, Skipped: skipN}, start)
		return
	}

	afterTotal := total - len(canary)
	for bi, batch := range batches {
		if r.stopping() {
			skipN = total - idx
			r.finish(PhaseAborted, Summary{OK: okN, Failed: failN, Skipped: skipN}, start)
			return
		}
		r.emit(Event{Phase: PhaseBatch, Batch: bi + 1})
		o, f := r.runBatch(idx, batch, bi+1)
		okN += o
		failN += f
		idx += len(batch)
		if ShouldStop(failN, idx-len(canary), afterTotal, r.proposal.Rollout.FailureThreshold) {
			skipN = total - idx
			r.finish(PhaseStopped, Summary{OK: okN, Failed: failN, Skipped: skipN}, start)
			return
		}
	}

	r.finish(PhaseDone, Summary{OK: okN, Failed: failN}, start)
}

// runBatch runs one batch with bounded concurrency, then counts its results.
func (r *run) runBatch(base int, batch []action.Step, batchNo int) (okN, failN int) {
	sem := make(chan struct{}, r.bus.concurrency)
	var wg sync.WaitGroup
	for i := range batch {
		i := i
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res := r.exec(batch[i])
			r.store(base+i, res)
			r.emit(Event{Phase: PhaseBatch, Batch: batchNo, Result: &res})
		}()
	}
	wg.Wait()
	for i := range batch {
		if stepFailed(r.resultAt(base + i)) {
			failN++
		} else {
			okN++
		}
	}
	return okN, failN
}

// exec runs one step with a per-step timeout and panic recovery.
func (r *run) exec(st action.Step) action.StepResult {
	start := r.bus.now()
	ctx, cancel := context.WithTimeout(r.ctx, r.bus.stepTimeout)
	defer cancel()

	var res action.StepResult
	func() {
		defer func() {
			if p := recover(); p != nil {
				res = action.StepResult{
					Target: st.Target,
					RC:     -1,
					Err:    errs.New(errs.Internal, "bus.step", fmt.Sprintf("step panicked: %v", p)),
				}
			}
		}()
		if st.Run == nil {
			res = action.StepResult{
				Target: st.Target,
				RC:     -1,
				Err:    errs.New(errs.Internal, "bus.step", "step has no Run"),
			}
			return
		}
		res = st.Run(ctx)
	}()
	if res.Target.Host == "" {
		res.Target = st.Target
	}
	res.Duration = r.bus.now().Sub(start)
	return res
}

// finish emits the final event, records the rollout and marks the run done.
func (r *run) finish(phase Phase, s Summary, start time.Time) {
	s.Duration = r.bus.now().Sub(start)
	r.emit(Event{Phase: phase, Summary: &s})
	r.bus.recordAction(r.proposal, s, r.rawResults())

	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
}

// emit delivers ev without ever blocking on a slow consumer: when the buffer
// is full a result-only event is dropped (the result is still kept for the
// recorder and Results), and a phase event replaces the oldest entry.
func (r *run) emit(ev Event) {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	for {
		select {
		case r.events <- ev:
			return
		default:
		}
		if ev.Result != nil && ev.Summary == nil {
			return
		}
		select {
		case <-r.events: // drop the oldest to make room
		case <-r.ctx.Done():
			return
		default:
			// The consumer took something; try the send again.
		}
	}
}

// store records a step result at its plan index.
func (r *run) store(i int, res action.StepResult) {
	r.mu.Lock()
	if i >= 0 && i < len(r.results) {
		r.results[i] = res
	}
	r.mu.Unlock()
}

// resultAt returns the stored result at i.
func (r *run) resultAt(i int) action.StepResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.results) {
		return action.StepResult{RC: -1}
	}
	return r.results[i]
}

// rawResults copies the results recorded so far.
func (r *run) rawResults() []action.StepResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]action.StepResult, len(r.results))
	copy(out, r.results)
	return out
}

// stopping reports whether Abort was called or the bus is shutting down.
func (r *run) stopping() bool {
	return r.aborted.Load() || r.ctx.Err() != nil
}

// stepFailed is the definition of a failed step.
func stepFailed(res action.StepResult) bool {
	return res.Err != nil || res.RC != 0
}
