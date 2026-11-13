package safe

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
)

// newTestLogger returns a debug logger writing JSON to buf.
func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestPanicInGoroutineIsRecovered is requirement 1: a panic at the top of a
// goroutine started with safe.Go is recovered, logged with its stack and
// reported to the hook; the process keeps running.
func TestPanicInGoroutineIsRecovered(t *testing.T) {
	log, buf := newTestLogger()
	var (
		mu    sync.Mutex
		got   []Panic
		after = make(chan struct{}, 1)
	)
	GoHook(log, "test worker", func(p Panic) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		after <- struct{}{}
	}, func() {
		panic("boom")
	})

	select {
	case <-after:
	case <-time.After(5 * time.Second):
		t.Fatal("the panic was not reported")
	}

	mu.Lock()
	panics := append([]Panic(nil), got...)
	mu.Unlock()
	if len(panics) != 1 {
		t.Fatalf("reported %d panics, want 1", len(panics))
	}
	p := panics[0]
	if p.Name != "test worker" {
		t.Errorf("panic name = %q, want %q", p.Name, "test worker")
	}
	if p.Value != "boom" {
		t.Errorf("panic value = %v, want boom", p.Value)
	}
	if !strings.Contains(string(p.Stack), "panic") && len(p.Stack) == 0 {
		t.Errorf("the panic has no stack: %q", p.Stack)
	}

	text := buf.String()
	for _, want := range []string{"panic recovered", "test worker", "boom", "goroutine"} {
		if !strings.Contains(text, want) {
			t.Errorf("log line is missing %q:\n%s", want, text)
		}
	}

	// The goroutine that panicked did not take anything with it: this test is
	// still running, and so is the rest of the program.
	time.Sleep(10 * time.Millisecond)
}

// TestPanicDoReturnsInternal is the "converted to an errs.Internal result where
// one is expected" half: Do hands the panic back as the error a caller returns.
func TestPanicDoReturnsInternal(t *testing.T) {
	log, _ := newTestLogger()

	err := Do(log, "pane reader", func() { panic("index out of range") })
	if err == nil {
		t.Fatal("Do returned nil after a panic")
	}
	if got := errs.KindOf(err); got != errs.Internal {
		t.Fatalf("panic error kind = %q, want %q (%v)", got, errs.Internal, err)
	}
	if !strings.Contains(err.Msg, "index out of range") {
		t.Errorf("panic error = %q, want the panic value in it", err.Msg)
	}
	if err.Op != "safe.pane reader" {
		t.Errorf("panic error op = %q, want %q", err.Op, "safe.pane reader")
	}

	if err := Do(log, "quiet", func() {}); err != nil {
		t.Fatalf("Do on a function that returned normally = %v, want nil", err)
	}
}

// TestPanicDoHookReports is Do with a reporter: the panic is logged, reported
// and returned, in that order, and only once.
func TestPanicDoHookReports(t *testing.T) {
	log, buf := newTestLogger()
	var seen []Panic
	err := DoHook(log, "bus step", func(p Panic) { seen = append(seen, p) }, func() {
		panic("step exploded")
	})
	if err == nil || errs.KindOf(err) != errs.Internal {
		t.Fatalf("err = %v, want an internal error", err)
	}
	if len(seen) != 1 || seen[0].Value != "step exploded" {
		t.Fatalf("reported panics = %+v, want one with the panic value", seen)
	}
	if !strings.Contains(buf.String(), "step exploded") {
		t.Errorf("the panic was not logged: %s", buf.String())
	}

	// A nil logger costs the log line, not the recovery.
	if err := Do(nil, "no logger", func() { panic("silent") }); err == nil {
		t.Fatal("Do with no logger swallowed the panic")
	}
}
