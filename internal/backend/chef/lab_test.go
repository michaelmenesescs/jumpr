//go:build lab

package chef

// The M10 exit checks: everything here talks to the real Chef workstation in the
// lab (SPEC §22). The lab must be up; without it a test skips with the
// controller command that starts it (labtest.Config). Nothing is installed on a
// node: the drift reads are knife calls on the workstation and the why-run is
// jumper's own fan-out over SSH.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"jumper/internal/labtest"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
)

const (
	// labTimeout bounds one knife or chef-client call a test makes.
	labTimeout = 5 * time.Minute
	// labDriftTimeout bounds one drift check.
	labDriftTimeout = 2 * time.Minute
	// labDomain is the suffix every lab host carries (SPEC §22).
	labDomain = ".lab.example.internal"
	// staleAge is how far back the drift test moves db-01's check-in. It is
	// past the 2 h stale threshold and past the 24 h critical one.
	staleAge = 72 * time.Hour
)

// labEnv is the Chef stack the lab tests share.
type labEnv struct {
	ssh     transport.Executor
	runner  *Runner
	backend *Backend
	ws      resource.HostID
}

// newLabEnv builds the stack from the lab config and skips when the lab is not
// running.
func newLabEnv(t *testing.T) *labEnv {
	t.Helper()
	cfg := labtest.Config(t) // skips when lab/ssh_config is missing
	ws := resource.HostID(strings.TrimSpace(cfg.Chef.Workstation))
	if ws == "" {
		t.Skip("lab config has no chef.workstation")
	}
	ex := labtest.SSH(t)
	runner := NewRunner(ex, ws, RunnerOptions{
		Knife:   cfg.Chef.Knife,
		Sudo:    cfg.Chef.Sudo,
		Timeout: cfg.Chef.Timeout.Duration(),
	})
	return &labEnv{
		ssh:     ex,
		runner:  runner,
		backend: New(Options{Runner: runner, Workstation: ws}),
		ws:      ws,
	}
}

// webHosts returns the three lab web host ids.
func webHosts() []resource.HostID {
	return []resource.HostID{
		"web-01" + labDomain, "web-02" + labDomain, "web-03" + labDomain,
	}
}

// TestLabChefDriftSignals is requirement 4's first half: the drift table reads
// the lab's nodes, groups their run lists (the webs agree, db-01 does not) and
// shows db-01 as stale once its check-in is aged past the threshold.
//
// The lab's db-01 run fails on purpose, but chef-zero keeps no run reports and
// the node carries no run-status attribute (SPEC §20 open question 3), so the
// failed-run column stays hidden and the stale check-in is the signal that can
// be read from the server. The test creates it itself and restores it in
// cleanup.
func TestLabChefDriftSignals(t *testing.T) {
	env := newLabEnv(t)
	hosts := append(webHosts(), resource.HostID("db-01"+labDomain))

	ageCheckin(t, env, "db-01", staleAge)

	ctx, cancel := context.WithTimeout(context.Background(), labDriftTimeout)
	defer cancel()
	rep, err := env.backend.Drift(ctx, hosts, "")
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if len(rep.Rows) != 4 {
		t.Fatalf("rows = %d, want one per host", len(rep.Rows))
	}
	if rep.Attr != "" {
		t.Errorf("Attr = %q, want hidden: the lab publishes no run-status attribute", rep.Attr)
	}
	for _, row := range rep.Rows {
		if !row.Known {
			t.Errorf("%s is not a Chef node although the lab registers it", row.Host)
		}
		if !row.HasCheckin {
			t.Errorf("%s has no check-in age", row.Host)
		}
		if row.RunStatus != "" {
			t.Errorf("%s run status = %q, want the column hidden", row.Host, row.RunStatus)
		}
	}
	if got := rep.Headline(); !strings.HasPrefix(got, "3 identical") || !strings.Contains(got, "1 differ") {
		t.Errorf("Headline() = %q, want three identical web nodes and db-01 differing", got)
	}
	if len(rep.Groups) != 2 {
		t.Fatalf("groups = %+v, want the webs and db-01", rep.Groups)
	}
	if got := strings.Join(rep.Groups[0].Nodes, ","); got != "web-01,web-02,web-03" {
		t.Errorf("largest group = %q", got)
	}
	if got := strings.Join(rep.Groups[1].Nodes, ","); got != "db-01" {
		t.Errorf("differing group = %q, want db-01", got)
	}

	var db DriftRow
	for _, row := range rep.Rows {
		if row.Node == "db-01" {
			db = row
		}
	}
	if db.Status == resource.StatusOK || db.Status == resource.StatusNA {
		t.Errorf("db-01 status = %v, want the stale signal (warn or crit)", db.Status)
	}
	if db.Checkin < staleAge {
		t.Errorf("db-01 check-in = %s, want at least %s", db.Checkin, staleAge)
	}
}

// TestLabChefWhyRunGroups is requirement 4's second half: the why-run fan-out on
// the web hosts, with the Cinc client the lab nodes run, groups into one group.
func TestLabChefWhyRunGroups(t *testing.T) {
	env := newLabEnv(t)
	c := NewClient(ClientOptions{
		Exec:    env.ssh,
		Client:  cincClient,
		Timeout: labTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()

	rep, err := c.WhyRun(ctx, webHosts())
	if err != nil {
		t.Fatalf("WhyRun: %v", err)
	}
	if len(rep.Summary.Errors) > 0 {
		t.Fatalf("transport errors: %+v", rep.Summary.Errors)
	}
	if rep.Total != 3 || len(rep.Rows) != 3 {
		t.Fatalf("total = %d rows = %d, want 3 and 3", rep.Total, len(rep.Rows))
	}
	if len(rep.Summary.Groups) != 1 {
		for _, g := range rep.Summary.Groups {
			t.Logf("group %s (%s):\n%s", hostNames(g.Hosts), g.Key, g.Output)
		}
		t.Fatalf("groups = %d, want the three web hosts in one group", len(rep.Summary.Groups))
	}
	if got := len(rep.Summary.Groups[0].Hosts); got != 3 {
		t.Errorf("group size = %d, want 3", got)
	}
	for _, row := range rep.Rows {
		if row.Locked {
			t.Errorf("%s hit the run lock: %s", row.Host, row.Output)
		}
		if !row.HasCounts {
			t.Errorf("%s output has no client summary:\n%s", row.Host, row.Output)
		}
	}
}

// ageCheckin moves node's check-in back by age through `knife exec` on the
// workstation, and restores it in cleanup. knife runs as the workstation's
// user, never as root: the config it needs is in that user's home.
func ageCheckin(t *testing.T, env *labEnv, node string, age time.Duration) {
	t.Helper()
	runKnifeScript(t, env, node, fmt.Sprintf("Time.now.to_f - %d", int64(age.Seconds())))
	t.Cleanup(func() {
		runKnifeScript(t, env, node, "Time.now.to_f")
	})
}

// runKnifeScript sets node's ohai_time to the given Ruby expression and saves
// the node. The script is one quoted argument, built by shq.
func runKnifeScript(t *testing.T, env *labEnv, node, expr string) {
	t.Helper()
	ruby := "n = Chef::Node.load('" + node + "'); n.ohai_time = " + expr + "; n.save"
	line := shq.Join("knife", "exec", "-E", ruby)
	ctx, cancel := context.WithTimeout(context.Background(), labTimeout)
	defer cancel()
	res, err := env.ssh.Exec(ctx, env.ws, transport.Command{Script: line, Timeout: labTimeout})
	if err != nil {
		t.Fatalf("knife exec on %s: %v", env.ws, err)
	}
	if res.RC != 0 {
		t.Fatalf("knife exec on %s: rc %d: %s", env.ws, res.RC, strings.TrimSpace(string(res.Stderr)))
	}
}
