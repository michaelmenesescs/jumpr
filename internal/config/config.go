// Package config holds jumpr's configuration schema, defaults, loader and
// validation. It never reads the environment or the real home directory: the
// home directory is passed in by the caller (BUILD-PLAN §6.1).
package config

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"jumpr/internal/errs"
)

// PrometheusConfig is one Prometheus server jumpr may query (SPEC §8.3).
// `connection` names an entry of connections.yaml; empty means a direct,
// unauthenticated connection.
type PrometheusConfig struct {
	Name       string `yaml:"name"`
	URL        string `yaml:"url"`
	Connection string `yaml:"connection"`
}

// AlertmanagerConfig is one Alertmanager jumpr may silence (SPEC §8.3). It has
// the same shape as a Prometheus entry: `connection` names an entry of
// connections.yaml, empty means a direct, unauthenticated connection.
type AlertmanagerConfig struct {
	Name       string `yaml:"name"`
	URL        string `yaml:"url"`
	Connection string `yaml:"connection"`
}

// GrafanaConfig is one Grafana jumpr may browse and query (SPEC §8.4). It has
// the same shape as a Prometheus entry: `connection` names an entry of
// connections.yaml, empty means a direct, unauthenticated connection.
type GrafanaConfig struct {
	Name       string `yaml:"name"`
	URL        string `yaml:"url"`
	Connection string `yaml:"connection"`
}

// ElasticsearchConfig is one Elasticsearch cluster jumpr may search
// (SPEC §8.5). It has the same name/url/connection shape as a Prometheus entry,
// plus the saved log sources that live in the cluster.
type ElasticsearchConfig struct {
	Name       string            `yaml:"name"`
	URL        string            `yaml:"url"`
	Connection string            `yaml:"connection"`
	Sources    []LogSourceConfig `yaml:"sources"`
}

// LogSourceConfig is one saved log source: an index pattern and the fields that
// carry time, host, message, level and (when the source has one) the systemd
// unit. An empty `time_field` means @timestamp, the Elastic Common Schema
// default; an empty host, message, level or unit field means that filter or
// column is unavailable.
type LogSourceConfig struct {
	Name         string `yaml:"name"`
	Index        string `yaml:"index"`
	TimeField    string `yaml:"time_field"`
	HostField    string `yaml:"host_field"`
	MessageField string `yaml:"message_field"`
	LevelField   string `yaml:"level_field"`
	UnitField    string `yaml:"unit_field"`
}

// KubernetesConfig is the `kubernetes:` section (SPEC §8.2). Kubeconfig is an
// explicit path; empty means the standard rules: $KUBECONFIG, then
// ~/.kube/config. ContextsProtected lists the contexts that make the session
// read-only when they are selected (SPEC §16).
type KubernetesConfig struct {
	Kubeconfig        string   `yaml:"kubeconfig"`
	ContextsProtected []string `yaml:"contexts_protected"`
}

// Config is the whole of config.yaml (BUILD-PLAN §11). Every key is optional.
type Config struct {
	UI            UIConfig              `yaml:"ui"`
	Mux           MuxConfig             `yaml:"mux"`
	SSH           SSHConfig             `yaml:"ssh"`
	Inventory     InventoryConfig       `yaml:"inventory"`
	Knowledge     KnowledgeConfig       `yaml:"knowledge"`
	Rollout       RolloutConfig         `yaml:"rollout"`
	Salt          SaltConfig            `yaml:"salt"`
	Prometheus    []PrometheusConfig    `yaml:"prometheus"`
	Alertmanager  []AlertmanagerConfig  `yaml:"alertmanager"`
	Grafana       []GrafanaConfig       `yaml:"grafana"`
	Elasticsearch []ElasticsearchConfig `yaml:"elasticsearch"`
	Kubernetes    KubernetesConfig      `yaml:"kubernetes"`
	Chef          ChefConfig            `yaml:"chef"`
	Interact      InteractConfig        `yaml:"interact"`
}

// UIConfig configures the presentation layer.
type UIConfig struct {
	Color  string `yaml:"color"`  // auto | truecolor | 256 | 16
	Glyphs string `yaml:"glyphs"` // auto | unicode | nerd | ascii
}

// MuxConfig configures panes, prefix keys and synchronized input.
type MuxConfig struct {
	Prefix       string `yaml:"prefix"`
	PrefixInTmux string `yaml:"prefix_in_tmux"`
	Scrollback   int    `yaml:"scrollback"`
	MaxSyncPanes int    `yaml:"max_sync_panes"`
}

// SSHConfig configures connections, pooling and limits.
type SSHConfig struct {
	ConfigFiles        []string `yaml:"config_files"`
	KnownHosts         []string `yaml:"known_hosts"`
	Concurrency        int      `yaml:"concurrency"`
	MaxConnsPerBastion int      `yaml:"max_conns_per_bastion"`
	MaxChannelsPerConn int      `yaml:"max_channels_per_conn"`
	ConnectTimeout     Duration `yaml:"connect_timeout"`
	ExecTimeout        Duration `yaml:"exec_timeout"`
	IdleTimeout        Duration `yaml:"idle_timeout"`
	Keepalive          Duration `yaml:"keepalive"`
	MaxOutput          int64    `yaml:"max_output"`
	// Transports routes host patterns away from the native client, e.g.
	// [{match: "*.legacy.example.internal", transport: system-ssh}].
	Transports []SSHTransportRule `yaml:"transports"`
}

// SSHTransportRule sends the hosts matching Match to Transport. Match is a glob
// over the host ID; Transport is "native" or "system-ssh" (SPEC §7.1).
type SSHTransportRule struct {
	Match     string `yaml:"match"`
	Transport string `yaml:"transport"`
}

// InventoryConfig configures inventory sources and identity rewriting.
type InventoryConfig struct {
	CacheTTL Duration          `yaml:"cache_ttl"`
	Sources  []InventorySource `yaml:"sources"`
	Identity IdentityConfig    `yaml:"identity"`
}

// InventorySource is one inventory provider.
type InventorySource struct {
	Type string `yaml:"type"` // ssh_config | static | salt | k8s | prom | chef
	Path string `yaml:"path"` // required for static
}

// IdentityConfig rewrites names between sources.
type IdentityConfig struct {
	Rewrites []Rewrite `yaml:"rewrites"`
}

// Rewrite replaces the first capture group of names matching From with To.
type Rewrite struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// KnowledgeConfig points at the Markdown knowledge base.
type KnowledgeConfig struct {
	Root string `yaml:"root"`
}

// RolloutConfig configures how actions roll out across a target set.
type RolloutConfig struct {
	Canary                  int `yaml:"canary"`
	BatchPercent            int `yaml:"batch_percent"`
	BatchMax                int `yaml:"batch_max"`
	FailureThresholdPercent int `yaml:"failure_threshold_percent"`
	TypedConfirmOver        int `yaml:"typed_confirm_over"`
}

// SaltConfig configures the Salt master connection.
type SaltConfig struct {
	Master       string   `yaml:"master"`
	Sudo         bool     `yaml:"sudo"`
	Timeout      Duration `yaml:"timeout"`
	PollInterval Duration `yaml:"poll_interval"`
	ListChunk    int      `yaml:"list_chunk"`
	SuggestOver  int      `yaml:"suggest_over"`
	AllowSaltSSH bool     `yaml:"allow_salt_ssh"`
}

// ChefConfig configures the Chef workstation jumpr reads knife through
// (SPEC §8.7). `workstation` is the inventory host or ssh alias the knife
// commands run on; empty means Chef is unconfigured.
type ChefConfig struct {
	Workstation string   `yaml:"workstation"`
	Knife       string   `yaml:"knife"`
	Sudo        bool     `yaml:"sudo"`
	Timeout     Duration `yaml:"timeout"`
}

// InteractConfig bounds how long a prompt waits for an answer.
type InteractConfig struct {
	Timeout Duration `yaml:"timeout"`
}

// Default returns every default from BUILD-PLAN §11.
func Default() Config {
	return Config{
		UI: UIConfig{
			Color:  "auto",
			Glyphs: "auto",
		},
		Mux: MuxConfig{
			Prefix:       "C-a",
			PrefixInTmux: "C-b",
			Scrollback:   10000,
			MaxSyncPanes: 8,
		},
		SSH: SSHConfig{
			ConfigFiles:        []string{"~/.ssh/config"},
			KnownHosts:         []string{},
			Concurrency:        64,
			MaxConnsPerBastion: 4,
			MaxChannelsPerConn: 8,
			ConnectTimeout:     Duration(10 * time.Second),
			ExecTimeout:        Duration(30 * time.Second),
			IdleTimeout:        Duration(5 * time.Minute),
			Keepalive:          Duration(30 * time.Second),
			MaxOutput:          2097152,
		},
		Inventory: InventoryConfig{
			CacheTTL: Duration(15 * time.Minute),
			Sources:  []InventorySource{{Type: "ssh_config"}},
			Identity: IdentityConfig{},
		},
		Knowledge: KnowledgeConfig{
			Root: "~/jumpr",
		},
		Rollout: RolloutConfig{
			Canary:                  1,
			BatchPercent:            10,
			BatchMax:                50,
			FailureThresholdPercent: 5,
			TypedConfirmOver:        20,
		},
		Salt: SaltConfig{
			Master:       "",
			Sudo:         true,
			Timeout:      Duration(60 * time.Second),
			PollInterval: Duration(2 * time.Second),
			ListChunk:    200,
			SuggestOver:  200,
			AllowSaltSSH: false,
		},
		Chef: ChefConfig{
			Workstation: "",
			Knife:       "knife",
			Sudo:        false,
			Timeout:     Duration(30 * time.Second),
		},
		Interact: InteractConfig{
			Timeout: Duration(2 * time.Minute),
		},
	}
}

var (
	// prefixRE matches a control-key binding such as C-a. The class also holds
	// backslash, ], ^, _ and @ because those are common terminal bindings.
	prefixRE   = regexp.MustCompile(`^C-[a-z\\\]\^_@]$`)
	colorSet   = map[string]bool{"auto": true, "truecolor": true, "256": true, "16": true}
	glyphSet   = map[string]bool{"auto": true, "unicode": true, "nerd": true, "ascii": true}
	sourceType = map[string]bool{
		"ssh_config": true,
		"static":     true,
		"salt":       true,
		"k8s":        true,
		"prom":       true,
		"chef":       true,
	}
)

// Validate checks the value against BUILD-PLAN §11. Errors are *errs.Error
// with kind Config.
func (c *Config) Validate() error {
	if !prefixRE.MatchString(c.Mux.Prefix) {
		return invalidf("mux.prefix: %q is not a valid control key (expected C-a style)", c.Mux.Prefix)
	}
	if !prefixRE.MatchString(c.Mux.PrefixInTmux) {
		return invalidf("mux.prefix_in_tmux: %q is not a valid control key (expected C-a style)", c.Mux.PrefixInTmux)
	}
	if !colorSet[c.UI.Color] {
		return invalidf("ui.color: %q is not one of auto, truecolor, 256, 16", c.UI.Color)
	}
	if !glyphSet[c.UI.Glyphs] {
		return invalidf("ui.glyphs: %q is not one of auto, unicode, nerd, ascii", c.UI.Glyphs)
	}

	for _, f := range []struct {
		name string
		v    int64
	}{
		{"mux.scrollback", int64(c.Mux.Scrollback)},
		{"mux.max_sync_panes", int64(c.Mux.MaxSyncPanes)},
		{"ssh.concurrency", int64(c.SSH.Concurrency)},
		{"ssh.max_conns_per_bastion", int64(c.SSH.MaxConnsPerBastion)},
		{"ssh.max_channels_per_conn", int64(c.SSH.MaxChannelsPerConn)},
		{"ssh.max_output", c.SSH.MaxOutput},
		{"rollout.canary", int64(c.Rollout.Canary)},
		{"rollout.batch_percent", int64(c.Rollout.BatchPercent)},
		{"rollout.batch_max", int64(c.Rollout.BatchMax)},
		{"rollout.failure_threshold_percent", int64(c.Rollout.FailureThresholdPercent)},
		{"rollout.typed_confirm_over", int64(c.Rollout.TypedConfirmOver)},
		{"salt.list_chunk", int64(c.Salt.ListChunk)},
		{"salt.suggest_over", int64(c.Salt.SuggestOver)},
	} {
		if f.v <= 0 {
			return invalidf("%s: must be positive, got %d", f.name, f.v)
		}
	}

	for _, f := range []struct {
		name string
		v    Duration
	}{
		{"inventory.cache_ttl", c.Inventory.CacheTTL},
		{"ssh.connect_timeout", c.SSH.ConnectTimeout},
		{"ssh.exec_timeout", c.SSH.ExecTimeout},
		{"ssh.idle_timeout", c.SSH.IdleTimeout},
		{"ssh.keepalive", c.SSH.Keepalive},
		{"salt.timeout", c.Salt.Timeout},
		{"salt.poll_interval", c.Salt.PollInterval},
		{"chef.timeout", c.Chef.Timeout},
		{"interact.timeout", c.Interact.Timeout},
	} {
		if f.v <= 0 {
			return invalidf("%s: must be a positive duration, got %s", f.name, f.v)
		}
	}

	for i, src := range c.Inventory.Sources {
		if !sourceType[src.Type] {
			return invalidf("inventory.sources[%d].type: %q is not one of ssh_config, static, salt, k8s, prom, chef", i, src.Type)
		}
		if src.Type == "static" && src.Path == "" {
			return invalidf("inventory.sources[%d].path: static sources need a path", i)
		}
	}

	for i, rw := range c.Inventory.Identity.Rewrites {
		if _, err := regexp.Compile(rw.From); err != nil {
			return invalidf("inventory.identity.rewrites[%d].from: %v", i, err)
		}
	}

	if strings.TrimSpace(c.Chef.Knife) == "" {
		return invalidf("chef.knife: the knife command must not be empty")
	}
	if strings.ContainsAny(c.Chef.Knife, " \t\n") {
		return invalidf("chef.knife: %q must be one command name or path", c.Chef.Knife)
	}
	if ws := strings.TrimSpace(c.Chef.Workstation); strings.ContainsAny(ws, " \t\n") {
		return invalidf("chef.workstation: %q must be one host name", ws)
	}

	names := make(map[string]bool, len(c.Prometheus))
	for i, p := range c.Prometheus {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return invalidf("prometheus[%d].name: a server needs a name", i)
		}
		if names[name] {
			return invalidf("prometheus[%d].name: %q is configured twice", i, name)
		}
		names[name] = true
		u, err := url.Parse(strings.TrimSpace(p.URL))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return invalidf("prometheus[%d].url: %q is not an http(s) URL", i, p.URL)
		}
	}

	amNames := make(map[string]bool, len(c.Alertmanager))
	for i, a := range c.Alertmanager {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			return invalidf("alertmanager[%d].name: a server needs a name", i)
		}
		if amNames[name] {
			return invalidf("alertmanager[%d].name: %q is configured twice", i, name)
		}
		amNames[name] = true
		u, err := url.Parse(strings.TrimSpace(a.URL))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return invalidf("alertmanager[%d].url: %q is not an http(s) URL", i, a.URL)
		}
	}

	grafanaNames := make(map[string]bool, len(c.Grafana))
	for i, g := range c.Grafana {
		name := strings.TrimSpace(g.Name)
		if name == "" {
			return invalidf("grafana[%d].name: a server needs a name", i)
		}
		if grafanaNames[name] {
			return invalidf("grafana[%d].name: %q is configured twice", i, name)
		}
		grafanaNames[name] = true
		u, err := url.Parse(strings.TrimSpace(g.URL))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return invalidf("grafana[%d].url: %q is not an http(s) URL", i, g.URL)
		}
	}

	elasticNames := make(map[string]bool, len(c.Elasticsearch))
	for i, e := range c.Elasticsearch {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return invalidf("elasticsearch[%d].name: a cluster needs a name", i)
		}
		if elasticNames[name] {
			return invalidf("elasticsearch[%d].name: %q is configured twice", i, name)
		}
		elasticNames[name] = true
		u, err := url.Parse(strings.TrimSpace(e.URL))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return invalidf("elasticsearch[%d].url: %q is not an http(s) URL", i, e.URL)
		}
		sourceNames := make(map[string]bool, len(e.Sources))
		for k, src := range e.Sources {
			sname := strings.TrimSpace(src.Name)
			if sname == "" {
				return invalidf("elasticsearch[%d].sources[%d].name: a log source needs a name", i, k)
			}
			if sourceNames[sname] {
				return invalidf("elasticsearch[%d].sources[%d].name: %q is configured twice", i, k, sname)
			}
			sourceNames[sname] = true
			if strings.TrimSpace(src.Index) == "" {
				return invalidf("elasticsearch[%d].sources[%d].index: a log source needs an index pattern", i, k)
			}
		}
	}

	seenContexts := make(map[string]bool, len(c.Kubernetes.ContextsProtected))
	for i, name := range c.Kubernetes.ContextsProtected {
		ctxName := strings.TrimSpace(name)
		if ctxName == "" {
			return invalidf("kubernetes.contexts_protected[%d]: a context needs a name", i)
		}
		if seenContexts[ctxName] {
			return invalidf("kubernetes.contexts_protected[%d]: %q is listed twice", i, ctxName)
		}
		seenContexts[ctxName] = true
	}

	for i, tr := range c.SSH.Transports {
		match := strings.TrimSpace(tr.Match)
		if match == "" {
			return invalidf("ssh.transports[%d].match: a host pattern is required", i)
		}
		if _, err := path.Match(match, "host"); err != nil {
			return invalidf("ssh.transports[%d].match: %v", i, err)
		}
		switch tr.Transport {
		case transportNative, transportSystemSSH:
		default:
			return invalidf("ssh.transports[%d].transport: %q is not one of native, system-ssh", i, tr.Transport)
		}
	}

	return nil
}

const (
	// transportNative is the in-process SSH client.
	transportNative = "native"
	// transportSystemSSH runs /usr/bin/ssh for hosts the native client cannot
	// handle (SPEC §7.1).
	transportSystemSSH = "system-ssh"
)

func invalidf(format string, args ...any) *errs.Error {
	return errs.New(errs.Config, "config.validate", fmt.Sprintf(format, args...))
}
