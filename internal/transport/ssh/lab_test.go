//go:build lab

package ssh_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"jumpr/internal/labtest"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	sshx "jumpr/internal/transport/ssh"
)

// labTimeout bounds one command in these tests.
const labTimeout = 30 * time.Second

// labCtx returns a context for one lab test.
func labCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// TestLabExecAllHosts runs `echo ok` on all nine hosts through the bastion.
func TestLabExecAllHosts(t *testing.T) {
	tr := labtest.SSH(t)
	ctx := labCtx(t, 3*time.Minute)

	for _, host := range labtest.Hosts() {
		host := host
		t.Run(string(host), func(t *testing.T) {
			res, err := tr.Exec(ctx, host, transport.Command{Script: "echo ok", Timeout: labTimeout})
			if err != nil {
				t.Fatalf("exec on %s: %v", host, err)
			}
			if res.RC != 0 {
				t.Fatalf("exec on %s: rc %d: %s", host, res.RC, strings.TrimSpace(string(res.Stderr)))
			}
			if got := strings.TrimSpace(string(res.Stdout)); got != "ok" {
				t.Fatalf("exec on %s: stdout %q, want ok", host, got)
			}
			if host != labtest.Hosts()[0] && !strings.Contains(res.Via, "bastion") {
				t.Errorf("exec on %s went via %q, want the lab bastion", host, res.Via)
			}
		})
	}
}

// TestLabSudo proves passwordless sudo on web-01.
func TestLabSudo(t *testing.T) {
	tr := labtest.SSH(t)
	ctx := labCtx(t, 2*time.Minute)

	res, err := tr.Exec(ctx, resource.HostID("web-01.lab.example.internal"),
		transport.Command{Script: "sudo -n id -u", Timeout: labTimeout})
	if err != nil {
		t.Fatalf("sudo -n id -u: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "0" || res.RC != 0 {
		t.Fatalf("sudo -n id -u = %q (rc %d), want 0", got, res.RC)
	}
}

// TestLabFanoutStress runs 5 rounds of 9 hosts × 4 concurrent commands through
// the bastion with the pool limits of BUILD-PLAN §12: MaxConnsPerBastion=2,
// MaxChannelsPerConn=4. Nothing may fail and the bastion must not be given more
// than two connections.
func TestLabFanoutStress(t *testing.T) {
	pool := labtest.Pool(t)
	tr := sshx.New(pool, sshx.Options{ExecTimeout: labTimeout})
	ctx := labCtx(t, 10*time.Minute)

	const (
		rounds     = 5
		perHost    = 4
		maxBastion = 2
	)
	hosts := labtest.Hosts()

	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		errs := make(chan error, len(hosts)*perHost)
		for _, host := range hosts {
			for i := 0; i < perHost; i++ {
				wg.Add(1)
				go func(host resource.HostID) {
					defer wg.Done()
					res, err := tr.Exec(ctx, host, transport.Command{Script: "echo ok", Timeout: labTimeout})
					if err != nil {
						errs <- fmt.Errorf("%s: %w", host, err)
						return
					}
					if got := strings.TrimSpace(string(res.Stdout)); res.RC != 0 || got != "ok" {
						errs <- fmt.Errorf("%s: rc %d: %q", host, res.RC, got)
					}
				}(host)
			}
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
		if t.Failed() {
			t.Fatalf("round %d failed; stopping", round)
		}
		if got := pool.Stats().Bastions; got > maxBastion {
			t.Fatalf("round %d: %d bastion connections, limit %d", round, got, maxBastion)
		}
	}

	if st := pool.Stats(); st.Bastions > maxBastion {
		t.Errorf("pool holds %d bastion connections, limit %d", st.Bastions, maxBastion)
	}
}

// TestLabReconnectAfterIdleReap proves a dropped pooled connection is dialled
// again: Forget is what the idle reaper does when it closes a connection, and
// the next command must not see the dead one.
func TestLabReconnectAfterIdleReap(t *testing.T) {
	pool := labtest.Pool(t)
	tr := sshx.New(pool, sshx.Options{ExecTimeout: labTimeout})
	ctx := labCtx(t, 3*time.Minute)
	host := resource.HostID("web-01.lab.example.internal")

	execOK := func(when string) {
		t.Helper()
		res, err := tr.Exec(ctx, host, transport.Command{Script: "echo ok", Timeout: labTimeout})
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != "ok" || res.RC != 0 {
			t.Fatalf("%s: %q (rc %d)", when, got, res.RC)
		}
	}

	execOK("first command")
	before := pool.Stats().Targets
	pool.Forget(host)
	if after := pool.Stats().Targets; after != before-1 {
		t.Fatalf("Forget left %d target connections, want %d", after, before-1)
	}
	execOK("after the reaper")
	if after := pool.Stats().Targets; after != before {
		t.Errorf("redial left %d target connections, want %d", after, before)
	}
}
