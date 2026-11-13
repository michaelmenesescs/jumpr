//go:build lab

package salt

// The M4 exit checks: everything here talks to the real Salt master in the lab
// (SPEC §22). The lab must be up; without it a test skips with the controller
// command that starts it (labtest.Config). Nothing is installed on a node, and
// every condition a test creates is undone in cleanup.

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/inventory"
	"jumper/internal/labtest"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

const (
	// labTimeout bounds one Salt call a test makes.
	labTimeout = 2 * time.Minute
	// labPollTimeout bounds polling one job until every minion returned.
	labPollTimeout = 2 * time.Minute
	// labPollInterval is how often a job is polled.
	labPollInterval = 2 * time.Second
	// labDomain is the suffix of every lab host (SPEC §22).
	labDomain = ".lab.example.internal"
	// driftedMotd is the comment the drift test appends to the Salt-managed motd.
	driftedMotd = "# jumper lab drift"
)

// labEnv is the Salt stack the lab tests share: the runner to the master, the
// inventory built from its grains cache, the backend and the executor.
type labEnv struct {
	ssh     transport.Executor
	runner  *Runner
	inv     *inventory.Store
	backend *Backend
	exec    *Executor
	master  resource.HostID
}

// newLabEnv builds the stack and skips when the lab is not running or the
// master's grains cache is empty (the inventory would have no hosts to check).
func newLabEnv(t *testing.T) *labEnv {
	t.Helper()
	cfg := labtest.Config(t) // skips when lab/ssh_config is missing
	ssh := labtest.SSH(t)
	master := resource.HostID("salt" + labDomain)

	runner := NewRunner(ssh, master, RunnerOptions{
		Sudo:    cfg.Salt.Sudo,
		Timeout: cfg.Salt.Timeout.Duration(),
	})
	store := inventory.New([]inventory.Source{NewSource(runner)}, nil, "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()
	if failed := store.Refresh(ctx); len(failed) > 0 {
		for name, err := range failed {
			t.Fatalf("inventory source %s: %v", name, err)
		}
	}
	if len(store.Hosts()) == 0 {
		t.Skip(EmptyCacheHint)
	}

	be := New(Options{Runner: runner, Inventory: store, Master: master})
	return &labEnv{
		ssh:     ssh,
		runner:  runner,
		inv:     store,
		backend: be,
		master:  master,
		exec: NewExecutor(runner, be, ExecOptions{
			Chunk:   cfg.Salt.ListChunk,
			Timeout: cfg.Salt.Timeout.Duration(),
		}),
	}
}

// webHosts returns the inventory hosts whose role grain is web.
func (e *labEnv) webHosts() []inventory.Host {
	return e.hostsWhere("salt.role", "web")
}

// hostsWhere returns the inventory hosts carrying label=value.
func (e *labEnv) hostsWhere(label, value string) []inventory.Host {
	var out []inventory.Host
	for _, h := range e.inv.Hosts() {
		if h.Labels[label] == value {
			out = append(out, h)
		}
	}
	return out
}

// webMinions returns the minion ids of the web hosts.
func (e *labEnv) webMinions(t *testing.T) []string {
	t.Helper()
	hosts := e.webHosts()
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		id, ok := e.backend.HostMinion(h.ID)
		if !ok {
			t.Fatalf("%s has no minion id", h.ID)
		}
		out = append(out, id)
	}
	return out
}

// allMinions returns every minion id the backend maps.
func (e *labEnv) allMinions(t *testing.T) []string {
	t.Helper()
	out := make([]string, 0, len(e.inv.Hosts()))
	for _, h := range e.inv.Hosts() {
		if id, ok := e.backend.HostMinion(h.ID); ok {
			out = append(out, id)
		}
	}
	return out
}

// minionRows lists the minion kind.
func (e *labEnv) minionRows(t *testing.T) []resource.Resource {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()
	ch, err := e.backend.List(ctx, resource.Query{Kind: KindMinion})
	if err != nil {
		t.Fatalf("list minions: %v", err)
	}
	var rows []resource.Resource
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("list minions: %v", res.Err)
		}
		rows = append(rows, res.Items...)
	}
	return rows
}

// waitJob polls one job until want minions returned, and returns the job as the
// master recorded it.
func (e *labEnv) waitJob(t *testing.T, jid string, want int) JobInfo {
	t.Helper()
	deadline := time.Now().Add(labPollTimeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
		info, err := e.runner.Lookup(ctx, jid)
		cancel()
		if err == nil && len(info.Result) >= want {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: %d of %d minions returned (last error: %v)", jid, len(info.Result), want, err)
		}
		time.Sleep(labPollInterval)
	}
}

// TestLabSaltMinionsUp checks that every lab minion is up as Salt sees it: one
// row per minion, all up, and the web/db/k8s hosts are all in the list.
func TestLabSaltMinionsUp(t *testing.T) {
	env := newLabEnv(t)
	rows := env.minionRows(t)
	if len(rows) < 5 {
		t.Fatalf("%d minion rows, want the lab's fleet", len(rows))
	}
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		id := strings.TrimSpace(r.Ref.Name)
		seen[id] = true
		if status := strings.ToLower(r.Str("status")); status != "up" {
			t.Errorf("%s: status %q, want up", id, status)
		}
	}
	for _, host := range labtest.Hosts() {
		short := strings.TrimSuffix(string(host), labDomain)
		switch short {
		case "web-01", "web-02", "web-03", "db-01", "k3s-1", "k3s-2":
			if !seen[short] {
				t.Errorf("minion %s missing from %d rows", short, len(rows))
			}
		}
	}
}

// TestLabSaltGrainsInventory checks the grains inventory source: the role and
// dc labels reach the hosts, role=web selects the three web hosts and every
// host reports dc=lab.
func TestLabSaltGrainsInventory(t *testing.T) {
	env := newLabEnv(t)
	all := env.inv.Hosts()
	if len(all) == 0 {
		t.Fatal("no hosts")
	}
	for _, h := range all {
		if h.Labels["salt.dc"] != "lab" {
			t.Errorf("%s: salt.dc = %q, want lab", h.ID, h.Labels["salt.dc"])
		}
		if h.Labels["salt.role"] == "" {
			t.Errorf("%s: no salt.role label", h.ID)
		}
	}

	web := env.webHosts()
	if len(web) != 3 {
		names := make([]string, 0, len(web))
		for _, h := range web {
			names = append(names, string(h.ID))
		}
		t.Fatalf("role=web matched %d hosts (%v), want 3", len(web), names)
	}
	for _, h := range web {
		if short := strings.TrimSuffix(string(h.ID), labDomain); !strings.HasPrefix(short, "web-") {
			t.Errorf("role=web matched %s", h.ID)
		}
	}
}

// TestLabSaltTargetCompile checks the targeting compiler against the master: a
// role=web selector compiles to a native expression and `--preview-target`
// resolves that expression to the same three minions the client resolved.
func TestLabSaltTargetCompile(t *testing.T) {
	env := newLabEnv(t)
	sel, err := resource.ParseSelector("salt.role=web")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	native, ok := CompileSelector(sel, env.backend.grainKeys())
	if !ok {
		t.Fatal("role=web did not compile natively; the inventory has no role grain")
	}
	if native.Type != "compound" || !strings.Contains(native.Expr, "G@role:web") {
		t.Fatalf("compiled target = %+v, want a compound G@role:web", native)
	}

	web := env.webMinions(t)
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()
	got, err := env.backend.PreviewTarget(ctx, native)
	if err != nil {
		t.Fatalf("preview %s: %v", native.Expr, err)
	}
	if !sameSet(got, web) {
		t.Fatalf("preview returned %v, want the resolved web minions %v", got, web)
	}
}

// TestLabSaltJobPing checks the job engine: test.ping started as a job returns
// one successful return per expected minion.
func TestLabSaltJobPing(t *testing.T) {
	env := newLabEnv(t)
	minions := env.webMinions(t)
	if len(minions) == 0 {
		t.Fatal("no web minions")
	}
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()
	jid, err := env.backend.TestPing(ctx, minions)
	if err != nil {
		t.Fatalf("test.ping: %v", err)
	}
	info := env.waitJob(t, jid, len(minions))
	for _, id := range minions {
		ret, ok := info.Result[id]
		if !ok {
			t.Errorf("%s returned nothing", id)
			continue
		}
		if string(ret.Return) != "true" {
			t.Errorf("%s: test.ping returned %s, want true", id, ret.Return)
		}
		if ret.Retcode != 0 {
			t.Errorf("%s: retcode %d, want 0", id, ret.Retcode)
		}
	}
}

// TestLabSaltExecutorMatchesSSH checks that the two transports answer the same:
// `uname -r` gives the same kernel release through the Salt executor and over
// SSH, host by host.
func TestLabSaltExecutorMatchesSSH(t *testing.T) {
	env := newLabEnv(t)
	cmd := transport.Command{Script: "uname -r"}
	for _, h := range env.webHosts() {
		ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
		viaSalt, err := env.exec.Exec(ctx, h.ID, cmd)
		cancel()
		if err != nil {
			t.Fatalf("%s via salt: %v", h.ID, err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), labTimeout)
		viaSSH, err := env.ssh.Exec(ctx, h.ID, cmd)
		cancel()
		if err != nil {
			t.Fatalf("%s via ssh: %v", h.ID, err)
		}
		a := strings.TrimSpace(string(viaSalt.Stdout))
		b := strings.TrimSpace(string(viaSSH.Stdout))
		if a == "" {
			t.Errorf("%s: salt transport returned no kernel release", h.ID)
		}
		if a != b {
			t.Errorf("%s: uname -r is %q via salt and %q via ssh", h.ID, a, b)
		}
	}
}

// TestLabSaltDryRunGrouped creates drift on one web host (a comment appended to
// the Salt-managed motd) and checks that the dry run groups the two outcomes:
// the drifted host apart from the rest.
func TestLabSaltDryRunGrouped(t *testing.T) {
	env := newLabEnv(t)
	hosts := env.webHosts()
	if len(hosts) < 2 {
		t.Fatalf("%d web hosts, want at least 2", len(hosts))
	}
	drifted := hosts[0]
	labtest.Break(t, drifted.ID,
		"printf '"+driftedMotd+"\\n' >> /etc/motd",
		"sed -i '/^"+driftedMotd+"$/d' /etc/motd")

	minions := env.webMinions(t)
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()
	job, compiled, err := env.backend.DryRun(ctx, minions, "common", resource.Selector{})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	defer job.Cancel()
	groups := collectGroups(t, job, len(minions))
	if len(compiled.Targets) == 0 {
		t.Fatalf("compiled targets are empty")
	}
	if len(groups) != 2 {
		keys := make([]string, 0, len(groups))
		for k, ids := range groups {
			keys = append(keys, k+" = "+strings.Join(ids, ","))
		}
		t.Fatalf("%d groups, want 2 (drifted host apart):\n%s", len(groups), strings.Join(keys, "\n"))
	}
	driftMinion, _ := env.backend.HostMinion(drifted.ID)
	found := false
	for key, ids := range groups {
		if !sameSet(ids, []string{driftMinion}) {
			continue
		}
		found = true
		if !strings.Contains(key, "motd") {
			t.Errorf("the drifted group does not mention motd:\n%s", key)
		}
	}
	if !found {
		t.Fatalf("%s is not in a group of its own: %v", driftMinion, groups)
	}
}

// TestLabSaltApplyThroughBus applies the nginx state to a host whose nginx was
// stopped, through the action bus: the canary is that host, the rollout ends
// clean, and a second dry run has nothing left to change.
func TestLabSaltApplyThroughBus(t *testing.T) {
	env := newLabEnv(t)
	hosts := env.webHosts()
	if len(hosts) == 0 {
		t.Fatal("no web hosts")
	}
	target := hosts[0]
	labtest.Break(t, target.ID, "systemctl stop nginx", "systemctl start nginx")

	b := bus.New(bus.Options{
		Now: time.Now,
		Rollout: bus.RolloutConfig{
			Canary:                  1,
			BatchPercent:            100,
			BatchMax:                10,
			FailureThresholdPercent: 100,
			TypedConfirmOver:        20,
		},
	})
	if err := b.Register(backendName, env.backend); err != nil {
		t.Fatalf("register salt planner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req := action.Request{
		SpecID:  SpecStateApply,
		Targets: []action.Target{{Host: target.ID}},
		Params:  map[string]string{ParamSLS: "nginx"},
		Actor:   action.Human,
	}
	p, err := b.Propose(ctx, req)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if p.Denied != nil {
		t.Fatalf("the lab policy denied the apply: %+v", p.Denied)
	}
	if len(p.Plan.Steps) != 1 || p.Plan.Steps[0].Target.Host != target.ID {
		t.Fatalf("plan steps = %+v, want one step on %s", p.Plan.Steps, target.ID)
	}
	if p.Rollout.Canary > 1 {
		t.Fatalf("canary = %d hosts, want the single target", p.Rollout.Canary)
	}

	run, err := b.Execute(ctx, p, bus.Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var summary bus.Summary
	for ev := range run.Events() {
		if ev.Phase == bus.PhasePaused {
			run.Continue()
		}
		if ev.Summary != nil {
			summary = *ev.Summary
		}
	}
	if summary.Failed != 0 || summary.OK != 1 {
		t.Fatalf("rollout summary = %+v, want ok 1 failed 0", summary)
	}

	// The state is in place now: the dry run has nothing to change.
	minion, ok := env.backend.HostMinion(target.ID)
	if !ok {
		t.Fatalf("%s has no minion id", target.ID)
	}
	job, _, err := env.backend.DryRun(ctx, []string{minion}, "nginx", resource.Selector{})
	if err != nil {
		t.Fatalf("dry run after apply: %v", err)
	}
	for res := range job.Results() {
		if res.NoReturn {
			t.Fatalf("%s did not answer the dry run", minion)
		}
		stateRun, err := ParseStateReturn(res.Return)
		if err != nil {
			t.Fatalf("%s: %v", minion, err)
		}
		if _, _, failed, pending := stateRun.Counts(); failed > 0 || pending > 0 {
			t.Fatalf("dry run after apply still reports changes: %s", stateRun.Normalized())
		}
	}
}

// TestLabSaltNoReturn stops salt-minion on web-03 and checks that the job
// engine reports it as `no return` instead of dropping it. The minion is
// restarted in cleanup.
func TestLabSaltNoReturn(t *testing.T) {
	env := newLabEnv(t)
	host := resource.HostID("web-03" + labDomain)
	minion := "web-03"
	labtest.Break(t, host, "systemctl stop salt-minion", "systemctl start salt-minion")

	minions := env.allMinions(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	job, err := env.runner.Start(ctx, Submit{
		Targets: CompileHosts(minions, defaultChunk),
		Fun:     "test.ping",
		Expect:  minions,
		Timeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatalf("start test.ping: %v", err)
	}
	defer job.Cancel()

	seen := make(map[string]MinionResult, len(minions))
	for res := range job.Results() {
		seen[res.Minion] = res
	}
	res, ok := seen[minion]
	if !ok {
		t.Fatalf("%s is not among the job's results (got %d of %d)", minion, len(seen), len(minions))
	}
	if !res.NoReturn {
		t.Fatalf("%s returned %q; want no return while its minion is stopped", minion, res.Return)
	}
}

// TestLabSaltNodegroupTarget checks a nodegroup target end to end: `-N webs`
// parses, `--preview-target` resolves it, and the answer is the web hosts.
func TestLabSaltNodegroupTarget(t *testing.T) {
	env := newLabEnv(t)
	target, err := ParseNative("-N webs")
	if err != nil {
		t.Fatalf("parse -N webs: %v", err)
	}
	if target.Type != "nodegroup" || target.Expr != "webs" {
		t.Fatalf("target = %+v, want the webs nodegroup", target)
	}
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()
	got, err := env.backend.PreviewTarget(ctx, target)
	if err != nil {
		t.Fatalf("preview -N webs: %v", err)
	}
	want := env.webMinions(t)
	if len(want) != 3 {
		t.Fatalf("the lab has %d web minions, want 3", len(want))
	}
	if !sameSet(got, want) {
		t.Fatalf("-N webs resolved to %v, want %v", got, want)
	}
}

// collectGroups drains a dry run into groups of identical normalized text and
// fails when a minion never answered or returned something unparsable.
func collectGroups(t *testing.T, job *Job, want int) map[string][]string {
	t.Helper()
	groups := make(map[string][]string)
	got := 0
	for res := range job.Results() {
		got++
		if res.NoReturn {
			t.Fatalf("%s did not answer the dry run", res.Minion)
		}
		run, err := ParseStateReturn(res.Return)
		if err != nil {
			t.Fatalf("%s: %v", res.Minion, err)
		}
		key := run.Normalized()
		groups[key] = append(groups[key], res.Minion)
	}
	if got != want {
		t.Fatalf("dry run returned %d minions, want %d", got, want)
	}
	return groups
}

// sameSet reports whether a and b hold the same strings, ignoring order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}
