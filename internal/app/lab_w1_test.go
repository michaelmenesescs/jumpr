//go:build lab

// The M3 lab tests: the wiring in internal/app, the policy guard rails and the
// W1 scenario from the mockup (SPEC §5 W1) without metrics. They need the lab
// (`go run ./cmd/sitrep-lab up`) and run with `make lab-test`; `go test ./...`
// never builds this file.
//
// Everything is asserted at package level, not through the TUI: the same bus,
// policy, record and probes the UI uses, driven directly. The record is written
// under a temp knowledge root, never to lab/.knowledge.

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend/linux"
	"jumper/internal/bus"
	"jumper/internal/bus/generic"
	"jumper/internal/config"
	"jumper/internal/fanout"
	"jumper/internal/group"
	"jumper/internal/inventory"
	"jumper/internal/investigation"
	"jumper/internal/labtest"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/transport/ssh"
	"jumper/internal/tui"
)

const (
	// w1Domain is the FQDN suffix of every lab machine.
	w1Domain = ".lab.example.internal"
	// w1ExecTimeout bounds one remote command.
	w1ExecTimeout = 60 * time.Second
	// w1PhaseTimeout bounds one whole rollout.
	w1PhaseTimeout = 10 * time.Minute
)

// w1WebNames are the short names of the lab web nodes, in the order the
// scenario targets them: web-01 is the canary and the host the fault hits.
func w1WebNames() []string { return []string{"web-01", "web-02", "web-03"} }

// w1WebHosts returns the web nodes as host IDs.
func w1WebHosts() []resource.HostID {
	names := w1WebNames()
	out := make([]resource.HostID, len(names))
	for i, n := range names {
		out[i] = resource.HostID(n + w1Domain)
	}
	return out
}

// labW1 is the M3 harness: a pooled SSH transport, the Linux backend over it and
// the M3 layer buildM3 assembles, exactly as internal/app wires it.
type labW1 struct {
	tr     *ssh.Transport
	linux  *linux.Backend
	rt     *m3Runtime
	hosts  []resource.HostID
	labels map[resource.HostID]map[string]string
}

// newLabW1 builds the harness or skips the test when the lab is not up.
func newLabW1(t *testing.T) *labW1 {
	t.Helper()
	cfg := labtest.Config(t) // skips without lab/ssh_config, chdirs to the repo root
	cfg.Knowledge.Root = t.TempDir()
	tr := labtest.SSH(t)
	lb := linux.New(linux.Options{Exec: tr, Stream: tr, Concurrency: 4, Timeout: w1ExecTimeout})

	// The targets come from a real inventory through the selector the demo
	// uses (`:hosts role=web`), so the scenario exercises target resolution
	// rather than a hard-coded list.
	invPath := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(invPath, []byte(w1Inventory()), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	inv := inventory.New([]inventory.Source{inventory.NewStaticSource(invPath)}, nil, "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if failed := inv.Refresh(ctx); len(failed) > 0 {
		t.Fatalf("inventory refresh failed: %v", failed)
	}
	sel, err := resource.ParseSelector("role=web")
	if err != nil {
		t.Fatalf("selector: %v", err)
	}
	set, err := targets.Resolve(inv, targets.NewStore(t.TempDir()), sel, nil)
	if err != nil {
		t.Fatalf("resolve role=web: %v", err)
	}
	if len(set.Hosts) != len(w1WebNames()) {
		t.Fatalf("role=web resolved %d hosts (%v), want %d", len(set.Hosts), set.Hosts, len(w1WebNames()))
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	session := &tui.SessionState{Name: "lab"}
	paths := config.Paths{
		ConfigDir:     filepath.Join("lab", "jumper"),
		CacheDir:      t.TempDir(),
		KnowledgeRoot: cfg.Knowledge.Root,
	}
	rt, err := buildM3(context.Background(), cfg, paths, home, session, []namedPlanner{
		{name: "linux", planner: lb},
		{name: "generic", planner: generic.New(map[string]transport.Executor{"ssh": tr}, "ssh")},
	}, nil)
	if err != nil {
		t.Fatalf("buildM3: %v", err)
	}

	labels := make(map[resource.HostID]map[string]string, len(set.Hosts))
	for _, h := range set.Hosts {
		if host, ok := inv.Resolve(string(h)); ok {
			labels[h] = host.Labels
		}
	}
	return &labW1{tr: tr, linux: lb, rt: rt, hosts: set.Hosts, labels: labels}
}

// w1Inventory is the static inventory fixture: the web nodes carry role=web,
// the rest carry a different role, so `role=web` has something to reject.
func w1Inventory() string {
	var b strings.Builder
	b.WriteString("hosts:\n")
	for _, name := range w1WebNames() {
		fmt.Fprintf(&b, "  - name: %s%s\n    addr: %s%s\n    labels:\n      role: web\n      env: lab\n",
			name, w1Domain, name, w1Domain)
	}
	for _, name := range []string{"db-01", "k3s-1"} {
		fmt.Fprintf(&b, "  - name: %s%s\n    addr: %s%s\n    labels:\n      role: data\n      env: lab\n",
			name, w1Domain, name, w1Domain)
	}
	return b.String()
}

// unitTarget is the target of one unit action on a host.
func (w *labW1) unitTarget(host resource.HostID, unit string) action.Target {
	return action.Target{
		Host:   host,
		Ref:    resource.Ref{Backend: "linux", Kind: linux.KindUnit, Host: host, Name: unit},
		Labels: w.labels[host],
	}
}

// proposeUnit asks the bus for one systemctl verb over hosts and fails the test
// when the proposal is denied (the tests that expect a denial check it
// themselves).
func (w *labW1) proposeUnit(t *testing.T, verb, unit string, hosts []resource.HostID) bus.Proposal {
	t.Helper()
	targets := make([]action.Target, 0, len(hosts))
	for _, h := range hosts {
		targets = append(targets, w.unitTarget(h, unit))
	}
	p, err := w.rt.Bus.Propose(context.Background(), action.Request{
		SpecID:  "linux.systemctl." + verb,
		Targets: targets,
		Actor:   action.Human,
	})
	if err != nil {
		t.Fatalf("propose %s %s: %v", verb, unit, err)
	}
	if p.Denied != nil {
		t.Fatalf("propose %s %s was denied: %s", verb, unit, p.Denied.Reason)
	}
	return p
}

// rollout executes p, continues past the canary pause and returns the summary
// and the phases it saw.
func (w *labW1) rollout(t *testing.T, p bus.Proposal) (bus.Summary, []bus.Phase) {
	t.Helper()
	r, err := w.rt.Bus.Execute(context.Background(), p, bus.Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("execute %s: %v", p.Plan.Spec.ID, err)
	}
	var sum bus.Summary
	var phases []bus.Phase
	deadline := time.After(w1PhaseTimeout)
	for {
		select {
		case ev, ok := <-r.Events():
			if !ok {
				return sum, phases
			}
			phases = append(phases, ev.Phase)
			if ev.Phase == bus.PhasePaused {
				r.Continue()
			}
			if ev.Summary != nil {
				sum = *ev.Summary
			}
		case <-deadline:
			t.Fatalf("rollout of %s did not finish within %s", p.Plan.Spec.ID, w1PhaseTimeout)
		}
	}
}

// probeSummary runs one catalog probe over the web hosts and groups identical
// output.
func (w *labW1) probeSummary(t *testing.T, name string) group.Summary {
	t.Helper()
	probe, ok := w1Probe(name)
	if !ok {
		t.Fatalf("the catalog has no %q probe", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	g := group.NewGrouper(len(w.hosts), trimLines)
	for hr := range fanout.Run(ctx, w.tr, w.hosts,
		transport.Command{Script: probe.Cmd, Sudo: probe.Sudo, Timeout: w1ExecTimeout},
		fanout.Options{Concurrency: 3}) {
		if hr.Err != nil {
			g.AddError(hr.Host, hr.Err)
			continue
		}
		g.Add(hr.Host, string(hr.Result.Stdout), hr.Result.RC)
	}
	sum := g.Summary()
	if len(sum.Errors) > 0 {
		t.Fatalf("probe %s errors: %v", name, sum.Errors)
	}
	return sum
}

// journalMessages returns the journal messages of one unit on one host.
func (w *labW1) journalMessages(t *testing.T, host resource.HostID, unit string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ch, err := w.linux.List(ctx, resource.Query{
		Kind:  linux.KindJournal,
		Hosts: []resource.HostID{host},
		Args:  map[string]string{"unit": unit},
	})
	if err != nil {
		t.Fatalf("journal %s on %s: %v", unit, host, err)
	}
	var out []string
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("journal %s on %s: %v", unit, host, res.Err)
		}
		for _, item := range res.Items {
			out = append(out, item.Str("message"))
		}
	}
	return out
}

// socketRows returns the sockets of one host.
func (w *labW1) socketRows(t *testing.T, host resource.HostID) []resource.Resource {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ch, err := w.linux.List(ctx, resource.Query{Kind: linux.KindSocket, Hosts: []resource.HostID{host}})
	if err != nil {
		t.Fatalf("sockets on %s: %v", host, err)
	}
	var rows []resource.Resource
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("sockets on %s: %v", host, res.Err)
		}
		rows = append(rows, res.Items...)
	}
	return rows
}

// w1Probe finds a catalog probe by name.
func w1Probe(name string) (linux.Probe, bool) {
	for _, p := range linux.Catalog() {
		if p.Name == name {
			return p, true
		}
	}
	return linux.Probe{}, false
}

// trimLines normalizes probe output before grouping: trailing whitespace on
// every line and trailing blank lines.
func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// TestLabW1NginxConflict is the M3 demo (BUILD-PLAN §2, SPEC §5 W1) without
// metrics: a port conflict, the probe that finds it, the journal that explains
// it, the socket that shows the culprit, the restart that fails at the canary,
// the real fix, and the record of all of it.
func TestLabW1NginxConflict(t *testing.T) {
	w := newLabW1(t)
	affected := w.hosts[0]
	labtest.Fault(t, "haproxy-443")

	if _, err := w.rt.Log.Open("nginx 443 conflict", resource.TimeWindow{}, w.hosts, nil); err != nil {
		t.Fatalf("open investigation: %v", err)
	}
	meta, ok := w.rt.Log.Current()
	if !ok {
		t.Fatal("no investigation open after Open")
	}

	// 1. The probe separates the faulted host from the rest.
	sum := w.probeSummary(t, "failed-units")
	recordProbe(t, w.rt, "failed-units", sum)
	differing := sum.Differing()
	if len(differing) == 0 {
		t.Fatalf("every host reports the same failed units, want one host apart:\n%s", sum.Headline())
	}
	found := false
	for _, g := range differing {
		if !strings.Contains(g.Output, "nginx.service") {
			continue
		}
		for _, h := range g.Hosts {
			if h == affected {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("%s is not in the differing failed-units group:\n%s", affected, sum.Headline())
	}

	// 2. The journal says why: nginx could not bind :443.
	var explained bool
	for _, msg := range w.journalMessages(t, affected, "nginx.service") {
		if strings.Contains(msg, "Address already in use") {
			explained = true
			break
		}
	}
	if !explained {
		t.Fatalf("the nginx journal on %s does not mention the port conflict", affected)
	}

	// 3. The sockets name the process holding :443.
	var culprit bool
	for _, row := range w.socketRows(t, affected) {
		if !strings.Contains(row.Str("local"), ":443") {
			continue
		}
		if strings.Contains(row.Str("process"), "haproxy") {
			culprit = true
			break
		}
	}
	if !culprit {
		t.Fatalf("no socket row on %s shows haproxy on :443", affected)
	}

	// 4. Restarting nginx fails in the canary: the unit never becomes active.
	failed := w.proposeUnit(t, "restart", "nginx.service", w.hosts)
	failedSum, phases := w.rollout(t, failed)
	if failedSum.Failed == 0 {
		t.Fatalf("the nginx restart reported no failure: %+v (phases %v)", failedSum, phases)
	}
	if failedSum.Skipped == 0 {
		t.Errorf("the failed canary did not skip the rest: %+v", failedSum)
	}

	// 5. Stopping haproxy is the real fix; then nginx restarts cleanly.
	stopped := w.proposeUnit(t, "stop", "haproxy.service", w.hosts)
	if stopSum, _ := w.rollout(t, stopped); stopSum.Failed != 0 || stopSum.OK != len(w.hosts) {
		t.Fatalf("stop haproxy = %+v, want %d ok", stopSum, len(w.hosts))
	}
	restarted := w.proposeUnit(t, "restart", "nginx.service", w.hosts)
	if restartSum, _ := w.rollout(t, restarted); restartSum.Failed != 0 || restartSum.OK != len(w.hosts) {
		t.Fatalf("restart nginx after the fix = %+v, want %d ok", restartSum, len(w.hosts))
	}

	// 6. The record holds the probe and the three actions, and no secrets.
	body, err := os.ReadFile(meta.Path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "**probe**") {
		t.Errorf("the record has no probe bullet:\n%s", text)
	}
	if n := strings.Count(text, "**action**"); n != 3 {
		t.Errorf("the record has %d action bullets, want 3:\n%s", n, text)
	}
	assertNoSecrets(t, text)
}

// TestLabPolicyDeniesStopSSHD checks the guard rail end to end: a proposal to
// stop sshd on a web node is denied by the built-in rule and the denial is in
// the record.
func TestLabPolicyDeniesStopSSHD(t *testing.T) {
	w := newLabW1(t)
	host := w.hosts[1]

	if _, err := w.rt.Log.Open("policy check", resource.TimeWindow{}, []resource.HostID{host}, nil); err != nil {
		t.Fatalf("open investigation: %v", err)
	}
	meta, ok := w.rt.Log.Current()
	if !ok {
		t.Fatal("no investigation open after Open")
	}

	p, err := w.rt.Bus.Propose(context.Background(), action.Request{
		SpecID:  "linux.systemctl.stop",
		Targets: []action.Target{w.unitTarget(host, "sshd.service")},
		Actor:   action.Human,
	})
	if err != nil {
		t.Fatalf("propose stop sshd: %v", err)
	}
	if p.Denied == nil {
		t.Fatalf("stopping sshd on %s was allowed; the built-in rule did not fire", host)
	}
	if !strings.Contains(p.Denied.Reason, "SSH") {
		t.Errorf("denial reason %q does not say why SSH is special", p.Denied.Reason)
	}

	// The denial is recorded by the bus, through the investigation log.
	body, err := os.ReadFile(meta.Path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "**denied**") {
		t.Errorf("the denial was not recorded:\n%s", text)
	}
	if !strings.Contains(text, "sshd.service") {
		t.Errorf("the record does not name the unit that was refused:\n%s", text)
	}

	// And it cannot be executed either: the confirmation check refuses it.
	if _, err := w.rt.Bus.Execute(context.Background(), p, bus.Confirmation{ProposalID: p.ID}); err == nil {
		t.Error("a denied proposal executed")
	}
}

// TestLabRolloutCanary checks the canary → pause → continue → done shape of a
// rollout over the web hosts, with the `:run` planner and a harmless command.
func TestLabRolloutCanary(t *testing.T) {
	w := newLabW1(t)

	p, err := w.rt.Bus.Propose(context.Background(), action.Request{
		SpecID:  generic.SpecRun,
		Targets: w.genericTargets(),
		Params:  map[string]string{generic.ParamCommand: "true", generic.ParamSudo: "no"},
		Actor:   action.Human,
	})
	if err != nil {
		t.Fatalf("propose :run true: %v", err)
	}
	if p.Denied != nil {
		t.Fatalf(":run true was denied: %s", p.Denied.Reason)
	}
	if p.Rollout.Canary != 1 {
		t.Fatalf("canary = %d, want 1 from rollout.canary", p.Rollout.Canary)
	}

	sum, phases := w.rollout(t, p)
	if sum.Failed != 0 || sum.OK != len(w.hosts) {
		t.Fatalf("rollout = %+v, want %d ok", sum, len(w.hosts))
	}
	for _, want := range []bus.Phase{bus.PhaseCanary, bus.PhasePaused, bus.PhaseBatch, bus.PhaseDone} {
		if !containsPhase(phases, want) {
			t.Errorf("phases %v do not include %v", phases, want)
		}
	}
}

// genericTargets is the `:run` target list for the web hosts.
func (w *labW1) genericTargets() []action.Target {
	out := make([]action.Target, 0, len(w.hosts))
	for _, h := range w.hosts {
		out = append(out, action.Target{Host: h, Labels: w.labels[h]})
	}
	return out
}

// containsPhase reports whether phases holds p.
func containsPhase(phases []bus.Phase, p bus.Phase) bool {
	for _, have := range phases {
		if have == p {
			return true
		}
	}
	return false
}

// recordProbe writes the probe bullet into the open investigation through the
// same helper the views use, so the record is the product's, not the test's.
func recordProbe(t *testing.T, rt *m3Runtime, name string, sum group.Summary) {
	t.Helper()
	env := &tui.Env{Recorder: rt.Log}
	text := fmt.Sprintf("`%s` on %d hosts → %s", name, sum.Total, sum.Headline())
	cmd := env.AppendInvestigation(investigation.EntryProbe, text, "")
	if cmd == nil {
		t.Fatal("no recorder: the probe entry was not written")
	}
	if msg := cmd(); !isAppended(msg) {
		// Anything but the appended message is a flash carrying the failure.
		t.Fatalf("recording the probe returned %#v", msg)
	}
}

// isAppended reports whether msg is the "entry written" acknowledgement.
func isAppended(msg tea.Msg) bool {
	_, ok := msg.(tui.InvestigationAppendedMsg)
	return ok
}

// assertNoSecrets fails when the record carries something that must never be
// stored: the lab's pillar fixture value, a private key block or an AWS key.
func assertNoSecrets(t *testing.T, text string) {
	t.Helper()
	for _, marker := range []string{"not-a-real-secret", "BEGIN ", "AKIA", "aws_secret_access_key"} {
		if strings.Contains(text, marker) {
			t.Errorf("the record contains %q, which must be redacted", marker)
		}
	}
}
