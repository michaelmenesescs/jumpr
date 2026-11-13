//go:build lab

package salt

// Task 095, requirement 5: the key actions against the real master. The lab has
// no `pending-key` fault yet (docs/HANDOFF.md "Requests to the lab track"), so
// the test creates one itself with labtest.Break: it stops web-03's minion,
// points it at a stray id through a minion.d override and starts it again. The
// stray key is then accepted through the bus, the minion must come up, and the
// key is deleted through the bus in cleanup (the Break undo removes the override
// and restarts the minion with its real id).

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/labtest"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

const (
	// strayID is the second minion identity the test gives web-03. It is a
	// valid key id (letters, digits, dot, underscore, hyphen) and nothing else
	// in the lab uses it.
	strayID = "jumper-stray"
	// strayOverride is the minion.d file that asks for that id.
	strayOverride = "/etc/salt/minion.d/jumper-lab-id.conf"
)

// TestLabSaltKeyAcceptThroughBus accepts a pending key through the bus and
// checks the minion then reports up; the delete runs in cleanup.
func TestLabSaltKeyAcceptThroughBus(t *testing.T) {
	env := newLabEnv(t)
	host := resource.HostID("web-03" + labDomain)

	labtest.Break(t, host,
		"systemctl stop salt-minion && "+
			"printf 'id: "+strayID+"\\n' > "+strayOverride+" && "+
			"systemctl start salt-minion",
		"rm -f "+strayOverride+" && systemctl restart salt-minion")
	t.Cleanup(func() {
		// The minion is stopped first so it cannot present the stray key again
		// while it is deleted; the Break undo starts it with its real id.
		stopMinion(t, host)
		deleteKeyThroughBus(t, env, strayID)
	})

	// The restarted minion presents its key only after a few seconds.
	waitKeyState(t, env, strayID, KeyPending)

	keyRow := resource.Resource{
		Ref:    resource.Ref{Backend: backendName, Kind: KindKey, Name: strayID},
		Fields: map[string]any{"id": strayID, "state": KeyPending},
	}
	offered := specIDs(env.backend.Actions([]resource.Resource{keyRow}))
	if len(offered) != 2 || offered[0] != SpecKeyAccept || offered[1] != SpecKeyReject {
		t.Fatalf("a pending key offers %v, want accept and reject", offered)
	}

	b := newLabBus(t, env)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := action.Request{
		SpecID:  SpecKeyAccept,
		Targets: []action.Target{{Ref: keyRow.Ref}},
		Actor:   action.Human,
	}
	p, err := b.Propose(ctx, req)
	if err != nil {
		t.Fatalf("propose accept: %v", err)
	}
	if p.Denied != nil {
		t.Fatalf("the lab policy denied the accept: %+v", p.Denied)
	}
	if len(p.Plan.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(p.Plan.Steps))
	}
	if got := p.Plan.Steps[0].Display; got != "salt-key -a "+strayID+" -y" {
		t.Errorf("display = %q, want the exact -a line with -y", got)
	}
	if p.Plan.Steps[0].Target.Host != env.master {
		t.Errorf("step host = %q, want the master %q", p.Plan.Steps[0].Target.Host, env.master)
	}
	if !strings.Contains(p.Plan.Preview, keyVerifyHint) {
		t.Errorf("accept preview %q does not tell the operator to check the fingerprint", p.Plan.Preview)
	}
	if p.Confirm != bus.ConfirmYesNo {
		t.Errorf("confirm = %v for one key, want %v", p.Confirm, bus.ConfirmYesNo)
	}

	run, err := b.Execute(ctx, p, bus.Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("execute accept: %v", err)
	}
	if s := drainLabRun(run); s.OK != 1 || s.Failed != 0 {
		t.Fatalf("rollout summary = %+v, want ok 1 failed 0", s)
	}

	waitKeyState(t, env, strayID, KeyAccepted)
	waitMinionUp(t, env, strayID)
}

// newLabBus builds a bus with the salt planner registered and the lab's rollout
// settings.
func newLabBus(t *testing.T, env *labEnv) bus.Bus {
	t.Helper()
	b := bus.New(bus.Options{
		Now: time.Now,
		Rollout: bus.RolloutConfig{
			Canary:                  0,
			BatchPercent:            100,
			BatchMax:                10,
			FailureThresholdPercent: 100,
			TypedConfirmOver:        20,
		},
	})
	if err := b.Register(backendName, env.backend); err != nil {
		t.Fatalf("register salt planner: %v", err)
	}
	return b
}

// deleteKeyThroughBus deletes a key through the bus with the typed count the
// delete target's _confirm label demands. It reports failures instead of
// failing, because it runs in cleanup.
func deleteKeyThroughBus(t *testing.T, env *labEnv, id string) {
	t.Helper()
	b := newLabBus(t, env)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	req := action.Request{
		SpecID:  SpecKeyDelete,
		Targets: []action.Target{{Ref: resource.Ref{Backend: backendName, Kind: KindKey, Name: id}}},
		Actor:   action.Human,
	}
	p, err := b.Propose(ctx, req)
	if err != nil {
		t.Errorf("cleanup: propose delete of %s: %v", id, err)
		return
	}
	if p.Confirm != bus.ConfirmTypedCount {
		t.Errorf("cleanup: delete confirm = %v, want the typed count", p.Confirm)
		return
	}
	if len(p.Plan.Steps) != 1 {
		t.Errorf("cleanup: delete plan has %d steps, want 1", len(p.Plan.Steps))
		return
	}
	if got := p.Plan.Steps[0].Display; got != "salt-key -d "+id+" -y" {
		t.Errorf("cleanup: delete display = %q", got)
	}
	run, err := b.Execute(ctx, p, bus.Confirmation{ProposalID: p.ID, Typed: "1"})
	if err != nil {
		t.Errorf("cleanup: execute delete of %s: %v", id, err)
		return
	}
	if s := drainLabRun(run); s.OK != 1 {
		t.Errorf("cleanup: delete summary = %+v, want ok 1", s)
	}
}

// drainLabRun drives a rollout to the end, leaving a paused canary, and returns
// its summary.
func drainLabRun(run bus.Run) bus.Summary {
	var summary bus.Summary
	for ev := range run.Events() {
		if ev.Phase == bus.PhasePaused {
			run.Continue()
		}
		if ev.Summary != nil {
			summary = *ev.Summary
		}
	}
	return summary
}

// stopMinion stops salt-minion on host.
func stopMinion(t *testing.T, host resource.HostID) {
	t.Helper()
	tr := labtest.SSH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*labTimeout)
	defer cancel()
	if _, err := tr.Exec(ctx, host, transport.Command{
		Script:  "systemctl stop salt-minion",
		Sudo:    true,
		Timeout: labTimeout,
	}); err != nil {
		t.Errorf("stop salt-minion on %s: %v", host, err)
	}
}

// waitKeyState polls salt-key -L until id is in the given state.
func waitKeyState(t *testing.T, env *labEnv, id, want string) {
	t.Helper()
	deadline := time.Now().Add(labPollTimeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
		out, err := env.runner.Run(ctx, Call{Tool: toolKey, Flags: []string{"-L"}})
		cancel()
		if err == nil {
			if keys, perr := ParseKeys(out); perr == nil {
				for _, k := range keys {
					if k.ID == id && k.State == want {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("key %s never became %s (last error: %v)", id, want, err)
		}
		time.Sleep(labPollInterval)
	}
}

// waitMinionUp polls the minion kind until id reports up.
func waitMinionUp(t *testing.T, env *labEnv, id string) {
	t.Helper()
	deadline := time.Now().Add(labPollTimeout)
	for {
		for _, r := range env.minionRows(t) {
			if r.Ref.Name == id && r.Str("status") == "up" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("minion %s never reported up after its key was accepted", id)
		}
		time.Sleep(labPollInterval)
	}
}
