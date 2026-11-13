//go:build lab

package linux

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumper/internal/fanout"
	"jumper/internal/group"
	"jumper/internal/labtest"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

// labListTimeout bounds one lab listing in this file.
const labListTimeout = 3 * time.Minute

// labRows runs one List over the given hosts and returns every row. A per-host
// error fails the test: these checks are about the lab answering, not about how
// the backend degrades.
func labRows(t *testing.T, lab *labEnv, q resource.Query) []resource.Resource {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), labListTimeout)
	defer cancel()
	ch, err := lab.backend.List(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q.Kind, err)
	}
	var rows []resource.Resource
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("%s on %s: %v", q.Kind, res.Host, res.Err)
		}
		rows = append(rows, res.Items...)
	}
	return rows
}

// rowForHost returns the single row of host, or fails the test.
func rowForHost(t *testing.T, rows []resource.Resource, host resource.HostID) resource.Resource {
	t.Helper()
	for _, r := range rows {
		if r.Ref.Host == host {
			return r
		}
	}
	t.Fatalf("no row for %s among %d rows", host, len(rows))
	return resource.Resource{}
}

// TestLabHostsOverview is the M2 exit check for the host table: every lab host
// answers the overview probe and reports its distribution, and the fleet is not
// uniform (the lab covers three distros on purpose).
func TestLabHostsOverview(t *testing.T) {
	lab := newLabEnv(t)
	hosts := labtest.Hosts()

	rows := labRows(t, lab, resource.Query{Kind: KindHost, Hosts: hosts})
	distros := make(map[string]bool)
	for _, host := range hosts {
		r := rowForHost(t, rows, host)
		osName := strings.TrimSpace(r.Str("os"))
		if osName == "" {
			t.Errorf("%s: empty os", host)
			continue
		}
		distros[osName] = true
		if strings.TrimSpace(r.Str("state")) == "" {
			t.Errorf("%s: empty state", host)
		}
	}
	if len(distros) < 3 {
		t.Fatalf("%d distinct distributions (%v), want at least 3", len(distros), keys(distros))
	}
}

// TestLabUnitsContainSSH checks the unit list of every host: sshd is what the
// transport itself runs over, so a missing ssh unit means the probe is wrong.
func TestLabUnitsContainSSH(t *testing.T) {
	lab := newLabEnv(t)
	for _, host := range labtest.Hosts() {
		rows := labRows(t, lab, resource.Query{Kind: KindUnit, Hosts: []resource.HostID{host}})
		found := false
		for _, r := range rows {
			unit := r.Str("unit")
			if unit == "sshd.service" || unit == "ssh.service" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: no sshd/ssh unit among %d units", host, len(rows))
		}
	}
}

// TestLabSocketsWithSudo checks that the sudo socket probe keeps the process
// column: the :22 listener must name the process holding it.
func TestLabSocketsWithSudo(t *testing.T) {
	lab := newLabEnv(t)
	for _, host := range labtest.Hosts() {
		rows := labRows(t, lab, resource.Query{Kind: KindSocket, Hosts: []resource.HostID{host}})
		found := false
		for _, r := range rows {
			if !isPort22(r.Str("local")) {
				continue
			}
			if strings.TrimSpace(r.Str("process")) == "" {
				t.Errorf("%s: :22 listener has no process (%q)", host, r.Str("local"))
				continue
			}
			found = true
		}
		if !found {
			t.Errorf("%s: no :22 listener with a process among %d sockets", host, len(rows))
		}
	}
}

// TestLabJournalWindow checks that a time window really bounds the journal
// rows: every row must fall inside it.
func TestLabJournalWindow(t *testing.T) {
	lab := newLabEnv(t)
	now := time.Now()
	window, err := resource.ParseTimeWindow("-2h", now)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	host := resource.HostID("web-01" + labDomain)
	rows := labRows(t, lab, resource.Query{
		Kind:   KindJournal,
		Hosts:  []resource.HostID{host},
		Window: window,
		Args:   map[string]string{"limit": "100"},
	})
	if len(rows) == 0 {
		t.Fatalf("%s: no journal rows in %s", host, window.Label)
	}
	for _, r := range rows {
		ts, ok := r.Time("time")
		if !ok {
			t.Fatalf("%s: row without a time: %+v", host, r.Fields)
		}
		if ts.Before(window.From.Add(-time.Minute)) || ts.After(now.Add(time.Minute)) {
			t.Fatalf("%s: journal row %s outside the window %s", host, ts, window.Label)
		}
	}
}

// TestLabFailedUnitsAfterFault injects the :443 conflict and checks the two
// things the M2 demo shows: nginx really is failed on the affected host, and
// the failed-units probe groups that host apart from the rest of the fleet.
func TestLabFailedUnitsAfterFault(t *testing.T) {
	lab := newLabEnv(t)
	labtest.Fault(t, "haproxy-443")

	hosts := labWebHosts()
	probe, ok := catalogProbe("failed-units")
	if !ok {
		t.Fatal("the catalog has no failed-units probe")
	}

	ctx, cancel := context.WithTimeout(context.Background(), labListTimeout)
	defer cancel()
	g := group.NewGrouper(len(hosts), func(s string) string {
		lines := strings.Split(strings.TrimSpace(s), "\n")
		for i, line := range lines {
			lines[i] = strings.TrimSpace(line)
		}
		return strings.Join(lines, "\n")
	})
	for hr := range fanout.Run(ctx, lab.exec, hosts,
		transport.Command{Script: probe.Cmd, Timeout: 30 * time.Second}, fanout.Options{}) {
		if hr.Err != nil {
			g.AddError(hr.Host, hr.Err)
			continue
		}
		g.Add(hr.Host, string(hr.Result.Stdout), hr.Result.RC)
	}
	summary := g.Summary()
	if len(summary.Errors) > 0 {
		t.Fatalf("failed-units errors = %v", summary.Errors)
	}

	differing := summary.Differing()
	if len(differing) == 0 {
		t.Fatalf("every host reports the same failed units, want one host apart:\n%s", summary.Headline())
	}
	affected := resource.HostID("web-01" + labDomain)
	var hit bool
	for _, grp := range differing {
		if !strings.Contains(grp.Output, "nginx.service") {
			continue
		}
		for _, h := range grp.Hosts {
			if h == affected {
				hit = true
			}
		}
	}
	if !hit {
		t.Fatalf("%s is not in the differing group with nginx.service:\n%s", affected, summary.Headline())
	}
}

// isPort22 reports whether a local address ends in :22.
func isPort22(local string) bool {
	return strings.HasSuffix(local, ":22") || strings.Contains(local, ":22 ")
}

// keys returns the keys of a string set, for a readable failure message.
func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// catalogProbe returns the named probe from the built-in catalog.
func catalogProbe(name string) (Probe, bool) {
	for _, p := range Catalog() {
		if p.Name == name {
			return p, true
		}
	}
	return Probe{}, false
}
