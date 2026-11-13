package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"sitrep/internal/errs"
)

const testHome = "/Users/tester"

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.yaml"), testHome)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := Default(); !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load of missing file = %+v, want %+v", cfg, want)
	}
}

func TestLoadFull(t *testing.T) {
	cfg, err := Load("testdata/full.yaml", testHome)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := cfg.UI.Color, "256"; got != want {
		t.Errorf("ui.color = %q, want %q", got, want)
	}
	if got, want := cfg.Mux.Prefix, "C-b"; got != want {
		t.Errorf("mux.prefix = %q, want %q", got, want)
	}
	if got, want := cfg.Mux.PrefixInTmux, "C-b"; got != want {
		t.Errorf("mux.prefix_in_tmux = %q, want %q", got, want)
	}
	if got, want := cfg.Mux.Scrollback, 5000; got != want {
		t.Errorf("mux.scrollback = %d, want %d", got, want)
	}
	if cfg.Mux.MaxSyncPanes != 4 {
		t.Errorf("mux.max_sync_panes = %d, want 4", cfg.Mux.MaxSyncPanes)
	}

	wantConfigFiles := []string{testHome + "/.ssh/config", testHome + "/.ssh/work_config"}
	if !reflect.DeepEqual(cfg.SSH.ConfigFiles, wantConfigFiles) {
		t.Errorf("ssh.config_files = %v, want %v", cfg.SSH.ConfigFiles, wantConfigFiles)
	}
	if want := []string{testHome + "/.ssh/known_hosts"}; !reflect.DeepEqual(cfg.SSH.KnownHosts, want) {
		t.Errorf("ssh.known_hosts = %v, want %v", cfg.SSH.KnownHosts, want)
	}
	if cfg.SSH.Concurrency != 16 || cfg.SSH.MaxConnsPerBastion != 2 || cfg.SSH.MaxChannelsPerConn != 4 {
		t.Errorf("ssh limits = %d/%d/%d, want 16/2/4",
			cfg.SSH.Concurrency, cfg.SSH.MaxConnsPerBastion, cfg.SSH.MaxChannelsPerConn)
	}
	for _, c := range []struct {
		name string
		got  Duration
		want time.Duration
	}{
		{"ssh.connect_timeout", cfg.SSH.ConnectTimeout, 5 * time.Second},
		{"ssh.exec_timeout", cfg.SSH.ExecTimeout, 20 * time.Second},
		{"ssh.idle_timeout", cfg.SSH.IdleTimeout, 10 * time.Minute},
		{"ssh.keepalive", cfg.SSH.Keepalive, 15 * time.Second},
		{"inventory.cache_ttl", cfg.Inventory.CacheTTL, 5 * time.Minute},
		{"salt.timeout", cfg.Salt.Timeout, 90 * time.Second},
		{"salt.poll_interval", cfg.Salt.PollInterval, 3 * time.Second},
		{"interact.timeout", cfg.Interact.Timeout, 3 * time.Minute},
	} {
		if got := c.got.Duration(); got != c.want {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}
	if got, want := cfg.SSH.MaxOutput, int64(1048576); got != want {
		t.Errorf("ssh.max_output = %d, want %d", got, want)
	}

	wantSources := []InventorySource{
		{Type: "ssh_config"},
		{Type: "static", Path: testHome + "/.config/sitrep/hosts.yaml"},
		{Type: "salt"},
	}
	if !reflect.DeepEqual(cfg.Inventory.Sources, wantSources) {
		t.Errorf("inventory.sources = %+v, want %+v", cfg.Inventory.Sources, wantSources)
	}
	wantRewrites := []Rewrite{{From: `^(.*):9100$`, To: "$1"}}
	if !reflect.DeepEqual(cfg.Inventory.Identity.Rewrites, wantRewrites) {
		t.Errorf("inventory.identity.rewrites = %+v, want %+v", cfg.Inventory.Identity.Rewrites, wantRewrites)
	}

	if got, want := cfg.Knowledge.Root, testHome+"/sitrep"; got != want {
		t.Errorf("knowledge.root = %q, want %q", got, want)
	}

	wantRollout := RolloutConfig{Canary: 2, BatchPercent: 20, BatchMax: 100, FailureThresholdPercent: 10, TypedConfirmOver: 30}
	if !reflect.DeepEqual(cfg.Rollout, wantRollout) {
		t.Errorf("rollout = %+v, want %+v", cfg.Rollout, wantRollout)
	}

	wantSalt := SaltConfig{
		Master:       "salt-master.example.internal",
		Sudo:         false,
		Timeout:      Duration(90 * time.Second),
		PollInterval: Duration(3 * time.Second),
		ListChunk:    100,
		SuggestOver:  150,
		AllowSaltSSH: true,
	}
	if !reflect.DeepEqual(cfg.Salt, wantSalt) {
		t.Errorf("salt = %+v, want %+v", cfg.Salt, wantSalt)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate on full config: %v", err)
	}
}

func TestLoadPartialKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mux:\n  scrollback: 1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, testHome)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mux.Scrollback != 1234 {
		t.Errorf("mux.scrollback = %d, want 1234", cfg.Mux.Scrollback)
	}
	want := Default()
	want.Mux.Scrollback = 1234
	expandPaths(&want, testHome)
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("partial load = %+v, want defaults with scrollback 1234", cfg)
	}
}

func TestUnknownKeyIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("ui:\n  colour: auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, testHome)
	if err == nil {
		t.Fatal("Load of a file with an unknown key returned no error")
	}
	if !errs.Is(err, errs.Config) {
		t.Fatalf("error kind = %q, want %q (err: %v)", errs.KindOf(err), errs.Config, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("error %q does not name the file %q", msg, path)
	}
	if !strings.Contains(msg, "line 2") {
		t.Errorf("error %q does not name the line", msg)
	}
	if !strings.Contains(msg, "colour") {
		t.Errorf("error %q does not name the offending key", msg)
	}
}

func TestBadPrefixIsConfigError(t *testing.T) {
	_, err := Load("testdata/bad-prefix.yaml", testHome)
	if err == nil {
		t.Fatal("Load of an invalid prefix returned no error")
	}
	if !errs.Is(err, errs.Config) {
		t.Fatalf("error kind = %q, want %q (err: %v)", errs.KindOf(err), errs.Config, err)
	}
	if !strings.Contains(err.Error(), "mux.prefix") {
		t.Errorf("error %q does not name mux.prefix", err)
	}
	if !strings.Contains(err.Error(), "bad-prefix.yaml") {
		t.Errorf("error %q does not name the file", err)
	}
}

func TestDurationRoundTrip(t *testing.T) {
	var doc struct {
		D Duration `yaml:"d"`
	}
	for _, s := range []string{"10s", "5m", "2h", "1h30m", "250ms"} {
		if err := yaml.Unmarshal([]byte("d: "+s+"\n"), &doc); err != nil {
			t.Fatalf("Unmarshal %q: %v", s, err)
		}
		want, err := time.ParseDuration(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := doc.D.Duration(); got != want {
			t.Errorf("parsed %s = %s, want %s", s, got, want)
		}
		out, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatalf("Marshal %q: %v", s, err)
		}
		var back struct {
			D Duration `yaml:"d"`
		}
		if err := yaml.Unmarshal(out, &back); err != nil {
			t.Fatalf("re-Unmarshal of %q: %v", s, err)
		}
		if got := back.D.Duration(); got != want {
			t.Errorf("round trip of %s = %s, want %s (yaml: %s)", s, got, want, out)
		}
	}

	for _, bad := range []string{"ten seconds", "", "10"} {
		doc.D = 0
		if err := yaml.Unmarshal([]byte("d: '"+bad+"'\n"), &doc); err == nil {
			t.Errorf("Unmarshal of %q returned no error", bad)
		}
	}
}

func TestExpandHome(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"~/.ssh/config", testHome + "/.ssh/config"},
		{"~/sitrep", testHome + "/sitrep"},
		{"~", testHome},
		{"~user/x", "~user/x"},
		{"/etc/ssh/ssh_config", "/etc/ssh/ssh_config"},
		{"relative/path", "relative/path"},
		{"", ""},
	} {
		if got := ExpandHome(tc.in, testHome); got != tc.want {
			t.Errorf("ExpandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolvePaths(t *testing.T) {
	got := ResolvePaths(testHome, "")
	want := Paths{
		ConfigDir:     testHome + "/.config/sitrep",
		CacheDir:      testHome + "/.cache/sitrep",
		KnowledgeRoot: testHome + "/sitrep",
	}
	if got != want {
		t.Errorf("ResolvePaths = %+v, want %+v", got, want)
	}

	got = ResolvePaths(testHome, "~/alt/sitrep")
	if want := testHome + "/alt/sitrep"; got.ConfigDir != want {
		t.Errorf("override config dir = %q, want %q", got.ConfigDir, want)
	}
	got = ResolvePaths(testHome, "/etc/sitrep")
	if want := "/etc/sitrep"; got.ConfigDir != want {
		t.Errorf("absolute override config dir = %q, want %q", got.ConfigDir, want)
	}
}

func TestDefaultsValidate(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate: %v", err)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string // substring of the error message
	}{
		{"bad prefix", func(c *Config) { c.Mux.Prefix = "C-A" }, "mux.prefix"},
		{"bad prefix in tmux", func(c *Config) { c.Mux.PrefixInTmux = "prefix" }, "mux.prefix_in_tmux"},
		{"bad color", func(c *Config) { c.UI.Color = "88" }, "ui.color"},
		{"zero scrollback", func(c *Config) { c.Mux.Scrollback = 0 }, "mux.scrollback"},
		{"negative concurrency", func(c *Config) { c.SSH.Concurrency = -1 }, "ssh.concurrency"},
		{"zero max output", func(c *Config) { c.SSH.MaxOutput = 0 }, "ssh.max_output"},
		{"zero batch max", func(c *Config) { c.Rollout.BatchMax = 0 }, "rollout.batch_max"},
		{"zero list chunk", func(c *Config) { c.Salt.ListChunk = 0 }, "salt.list_chunk"},
		{"zero timeout", func(c *Config) { c.SSH.ConnectTimeout = 0 }, "ssh.connect_timeout"},
		{"negative poll interval", func(c *Config) { c.Salt.PollInterval = Duration(-time.Second) }, "salt.poll_interval"},
		{"zero interact timeout", func(c *Config) { c.Interact.Timeout = 0 }, "interact.timeout"},
		{"bad source type", func(c *Config) {
			c.Inventory.Sources = []InventorySource{{Type: "nagios"}}
		}, "inventory.sources[0].type"},
		{"static without path", func(c *Config) {
			c.Inventory.Sources = []InventorySource{{Type: "static"}}
		}, "inventory.sources[0].path"},
		{"bad rewrite", func(c *Config) {
			c.Inventory.Identity.Rewrites = []Rewrite{{From: "([a-z"}}
		}, "inventory.identity.rewrites[0].from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate returned no error")
			}
			if !errs.Is(err, errs.Config) {
				t.Errorf("error kind = %q, want %q", errs.KindOf(err), errs.Config)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestPrefixPatternAcceptsSpecialKeys(t *testing.T) {
	for _, p := range []string{"C-a", "C-z", "C-\\", "C-]", "C-^", "C-_", "C-@"} {
		cfg := Default()
		cfg.Mux.Prefix = p
		if err := cfg.Validate(); err != nil {
			t.Errorf("prefix %q rejected: %v", p, err)
		}
	}
	for _, p := range []string{"C-A", "ctrl-a", "C-ab", "C-1", ""} {
		cfg := Default()
		cfg.Mux.Prefix = p
		if err := cfg.Validate(); err == nil {
			t.Errorf("prefix %q accepted, want rejection", p)
		}
	}
}
