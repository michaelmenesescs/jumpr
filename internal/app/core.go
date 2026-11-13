package app

import (
	"fmt"
	"log/slog"

	"jumper/internal/backend/prom"
	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/transport/httpx"
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
