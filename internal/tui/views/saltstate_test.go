package views

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/salt"
	"jumpr/internal/inventory"
	"jumpr/internal/resource"
	"jumpr/internal/targets"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
	"jumpr/internal/tui"
)

// stateMasterHost is the inventory host of the fake Salt master.
const stateMasterHost = resource.HostID("saltmaster.example.internal")

// stateJID is the job id the fake master hands out.
const stateJID = "20261112135501123456"

// stateClean is a dry-run return with one state already in place.
const stateClean = `{
  "file_|-nginx-config_|-/etc/nginx/nginx.conf_|-managed": {
    "result": true,
    "changes": {},
    "comment": "File is in the correct state",
    "name": "/etc/nginx/nginx.conf",
    "__sls__": "nginx.config",
    "__run_num__": 1,
    "__id__": "nginx-config"
  }
}`

// stateDiff is a dry-run return with one state that would change.
const stateDiffState = `{
  "service_|-nginx-service_|-nginx_|-running": {
    "result": null,
    "changes": {"diff": "--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new"},
    "comment": "Service nginx is set to start",
    "name": "nginx",
    "__sls__": "nginx.service",
    "__run_num__": 1,
    "__id__": "nginx-service"
  }
}`

// stateInv is a minimal inventory: one host per minion, mapped through the
// salt.id label so HostMinion needs no salt-key call.
type stateInv struct {
	hosts []inventory.Host
	byID  map[string]inventory.Host
}

// newStateInv builds the inventory of the given minions.
func newStateInv(minions ...string) *stateInv {
	inv := &stateInv{byID: make(map[string]inventory.Host, len(minions))}
	for _, m := range minions {
		h := inventory.Host{
			ID:      resource.HostID(m + ".lab.example.internal"),
			Aliases: []string{m},
			Labels:  map[string]string{"host": m + ".lab.example.internal", "salt.id": m, "salt.role": "web"},
		}
		inv.hosts = append(inv.hosts, h)
		inv.byID[string(h.ID)] = h
		inv.byID[m] = h
	}
	return inv
}

func (i *stateInv) Hosts() []inventory.Host { return append([]inventory.Host(nil), i.hosts...) }

func (i *stateInv) Select(resource.Selector) []inventory.Host { return nil }

func (i *stateInv) Resolve(name string) (inventory.Host, bool) {
	h, ok := i.byID[name]
	return h, ok
}

func (i *stateInv) Refresh(context.Context) map[string]error { return nil }

func (i *stateInv) Age() time.Duration { return 0 }

// stateWireResult and stateWireJob mirror jobs.list_job's JSON.
type stateWireResult struct {
	Return  json.RawMessage `json:"return"`
	Retcode int             `json:"retcode"`
	Success bool            `json:"success"`
}

type stateWireJob struct {
	JID        string                     `json:"jid"`
	Function   string                     `json:"Function"`
	Arguments  []string                   `json:"Arguments"`
	Target     any                        `json:"Target"`
	TargetType string                     `json:"Target-type"`
	User       string                     `json:"User"`
	StartTime  string                     `json:"StartTime"`
	Minions    []string                   `json:"Minions"`
	Result     map[string]stateWireResult `json:"Result"`
}

// stateMaster is a fake Salt master: every submission gets stateJID and every
// jobs.list_job answer carries the scripted returns.
type stateMaster struct {
	mu      sync.Mutex
	returns map[string]string
	fake    *transporttest.Fake
}

// newStateMaster scripts one state.apply answer per minion.
func newStateMaster(t *testing.T, returns map[string]string) *stateMaster {
	t.Helper()
	m := &stateMaster{returns: returns}
	m.fake = transporttest.New().OnFunc(m.reply)
	return m
}

func (m *stateMaster) reply(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
	switch {
	case strings.Contains(cmd.Script, "--async"):
		return transporttest.Reply{Stdout: "Executed command with job ID: " + stateJID + "\n"}, true
	case strings.Contains(cmd.Script, "jobs.list_job"):
		return transporttest.Reply{Stdout: m.jobJSON() + "\n"}, true
	}
	return transporttest.Reply{}, false
}

// jobJSON renders the job the master reports for stateJID.
func (m *stateMaster) jobJSON() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	minions := make([]string, 0, len(m.returns))
	result := make(map[string]stateWireResult, len(m.returns))
	for id, raw := range m.returns {
		minions = append(minions, id)
		result[id] = stateWireResult{Return: json.RawMessage(raw), Success: true}
	}
	sort.Strings(minions)
	b, err := json.Marshal(stateWireJob{
		JID:        stateJID,
		Function:   "state.apply",
		Arguments:  []string{"test=True"},
		Target:     minions,
		TargetType: "list",
		User:       "sudo_sre",
		StartTime:  "2026, Nov 12 13:55:01.123456",
		Minions:    minions,
		Result:     result,
	})
	if err != nil {
		return "{}"
	}
	return string(b)
}

// newStateBackend wires a real Salt backend over the fake master.
func newStateBackend(t *testing.T, m *stateMaster, minions ...string) *salt.Backend {
	t.Helper()
	r := salt.NewRunner(m.fake, stateMasterHost, salt.RunnerOptions{})
	return salt.New(salt.Options{Runner: r, Inventory: newStateInv(minions...), Master: stateMasterHost})
}

// stateHosts builds the host ids of the given minions.
func stateHosts(minions ...string) []resource.HostID {
	out := make([]resource.HostID, 0, len(minions))
	for _, m := range minions {
		out = append(out, resource.HostID(m+".lab.example.internal"))
	}
	return out
}

// stateEnv is the environment of a session with the given target hosts.
func stateEnv(hosts []resource.HostID) *tui.Env {
	return &tui.Env{Session: &tui.SessionState{Name: "s1", Targets: targets.FromHosts("all", hosts)}}
}

// driveState runs the view's commands until nothing is left to do.
func driveState(t *testing.T, v *SaltState) {
	t.Helper()
	cmd := v.Init()
	deadline := time.Now().Add(30 * time.Second)
	for cmd != nil {
		if time.Now().After(deadline) {
			t.Fatal("the state view did not settle")
		}
		_, next := v.Update(cmd())
		cmd = next
	}
}

// TestSaltStateViewGroups: 24 minions, 22 clean and 2 with the same diff group into
// two rows and the banner says so.
func TestSaltStateViewGroups(t *testing.T) {
	minions := make([]string, 0, 24)
	returns := make(map[string]string, 24)
	for i := 1; i <= 24; i++ {
		m := fmt.Sprintf("web-%02d", i)
		minions = append(minions, m)
		returns[m] = stateClean
	}
	returns["web-23"] = stateDiffState
	returns["web-24"] = stateDiffState

	m := newStateMaster(t, returns)
	b := newStateBackend(t, m, minions...)
	v := NewSaltState(stateEnv(stateHosts(minions...)), b, "")
	driveState(t, v)

	if len(v.groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(v.groups))
	}
	changed := v.groups[0]
	if len(changed.Minions) != 2 || changed.Minions[0] != "web-23" || changed.Minions[1] != "web-24" {
		t.Fatalf("first group = %v, want web-23 and web-24", changed.Minions)
	}
	if changed.SummaryLine() != "1 would change" {
		t.Errorf("group summary = %q, want 1 would change", changed.SummaryLine())
	}
	if changed.FirstChange() != "nginx-service" {
		t.Errorf("first change = %q, want nginx-service", changed.FirstChange())
	}

	frame := v.Render(120, 30)
	const banner = "24 minions · 22 clean · 2 would change · 0 failed · 0 no return"
	if !strings.Contains(frame, banner) {
		t.Fatalf("frame does not contain the banner %q:\n%s", banner, frame)
	}
	for _, want := range []string{"1 would change", "nginx-service", "clean (1 states)"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame does not contain %q:\n%s", want, frame)
		}
	}
	_ = m
}

// TestSaltStateViewNotAMinion: a session target Salt does not manage is listed as
// "not a minion", never dropped.
func TestSaltStateViewNotAMinion(t *testing.T) {
	minions := []string{"web-01", "web-02"}
	returns := map[string]string{"web-01": stateClean, "web-02": stateClean}
	m := newStateMaster(t, returns)
	b := newStateBackend(t, m, minions...)

	hosts := append(stateHosts(minions...), "db-01.lab.example.internal")
	v := NewSaltState(stateEnv(hosts), b, "nginx")
	driveState(t, v)

	if len(v.notMinion) != 1 || string(v.notMinion[0]) != "db-01.lab.example.internal" {
		t.Fatalf("notMinion = %v, want db-01", v.notMinion)
	}
	frame := v.Render(120, 30)
	if !strings.Contains(frame, "not a minion") || !strings.Contains(frame, "db-01.lab.example.internal") {
		t.Fatalf("frame does not name the host that is not a minion:\n%s", frame)
	}
	if !strings.Contains(frame, "2 minions · 2 clean") {
		t.Errorf("frame does not count the mapped minions:\n%s", frame)
	}
}

// TestSaltStateViewActionsOnGroup: x asks the root for actions over the selected
// group's minions, as salt-minion rows.
func TestSaltStateViewActionsOnGroup(t *testing.T) {
	minions := []string{"web-01", "web-02", "web-03"}
	returns := map[string]string{
		"web-01": stateClean,
		"web-02": stateDiffState,
		"web-03": stateDiffState,
	}
	m := newStateMaster(t, returns)
	b := newStateBackend(t, m, minions...)
	v := NewSaltState(stateEnv(stateHosts(minions...)), b, "")
	driveState(t, v)

	if len(v.groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(v.groups))
	}
	_, cmd := v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd == nil {
		t.Fatal("x returned no command")
	}
	msg, ok := cmd().(tui.ActionsRequestMsg)
	if !ok {
		t.Fatalf("x produced %T, want an actions request", cmd())
	}
	if msg.Kind != salt.KindMinion {
		t.Errorf("kind = %q, want %q", msg.Kind, salt.KindMinion)
	}
	if len(msg.Selection) != 2 {
		t.Fatalf("selection = %d rows, want 2", len(msg.Selection))
	}
	for i, want := range []string{"web-02", "web-03"} {
		if got := msg.Selection[i].Ref.Name; got != want {
			t.Errorf("selection %d = %q, want %q", i, got, want)
		}
		if kind := msg.Selection[i].Ref.Kind; kind != salt.KindMinion {
			t.Errorf("selection %d kind = %q, want %q", i, kind, salt.KindMinion)
		}
	}
}
