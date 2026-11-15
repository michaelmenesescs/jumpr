package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/bus"
	"jumpr/internal/bus/bustest"
	"jumpr/internal/resource"
	"jumpr/internal/server"
)

// Shutdown of task 126 requirement 5: kill-server and SIGTERM (the server's
// context) end the process inside three seconds, and a rollout caught between
// batches is recorded as aborted rather than left running.

// shutdownDeadline is what the requirement allows; the tests give the process
// a little longer before they call it a hang, so a failure says "too slow"
// rather than timing out the test binary.
const shutdownDeadline = 3 * time.Second

// serverFixture is a runnable server: a temp home and config with a static
// inventory, and a socket in the temp directory.
type serverFixture struct {
	home, configDir, cacheDir, socket string
	env                               map[string]string
}

// newServerFixture writes a hermetic configuration for RunServer.
func newServerFixture(t *testing.T, hosts int) serverFixture {
	t.Helper()
	// A short root: macOS limits a unix socket path to 103 bytes, and the
	// default test temp directory is already most of that.
	dir, err := os.MkdirTemp("/tmp", "js")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	home := filepath.Join(dir, "home")
	cfgDir := filepath.Join(dir, "config")
	for _, d := range []string{home, cfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	var inv []byte
	inv = append(inv, "hosts:\n"...)
	for i := 0; i < hosts; i++ {
		inv = append(inv, fmt.Sprintf("  - name: web-%02d\n    addr: 127.0.0.1\n    labels:\n      role: web\n", i+1)...)
	}
	invPath := filepath.Join(dir, "hosts.yaml")
	writeFile(t, invPath, string(inv))
	writeFile(t, filepath.Join(cfgDir, "config.yaml"), fmt.Sprintf(
		"inventory:\n  cache_ttl: 1h\n  sources:\n    - type: static\n      path: %q\n", invPath))
	return serverFixture{
		home:      home,
		configDir: cfgDir,
		cacheDir:  filepath.Join(home, ".cache", "jumpr"),
		socket:    filepath.Join(dir, "jumpr.sock"),
		env:       map[string]string{"USER": "tester"},
	}
}

// start runs RunServer in the background and waits until its socket answers.
func (f serverFixture) start(t *testing.T) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	options := ServerOptions{
		Options:    Options{Home: f.home, ConfigDir: f.configDir, Env: f.env, Version: "test"},
		SocketPath: f.socket,
	}
	go func() { errc <- RunServer(ctx, options) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(f.socket); err == nil {
			return cancel, errc
		}
		select {
		case err := <-errc:
			t.Fatalf("the server stopped before it listened: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the server never listened")
	return nil, nil
}

// waitStopped waits for the server to return, reporting how long it took.
func waitStopped(t *testing.T, errc <-chan error, since time.Time) {
	t.Helper()
	select {
	case err := <-errc:
		if took := time.Since(since); took > shutdownDeadline {
			t.Fatalf("the server took %s to shut down, want under %s", took.Round(time.Millisecond), shutdownDeadline)
		}
		if err != nil {
			t.Fatalf("RunServer returned %v", err)
		}
	case <-time.After(shutdownDeadline + 2*time.Second):
		t.Fatalf("the server did not stop within %s", shutdownDeadline)
	}
}

// TestShutdownKillServerWithinDeadline is the `jumpr kill-server` half: a
// running session does not hold the process open.
func TestShutdownKillServerWithinDeadline(t *testing.T) {
	f := newServerFixture(t, 3)
	cancel, errc := f.start(t)
	defer cancel()

	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if _, err := server.Control(ctx, f.socket, server.Hello{
		Command: "new", Session: "main", Detached: true,
	}); err != nil {
		t.Fatalf("create a detached session: %v", err)
	}

	start := time.Now()
	if _, err := server.Control(ctx, f.socket, server.Hello{Command: "kill-server"}); err != nil {
		t.Fatalf("kill-server: %v", err)
	}
	waitStopped(t, errc, start)

	if _, err := os.Stat(f.socket); !os.IsNotExist(err) {
		t.Fatalf("the socket survived the shutdown (err = %v)", err)
	}
}

// TestShutdownSignalWithinDeadline is the SIGTERM half: cmd/jumpr turns the
// signal into a canceled context, and the server must stop inside three
// seconds even with a session running.
func TestShutdownSignalWithinDeadline(t *testing.T) {
	f := newServerFixture(t, 3)
	cancel, errc := f.start(t)

	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if _, err := server.Control(ctx, f.socket, server.Hello{
		Command: "new", Session: "main", Detached: true,
	}); err != nil {
		t.Fatalf("create a detached session: %v", err)
	}

	start := time.Now()
	cancel()
	waitStopped(t, errc, start)
}

// TestShutdownAbortsRolloutBetweenBatches is the "recorded as aborted" half: a
// rollout paused after its canary stops when the session is killed, and says so
// in its final event instead of hanging.
func TestShutdownAbortsRolloutBetweenBatches(t *testing.T) {
	names := []string{"web-01", "web-02", "web-03", "web-04", "web-05", "web-06"}
	core := testCore(t, names, nil)
	sess := testSession(t, core)

	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Default: bustest.Result{Output: "ok"}})
	if err := sess.bus.Register("fake", pl); err != nil {
		t.Fatalf("register the planner: %v", err)
	}
	targets := make([]action.Target, len(names))
	for i, name := range names {
		targets[i] = action.Target{Host: resource.HostID(name)}
	}
	p, err := sess.bus.Propose(context.Background(), action.Request{
		SpecID: "fake.restart", Targets: targets, Actor: action.Human,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if p.Rollout.Canary != 1 || p.Rollout.Batches < 2 {
		t.Fatalf("rollout = %+v, want a canary and at least one batch after it", p.Rollout)
	}
	run, err := sess.bus.Execute(context.Background(), p, bus.Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Wait for the rollout to pause after its canary: the shutdown finds it
	// between batches, which is the case the requirement names.
	events := run.Events()
	paused := false
	for !paused {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("the rollout ended before it paused")
			}
			if ev.Phase == bus.PhasePaused {
				paused = true
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the rollout never paused after its canary")
		}
	}

	start := time.Now()
	sess.Kill()

	aborted := false
	for !aborted {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the rollout ended without an aborted phase (killed in %s)", time.Since(start))
			}
			if ev.Phase == bus.PhaseAborted {
				if ev.Summary == nil || ev.Summary.Skipped != len(names)-1 {
					t.Fatalf("aborted summary = %+v, want the %d unrun hosts skipped", ev.Summary, len(names)-1)
				}
				aborted = true
			}
		case <-time.After(shutdownDeadline):
			t.Fatalf("the rollout was not aborted within %s of the session ending", shutdownDeadline)
		}
	}
	if took := time.Since(start); took > shutdownDeadline {
		t.Fatalf("the abort took %s, want under %s", took.Round(time.Millisecond), shutdownDeadline)
	}
	// The run records the abort before it closes its event stream: wait for
	// that, so the recorder is not still writing when the test's temp
	// directory goes away.
	for range events {
	}

	// The canary ran, and nothing after it did.
	if got := len(pl.Ran()); got != 1 {
		t.Fatalf("%d hosts ran, want only the canary", got)
	}
}
