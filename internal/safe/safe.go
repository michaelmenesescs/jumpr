// Package safe is how jumpr starts the goroutines it owns: a panic inside one
// of them is recovered at its top, logged with the stack that produced it and
// reported, instead of taking the process (and every session in it) down
// (SPEC §15 "Crash safety", task 126 requirement 1).
//
// Nothing here is stateful: the logger and the name come from the caller, so
// two goroutines of the same process never share anything.
package safe

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"jumpr/internal/errs"
)

// Panic is one recovered panic: what was running, the value it panicked with
// and the stack of the goroutine that panicked.
type Panic struct {
	Name  string // what the goroutine was doing, e.g. "session program"
	Value any    // the recovered panic value
	Stack []byte // the panicking goroutine's stack, from runtime/debug
}

// Err is the panic as the error a caller expects across a package boundary: an
// *errs.Error of kind Internal naming the goroutine.
func (p Panic) Err() *errs.Error {
	return errs.New(errs.Internal, "safe."+p.Name, "panic: "+fmt.Sprint(p.Value))
}

// Go runs fn in a new goroutine, recovering a panic at the goroutine's top. The
// panic is logged with its stack; nothing else happens here, so a goroutine
// whose failure must reach the UI uses GoHook.
func Go(log *slog.Logger, name string, fn func()) {
	GoHook(log, name, nil, fn)
}

// GoHook is Go plus a reporter: onPanic is called once with the recovered panic
// after it has been logged. A recovered panic is never silent — it is always
// logged, and the caller counts it (§15: `:health` shows the internal errors).
func GoHook(log *slog.Logger, name string, onPanic func(Panic), fn func()) {
	go func() {
		defer func() {
			Recover(log, name, onPanic, recover())
		}()
		fn()
	}()
}

// Do runs fn on the calling goroutine with the same recovery, returning the
// panic as an *errs.Error where the caller has a result to fill in. A nil
// return means fn finished normally.
func Do(log *slog.Logger, name string, fn func()) *errs.Error {
	return DoHook(log, name, nil, fn)
}

// DoHook is Do plus a reporter, for work whose panic must also be shown.
func DoHook(log *slog.Logger, name string, onPanic func(Panic), fn func()) *errs.Error {
	var out *errs.Error
	func() {
		defer func() {
			out = Recover(log, name, onPanic, recover())
		}()
		fn()
	}()
	return out
}

// Recover turns the value recover() returned into the error the helpers report:
// nil when there was no panic, otherwise the panic logged, reported and wrapped
// as *errs.Error of kind Internal. It is for the places that write their own
// defer, and it must be called directly by the deferred function.
func Recover(log *slog.Logger, name string, onPanic func(Panic), v any) *errs.Error {
	if v == nil {
		return nil
	}
	p := Panic{Name: name, Value: v, Stack: debug.Stack()}
	if log != nil {
		log.Error("panic recovered", "goroutine", p.Name, "value", fmt.Sprint(p.Value), "stack", string(p.Stack))
	}
	if onPanic != nil {
		onPanic(p)
	}
	return p.Err()
}
