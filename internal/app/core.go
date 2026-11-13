package app

import (
	"fmt"
	"log/slog"

	"jumper/internal/backend/elastic"
	"jumper/internal/backend/grafana"
	"jumper/internal/backend/prom"
	"jumper/internal/clip"
	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/transport"
	"jumper/internal/transport/httpx"
	"jumper/internal/transport/ssh"
	"jumper/internal/transport/sysssh"
	"jumper/internal/tui"
)

// PrometheusBackend builds the Prometheus backend from cfg's `prometheus:`
// section (SPEC §8.3).
//
// One backend serves every configured server: the kinds are global, so two
// backends would collide in the registry. Each entry names a connection profile
// in connections.yaml through `connection`; an entry without one gets a plain
// client, which is what an unauthenticated in-cluster Prometheus needs. It
// returns nil when no server is configured, so callers can skip registration.
//
// The wiring lives here rather than in app.go because app.buildRegistry is
// still the M0 version; the task that owns app.go calls this function to add
// the backend to the registry.
func PrometheusBackend(cfg config.Config, profiles map[string]*httpx.Profile, deps httpx.Deps, log *slog.Logger) (*prom.Backend, error) {
	if len(cfg.Prometheus) == 0 {
		return nil, nil
	}
	servers := make([]prom.Server, 0, len(cfg.Prometheus))
	seen := make(map[string]bool, len(cfg.Prometheus))

	for i, entry := range cfg.Prometheus {
		name := entry.Name
		if seen[name] {
			return nil, errs.New(errs.Config, "app.PrometheusBackend",
				fmt.Sprintf("prometheus[%d]: %q is configured twice", i, name))
		}
		seen[name] = true

		profile, err := profileFor("prometheus", name, entry.Connection, profiles)
		if err != nil {
			return nil, err
		}
		hc, err := profile.Client(deps)
		if err != nil {
			return nil, errs.Wrap(errs.Config, "app.PrometheusBackend", err).WithHost(name)
		}
		for _, warning := range hc.Warnings() {
			if log != nil {
				log.Warn("prometheus client warning", "server", name, "warning", warning)
			}
		}

		client, err := prom.NewClient(prom.ClientOptions{
			URL:    entry.URL,
			Name:   name,
			Client: hc,
			Warn:   warnFunc("prometheus", log, name),
		})
		if err != nil {
			return nil, err
		}
		servers = append(servers, prom.Server{Name: name, Client: client})
	}
	return prom.New(prom.Options{Servers: servers, Log: log}), nil
}

// GrafanaHooks are the app-supplied effects a Grafana dashboard needs. `o` on a
// dashboard or a panel calls Open with the browser URL; when Open is nil the
// backend's default applies and the URL is copied to Clipboard instead
// (BUILD-PLAN §6.1 rule 6).
type GrafanaHooks struct {
	Open      func(url string) error
	Clipboard clip.Clipboard
}

// GrafanaBackend builds the Grafana backend from cfg's `grafana:` section
// (SPEC §8.4).
//
// One backend serves every configured server: the kinds are global, so two
// backends would collide in the registry. Each entry names a connection profile
// in connections.yaml through `connection`; an entry without one gets a plain
// client. It returns nil when no server is configured, so callers can skip
// registration.
//
// The wiring lives here rather than in app.go because app.buildRegistry is
// still the M0 version; the task that owns app.go calls this function to add
// the backend to the registry.
func GrafanaBackend(cfg config.Config, profiles map[string]*httpx.Profile, deps httpx.Deps, hooks GrafanaHooks, log *slog.Logger) (*grafana.Backend, error) {
	if len(cfg.Grafana) == 0 {
		return nil, nil
	}
	servers := make([]grafana.Server, 0, len(cfg.Grafana))
	seen := make(map[string]bool, len(cfg.Grafana))

	for i, entry := range cfg.Grafana {
		name := entry.Name
		if seen[name] {
			return nil, errs.New(errs.Config, "app.GrafanaBackend",
				fmt.Sprintf("grafana[%d]: %q is configured twice", i, name))
		}
		seen[name] = true

		profile, err := profileFor("grafana", name, entry.Connection, profiles)
		if err != nil {
			return nil, err
		}
		hc, err := profile.Client(deps)
		if err != nil {
			return nil, errs.Wrap(errs.Config, "app.GrafanaBackend", err).WithHost(name)
		}
		for _, warning := range hc.Warnings() {
			if log != nil {
				log.Warn("grafana client warning", "server", name, "warning", warning)
			}
		}

		client, err := grafana.NewClient(grafana.ClientOptions{
			URL:    entry.URL,
			Name:   name,
			Client: hc,
			Warn:   warnFunc("grafana", log, name),
		})
		if err != nil {
			return nil, err
		}
		servers = append(servers, grafana.Server{Name: name, Client: client})
	}
	return grafana.New(grafana.Options{
		Servers:   servers,
		Log:       log,
		Open:      hooks.Open,
		Clipboard: hooks.Clipboard,
	}), nil
}

// ElasticBackend builds the Elasticsearch backend from cfg's `elasticsearch:`
// section (SPEC §8.5).
//
// One backend serves every configured cluster: the log kind is global, so two
// backends would collide in the registry. Each entry names a connection profile
// in connections.yaml through `connection`; an entry without one gets a plain
// client. Each cluster carries its saved log sources (index pattern and the
// time, host, message and level fields). It returns nil when no cluster is
// configured, so callers can skip registration. A redactor cleans every
// document the backend hands out.
//
// The wiring lives here rather than in app.go because app.buildRegistry is
// still the M0 version; the task that owns app.go calls this function to add
// the backend to the registry.
func ElasticBackend(cfg config.Config, profiles map[string]*httpx.Profile, deps httpx.Deps, redactText func(string) string, log *slog.Logger) (*elastic.Backend, error) {
	if len(cfg.Elasticsearch) == 0 {
		return nil, nil
	}
	clusters := make([]elastic.Cluster, 0, len(cfg.Elasticsearch))
	seen := make(map[string]bool, len(cfg.Elasticsearch))

	for i, entry := range cfg.Elasticsearch {
		name := entry.Name
		if seen[name] {
			return nil, errs.New(errs.Config, "app.ElasticBackend",
				fmt.Sprintf("elasticsearch[%d]: %q is configured twice", i, name))
		}
		seen[name] = true

		profile, err := profileFor("elasticsearch", name, entry.Connection, profiles)
		if err != nil {
			return nil, err
		}
		hc, err := profile.Client(deps)
		if err != nil {
			return nil, errs.Wrap(errs.Config, "app.ElasticBackend", err).WithHost(name)
		}
		for _, warning := range hc.Warnings() {
			if log != nil {
				log.Warn("elasticsearch client warning", "cluster", name, "warning", warning)
			}
		}

		client, err := elastic.NewClient(elastic.ClientOptions{
			URL:    entry.URL,
			Name:   name,
			Client: hc,
			Warn:   warnFunc("elasticsearch", log, name),
		})
		if err != nil {
			return nil, err
		}
		sources := make([]elastic.LogSource, 0, len(entry.Sources))
		for _, src := range entry.Sources {
			sources = append(sources, elastic.LogSource{
				Name:         src.Name,
				Index:        src.Index,
				TimeField:    src.TimeField,
				HostField:    src.HostField,
				MessageField: src.MessageField,
				LevelField:   src.LevelField,
			})
		}
		clusters = append(clusters, elastic.Cluster{Name: name, Client: client, Sources: sources})
	}
	return elastic.New(elastic.Options{Clusters: clusters, Log: log, Redact: redactText}), nil
}

// Alertmanager builds the Alertmanager backend and its silence planner from
// cfg's `alertmanager:` section (SPEC §8.3), over one set of clients.
//
// The backend's health dot and refs are named "am"; the planner is the one the
// bus registers, and every silence it plans is signed with creator, the local
// user name the caller passes in (app.defaultUser). An entry names a connection
// profile in connections.yaml through `connection`; without one it gets a plain
// client. Both results are nil when no server is configured.
func Alertmanager(cfg config.Config, profiles map[string]*httpx.Profile, deps httpx.Deps, creator string, log *slog.Logger) (*prom.AlertmanagerBackend, *prom.AlertmanagerPlanner, error) {
	if len(cfg.Alertmanager) == 0 {
		return nil, nil, nil
	}
	servers := make([]prom.AlertmanagerServer, 0, len(cfg.Alertmanager))
	seen := make(map[string]bool, len(cfg.Alertmanager))

	for i, entry := range cfg.Alertmanager {
		name := entry.Name
		if seen[name] {
			return nil, nil, errs.New(errs.Config, "app.AlertmanagerBackend",
				fmt.Sprintf("alertmanager[%d]: %q is configured twice", i, name))
		}
		seen[name] = true

		profile, err := profileFor("alertmanager", name, entry.Connection, profiles)
		if err != nil {
			return nil, nil, err
		}
		hc, err := profile.Client(deps)
		if err != nil {
			return nil, nil, errs.Wrap(errs.Config, "app.AlertmanagerBackend", err).WithHost(name)
		}
		for _, warning := range hc.Warnings() {
			if log != nil {
				log.Warn("alertmanager client warning", "server", name, "warning", warning)
			}
		}

		client, err := prom.NewAlertmanagerClient(prom.AlertmanagerClientOptions{
			URL:    entry.URL,
			Name:   name,
			Client: hc,
		})
		if err != nil {
			return nil, nil, err
		}
		servers = append(servers, prom.AlertmanagerServer{Name: name, Client: client})
	}
	amBackend := prom.NewAlertmanager(prom.AlertmanagerOptions{Servers: servers, Log: log})
	planner := prom.NewAlertmanagerPlanner(prom.AlertmanagerPlannerOptions{
		Servers: servers,
		Creator: creator,
	})
	return amBackend, planner, nil
}

// profileFor returns the connection profile an entry names, or a plain profile
// for an entry without one. A name that is not in connections.yaml is an
// errs.Config error naming the server: silently falling back to no auth would
// turn a typo into a diagnosis. product is the config section the entry came
// from, so the error says which file to fix.
func profileFor(product, server, connection string, profiles map[string]*httpx.Profile) (*httpx.Profile, error) {
	if connection == "" {
		return &httpx.Profile{Name: product + " " + server}, nil
	}
	if p, ok := profiles[connection]; ok && p != nil {
		return p, nil
	}
	return nil, errs.New(errs.Config, "app."+product+"Backend",
		fmt.Sprintf("%s %q: connection %q is not in connections.yaml", product, server, connection))
}

// warnFunc turns a client warning into a log line.
func warnFunc(product string, log *slog.Logger, server string) func(string) {
	if log == nil {
		return nil
	}
	return func(msg string) {
		log.Warn(product, "server", server, "warning", msg)
	}
}

// execSet is what the app runs remote work through: one executor and one
// streamer for the backends, one shell opener for the panes, and the pool
// holding for `:health`. The three faces are the same value on purpose, so a
// routed host is routed for commands, streams and shells alike.
type execSet struct {
	transport.Executor
	transport.Streamer
	transport.ShellOpener

	pool *ssh.Pool
}

// PoolStats reports what the connection pool currently holds.
func (e *execSet) PoolStats() tui.PoolStats {
	if e == nil || e.pool == nil {
		return tui.PoolStats{}
	}
	st := e.pool.Stats()
	return tui.PoolStats{Bastions: st.Bastions, Targets: st.Targets, Leases: st.Leases}
}

// buildExecSet chooses the transports cfg asks for: the native SSH transport
// for every host, or — when ssh.transports routes patterns away — a
// transport.Router that sends those hosts through the system ssh client
// (SPEC §7.1). Nothing is dialled here.
func buildExecSet(cfg config.Config, o Options, stack *sshStack, paths config.Paths, log *slog.Logger) (*execSet, error) {
	if stack == nil || stack.Transport == nil {
		return nil, errs.New(errs.Config, "ssh.build", "no ssh transport")
	}
	if len(cfg.SSH.Transports) == 0 {
		return &execSet{
			Executor:    &runtimeTransport{Transport: stack.Transport, pool: stack.Pool},
			Streamer:    stack.Transport,
			ShellOpener: stack.Transport,
			pool:        stack.Pool,
		}, nil
	}

	// The fallback runs the local ssh with the same ssh_config the native
	// client reads (its first configured file: OpenSSH takes one -F) and the
	// control sockets next to the rest of the cache.
	files := cfg.SSH.ConfigFiles
	cfgFile := ""
	if len(files) > 0 {
		cfgFile = config.ExpandHome(files[0], o.Home)
	}
	fallback := sysssh.New(sysssh.Options{
		ConfigFile:     cfgFile,
		CacheDir:       paths.CacheDir,
		ConnectTimeout: cfg.SSH.ConnectTimeout.Duration(),
		ExecTimeout:    cfg.SSH.ExecTimeout.Duration(),
		MaxOutput:      int(cfg.SSH.MaxOutput),
		Log:            log,
	})

	rules := make([]transport.RouteRule, 0, len(cfg.SSH.Transports))
	for _, tr := range cfg.SSH.Transports {
		rules = append(rules, transport.RouteRule{Match: tr.Match, Transport: tr.Transport})
	}
	router, err := transport.NewRouter(stack.Transport, fallback, rules)
	if err != nil {
		return nil, err
	}
	if log != nil {
		log.Debug("ssh.transports configured", "rules", len(rules))
	}
	return &execSet{Executor: router, Streamer: router, ShellOpener: router, pool: stack.Pool}, nil
}
