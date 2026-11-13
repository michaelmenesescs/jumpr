// Package fanout runs one command on many hosts with bounded concurrency.
package fanout

import (
	"context"
	"sync"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

// defaultConcurrency is used when Options.Concurrency is zero (BUILD-PLAN §8: global fan-out 64).
const defaultConcurrency = 64

// resultBuffer is the buffer between workers and the consumer (BUILD-PLAN §8: buffered 64).
const resultBuffer = 64

// Options tunes a run.
type Options struct {
	Concurrency int // max hosts in flight; 0 = 64
}

// HostResult is an alias so callers can keep writing fanout.HostResult.
type HostResult = transport.HostResult

// Run executes cmd on every host with bounded concurrency. Results arrive in completion order.
// The channel is closed when all hosts are done or ctx is canceled; every host yields exactly one HostResult
// (hosts not started before cancellation yield Err with Kind Canceled). Duplicate hosts run once.
// If ex implements transport.BatchExecutor, Run delegates to it. Callers must drain the channel.
//
// The consumer contract is absolute: Run always closes the channel after exactly one result per
// (deduplicated) host, and workers therefore send unconditionally. Callers must drain until it is closed;
// stopping early leaks the goroutines feeding the channel.
func Run(ctx context.Context, ex transport.Executor, hosts []resource.HostID, cmd transport.Command, opt Options) <-chan HostResult {
	uniq := dedup(hosts)
	if len(uniq) == 0 {
		out := make(chan HostResult)
		close(out)
		return out
	}
	if batch, ok := ex.(transport.BatchExecutor); ok {
		// The batch executor owns the channel and closes it after one result per host.
		return batch.ExecBatch(ctx, uniq, cmd)
	}

	conc := opt.Concurrency
	if conc <= 0 {
		conc = defaultConcurrency
	}

	out := make(chan HostResult, resultBuffer)
	go func() {
		defer close(out)
		sem := make(chan struct{}, conc)
		var wg sync.WaitGroup

	feed:
		for i, host := range uniq {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				// Nothing more may start: report the rest as canceled without calling the executor.
				for _, rest := range uniq[i:] {
					out <- canceled(rest)
				}
				break feed
			}
			if ctx.Err() != nil {
				<-sem
				for _, rest := range uniq[i:] {
					out <- canceled(rest)
				}
				break feed
			}
			wg.Add(1)
			go func(h resource.HostID) {
				defer wg.Done()
				defer func() { <-sem }()
				out <- runOne(ctx, ex, h, cmd)
			}(host)
		}
		wg.Wait()
	}()
	return out
}

// Collect drains ch into a slice. It returns when ch is closed.
func Collect(ch <-chan HostResult) []HostResult {
	var res []HostResult
	for r := range ch {
		res = append(res, r)
	}
	return res
}

func dedup(hosts []resource.HostID) []resource.HostID {
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

func canceled(host resource.HostID) HostResult {
	return HostResult{
		Host:   host,
		Result: transport.ExecResult{Host: host, RC: -1},
		Err:    errs.New(errs.Canceled, "fanout", "canceled").WithHost(string(host)),
	}
}

// runOne calls ex.Exec once and turns a panic into an internal error for that host.
func runOne(ctx context.Context, ex transport.Executor, host resource.HostID, cmd transport.Command) (res HostResult) {
	res.Host = host
	defer func() {
		if r := recover(); r != nil {
			res = HostResult{
				Host:   host,
				Result: transport.ExecResult{Host: host, RC: -1},
				Err:    errs.New(errs.Internal, "fanout", "executor panicked").WithHost(string(host)),
			}
		}
	}()
	r, err := ex.Exec(ctx, host, cmd)
	res.Result = r
	res.Err = err
	return res
}
