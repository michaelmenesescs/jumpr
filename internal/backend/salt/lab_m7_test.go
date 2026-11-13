//go:build lab

package salt

// The M7 exit checks (task 097, SPEC §8.6): nodegroups resolve to the lab's web
// hosts, salt-call local mode parses a real dry run, the pillar tree carries
// keys and no values, and the event stream shows a job-ret for a test.ping.
// Everything here needs the lab controller (labtest.Config skips otherwise).

import (
	"context"
	"testing"
	"time"
)

// TestLabM7Nodegroups: the lab master defines the `webs` nodegroup; it must
// resolve to the three web hosts and compile to -N webs.
func TestLabM7Nodegroups(t *testing.T) {
	e := newLabEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()

	groups, err := e.backend.Nodegroups(ctx)
	if err != nil {
		t.Fatalf("Nodegroups: %v", err)
	}
	var webs *Nodegroup
	for i := range groups {
		if groups[i].Name == "webs" {
			webs = &groups[i]
		}
	}
	if webs == nil {
		t.Fatalf("nodegroups = %v, want one named webs", groups)
	}
	if webs.Err != nil {
		t.Fatalf("webs preview: %v", webs.Err)
	}
	want := e.webMinions(t)
	if len(webs.Members) != len(want) {
		t.Fatalf("webs members = %v, want the %d web minions %v", webs.Members, len(want), want)
	}
	for _, m := range want {
		found := false
		for _, got := range webs.Members {
			if got == m {
				found = true
			}
		}
		if !found {
			t.Fatalf("webs members = %v, missing %s", webs.Members, m)
		}
	}

	sel := mustSelector(t, "@salt:webs")
	target, ok := NodegroupTarget(sel)
	if !ok || target.String() != "-N webs" {
		t.Fatalf("nodegroup target = %+v (%v), want -N webs", target, ok)
	}
	compiled := Compile(sel, webs.Members, nil, 200)
	if len(compiled.Targets) != 1 || compiled.Targets[0].Type != "nodegroup" {
		t.Fatalf("compiled = %+v, want the single nodegroup target", compiled)
	}
}

// TestLabM7LocalState: `salt-call --local` reads grains and a state dry run on a
// real web host, over SSH, with nothing installed.
func TestLabM7LocalState(t *testing.T) {
	e := newLabEnv(t)
	hosts := e.webHosts()
	if len(hosts) == 0 {
		t.Skip("no web host in the inventory")
	}
	host := hosts[0].ID

	be := New(Options{Runner: e.runner, Inventory: e.inv, Master: e.master, Executor: e.ssh})
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()

	grains, err := be.LocalGrains(ctx, host)
	if err != nil {
		t.Fatalf("LocalGrains(%s): %v", host, err)
	}
	if len(grains) == 0 {
		t.Fatalf("LocalGrains(%s) returned no grains", host)
	}
	if grains.Str("id") == "" {
		t.Fatalf("local grains have no id: %v keys", len(grains))
	}

	run, err := be.LocalState(ctx, host, "")
	if err != nil {
		t.Fatalf("LocalState(%s): %v", host, err)
	}
	if !run.Test {
		t.Fatalf("local run is not marked as a dry run")
	}
	if len(run.Rows) == 0 && len(run.Errors) == 0 {
		t.Fatalf("local dry run returned neither rows nor errors")
	}
}

// TestLabM7PillarKeys: the pillar tree of a web host carries key paths and
// types, and never a value.
func TestLabM7PillarKeys(t *testing.T) {
	e := newLabEnv(t)
	hosts := e.webHosts()
	if len(hosts) == 0 {
		t.Skip("no web host in the inventory")
	}
	minion, ok := e.backend.HostMinion(hosts[0].ID)
	if !ok {
		t.Skipf("%s is not a minion", hosts[0].ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()

	tree, err := e.backend.PillarKeys(ctx, minion)
	if err != nil {
		t.Fatalf("PillarKeys(%s): %v", minion, err)
	}
	if len(tree.Nodes) == 0 {
		t.Fatalf("PillarKeys(%s) returned no keys", minion)
	}
	for _, n := range tree.Nodes {
		// A node is a key path, a JSON type and a length; there is no value
		// field to leak one.
		if n.Key == "" || n.Type == "" {
			t.Fatalf("node = %+v, want a key path and a type", n)
		}
	}
}

// TestLabM7EventJobRet: a test.ping job shows up on the master's event bus as a
// job-ret.
func TestLabM7EventJobRet(t *testing.T) {
	e := newLabEnv(t)
	minions := e.webMinions(t)
	if len(minions) == 0 {
		t.Skip("no web minion")
	}
	ctx, cancel := context.WithTimeout(context.Background(), labPollTimeout)
	defer cancel()

	events, err := e.backend.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// Give state.event a moment to subscribe before the job is submitted.
	time.Sleep(time.Second)

	job, err := e.runner.Start(ctx, Submit{
		Targets: CompileHosts(minions[:1], 200),
		Fun:     "test.ping",
		Expect:  minions[:1],
		Timeout: labPollTimeout,
	})
	if err != nil {
		t.Fatalf("test.ping: %v", err)
	}
	defer job.Cancel()
	jids := job.JIDs()
	if len(jids) == 0 {
		t.Fatalf("test.ping returned no job id")
	}

	deadline := time.After(labPollTimeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the event stream closed before the job returned")
			}
			if ev.Kind == EventJobRet && ev.JID == jids[0] {
				return
			}
		case <-deadline:
			t.Fatalf("no job-ret event for %s within %s", jids[0], labPollTimeout)
		}
	}
}
