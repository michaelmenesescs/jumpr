//go:build lab

package linux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumper/internal/fanout"
	"jumper/internal/group"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
	"jumper/internal/transport/ssh"
)

// labDomain is the FQDN suffix of every lab machine.
const labDomain = ".lab.example.internal"

// labWebHosts are the web nodes of `:unitdiff`'s fixture: two Rocky and one
// Ubuntu host, so the fleet is not uniform on purpose.
func labWebHosts() []resource.HostID {
	return []resource.HostID{"web-01" + labDomain, "web-02" + labDomain, "web-03" + labDomain}
}

// labEnv is the M6 lab harness: a real pooled SSH transport and the Linux
// backend over it.
type labEnv struct {
	backend *Backend
	exec    *ssh.Transport
	pool    *ssh.Pool
}

// newLabEnv builds the harness or skips the test when the lab is not up. The
// lab is created with `go run ./cmd/@@ up`.
func newLabEnv(t *testing.T) *labEnv {
	t.Helper()
	cfg := os.Getenv("JUMPER_LAB_SSH_CONFIG")
	if cfg == "" {
		cfg = filepath.Join("..", "..", "..", "lab", "ssh_config")
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Skipf("lab ssh config %s is missing; run `go run ./cmd/@@ up`", cfg)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	resolver, err := ssh.NewResolver([]string{cfg}, home, "sre")
	if err != nil {
		t.Fatalf("ssh config: %v", err)
	}
	pool := ssh.NewPool(ssh.PoolOptions{
		Resolver: resolver,
		Dial: ssh.DialOptions{
			ConnectTimeout: 5 * time.Second,
			Keepalive:      30 * time.Second,
		},
	})
	t.Cleanup(func() { _ = pool.Close() })
	ex := ssh.New(pool, ssh.Options{ExecTimeout: 30 * time.Second})
	return &labEnv{
		backend: New(Options{Exec: ex, Timeout: 30 * time.Second}),
		exec:    ex,
		pool:    pool,
	}
}

// listHost runs a kind's List for one host and returns its rows.
func (l *labEnv) listHost(t *testing.T, kind resource.KindName, host resource.HostID) []resource.Resource {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := l.backend.List(ctx, resource.Query{Kind: kind, Hosts: []resource.HostID{host}})
	if err != nil {
		t.Fatalf("%s on %s: %v", kind, host, err)
	}
	var rows []resource.Resource
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("%s on %s: %v", kind, host, res.Err)
		}
		rows = append(rows, res.Items...)
	}
	return rows
}

// execHost runs one read-only command on one host and returns its stdout.
func (l *labEnv) execHost(t *testing.T, host resource.HostID, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := l.exec.Exec(ctx, host, transport.Command{Script: script, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("%q on %s: %v", script, host, err)
	}
	return string(res.Stdout)
}

// TestLabM6 is the M6 exit check: the unit detail, mounts, interfaces, the unit
// diff grouping and the process drill-in against the real lab machines.
func TestLabM6(t *testing.T) {
	lab := newLabEnv(t)

	t.Run("UnitDetail", func(t *testing.T) {
		// One host per distro the lab covers: Rocky 9 and Ubuntu 24.04/22.04.
		for _, tc := range []struct {
			host string
			unit string
		}{
			{"web-01" + labDomain, "sshd.service"},
			{"web-03" + labDomain, "ssh.service"},
			{"db-01" + labDomain, "ssh.service"},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			info, err := lab.backend.UnitInfo(ctx, resource.HostID(tc.host), tc.unit)
			cancel()
			if err != nil {
				t.Fatalf("%s on %s: %v", tc.unit, tc.host, err)
			}
			if strings.TrimSpace(info.Status) == "" {
				t.Errorf("%s on %s: empty status", tc.unit, tc.host)
			}
			if len(info.Props) == 0 {
				t.Errorf("%s on %s: empty systemctl show", tc.unit, tc.host)
			}
			if strings.TrimSpace(info.Cat) == "" {
				t.Errorf("%s on %s: empty systemctl cat", tc.unit, tc.host)
			}
		}
	})

	t.Run("Timers", func(t *testing.T) {
		out := lab.execHost(t, "web-01"+labDomain,
			shq.Join("systemctl", "list-timers", "--all", "--plain", "--no-legend", "--no-pager"))
		if strings.TrimSpace(out) == "" {
			t.Error("the lab host lists no systemd timers")
		}
	})

	t.Run("Mounts", func(t *testing.T) {
		rows := lab.listHost(t, KindMount, "web-01"+labDomain)
		for _, r := range rows {
			if r.Str("mount") == "/" {
				return
			}
		}
		t.Fatalf("the mount list has no %q row (%d rows)", "/", len(rows))
	})

	t.Run("Interfaces", func(t *testing.T) {
		rows := lab.listHost(t, KindInterface, "web-01"+labDomain)
		for _, r := range rows {
			addrs := r.Str("addrs")
			if addrs == "" || strings.HasPrefix(addrs, "127.") || strings.Contains(addrs, "::1") {
				continue
			}
			return
		}
		t.Fatalf("no interface carries a non-loopback address (%d rows)", len(rows))
	})

	t.Run("UnitDiff", func(t *testing.T) {
		hosts := labWebHosts()
		script := shq.Join("systemctl", "show", "nginx.service",
			"-p", "ActiveState,SubState,UnitFileState,Restart,RestartUSec", "--no-pager")
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		g := group.NewGrouper(len(hosts), func(s string) string {
			lines := strings.Split(strings.TrimSpace(s), "\n")
			for i, line := range lines {
				lines[i] = strings.TrimSpace(line)
			}
			return strings.Join(lines, "\n")
		})
		for hr := range fanout.Run(ctx, lab.exec, hosts,
			transport.Command{Script: script, Timeout: 30 * time.Second}, fanout.Options{}) {
			if hr.Err != nil {
				g.AddError(hr.Host, hr.Err)
				continue
			}
			g.Add(hr.Host, string(hr.Result.Stdout), hr.Result.RC)
		}
		summary := g.Summary()
		if len(summary.Errors) > 0 {
			t.Fatalf(":unitdiff errors = %v", summary.Errors)
		}
		if len(summary.Groups) > 2 {
			t.Fatalf("go %d groups across %d web hosts, want at most 2:\n%s",
				len(summary.Groups), len(hosts), summary.Headline())
		}
	})

	t.Run("ProcessDrillIn", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		info, err := lab.backend.ProcInfo(ctx, "web-01"+labDomain, "1")
		if err != nil {
			t.Fatalf("ProcInfo(1): %v", err)
		}
		if len(info.Status) == 0 || info.Name == "" {
			t.Errorf("pid 1 status is empty: %+v", info.Status)
		}
		if info.Exe == "" {
			t.Error("pid 1 has no exe link")
		}
		if len(info.Limits) == 0 {
			t.Error("pid 1 limits are empty")
		}
		if info.Cgroup.Path == "" {
			t.Error("pid 1 has no cgroup")
		}
		if info.FDs <= 0 {
			t.Errorf("pid 1 open-file count = %d", info.FDs)
		}
	})
}
