package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend"
	"jumpr/internal/inventory"
	"jumpr/internal/render"
)

const (
	// healthCheckTimeout bounds one backend health check printed by `:health`.
	// BUILD-PLAN §8: health check 5 s.
	healthCheckTimeout = 5 * time.Second
	// inventoryRefreshTimeout bounds one `:inventory refresh`. It is a little
	// over the store's own 30 s per-source budget.
	inventoryRefreshTimeout = 35 * time.Second
)

// PoolStats is what `:health` prints about the SSH connection pool.
type PoolStats struct {
	Bastions int // open first-hop bastion connections
	Targets  int // pooled target connections
	Leases   int // leases held by callers
}

// PoolStatter is implemented by the executor internal/app hands to Env.Exec
// when a connection pool is behind it. `:health` prints its holdings; an
// executor without it (the demo's fake backend) simply has no pool line.
type PoolStatter interface {
	PoolStats() PoolStats
}

// SourceErrorReporter is implemented by the inventory internal/app hands to
// Env.Inventory: it remembers the last error of every source, so `:health` can
// list the ones that failed without making a refresh happen first.
type SourceErrorReporter interface {
	SourceErrors() map[string]error
}

// InventoryTickMsg is what the inventory refresh tick emits. internal/app owns
// the tick (the cache_ttl is configuration, not UI state): it turns this
// message into InventoryRefreshCmd and re-arms InventoryTick.
type InventoryTickMsg struct {
	Interval time.Duration
}

// InventoryTick returns a command that fires one InventoryTickMsg after
// interval. A non-positive interval never ticks.
func InventoryTick(interval time.Duration) tea.Cmd {
	if interval <= 0 {
		return nil
	}
	return tea.Tick(interval, func(time.Time) tea.Msg { return InventoryTickMsg{Interval: interval} })
}

// InventoryRefreshCmd refreshes the inventory once, off the UI goroutine, and
// reports the outcome as a flash. A source that fails does not lose its
// previous hosts: the store keeps them.
func InventoryRefreshCmd(env *Env) tea.Cmd {
	if env == nil || env.Inventory == nil {
		return nil
	}
	inv := env.Inventory
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), inventoryRefreshTimeout)
		defer cancel()
		failed := inv.Refresh(ctx)
		n := len(inv.Hosts())
		if len(failed) > 0 {
			return FlashMsg{
				Level: FlashWarn,
				Text:  fmt.Sprintf("inventory refreshed: %d hosts, %s", n, summarizeSourceErrors(failed)),
			}
		}
		return FlashMsg{Level: FlashOK, Text: fmt.Sprintf("inventory refreshed: %d hosts", n)}
	}
}

// InventoryStale reports whether the inventory is older than twice ttl, which
// is when the status line calls it `inv stale`.
func InventoryStale(env *Env, ttl time.Duration) bool {
	if env == nil || env.Inventory == nil || ttl <= 0 {
		return false
	}
	return env.Inventory.Age() > 2*ttl
}

// RegisterHealthCommands registers `:health` and `:inventory`. It is called by
// internal/app when it builds the command registry.
func RegisterHealthCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	if err := c.Register(healthCommand()); err != nil {
		return err
	}
	return c.Register(inventoryCommand())
}

// healthCommand is `:health`: a read-only text view of every backend's state,
// the SSH pool's holdings, the inventory's age and its per-source errors.
func healthCommand() Command {
	return Command{
		Name:  "health",
		Usage: ":health",
		Help:  "backends, SSH pool and inventory state",
		Run: func(env *Env, _ string) tea.Cmd {
			return func() tea.Msg {
				return PushViewMsg{View: &noticeView{
					t:     themeOf(env),
					title: "health",
					lines: healthLines(env),
				}}
			}
		},
	}
}

// inventoryCommand is `:inventory`: `refresh` forces a refresh, anything else
// prints the current state.
func inventoryCommand() Command {
	return Command{
		Name:  "inventory",
		Usage: ":inventory [refresh]",
		Help:  "show the host inventory or refresh it",
		Run: func(env *Env, args string) tea.Cmd {
			switch strings.TrimSpace(args) {
			case "refresh":
				if cmd := InventoryRefreshCmd(env); cmd != nil {
					return cmd
				}
				return Flash(FlashWarn, "no inventory configured")
			case "":
				return func() tea.Msg {
					return PushViewMsg{View: &noticeView{
						t:     themeOf(env),
						title: "inventory",
						lines: inventoryLines(env),
					}}
				}
			}
			return Flash(FlashError, "usage: :inventory refresh")
		},
		Complete: func(_ *Env, args string) []string {
			return filterPrefix([]string{"refresh"}, partialWord(args))
		},
	}
}

// healthLines renders everything `:health` shows. It runs inside a tea.Cmd, so
// it may ask the backends for their health; every call carries a timeout.
func healthLines(env *Env) []string {
	var lines []string
	lines = append(lines, kvLine("session", sessionName(env)))
	lines = append(lines, inventoryHealthLines(env)...)
	if ps, ok := poolStats(env); ok {
		lines = append(lines, kvLine("pool", fmt.Sprintf("bastions %d, targets %d, leases %d",
			ps.Bastions, ps.Targets, ps.Leases)))
	} else {
		lines = append(lines, kvLine("pool", "(no connection pool)"))
	}

	lines = append(lines, "")
	lines = append(lines, "backends:")
	for _, row := range backendHealth(env) {
		lines = append(lines, fmt.Sprintf("  %-10s %-12s %s", row.name, row.state, row.detail))
	}
	if len(lines) == 0 {
		lines = append(lines, "no backends registered")
	}
	if errs := sourceErrorLines(env); len(errs) > 0 {
		lines = append(lines, "")
		lines = append(lines, "inventory source errors:")
		for _, line := range errs {
			lines = append(lines, "  "+line)
		}
	}
	return lines
}

// inventoryLines renders the inventory's own view: the hosts and where they
// came from.
func inventoryLines(env *Env) []string {
	if env == nil || env.Inventory == nil {
		return []string{
			"no inventory configured",
			"",
			"add an inventory.sources entry to config.yaml:",
			"  - type: ssh_config",
			"  - type: static",
			"    path: ~/hosts.yaml",
		}
	}
	hosts := env.Inventory.Hosts()
	lines := []string{
		kvLine("hosts", fmt.Sprintf("%d", len(hosts))),
		kvLine("age", render.Duration(env.Inventory.Age())),
	}
	for _, id := range hostIDs(hosts) {
		lines = append(lines, "  "+id)
	}
	if errs := sourceErrorLines(env); len(errs) > 0 {
		lines = append(lines, "", "source errors:")
		for _, line := range errs {
			lines = append(lines, "  "+line)
		}
	}
	return lines
}

// inventoryHealthLines is the inventory part of `:health`.
func inventoryHealthLines(env *Env) []string {
	if env == nil || env.Inventory == nil {
		return []string{kvLine("inventory", "(none configured; try --demo)")}
	}
	return []string{kvLine("inventory", fmt.Sprintf("%d hosts, %s old",
		len(env.Inventory.Hosts()), render.Duration(env.Inventory.Age())))}
}

// healthRow is one backend's line in `:health`.
type healthRow struct {
	name   string
	state  string
	detail string
}

// backendHealth asks every registered backend for its health concurrently, each
// with healthCheckTimeout, and returns the rows in registration order.
func backendHealth(env *Env) []healthRow {
	if env == nil || env.Registry == nil {
		return nil
	}
	backends := env.Registry.Backends()
	rows := make([]healthRow, len(backends))
	var wg sync.WaitGroup
	for i, b := range backends {
		wg.Add(1)
		go func(i int, b backend.Backend) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
			defer cancel()
			h := b.Health(ctx)
			if ctx.Err() != nil {
				rows[i] = healthRow{name: b.Name(), state: backend.HealthDown.String(), detail: "health check timed out"}
				return
			}
			rows[i] = healthRow{name: b.Name(), state: h.State.String(), detail: h.Detail}
		}(i, b)
	}
	wg.Wait()
	return rows
}

// poolStats reads the pool's holdings off the executor, when it has any.
func poolStats(env *Env) (PoolStats, bool) {
	if env == nil || env.Exec == nil {
		return PoolStats{}, false
	}
	ps, ok := env.Exec.(PoolStatter)
	if !ok {
		return PoolStats{}, false
	}
	return ps.PoolStats(), true
}

// sourceErrorLines renders the remembered per-source errors, one per source,
// sorted by source name.
func sourceErrorLines(env *Env) []string {
	if env == nil || env.Inventory == nil {
		return nil
	}
	rep, ok := env.Inventory.(SourceErrorReporter)
	if !ok {
		return nil
	}
	byName := rep.SourceErrors()
	if len(byName) == 0 {
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, name+": "+byName[name].Error())
	}
	return lines
}

// summarizeSourceErrors renders a source-error map on one line.
func summarizeSourceErrors(failed map[string]error) string {
	lines := make([]string, 0, len(failed))
	names := make([]string, 0, len(failed))
	for name := range failed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lines = append(lines, name+": "+failed[name].Error())
	}
	return strings.Join(lines, "; ")
}

// kvLine renders one "key   value" line with the keys aligned.
func kvLine(key, value string) string {
	return fmt.Sprintf("%-10s %s", key, value)
}

// sessionName is the session's name, or "main" when there is none.
func sessionName(env *Env) string {
	if env != nil && env.Session != nil && env.Session.Name != "" {
		return env.Session.Name
	}
	return "main"
}

// hostIDs returns the hosts' IDs, sorted, for the inventory view.
func hostIDs(hosts []inventory.Host) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, string(h.ID))
	}
	sort.Strings(out)
	return out
}
