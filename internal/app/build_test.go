package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	gossh "golang.org/x/crypto/ssh"

	"jumpr/internal/config"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/ssh/sshtest"
	"jumpr/internal/tui"
	"jumpr/internal/tui/theme"
)

// buildWait bounds how long the test waits for one command to come back.
const buildWait = 2 * time.Second

// hostNames are the static inventory's hosts; every one is served by its own
// in-process SSH server, so nothing outside loopback is touched. They are ssh
// aliases, so they stay short enough for the HOST column.
var hostNames = []string{"web-01", "web-02"}

// buildFixture is a temp config directory: an ssh_config with one entry per
// test SSH server, the key and known_hosts they need, a static inventory and
// config.yaml pointing at both.
type buildFixture struct {
	home    string
	cfgDir  string
	cfgPath string
}

// newBuildFixture starts one sshtest server per name and writes the temp
// config. Every value is loopback-only.
func newBuildFixture(t *testing.T, names []string, inventoryYAML string) *buildFixture {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cfgDir := filepath.Join(dir, "config")
	for _, d := range []string{home, cfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	keyPath, pub := writeTestKey(t, dir)
	knownPath := filepath.Join(dir, "known_hosts")

	var sshCfg, known strings.Builder
	for _, name := range names {
		srv := startTestServer(t, pub)
		fmt.Fprintf(&sshCfg, "Host %s\n  HostName 127.0.0.1\n  Port %d\n  User tester\n"+
			"  IdentityFile %s\n  UserKnownHostsFile %s\n"+
			"  StrictHostKeyChecking yes\n  ConnectTimeout 5\n\n",
			name, srv.Port(), keyPath, knownPath)
		fmt.Fprintf(&known, "[127.0.0.1]:%d %s", srv.Port(),
			strings.TrimSpace(string(gossh.MarshalAuthorizedKey(srv.HostKey()))))
		known.WriteByte('\n')
	}
	sshCfgPath := filepath.Join(dir, "ssh_config")
	writeFile(t, sshCfgPath, sshCfg.String())
	writeFile(t, knownPath, known.String())

	invPath := filepath.Join(dir, "hosts.yaml")
	writeFile(t, invPath, inventoryYAML)

	cfgPath := filepath.Join(cfgDir, "config.yaml")
	writeFile(t, cfgPath, fmt.Sprintf(`ssh:
  config_files: [%q]
  connect_timeout: 5s
  keepalive: 30s
inventory:
  cache_ttl: 1h
  sources:
    - type: static
      path: %q
`, sshCfgPath, invPath))

	return &buildFixture{home: home, cfgDir: cfgDir, cfgPath: cfgPath}
}

// load returns the fixture's configuration the way Run does.
func (f *buildFixture) load(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(f.cfgPath, f.home)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// TestBuildFromConfig is the task's acceptance check: a static inventory over
// test SSH servers builds, the root starts on `:hosts` and lists the hosts, and
// Close leaves nothing dialling.
func TestBuildFromConfig(t *testing.T) {
	theme.SetProfile("none")
	f := newBuildFixture(t, hostNames, staticInventory(t, hostNames))
	cfg := f.load(t)

	before := runtime.NumGoroutine()
	b, err := build(cfg, Options{
		Home:      f.home,
		ConfigDir: f.cfgDir,
		Env:       map[string]string{"USER": "tester"},
	}, config.ResolvePaths(f.home, f.cfgDir))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if b.Root == nil || b.Env == nil || b.Registry == nil {
		t.Fatalf("build returned an incomplete runtime: %+v", b)
	}
	if b.Inventory == nil {
		t.Fatal("build has no inventory")
	}
	if _, _, ok := b.Registry.Lookup("host"); !ok {
		t.Fatal("registry has no host kind")
	}

	// The first frame never waits for a source: the hosts stream in.
	waitForHosts(t, b, len(hostNames))

	// A `:hosts` query lists them.
	b.Root.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	pump(t, b.Root, b.Root.Init())
	_, cmd := b.Root.Update(tui.RunCommandMsg{Line: ":hosts"})
	pump(t, b.Root, cmd)

	screen := b.Root.View()
	for _, name := range hostNames {
		if !strings.Contains(screen, name) {
			t.Fatalf("the host list does not show %s:\n%s", name, screen)
		}
	}

	// Close stops the pool: nothing can dial afterwards and the background
	// goroutines are gone.
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = b.Env.Exec.Exec(context.Background(), resource.HostID(hostNames[0]),
		transport.Command{Script: "true", Timeout: time.Second})
	if err == nil {
		t.Fatal("Exec succeeded after Close, want a closed-pool error")
	}
	if k := errs.KindOf(err); k != errs.Canceled {
		t.Fatalf("Exec after Close = %v (kind %q), want kind %q", err, k, errs.Canceled)
	}
	waitForGoroutines(t, before+3)
}

// TestBuildWithoutInventorySourcesStarts is requirement 2: with no usable
// source the run still comes up, empty, and says how to add hosts.
func TestBuildWithoutInventorySourcesStarts(t *testing.T) {
	theme.SetProfile("none")
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cfgDir := filepath.Join(dir, "config")
	for _, d := range []string{home, cfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	writeFile(t, filepath.Join(cfgDir, "config.yaml"), "inventory:\n  sources: []\n")

	cfg, err := config.Load(filepath.Join(cfgDir, "config.yaml"), home)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	b, err := build(cfg, Options{Home: home, ConfigDir: cfgDir}, config.ResolvePaths(home, cfgDir))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() { _ = b.Close() }()

	if b.Root == nil {
		t.Fatal("build returned no root")
	}
	if n := len(b.Inventory.Hosts()); n != 0 {
		t.Fatalf("inventory has %d hosts, want none", n)
	}
	if !hasWarning(b.Warnings(), "no inventory sources") {
		t.Fatalf("warnings = %q, want one naming inventory.sources", b.Warnings())
	}
}

// TestBuildDemoUsesFakeBackend is requirement 2's other half: --demo keeps the
// fake backend and touches no SSH at all.
func TestBuildDemoUsesFakeBackend(t *testing.T) {
	theme.SetProfile("none")
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".config", "jumpr"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := config.Default()
	b, err := build(cfg, Options{Demo: true, Home: home}, config.ResolvePaths(home, ""))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, ok := b.Registry.Get("fake"); !ok {
		t.Fatal("the demo run has no fake backend")
	}
	if b.Inventory != nil {
		t.Fatal("the demo run has an inventory")
	}
	if b.Env.Exec != nil || b.Env.Shells != nil {
		t.Fatal("the demo run has a transport")
	}
	if b.Root == nil {
		t.Fatal("the demo run has no root")
	}
}

// staticInventory is the static source file for names, one label per host.
func staticInventory(t *testing.T, names []string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("hosts:\n")
	for i, name := range names {
		fmt.Fprintf(&b, "  - name: %s\n    addr: %s\n    labels:\n      role: web\n      rack: r%d\n",
			name, name, i+1)
	}
	return b.String()
}

// startTestServer starts one in-process SSH server whose exec handler reports
// success with no output: the probe sections are then all missing and every
// host row is degraded, which is enough to prove the row is on screen.
func startTestServer(t *testing.T, pub gossh.PublicKey) *sshtest.Server {
	t.Helper()
	return sshtest.Start(t, sshtest.Options{
		AuthorizedKeys: []gossh.PublicKey{pub},
		Handler:        func(string, io.Reader, io.Writer, io.Writer) int { return 0 },
	})
}

// writeTestKey writes an ed25519 private key in OpenSSH form and returns its
// path plus its public key.
func writeTestKey(t *testing.T, dir string) (string, gossh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(dir, "id_ed25519")
	writeFile(t, path, string(pem.EncodeToMemory(block)))
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return path, signer.PublicKey()
}

// writeFile writes one file, failing the test when it cannot.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// waitForHosts waits until the background refresh has produced n hosts.
func waitForHosts(t *testing.T, b *built, n int) {
	t.Helper()
	deadline := time.Now().Add(buildWait)
	for time.Now().Before(deadline) {
		if len(b.Inventory.Hosts()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("inventory still has %d of %d hosts after %s", len(b.Inventory.Hosts()), n, buildWait)
}

// waitForGoroutines waits until the process has no more than want goroutines.
// The pool's reaper, keepalives and connection watchers must all be gone after
// Close.
func waitForGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(buildWait)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d goroutines after Close, want at most %d:\n%s",
		runtime.NumGoroutine(), want, stack())
}

// stack dumps every goroutine's stack, so a leak is diagnosable.
func stack() string {
	buf := make([]byte, 1<<16)
	return string(buf[:runtime.Stack(buf, true)])
}

// pump runs cmd and feeds whatever it produces back into root, skipping
// commands that do not finish inside buildWait (the flash expiry tick waits 5 s
// and is not worth waiting for) and batch messages, which carry more commands.
func pump(t *testing.T, root *tui.Root, cmd tea.Cmd) {
	t.Helper()
	var run func(next tea.Cmd, depth int)
	run = func(next tea.Cmd, depth int) {
		if next == nil || depth > 16 {
			return
		}
		msg, ok := runCmd(next)
		if !ok || msg == nil {
			return
		}
		if batch, isBatch := msg.(tea.BatchMsg); isBatch {
			for _, sub := range batch {
				run(sub, depth+1)
			}
			return
		}
		if _, quit := msg.(tea.QuitMsg); quit {
			return
		}
		_, after := root.Update(msg)
		run(after, depth+1)
	}
	run(cmd, 0)
}

// runCmd runs one command and waits buildWait for its message.
func runCmd(cmd tea.Cmd) (tea.Msg, bool) {
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		return msg, true
	case <-time.After(buildWait):
		return nil, false
	}
}

// hasWarning reports whether one warning contains want.
func hasWarning(warns []string, want string) bool {
	for _, w := range warns {
		if strings.Contains(w, want) {
			return true
		}
	}
	return false
}
