// Command @@ manages the local OrbStack test lab used to develop and test jumper.
//
// The lab is a set of OrbStack Linux machines (named sr-<name>) behind a bastion, with a Salt master,
// a two-node k3s cluster, and fault-injection scenarios that reproduce real incidents. Everything it
// installs lives only in these throwaway lab machines; jumper itself never installs anything on nodes.
//
// Usage:
//
//	@@ up [machine...]       create + provision machines (idempotent)
//	@@ down [--stop]         delete (or stop) all lab machines
//	@@ status                machine state, IPs, salt and k8s health
//	@@ ssh-config            regenerate lab/ssh_config
//	@@ fault list            list fault scenarios
//	@@ fault <name> [--undo] inject (or revert) a fault scenario
//	@@ capture linux        record the Linux probes from the lab into testdata/
//	@@ capture salt         record the Salt CLI output from the lab into testdata/
//	@@ sanitize <file...>   sanitize captured files in place
package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"jumper/internal/config"
	"jumper/internal/devtools"
	"jumper/internal/transport"
	"jumper/internal/transport/ssh"
)

//go:embed provision/*.sh
var provision embed.FS

// Machine is one lab node.
type Machine struct {
	Name   string // short name; OrbStack machine is "sr-"+Name, FQDN is Name+".lab.example.internal"
	Distro string // OrbStack distro:version
	Role   string // bastion | salt-master | web | db | k3s-server | k3s-agent | chef
}

// Machines is the lab topology.
var Machines = []Machine{
	{"bastion", "ubuntu:24.04", "bastion"},
	{"salt", "rocky:9", "salt-master"},
	{"web-01", "rocky:9", "web"},
	{"web-02", "rocky:9", "web"},
	{"web-03", "ubuntu:24.04", "web"},
	{"db-01", "ubuntu:22.04", "db"},
	{"k3s-1", "ubuntu:24.04", "k3s-server"},
	{"k3s-2", "ubuntu:24.04", "k3s-agent"},
	{"chef", "ubuntu:22.04", "chef"},
}

const domain = ".lab.example.internal"

var labDir, keysDir, logsDir string

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "@@:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	labDir = filepath.Join(root, "lab")
	keysDir = filepath.Join(labDir, ".keys")
	logsDir = filepath.Join(labDir, ".logs")
	if len(args) == 0 {
		return errors.New("usage: @@ up|down|status|ssh-config|fault ...")
	}
	switch args[0] {
	case "up":
		return up(args[1:])
	case "down":
		return down(len(args) > 1 && args[1] == "--stop")
	case "status":
		return status()
	case "ssh-config":
		return writeSSHConfig()
	case "fault":
		return fault(args[1:])
	case "capture":
		return capture(args[1:])
	case "sanitize":
		return sanitizeFiles(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func repoRoot() (string, error) {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run inside the jumper repo (no go.mod found)")
		}
		dir = parent
	}
}

// ---- OrbStack helpers ----

// orbRun runs a script as root inside a machine, feeding it on stdin (never shares our stdin).
func orbRun(m, script string, args ...string) (string, error) {
	argv := append([]string{"-m", "sr-" + m, "-u", "root", "bash", "-s", "--"}, args...)
	cmd := exec.Command("orb", argv...)
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s: %w\n%s", m, err, tail(out.String(), 12))
	}
	return out.String(), nil
}

func script(name string) string {
	b, err := provision.ReadFile("provision/" + name + ".sh")
	if err != nil {
		panic(err)
	}
	return string(b)
}

func existing() map[string]bool {
	out, _ := exec.Command("orb", "list", "-q").Output()
	set := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		set[l] = true
	}
	return set
}

func ipOf(m string) (string, error) {
	out, err := orbRun(m, "ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1")
	ip := strings.TrimSpace(out)
	if err != nil || ip == "" {
		return "", fmt.Errorf("no IP for %s: %v", m, err)
	}
	return ip, nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// parallel runs fn for each machine concurrently, logging to lab/.logs/<name>.<step>.log.
func parallel(ms []Machine, step string, fn func(Machine) (string, error)) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, m := range ms {
		wg.Add(1)
		go func(m Machine) {
			defer wg.Done()
			start := time.Now()
			out, err := fn(m)
			_ = os.WriteFile(filepath.Join(logsDir, m.Name+"."+step+".log"), []byte(out), 0o644)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				fmt.Printf("  ✗ %-8s %s (%s)\n", m.Name, step, time.Since(start).Round(time.Second))
			} else {
				fmt.Printf("  ✓ %-8s %s (%s)\n", m.Name, step, time.Since(start).Round(time.Second))
			}
		}(m)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func selectMachines(names []string) ([]Machine, error) {
	if len(names) == 0 {
		return Machines, nil
	}
	var out []Machine
	for _, n := range names {
		found := false
		for _, m := range Machines {
			if m.Name == n {
				out, found = append(out, m), true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown machine %q", n)
		}
	}
	return out, nil
}

func has(ms []Machine, name string) bool {
	for _, m := range ms {
		if m.Name == name {
			return true
		}
	}
	return false
}

// ---- up / down ----

func up(names []string) error {
	ms, err := selectMachines(names)
	if err != nil {
		return err
	}
	for _, d := range []string{keysDir, logsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	key := filepath.Join(keysDir, "id_ed25519")
	if _, err := os.Stat(key); err != nil {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "@@", "-f", key).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen: %v %s", err, out)
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}

	fmt.Println("==> machines + base")
	have := existing()
	if err := parallel(ms, "base", func(m Machine) (string, error) {
		if !have["sr-"+m.Name] {
			if out, err := exec.Command("orb", "create", m.Distro, "sr-"+m.Name).CombinedOutput(); err != nil {
				return string(out), fmt.Errorf("create %s: %v", m.Name, err)
			}
		}
		return orbRun(m.Name, script("base"), strings.TrimSpace(string(pub)))
	}); err != nil {
		return err
	}

	saltIP, err := ipOf("salt")
	if err != nil {
		return err
	}
	if has(ms, "salt") {
		fmt.Println("==> salt master", saltIP)
		if err := parallel([]Machine{{Name: "salt"}}, "salt-master", func(Machine) (string, error) {
			out, err := orbRun("salt", script("salt-minion"), saltIP, "salt", "master")
			if err != nil {
				return out, err
			}
			out2, err := orbRun("salt", script("salt-master")+grainsSnippet("salt"))
			return out + out2, err
		}); err != nil {
			return err
		}
	}

	fmt.Println("==> roles + salt minions")
	var minions []Machine
	for _, m := range ms {
		switch m.Role {
		case "bastion", "salt-master", "chef":
		default:
			minions = append(minions, m)
		}
	}
	if err := parallel(minions, "role", func(m Machine) (string, error) {
		var log strings.Builder
		if s := map[string]string{"web": "web", "db": "db"}[m.Role]; s != "" {
			out, err := orbRun(m.Name, script(s))
			log.WriteString(out)
			if err != nil {
				return log.String(), err
			}
		}
		grain := m.Role
		if strings.HasPrefix(grain, "k3s") {
			grain = "k8s"
		}
		out, err := orbRun(m.Name, script("salt-minion")+grainsSnippet(grain), saltIP, m.Name)
		log.WriteString(out)
		return log.String(), err
	}); err != nil {
		return err
	}
	out, _ := orbRun("salt", "sleep 8; salt-key -A -y >/dev/null; sleep 4; salt '*' test.ping --out=txt 2>/dev/null | sort")
	fmt.Print(indent(out))

	if has(ms, "k3s-1") || has(ms, "k3s-2") {
		fmt.Println("==> k3s")
		if out, err := orbRun("k3s-1", script("k3s-server")); err != nil {
			return fmt.Errorf("k3s server: %v %s", err, out)
		}
		k3sIP, err := ipOf("k3s-1")
		if err != nil {
			return err
		}
		token, err := orbRun("k3s-1", "cat /var/lib/rancher/k3s/server/node-token")
		if err != nil {
			return err
		}
		if out, err := orbRun("k3s-2", script("k3s-agent"), k3sIP, strings.TrimSpace(token)); err != nil {
			return fmt.Errorf("k3s agent: %v %s", err, out)
		}
		kc, err := orbRun("k3s-1", "cat /etc/rancher/k3s/k3s.yaml")
		if err != nil {
			return err
		}
		kc = strings.ReplaceAll(kc, "127.0.0.1", k3sIP)
		kc = strings.ReplaceAll(kc, ": default\n", ": @@\n")
		if err := os.WriteFile(filepath.Join(keysDir, "kubeconfig"), []byte(kc), 0o600); err != nil {
			return err
		}
		fmt.Println("  ✓ kubeconfig → lab/.keys/kubeconfig (context @@)")
	}

	if has(ms, "chef") {
		fmt.Println("==> chef (chef-zero :8889 + knife)")
		if err := parallel([]Machine{{Name: "chef"}}, "chef", func(Machine) (string, error) {
			return orbRun("chef", script("chef"))
		}); err != nil {
			return err
		}
		chefIP, err := ipOf("chef")
		if err != nil {
			return err
		}
		var nodes []Machine
		for _, m := range Machines {
			if m.Role == "web" || m.Role == "db" {
				nodes = append(nodes, m)
			}
		}
		if err := parallel(nodes, "chef-node", func(m Machine) (string, error) {
			return orbRun(m.Name, script("chef-node"), chefIP, m.Name, m.Role)
		}); err != nil {
			return err
		}
		out, _ := orbRun("chef", "cd /home/sre/chef-repo && sudo -u sre -H knife status 2>&1")
		fmt.Print(indent(out))
	}

	if err := writeSSHConfig(); err != nil {
		return err
	}
	fmt.Println("done.\n  ssh -F lab/ssh_config web-01" + domain + "\n  KUBECONFIG=lab/.keys/kubeconfig kubectl get nodes")
	return nil
}

func grainsSnippet(role string) string {
	return fmt.Sprintf("\nprintf 'grains:\\n  role: %s\\n  dc: lab\\n' > /etc/salt/minion.d/grains.conf\nsystemctl restart salt-minion\n", role)
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ") + "\n"
}

func down(stop bool) error {
	have := existing()
	for _, m := range Machines {
		if !have["sr-"+m.Name] {
			continue
		}
		verb := []string{"delete", "-f", "sr-" + m.Name}
		if stop {
			verb = []string{"stop", "sr-" + m.Name}
		}
		if out, err := exec.Command("orb", verb...).CombinedOutput(); err != nil {
			fmt.Printf("  ✗ %s: %s\n", m.Name, strings.TrimSpace(string(out)))
			continue
		}
		fmt.Printf("  ✓ %s %s\n", verb[0], m.Name)
	}
	return nil
}

// ---- ssh config / status ----

func writeSSHConfig() error {
	have := existing()
	var b strings.Builder
	fmt.Fprintf(&b, "# generated by @@ — use: ssh -F lab/ssh_config web-01%s\n", domain)
	fmt.Fprintf(&b, "Host *%s\n  User sre\n  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n  StrictHostKeyChecking accept-new\n  ConnectTimeout 5\n",
		domain, filepath.Join(keysDir, "id_ed25519"), filepath.Join(keysDir, "known_hosts"))
	for _, m := range Machines {
		if !have["sr-"+m.Name] {
			continue
		}
		ip, err := ipOf(m.Name)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "\nHost %s%s\n  HostName %s\n", m.Name, domain, ip)
		if m.Name != "bastion" {
			fmt.Fprintf(&b, "  ProxyJump bastion%s\n", domain)
		}
	}
	if err := os.WriteFile(filepath.Join(labDir, "ssh_config"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Println("  ✓ lab/ssh_config")
	return nil
}

func status() error {
	have := existing()
	for _, m := range Machines {
		st := "absent"
		ip := ""
		if have["sr-"+m.Name] {
			st = "present"
			ip, _ = ipOf(m.Name)
		}
		fmt.Printf("%-8s %-13s %-12s %-8s %s\n", m.Name, m.Distro, m.Role, st, ip)
	}
	if have["sr-salt"] {
		out, _ := orbRun("salt", "salt '*' test.ping --out=txt --timeout=5 2>/dev/null | sort")
		fmt.Print("\nsalt:\n" + indent(out))
	}
	if have["sr-k3s-1"] {
		out, _ := orbRun("k3s-1", "k3s kubectl get nodes --no-headers")
		fmt.Print("\nk8s:\n" + indent(out))
	}
	return nil
}

// ---- faults ----

// Fault is a reproducible incident scenario.
type Fault struct {
	Desc   string
	Inject func() error
	Undo   func() error
}

var faults = map[string]Fault{
	"haproxy-443": {
		Desc: "web-01/web-02: a stray haproxy (salt grain role=lb) grabs :443, nginx fails to bind (mockup W1)",
		Inject: func() error {
			return each([]string{"web-01", "web-02"}, `
dnf -y -q install haproxy >/dev/null
cat > /etc/haproxy/haproxy.cfg <<'C'
global
  daemon
defaults
  mode tcp
  timeout connect 5s
  timeout client 30s
  timeout server 30s
frontend fe_https
  bind *:443
  default_backend be
backend be
  server local 127.0.0.1:8443
C
printf 'grains:\n  role: [web, lb]\n  dc: lab\n' > /etc/salt/minion.d/grains.conf
systemctl restart salt-minion
systemctl stop nginx; systemctl enable --now haproxy; systemctl restart nginx || true
systemctl is-failed nginx`)
		},
		Undo: func() error {
			return each([]string{"web-01", "web-02"}, `
systemctl disable --now haproxy 2>/dev/null || true
printf 'grains:\n  role: web\n  dc: lab\n' > /etc/salt/minion.d/grains.conf
systemctl restart salt-minion
systemctl reset-failed nginx; systemctl restart nginx; systemctl is-active nginx`)
		},
	},
	"disk-fill": {
		Desc: "db-01: fill / to ~92% with a ballast file under /var (DiskWillFillIn4h)",
		Inject: func() error {
			return each([]string{"db-01"}, `
avail=$(df -B1 --output=avail / | tail -1); size=$(df -B1 --output=size / | tail -1)
target=$(( size * 92 / 100 - (size - avail) )); [ $target -gt 0 ] && fallocate -l $target /var/jumper-ballast
df -h / | tail -1`)
		},
		Undo: func() error { return each([]string{"db-01"}, "rm -f /var/jumper-ballast; df -h / | tail -1") },
	},
	"oom-pod": {
		Desc: "k8s: deployment checkout/checkout crash-loops with OOMKilled (mockup W3)",
		Inject: func() error {
			return each([]string{"k3s-1"}, `
k3s kubectl create namespace checkout --dry-run=client -o yaml | k3s kubectl apply -f - >/dev/null
cat <<'Y' | k3s kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata: {name: checkout, namespace: checkout}
spec:
  replicas: 2
  selector: {matchLabels: {app: checkout}}
  template:
    metadata: {labels: {app: checkout}}
    spec:
      containers:
      - name: checkout
        image: python:3.13-alpine
        command: ["python", "-c", "import time; print('Starting checkout', flush=True); time.sleep(5); b = bytearray(400*1024*1024); time.sleep(3600)"]
        resources: {limits: {memory: 128Mi}, requests: {memory: 64Mi}}
Y`)
		},
		Undo: func() error { return each([]string{"k3s-1"}, "k3s kubectl delete namespace checkout --wait=false") },
	},
	"unit-flap": {
		Desc: "web-03: a oneshot-looking service that crashes every few seconds (restart counter climbs)",
		Inject: func() error {
			return each([]string{"web-03"}, `
cat > /etc/systemd/system/report-sync.service <<'U'
[Unit]
Description=Nightly report sync (lab fault)
[Service]
ExecStart=/bin/sh -c 'echo "connecting to reports.example.internal:5432"; sleep 2; echo "FATAL: connection refused" >&2; exit 3'
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
U
systemctl daemon-reload; systemctl enable --now report-sync`)
		},
		Undo: func() error {
			return each([]string{"web-03"}, "systemctl disable --now report-sync; rm -f /etc/systemd/system/report-sync.service; systemctl daemon-reload; systemctl reset-failed")
		},
	},
}

func each(ms []string, sh string) error {
	var errs []error
	for _, m := range ms {
		out, err := orbRun(m, sh)
		fmt.Print(indent(m + ": " + strings.TrimSpace(out)))
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func fault(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		names := make([]string, 0, len(faults))
		for n := range faults {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("%-12s %s\n", n, faults[n].Desc)
		}
		return nil
	}
	f, ok := faults[args[0]]
	if !ok {
		return fmt.Errorf("unknown fault %q (try: @@ fault list)", args[0])
	}
	if len(args) > 1 && args[1] == "--undo" {
		return f.Undo()
	}
	return f.Inject()
}

// capture records real lab output into testdata/. It only writes local files;
// the remote side runs the read-only probe commands (task 053) or the Salt CLI
// (task 067).
func capture(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: @@ capture linux|salt [--config-dir dir] [--out dir]")
	}
	switch args[0] {
	case "linux":
		return captureLinux(args[1:])
	case "salt":
		return captureSalt(args[1:])
	}
	return fmt.Errorf("unknown capture target %q (use linux or salt)", args[0])
}

// captureLinux records the Linux probes.
func captureLinux(args []string) error {
	fs := flag.NewFlagSet("capture linux", flag.ContinueOnError)
	configDir := fs.String("config-dir", filepath.Join("lab", "jumper"), "directory holding the lab's config.yaml")
	outDir := fs.String("out", filepath.Join("testdata", "linux"), "testdata root to write under")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	exec, closeExec, err := labTransport(*configDir)
	if err != nil {
		return err
	}
	defer closeExec()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return devtools.CaptureLinux(ctx, devtools.Options{
		Exec:      exec,
		OutDir:    *outDir,
		LocalUser: os.Getenv("USER"),
	}, os.Stdout)
}

// captureSalt records the Salt CLI's real output (task 067).
func captureSalt(args []string) error {
	fs := flag.NewFlagSet("capture salt", flag.ContinueOnError)
	configDir := fs.String("config-dir", filepath.Join("lab", "jumper"), "directory holding the lab's config.yaml")
	outDir := fs.String("out", filepath.Join("testdata", "salt"), "testdata directory to write under")
	if err := fs.Parse(args); err != nil {
		return err
	}
	exec, closeExec, err := labTransport(*configDir)
	if err != nil {
		return err
	}
	defer closeExec()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	return devtools.CaptureSalt(ctx, devtools.SaltOptions{
		Exec:      exec,
		OutDir:    *outDir,
		LocalUser: os.Getenv("USER"),
	}, os.Stdout)
}

// sanitizeFiles rewrites captured files in place with the default lab name map.
func sanitizeFiles(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: @@ sanitize <file...>")
	}
	hostMap := devtools.HostMap(devtools.CaptureHosts(), os.Getenv("USER"))
	for _, path := range args {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, devtools.Sanitize(b, hostMap), 0o644); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, path)
	}
	return nil
}

// labTransport builds an SSH transport over the lab's ssh_config with the limits
// from the lab's jumper configuration. The returned close function releases the
// connection pool.
func labTransport(configDir string) (transport.Executor, func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, err
	}
	cfgPath := filepath.Join(configDir, "config.yaml")
	cfg, err := config.Load(cfgPath, home)
	if err != nil {
		return nil, nil, err
	}
	if len(cfg.SSH.ConfigFiles) == 0 {
		return nil, nil, fmt.Errorf("%s: ssh.config_files is empty", cfgPath)
	}
	if _, err := os.Stat(config.ExpandHome(cfg.SSH.ConfigFiles[0], home)); err != nil {
		return nil, nil, fmt.Errorf("lab not running: %s missing; run: go run ./cmd/sitrep-lab up", cfg.SSH.ConfigFiles[0])
	}
	resolver, err := ssh.NewResolver(cfg.SSH.ConfigFiles, home, "sre")
	if err != nil {
		return nil, nil, err
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
	tr := ssh.New(pool, ssh.Options{
		ExecTimeout: cfg.SSH.ExecTimeout.Duration(),
		MaxOutput:   int(cfg.SSH.MaxOutput),
	})
	return tr, func() { _ = pool.Close() }, nil
}
