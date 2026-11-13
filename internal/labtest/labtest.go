//go:build lab

// Package labtest holds the helpers every `//go:build lab` test uses: the lab's
// jumper configuration, an SSH transport through the lab bastion, the host list,
// and the two ways a test creates (and undoes) a lab-side condition.
//
// It is compiled only with -tags lab, so `go test ./...` never builds it. Tests
// using it are named TestLab… and skip with a clear message when the lab is not
// running (BUILD-PLAN §9).
package labtest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumper/internal/config"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/ssh"
)

// labDomain is the suffix every lab host carries (SPEC §22).
const labDomain = ".lab.example.internal"

// labController is the lab controller binary (owned by the lab track).
const labController = "./cmd/sitrep-lab"

// remoteTimeout bounds one command a lab test runs.
const remoteTimeout = time.Minute

// Hosts returns the nine lab hosts, bastion first.
func Hosts() []resource.HostID {
	names := []string{"bastion", "salt", "web-01", "web-02", "web-03", "db-01", "k3s-1", "k3s-2", "chef"}
	hosts := make([]resource.HostID, len(names))
	for i, n := range names {
		hosts[i] = resource.HostID(n + labDomain)
	}
	return hosts
}

// Root returns the repository root: the nearest directory above the test's
// working directory that holds go.mod.
func Root(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("labtest: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("labtest: no go.mod above %s", dir)
		}
		dir = parent
	}
}

// Config loads lab/jumper/config.yaml and skips the test when lab/ssh_config is
// missing, which means the lab is not up. The process working directory is moved
// to the repository root, because the config's paths are relative to it.
func Config(t *testing.T) config.Config {
	t.Helper()
	root := Root(t)
	if _, err := os.Stat(filepath.Join(root, "lab", "ssh_config")); err != nil {
		t.Skip("lab not running: lab/ssh_config missing; run: go run " + labController + " up")
	}
	chdir(t, root)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("labtest: %v", err)
	}
	cfg, err := config.Load(filepath.Join("lab", "jumper", "config.yaml"), home)
	if err != nil {
		t.Fatalf("labtest: config: %v", err)
	}
	return cfg
}

// chdir moves to dir for the rest of the test and restores the old directory in
// cleanup.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("labtest: %v", err)
	}
	if old == dir {
		return
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("labtest: chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// Pool returns a pool over the lab's ssh_config with the limits from
// lab/jumper/config.yaml. Host keys are accept-new: the lab's key is regenerated
// whenever the lab is rebuilt. It is closed in cleanup.
func Pool(t *testing.T) *ssh.Pool {
	t.Helper()
	cfg := Config(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("labtest: %v", err)
	}
	resolver, err := ssh.NewResolver(cfg.SSH.ConfigFiles, home, "sre")
	if err != nil {
		t.Fatalf("labtest: resolver: %v", err)
	}
	pool := ssh.NewPool(ssh.PoolOptions{
		Resolver: resolver,
		Dial: ssh.DialOptions{
			Auth: ssh.AuthOptions{
				AgentSocket: func() string { return os.Getenv("SSH_AUTH_SOCK") },
			},
			HostKeys: ssh.HostKeyOptions{
				Files:  cfg.SSH.KnownHosts,
				Strict: "accept-new",
			},
			ConnectTimeout: cfg.SSH.ConnectTimeout.Duration(),
			Keepalive:      cfg.SSH.Keepalive.Duration(),
		},
		MaxConnsPerBastion: cfg.SSH.MaxConnsPerBastion,
		MaxChannelsPerConn: cfg.SSH.MaxChannelsPerConn,
		IdleTimeout:        cfg.SSH.IdleTimeout.Duration(),
	})
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// SSH returns a transport over Pool. The agent, or the lab key named by
// lab/ssh_config, authenticates; nothing is installed on the nodes.
func SSH(t *testing.T) *ssh.Transport {
	t.Helper()
	cfg := Config(t)
	return ssh.New(Pool(t), ssh.Options{
		ExecTimeout: cfg.SSH.ExecTimeout.Duration(),
		MaxOutput:   int(cfg.SSH.MaxOutput),
	})
}

// Fault injects the named lab fault with the lab controller and undoes it in
// cleanup. Fault names: haproxy-443, disk-fill, oom-pod, unit-flap
// (`go run ./cmd/sitrep-lab fault list`).
func Fault(t *testing.T, name string) {
	t.Helper()
	runController(t, "fault", name)
	t.Cleanup(func() {
		undoController(t, "fault", name, "--undo")
	})
}

// Break runs a lab-only shell command as root on one host and its undo in
// cleanup. It is for situations no packaged fault covers (stopping a minion,
// editing a managed file); it goes through the SSH transport, never the lab CLI.
func Break(t *testing.T, host resource.HostID, do, undo string) {
	t.Helper()
	tr := SSH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*remoteTimeout)
	defer cancel()
	res, err := tr.Exec(ctx, host, transport.Command{Script: do, Sudo: true, Timeout: remoteTimeout})
	if err != nil {
		t.Fatalf("labtest: break %s: %v", host, err)
	}
	if res.RC != 0 {
		t.Fatalf("labtest: break %s: rc %d: %s", host, res.RC, strings.TrimSpace(string(res.Stderr)))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*remoteTimeout)
		defer cancel()
		res, err := tr.Exec(ctx, host, transport.Command{Script: undo, Sudo: true, Timeout: remoteTimeout})
		if err != nil {
			t.Errorf("labtest: undo on %s: %v", host, err)
			return
		}
		if res.RC != 0 {
			t.Errorf("labtest: undo on %s: rc %d: %s", host, res.RC, strings.TrimSpace(string(res.Stderr)))
		}
	})
}

// runController runs one lab controller subcommand and fails the test when it
// does.
func runController(t *testing.T, args ...string) {
	t.Helper()
	if out, err := controller(Root(t), args...); err != nil {
		t.Fatalf("labtest: go run %s %s: %v\n%s", labController, strings.Join(args, " "), err, out)
	} else if len(out) > 0 {
		t.Logf("go run %s %s:\n%s", labController, strings.Join(args, " "), out)
	}
}

// undoController runs an undo subcommand; a failure is reported but does not
// stop the other cleanups.
func undoController(t *testing.T, args ...string) {
	t.Helper()
	if out, err := controller(Root(t), args...); err != nil {
		t.Errorf("labtest: go run %s %s: %v\n%s", labController, strings.Join(args, " "), err, out)
	}
}

// controller runs the lab controller from the repository root.
func controller(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", append([]string{"run", labController}, args...)...)
	cmd.Dir = root
	return cmd.CombinedOutput()
}
