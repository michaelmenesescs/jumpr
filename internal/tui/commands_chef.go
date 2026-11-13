package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend/chef"
	"jumper/internal/bus"
	"jumper/internal/render"
	"jumper/internal/resource"
)

// The `:chef` command family (SPEC §8.7, BUILD-PLAN §2 M10): `drift` shows the
// check-in age, the last run status and the run-list spread of the target set,
// and `whyrun`/`run` propose the chef-client dry run and the converging run
// through the action bus. Nothing here blocks: every remote read runs in a
// tea.Cmd with a deadline.
const (
	// chefBackendName is the registration name of the Chef backend.
	chefBackendName = "chef"
	// chefDriftTimeout bounds the two knife reads `:chef drift` makes.
	chefDriftTimeout = 2 * time.Minute
	// chefProposeTimeout bounds one planning round trip. It matches the bus's
	// own plan budget, which is what actually cuts a long why-run preview.
	chefProposeTimeout = 30 * time.Second
	// chefUsage is the argument grammar shown on a parse error.
	chefUsage = "usage: :chef drift|whyrun|run"
)

// ChefOptions is what the `:chef` commands need beyond the backend itself.
// RunStatusAttr is the node attribute path holding the last run status
// (`chef.run_status_attr`); empty hides the column (SPEC §20 open question 3).
type ChefOptions struct {
	RunStatusAttr string
}

// ChefDrift is the read half of the Chef backend that `:chef drift` uses.
// *chef.Backend implements it; tests use a fake.
type ChefDrift interface {
	Drift(ctx context.Context, hosts []resource.HostID, statusAttr string) (chef.DriftReport, error)
}

// RegisterChefCommands registers `:chef` on c. b is the bus the client runs are
// planned through; nil still registers the command but refuses to run.
func RegisterChefCommands(c *Commands, b bus.Bus, opts ChefOptions) error {
	if c == nil {
		return nil
	}
	return c.Register(Command{
		Name:  "chef",
		Usage: ":chef drift|whyrun|run",
		Help:  "Chef: drift signals, why-run and chef-client through the bus",
		Run: func(env *Env, args string) tea.Cmd {
			return runChef(env, b, opts, args)
		},
		Complete: func(_ *Env, args string) []string {
			return filterPrefix([]string{"drift", "run", "whyrun"}, partialWord(args))
		},
	})
}

// runChef dispatches the `:chef` argument text.
func runChef(env *Env, b bus.Bus, opts ChefOptions, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	if args == "" {
		return Flash(FlashError, "%s", chefUsage)
	}
	word, rest := splitFirstWord(args)
	if rest != "" {
		return Flash(FlashError, "usage: :chef %s", word)
	}
	switch word {
	case "drift":
		return chefDriftCmd(env, opts)
	case "whyrun":
		return chefClientCmd(env, b, chef.SpecWhyRun)
	case "run":
		return chefClientCmd(env, b, chef.SpecClientRun)
	}
	return Flash(FlashError, "%s", chefUsage)
}

// chefDriftOf returns the Chef backend as the interface the drift command uses.
func chefDriftOf(env *Env) (ChefDrift, bool) {
	if env == nil || env.Registry == nil {
		return nil, false
	}
	b, ok := env.Registry.Get(chefBackendName)
	if !ok {
		return nil, false
	}
	ops, ok := b.(ChefDrift)
	return ops, ok
}

// chefDriftCmd reads the drift signals of the session targets and opens them as
// a page. The two knife reads happen inside the command, never in Update.
func chefDriftCmd(env *Env, opts ChefOptions) tea.Cmd {
	ops, ok := chefDriftOf(env)
	if !ok {
		return Flash(FlashError, "chef is not configured: set chef.workstation in config.yaml")
	}
	hosts := sessionHosts(env)
	if len(hosts) == 0 {
		return Flash(FlashError, "no targets; try :targets <selector> first")
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), chefDriftTimeout)
		defer cancel()
		rep, err := ops.Drift(ctx, hosts, opts.RunStatusAttr)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "chef drift: " + err.Error()}
		}
		return PushViewMsg{View: newTextPage(env, "chef drift", chefDriftLines(rep))}
	}
}

// chefDriftLines renders the drift table: the header counts, one row per host
// with its check-in age and the run status when that column is configured, then
// the run-list groups.
func chefDriftLines(rep chef.DriftReport) []string {
	header := fmt.Sprintf("%s · %s", countOf(len(rep.Rows), "node"), rep.Headline())
	if n := rep.Unknown(); n > 0 {
		header += fmt.Sprintf(" · %d not in chef", n)
	}
	lines := []string{header, ""}

	nameW := len("NODE")
	for _, row := range rep.Rows {
		if n := len(rowName(row)); n > nameW {
			nameW = n
		}
	}
	nameW = min(nameW, 40)

	cols := []string{padRight("NODE", nameW), padRight("CHECK-IN", 10), padRight("STATUS", 8)}
	if rep.Attr != "" {
		cols = append(cols, padRight("RUN STATUS", 14))
	}
	lines = append(lines, strings.TrimRight(strings.Join(cols, "  "), " "))
	for _, row := range rep.Rows {
		cells := []string{
			padRight(truncate(rowName(row), nameW), nameW),
			padRight(checkinText(row), 10),
			padRight(statusText(row.Status), 8),
		}
		if rep.Attr != "" {
			cells = append(cells, padRight(emptyAsDash(row.RunStatus), 14))
		}
		lines = append(lines, strings.TrimRight(strings.Join(cells, "  "), " "))
	}

	lines = append(lines, "", "run lists: "+rep.Headline())
	for _, g := range rep.Groups {
		lines = append(lines, fmt.Sprintf("%s: %s", countOf(len(g.Nodes), "node"),
			strings.Join(g.Nodes, ", ")))
		lines = append(lines, "  "+emptyAsDash(strings.Join(g.RunList, ", ")))
	}
	if rep.Attr == "" {
		lines = append(lines, "", "no run-status attribute configured: set chef.run_status_attr to show it")
	}
	return lines
}

// rowName is the name a drift row is shown under: the Chef node, or the host
// when Chef does not know it.
func rowName(row chef.DriftRow) string {
	if row.Node != "" {
		return row.Node
	}
	return string(row.Host)
}

// checkinText renders a row's check-in age, "—" when the node reported none.
func checkinText(row chef.DriftRow) string {
	if !row.HasCheckin {
		return "—"
	}
	return render.Duration(row.Checkin)
}

// emptyAsDash renders an empty cell as "—".
func emptyAsDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// statusText names a resource status the way the drift table prints it.
func statusText(s resource.Status) string {
	switch s {
	case resource.StatusOK:
		return "ok"
	case resource.StatusWarn:
		return "warn"
	case resource.StatusCrit:
		return "crit"
	case resource.StatusNA:
		return "—"
	}
	return "unknown"
}

// truncate cuts s to at most w bytes: a node name is ASCII, so bytes are cells.
func truncate(s string, w int) string {
	if len(s) <= w {
		return s
	}
	if w <= 1 {
		return s[:w]
	}
	return s[:w-1] + "…"
}

// chefClientCmd proposes a chef-client spec on the session targets and opens the
// plan dialog. The kill switch, the policy and the confirmation are the bus's,
// so a Chef write is guarded exactly like every other one.
func chefClientCmd(env *Env, b bus.Bus, specID string) tea.Cmd {
	hosts := sessionHosts(env)
	if len(hosts) == 0 {
		return Flash(FlashError, "no targets; try :targets <selector> first")
	}
	if b == nil {
		return Flash(FlashError, "no action bus configured")
	}
	req := action.Request{
		SpecID:  specID,
		Targets: actionTargets(env, hosts),
		Actor:   action.Human,
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), chefProposeTimeout)
		defer cancel()
		p, err := b.Propose(ctx, req)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: specID + ": " + err.Error()}
		}
		return OpenDialogMsg{Dialog: newRunPlanDialog(b, env, p)}
	}
}
