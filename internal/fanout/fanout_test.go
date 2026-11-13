package fanout

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
)

func hid(i int) resource.HostID { return resource.HostID(fmt.Sprintf("h%05d", i)) }

func hosts(n int) []resource.HostID {
	hs := make([]resource.HostID, n)
	for i := range hs {
		hs[i] = hid(i)
	}
	return hs
}

func cmd() transport.Command { return transport.Command{Script: "uptime"} }

// TestAllHostsReportOnce: every host yields exactly one result and the channel closes.
func TestAllHostsReportOnce(t *testing.T) {
	const n = 2000
	fake := transporttest.New().On("uptime", transporttest.Reply{Stdout: "ok\n"})
	got := Collect(Run(context.Background(), fake, hosts(n), cmd(), Options{Concurrency: 64}))

	if len(got) != n {
		t.Fatalf("got %d results, want %d", len(got), n)
	}
	seen := map[resource.HostID]int{}
	for _, r := range got {
		seen[r.Host]++
		if r.Err != nil {
			t.Fatalf("host %s: unexpected error: %v", r.Host, r.Err)
		}
		if string(r.Result.Stdout) != "ok\n" {
			t.Fatalf("host %s: stdout %q", r.Host, r.Result.Stdout)
		}
	}
	for i := 0; i < n; i++ {
		if c := seen[hid(i)]; c != 1 {
			t.Fatalf("host %s reported %d times", hid(i), c)
		}
	}
}

// TestConcurrencyBound: no more than Options.Concurrency execs run at once, and more than one does.
func TestConcurrencyBound(t *testing.T) {
	const conc = 8
	fake := transporttest.New().On("uptime", transporttest.Reply{Stdout: "ok\n", Delay: 5 * time.Millisecond})
	got := Collect(Run(context.Background(), fake, hosts(100), cmd(), Options{Concurrency: conc}))
	if len(got) != 100 {
		t.Fatalf("got %d results, want 100", len(got))
	}
	if max := fake.InFlightMax(); max > conc || max <= 1 {
		t.Fatalf("InFlightMax() = %d, want > 1 and <= %d", max, conc)
	}
}

// TestCancelMidway: after cancellation every host still yields one result and Run returns promptly.
func TestCancelMidway(t *testing.T) {
	const n = 200
	fake := transporttest.New().On("uptime", transporttest.Reply{Stdout: "ok\n", Delay: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	ch := Run(ctx, fake, hosts(n), cmd(), Options{Concurrency: 4})

	start := time.Now()
	var got []HostResult
	// Consume a couple of results, then cancel.
	for i := 0; i < 2; i++ {
		select {
		case r := <-ch:
			got = append(got, r)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for first results")
		}
	}
	cancel()
	got = append(got, Collect(ch)...)
	elapsed := time.Since(start)

	if len(got) != n {
		t.Fatalf("got %d results after cancel, want %d", len(got), n)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("drain after cancel took %v", elapsed)
	}
	seen := map[resource.HostID]int{}
	canceled := 0
	for _, r := range got {
		seen[r.Host]++
		if r.Err != nil && errs.KindOf(r.Err) == errs.Canceled {
			canceled++
		}
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct hosts, want %d", len(seen), n)
	}
	for h, c := range seen {
		if c != 1 {
			t.Fatalf("host %s reported %d times", h, c)
		}
	}
	if canceled == 0 {
		t.Fatal("expected at least one canceled result")
	}
}

// TestErrorsDoNotStopOthers: one host failing is reported, the rest still run.
func TestErrorsDoNotStopOthers(t *testing.T) {
	bad := hid(3)
	fake := transporttest.New().OnHost(bad, "uptime", transporttest.Reply{
		Err: errs.New(errs.Unreachable, "fake.exec", "no route").WithHost(string(bad)),
	})
	fake.On("uptime", transporttest.Reply{Stdout: "ok\n"})

	got := Collect(Run(context.Background(), fake, hosts(10), cmd(), Options{Concurrency: 4}))
	if len(got) != 10 {
		t.Fatalf("got %d results, want 10", len(got))
	}
	failed, ok := 0, 0
	for _, r := range got {
		if r.Host == bad {
			if errs.KindOf(r.Err) != errs.Unreachable {
				t.Fatalf("host %s: kind %q, want unreachable", r.Host, errs.KindOf(r.Err))
			}
			failed++
			continue
		}
		if r.Err != nil {
			t.Fatalf("host %s: unexpected error: %v", r.Host, r.Err)
		}
		ok++
	}
	if failed != 1 || ok != 9 {
		t.Fatalf("failed=%d ok=%d, want 1 and 9", failed, ok)
	}
}

// TestDedup: duplicate hosts execute once; the first occurrence's position is kept.
func TestDedup(t *testing.T) {
	in := []resource.HostID{"a", "b", "a", "c", "b", "a"}
	fake := transporttest.New().On("uptime", transporttest.Reply{Stdout: "ok\n"})
	got := Collect(Run(context.Background(), fake, in, cmd(), Options{Concurrency: 2}))

	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	if calls := len(fake.Calls()); calls != 3 {
		t.Fatalf("executor called %d times, want 3", calls)
	}
	seen := map[resource.HostID]bool{}
	for _, r := range got {
		if seen[r.Host] {
			t.Fatalf("host %s reported twice", r.Host)
		}
		seen[r.Host] = true
	}
	for _, h := range []resource.HostID{"a", "b", "c"} {
		if !seen[h] {
			t.Fatalf("host %s missing", h)
		}
	}
}

// batchOnly implements transport.Executor and transport.BatchExecutor; it
// records which path fanout.Run takes.
type batchOnly struct {
	mu       sync.Mutex
	execs    int
	batches  int
	gotHosts []resource.HostID
	gotCmd   transport.Command
}

func (b *batchOnly) Name() string { return "batch" }

func (b *batchOnly) Exec(_ context.Context, host resource.HostID, _ transport.Command) (transport.ExecResult, error) {
	b.mu.Lock()
	b.execs++
	b.mu.Unlock()
	return transport.ExecResult{Host: host, Via: "batch"}, nil
}

func (b *batchOnly) ExecBatch(ctx context.Context, hosts []resource.HostID, cmd transport.Command) <-chan transport.HostResult {
	b.mu.Lock()
	b.batches++
	b.gotHosts = append([]resource.HostID(nil), hosts...)
	b.gotCmd = cmd
	b.mu.Unlock()
	out := make(chan transport.HostResult, len(hosts))
	go func() {
		defer close(out)
		for _, h := range hosts {
			out <- transport.HostResult{Host: h, Result: transport.ExecResult{Host: h, Stdout: []byte("ok\n"), Via: "batch"}}
		}
	}()
	return out
}

func (b *batchOnly) counts() (execs, batches int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.execs, b.batches
}

// TestFanoutUsesBatchExecutor: when the executor can run one command on many
// hosts at once, Run delegates to it instead of fanning out per host.
func TestFanoutUsesBatchExecutor(t *testing.T) {
	ex := &batchOnly{}
	in := append(hosts(5), hid(0), hid(4)) // duplicates must still run once

	got := Collect(Run(context.Background(), ex, in, cmd(), Options{Concurrency: 2}))

	execs, batches := ex.counts()
	if execs != 0 {
		t.Errorf("Exec called %d times, want 0", execs)
	}
	if batches != 1 {
		t.Fatalf("ExecBatch called %d times, want 1", batches)
	}
	if len(ex.gotHosts) != 5 {
		t.Errorf("ExecBatch got %d hosts, want the 5 de-duplicated hosts %v", len(ex.gotHosts), ex.gotHosts)
	}
	if ex.gotCmd.Script != "uptime" {
		t.Errorf("ExecBatch got script %q, want uptime", ex.gotCmd.Script)
	}
	if len(got) != 5 {
		t.Fatalf("got %d results, want 5", len(got))
	}
	seen := map[resource.HostID]bool{}
	for _, r := range got {
		if seen[r.Host] {
			t.Fatalf("host %s reported twice", r.Host)
		}
		seen[r.Host] = true
		if r.Err != nil {
			t.Fatalf("host %s: unexpected error: %v", r.Host, r.Err)
		}
	}
}

// TestPanicRecovered: a panicking executor yields an internal error for that host only.
func TestPanicRecovered(t *testing.T) {
	bad := hid(2)
	fake := transporttest.New().OnFunc(func(h resource.HostID, _ transport.Command) (transporttest.Reply, bool) {
		if h == bad {
			panic("boom")
		}
		return transporttest.Reply{Stdout: "ok\n"}, true
	})

	got := Collect(Run(context.Background(), fake, hosts(6), cmd(), Options{Concurrency: 3}))
	if len(got) != 6 {
		t.Fatalf("got %d results, want 6", len(got))
	}
	ok := 0
	for _, r := range got {
		if r.Host == bad {
			if errs.KindOf(r.Err) != errs.Internal {
				t.Fatalf("host %s: kind %q, want internal", r.Host, errs.KindOf(r.Err))
			}
			continue
		}
		if r.Err != nil {
			t.Fatalf("host %s: unexpected error: %v", r.Host, r.Err)
		}
		ok++
	}
	if ok != 5 {
		t.Fatalf("ok=%d, want 5", ok)
	}
}

// TestZeroHostsClosesImmediately: no hosts means a closed, empty channel.
func TestZeroHostsClosesImmediately(t *testing.T) {
	fake := transporttest.New()
	ch := Run(context.Background(), fake, nil, cmd(), Options{})

	select {
	case r, ok := <-ch:
		if ok {
			t.Fatalf("unexpected result %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed for zero hosts")
	}
	if calls := len(fake.Calls()); calls != 0 {
		t.Fatalf("executor called %d times, want 0", calls)
	}
}

// TestCollectEmpty: Collect on a closed empty channel returns nothing.
func TestCollectEmpty(t *testing.T) {
	ch := make(chan HostResult)
	close(ch)
	if got := Collect(ch); len(got) != 0 {
		t.Fatalf("got %d results, want 0", len(got))
	}
}
