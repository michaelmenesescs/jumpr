package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/backend/linux"
	"jumper/internal/backend/salt"
	"jumper/internal/bus"
	"jumper/internal/resource"
	"jumper/internal/targets"
)

// The `:salt` command family (SPEC §8.6, BUILD-PLAN §2 M4): native targets
// become jumper target sets, `:salt state` opens the dry-run view, `:salt run`
// proposes a module execution through the bus, and `:salt target` explains how
// the current selection compiles. Nothing here blocks: every remote call runs in
// a tea.Cmd with a deadline.
const (
	// saltBackendName is the registration name of the Salt backend.
	saltBackendName = "salt"
	// saltStateKind is the factory key of the dry-run state view. It is a view
	// kind, not a registry kind, so it is resolved through Env.Factories.
	saltStateKind resource.KindName = "salt-state"
	// saltJobDetailKind is the factory key of the grouped job detail.
	saltJobDetailKind resource.KindName = "salt-job-detail"
	// saltPillarKind is the factory key of the pillar screen (task 093).
	saltPillarKind resource.KindName = "salt-pillar"

	// saltProposeTimeout bounds one planning round trip started by `:salt run`.
	saltProposeTimeout = 30 * time.Second
	// saltTargetTimeout bounds one `--preview-target` resolution.
	saltTargetTimeout = 30 * time.Second
	// saltPillarTimeout bounds the two pillar reads of `:pillar-diff`.
	saltPillarTimeout = 3 * time.Minute
	// saltJobCacheWarning is the once-per-session warning of SPEC §8.6: a Salt
	// transport run leaves the command and its output in the master's job cache.
	saltJobCacheWarning = "salt transport: the command and its output stay in the master's job cache"

	// saltUsage is the argument grammar shown on a parse error.
	saltUsage = "usage: :salt [state [sls]|target|run <fun> [args…] [k=v…]|ping] or :salt -C|-G|-E|-L|-N|-I <expr>|:salt <glob>"
)

// SaltOps is what the `:salt` commands need from the Salt backend. The salt
// package implements it; tests use a fake.
type SaltOps interface {
	// PreviewTarget resolves a native target to the minion IDs Salt matches.
	PreviewTarget(ctx context.Context, t salt.Target) ([]string, error)
	// MinionHost maps a minion ID onto its inventory host.
	MinionHost(id string) (resource.HostID, bool)
	// HostMinion maps an inventory host onto its minion ID.
	HostMinion(host resource.HostID) (string, bool)
	// CompileSet is the targeting plan for a resolved set plus the hosts Salt
	// does not manage.
	CompileSet(hosts []resource.HostID, sel resource.Selector) (salt.Compiled, []resource.HostID)
	// TestPing starts test.ping on minions and returns the job id.
	TestPing(ctx context.Context, minions []string) (string, error)
}

// saltTargetsMsg is `:salt <native target>`'s result: the session targets it
// resolves to, the flash line and the host list to open. The root applies all
// three in order, so the counts flash is not overwritten by the target one.
type saltTargetsMsg struct {
	Set   targets.Set
	Text  string
	Query resource.Query
}

// saltPingMsg is `:salt ping`'s result: the job it started. The root opens the
// job detail and reports the first use of the Salt transport.
type saltPingMsg struct {
	JID      string
	Minions  int
	Unmapped int
	First    bool
}

// RegisterSaltCommands registers `:salt` on c. b is the bus `:salt run` plans
// through; nil still registers the command but refuses to run.
func RegisterSaltCommands(c *Commands, b bus.Bus) error {
	if c == nil {
		return nil
	}
	if err := c.Register(Command{
		Name:  "salt",
		Usage: ":salt [state [sls]|target|run <fun> [args…] [k=v…]|ping|<target>]",
		Help:  "Salt: dry run, targets, module run, ping",
		Run: func(env *Env, args string) tea.Cmd {
			return runSalt(env, b, args)
		},
		Complete: func(_ *Env, args string) []string {
			return filterPrefix([]string{"ping", "run", "state", "target"}, partialWord(args))
		},
	}); err != nil {
		return err
	}
	// :states is the state tree browser (SPEC §8.6): envs, sls files, the top
	// file and the rendered states of one minion.
	if err := c.Register(Command{
		Name:    "states",
		Aliases: []string{"sls"},
		Usage:   ":states [env]",
		Help:    "Salt state tree: envs, sls files, top file, rendered states",
		Run: func(env *Env, args string) tea.Cmd {
			return saltStatesCmd(env, strings.TrimSpace(args))
		},
	}); err != nil {
		return err
	}
	// :pillar is the pillar screen (SPEC §8.6 "Pillar"): structure and keys
	// visible, values hidden until one is revealed on demand.
	if err := c.Register(Command{
		Name:  "pillar",
		Usage: ":pillar <minion>",
		Help:  "Salt pillar: keys and structure, values hidden",
		Run: func(env *Env, args string) tea.Cmd {
			return saltPillarCmd(env, strings.TrimSpace(args))
		},
	}); err != nil {
		return err
	}
	// :pillar-diff compares two minions' pillar shape without reading a value.
	return c.Register(Command{
		Name:  "pillar-diff",
		Usage: ":pillar-diff <minion> <minion>",
		Help:  "Salt pillar: which key paths and types differ between two minions",
		Run: func(env *Env, args string) tea.Cmd {
			return saltPillarDiffCmd(env, strings.TrimSpace(args))
		},
	})
}

// PillarOps is what the `:pillar` commands need from the Salt backend. The salt
// package implements it; tests use a fake.
type PillarOps interface {
	// PillarKeys returns one minion's pillar as types and lengths only.
	PillarKeys(ctx context.Context, minion string) (salt.PillarTree, error)
	// PillarValue reads exactly one value, for the reveal dialog.
	PillarValue(ctx context.Context, minion, keyPath string) (string, error)
}

// saltPillarOf returns the Salt backend as the interface the pillar commands
// use.
func saltPillarOf(env *Env) (PillarOps, bool) {
	if env == nil || env.Registry == nil {
		return nil, false
	}
	b, ok := env.Registry.Get(saltBackendName)
	if !ok {
		return nil, false
	}
	ops, ok := b.(PillarOps)
	return ops, ok
}

// saltPillarCmd opens the pillar screen of minion. Without an argument the
// session's single minion is used; anything else would be a guess.
func saltPillarCmd(env *Env, minion string) tea.Cmd {
	if _, ok := saltPillarOf(env); !ok {
		return Flash(FlashError, "salt is not configured: set salt.master in config.yaml")
	}
	if minion == "" {
		m, ok := singleSessionMinion(env)
		if !ok {
			return Flash(FlashError, "usage: :pillar <minion>")
		}
		minion = m
	}
	q := resource.Query{Kind: saltPillarKind, Args: map[string]string{"minion": minion}}
	return saltViewCmd(env, saltPillarKind, q)
}

// singleSessionMinion is the session's one minion, when it has exactly one.
func singleSessionMinion(env *Env) (string, bool) {
	ops, ok := saltOpsOf(env)
	if !ok {
		return "", false
	}
	minions, _ := minionsOf(ops, sessionHosts(env))
	unique := make(map[string]bool, len(minions))
	for _, m := range minions {
		if m != "" {
			unique[m] = true
		}
	}
	if len(unique) != 1 {
		return "", false
	}
	for m := range unique {
		return m, true
	}
	return "", false
}

// saltPillarDiffCmd reads two minions' pillar shape and shows the key paths
// whose type differs. No value is read: the comparison never needs one.
func saltPillarDiffCmd(env *Env, args string) tea.Cmd {
	ops, ok := saltPillarOf(env)
	if !ok {
		return Flash(FlashError, "salt is not configured: set salt.master in config.yaml")
	}
	fields := strings.Fields(args)
	if len(fields) != 2 {
		return Flash(FlashError, "usage: :pillar-diff <minion> <minion>")
	}
	left, right := fields[0], fields[1]
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), saltPillarTimeout)
		defer cancel()
		lt, err := ops.PillarKeys(ctx, left)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "pillar-diff " + left + ": " + err.Error()}
		}
		rt, err := ops.PillarKeys(ctx, right)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "pillar-diff " + right + ": " + err.Error()}
		}
		return PushViewMsg{View: newTextPage(env, "pillar diff", pillarDiffLines(left, right, salt.DiffPillars(lt, rt)))}
	}
}

// pillarDiffLines renders the differences between two pillar shapes: the key
// path and the type on each side, "missing" where the key is absent.
func pillarDiffLines(left, right string, diffs []salt.PillarDiff) []string {
	lines := []string{fmt.Sprintf("%s ↔ %s · %s", left, right, countOf(len(diffs), "difference"))}
	if len(diffs) == 0 {
		return append(lines, "", "the two minions have the same key paths and types")
	}
	pathW := 0
	for _, d := range diffs {
		pathW = max(pathW, len(d.Path))
	}
	pathW = min(pathW, 60)
	lines = append(lines, "KEY PATH"+"  "+padRight(left, 12)+"  "+right)
	for _, d := range diffs {
		lines = append(lines, padRight(d.Path, pathW)+"  "+
			padRight(pillarSide(d.Left), 12)+"  "+pillarSide(d.Right))
	}
	lines = append(lines, "", "only structure: no pillar value is read by a diff")
	return lines
}

// pillarSide names one side of a difference.
func pillarSide(typ string) string {
	if typ == "" {
		return "missing"
	}
	return typ
}

// padRight pads s to w cells with spaces; longer text is kept whole.
func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// saltStatesCmd opens the state tree browser, narrowed to env when given. The
// session targets decide which minions the assignment counts cover.
func saltStatesCmd(env *Env, envName string) tea.Cmd {
	if _, ok := saltOpsOf(env); !ok {
		return Flash(FlashError, "salt is not configured: set salt.master in config.yaml")
	}
	q := resource.Query{Kind: salt.KindSls, Hosts: sessionHosts(env)}
	if envName != "" {
		q.Args = map[string]string{"env": envName}
	}
	return saltViewCmd(env, salt.KindSls, q)
}

// runSalt dispatches the `:salt` argument text. The verbs come first; anything
// else is a native target, which is what makes `:salt -C '…'` work.
func runSalt(env *Env, b bus.Bus, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	if args == "" {
		return Flash(FlashError, "%s", saltUsage)
	}
	word, rest := splitFirstWord(args)
	switch word {
	case "state":
		return saltStateCmd(env, strings.TrimSpace(rest))
	case "target":
		if strings.TrimSpace(rest) != "" {
			return Flash(FlashError, "usage: :salt target")
		}
		return saltTargetCmd(env)
	case "run":
		return saltRunCmd(env, b, strings.TrimSpace(rest))
	case "ping":
		if strings.TrimSpace(rest) != "" {
			return Flash(FlashError, "usage: :salt ping")
		}
		return saltPingCmd(env)
	}
	return saltNativeCmd(env, args)
}

// saltOpsOf returns the Salt backend as the interface the commands use.
func saltOpsOf(env *Env) (SaltOps, bool) {
	if env == nil || env.Registry == nil {
		return nil, false
	}
	b, ok := env.Registry.Get(saltBackendName)
	if !ok {
		return nil, false
	}
	ops, ok := b.(SaltOps)
	return ops, ok
}

// saltStateCmd opens the dry-run state view of sls (empty means the highstate)
// on the session targets. The view runs the dry run itself.
func saltStateCmd(env *Env, sls string) tea.Cmd {
	if _, ok := saltOpsOf(env); !ok {
		return Flash(FlashError, "salt is not configured: set salt.master in config.yaml")
	}
	if len(sessionHosts(env)) == 0 {
		return Flash(FlashError, "no targets; try :targets <selector> first")
	}
	q := resource.Query{Kind: saltStateKind, Args: map[string]string{"sls": sls}}
	return saltViewCmd(env, saltStateKind, q)
}

// saltTargetCmd shows how the current target set compiles.
func saltTargetCmd(env *Env) tea.Cmd {
	ops, ok := saltOpsOf(env)
	if !ok {
		return Flash(FlashError, "salt is not configured: set salt.master in config.yaml")
	}
	if env == nil || env.Session == nil || len(env.Session.Targets.Hosts) == 0 {
		return Flash(FlashInfo, "no targets; try :targets <selector> first")
	}
	set := env.Session.Targets
	return func() tea.Msg {
		compiled, unmapped := ops.CompileSet(set.Hosts, set.Selector)
		return PushViewMsg{View: newTextPage(env, "salt target", saltTargetLines(set, compiled, unmapped))}
	}
}

// saltTargetLines renders `:salt target`: the selection, its minions and the
// compiled targets as the CLI would take them.
func saltTargetLines(set targets.Set, compiled salt.Compiled, unmapped []resource.HostID) []string {
	label := set.Label
	if label == "" {
		label = "(none)"
	}
	lines := []string{
		"label: " + label,
		fmt.Sprintf("hosts: %d (minions: %d)", len(set.Hosts), len(compiled.Minions)),
	}
	if len(unmapped) > 0 {
		lines = append(lines, fmt.Sprintf("not minions: %d hosts", len(unmapped)))
	}
	lines = append(lines, "compiled: "+compiled.Explain)
	for _, t := range compiled.Targets {
		lines = append(lines, "cli: "+t.String())
	}
	return lines
}

// saltRunCmd proposes a `salt.module.run`: any execution module function on the
// session targets, planned and executed through the bus.
func saltRunCmd(env *Env, b bus.Bus, args string) tea.Cmd {
	fun, posArgs, kwargs, err := parseModuleArgs(args)
	if err != nil {
		return Flash(FlashError, "%v; %s", err, saltUsage)
	}
	hosts := sessionHosts(env)
	if len(hosts) == 0 {
		return Flash(FlashError, "no targets; try :targets <selector> first")
	}
	if b == nil {
		return Flash(FlashError, "no action bus configured")
	}
	params := map[string]string{salt.ParamFun: fun}
	if posArgs != "" {
		params[salt.ParamArgs] = posArgs
	}
	if kwargs != "" {
		params[salt.ParamKwargs] = kwargs
	}
	req := action.Request{
		SpecID:  salt.SpecModuleRun,
		Targets: actionTargets(env, hosts),
		Params:  params,
		Actor:   action.Human,
	}
	first := noteSaltUse(env)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), saltProposeTimeout)
		defer cancel()
		p, err := b.Propose(ctx, req)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "salt run: " + err.Error()}
		}
		return OpenDialogMsg{Dialog: newRunPlanDialog(b, env, saltPlanWarning(p, first))}
	}
}

// parseModuleArgs splits `:salt run`'s argument text into the function, its
// positional arguments and its `k=v` keyword arguments, each kept as typed.
func parseModuleArgs(args string) (fun, posArgs, kwargs string, err error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", "", "", fmt.Errorf("%s needs a function", saltUsage)
	}
	fun = fields[0]
	var pos, kw []string
	for _, word := range fields[1:] {
		if k, _, ok := strings.Cut(word, "="); ok && isKwargName(k) {
			kw = append(kw, word)
			continue
		}
		pos = append(pos, word)
	}
	return fun, strings.Join(pos, " "), strings.Join(kw, " "), nil
}

// isKwargName reports whether k can be a Salt keyword argument name.
func isKwargName(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// saltPingCmd starts test.ping on the session targets as one Salt job and opens
// the job detail. Hosts Salt does not manage are counted, not sent.
func saltPingCmd(env *Env) tea.Cmd {
	ops, ok := saltOpsOf(env)
	if !ok {
		return Flash(FlashError, "salt is not configured: set salt.master in config.yaml")
	}
	hosts := sessionHosts(env)
	if len(hosts) == 0 {
		return Flash(FlashError, "no targets; try :targets <selector> first")
	}
	first := noteSaltUse(env)
	return func() tea.Msg {
		minions, unmapped := minionsOf(ops, hosts)
		if len(minions) == 0 {
			return FlashMsg{Level: FlashError, Text: "none of the targets is a salt minion"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), saltProposeTimeout)
		defer cancel()
		jid, err := ops.TestPing(ctx, minions)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "salt ping: " + err.Error()}
		}
		return saltPingMsg{JID: jid, Minions: len(minions), Unmapped: unmapped, First: first}
	}
}

// saltNativeCmd resolves a native target (`-C`, `-G`, `-E`, `-L`, `-N`, `-I`
// or a glob) through `--preview-target`, makes the mapped hosts the session
// targets and opens the host list on them.
func saltNativeCmd(env *Env, args string) tea.Cmd {
	ops, ok := saltOpsOf(env)
	if !ok {
		return Flash(FlashError, "salt is not configured: set salt.master in config.yaml")
	}
	t, err := salt.ParseNative(args)
	if err != nil {
		return Flash(FlashError, "%v; %s", err, saltUsage)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), saltTargetTimeout)
		defer cancel()
		set, text, err := previewTargets(ctx, ops, t)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "salt target: " + err.Error()}
		}
		if len(set.Hosts) == 0 {
			return FlashMsg{Level: FlashWarn, Text: text + "; no target set"}
		}
		return saltTargetsMsg{
			Set:   set,
			Text:  text,
			Query: resource.Query{Kind: linux.KindHost, Hosts: set.Hosts},
		}
	}
}

// previewTargets resolves a native target to a target set and the one-line
// summary of what came back. Minions without an inventory host are counted but
// never become targets.
func previewTargets(ctx context.Context, ops SaltOps, t salt.Target) (targets.Set, string, error) {
	minions, err := ops.PreviewTarget(ctx, t)
	if err != nil {
		return targets.Set{}, "", err
	}
	hosts := make([]resource.HostID, 0, len(minions))
	unmapped := 0
	for _, m := range minions {
		h, ok := ops.MinionHost(m)
		if !ok {
			unmapped++
			continue
		}
		hosts = append(hosts, h)
	}
	set := targets.FromHosts(t.String(), hosts)
	text := fmt.Sprintf("%s → %s", countOf(len(minions), "minion"), countOf(len(set.Hosts), "host"))
	if unmapped > 0 {
		text += fmt.Sprintf(", %d unmapped", unmapped)
	}
	return set, text, nil
}

// minionsOf maps hosts onto minion IDs and counts the ones Salt does not manage.
func minionsOf(ops SaltOps, hosts []resource.HostID) ([]string, int) {
	minions := make([]string, 0, len(hosts))
	unmapped := 0
	for _, h := range hosts {
		id, ok := ops.HostMinion(h)
		if !ok {
			unmapped++
			continue
		}
		minions = append(minions, id)
	}
	return minions, unmapped
}

// saltViewCmd builds a view of a synthetic kind through Env.Factories. The
// kinds are not in the registry (no backend serves them), so the factory is
// resolved directly; the work happens inside the command.
func saltViewCmd(env *Env, kind resource.KindName, q resource.Query) tea.Cmd {
	var f Factory
	if env != nil && env.Factories != nil {
		f = env.Factories[kind]
	}
	if f == nil {
		return Flash(FlashError, "no %s view available", kind)
	}
	return func() tea.Msg {
		v, err := f(env, q)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		if v == nil {
			return FlashMsg{Level: FlashError, Text: "no " + string(kind) + " view"}
		}
		return PushViewMsg{View: v}
	}
}

// noteSaltUse records the first use of the Salt transport in this session and
// reports whether this call is that first use. It runs in the model goroutine,
// never inside a tea.Cmd.
func noteSaltUse(env *Env) bool {
	if env == nil || env.Session == nil {
		return false
	}
	if env.Session.saltUsed {
		return false
	}
	env.Session.saltUsed = true
	return true
}

// saltPlanWarning appends the job-cache warning to a Salt plan the first time
// the transport is used in a session. first is what noteSaltUse reported.
func saltPlanWarning(p bus.Proposal, first bool) bus.Proposal {
	if !first {
		return p
	}
	p.Plan.Warnings = append(p.Plan.Warnings, saltJobCacheWarning)
	return p
}

// saltSuggestHint is the plan line that suggests the Salt transport: the target
// set is larger than salt.suggest_over. Empty when the hint does not apply.
func saltSuggestHint(env *Env, targets int) string {
	if env == nil || !env.Salt.Configured || env.Salt.SuggestOver <= 0 {
		return ""
	}
	if targets <= env.Salt.SuggestOver {
		return ""
	}
	return fmt.Sprintf("%s targets: consider --via salt", countWithCommas(targets))
}

// saltHealthy reports whether the Salt backend answers manage.status in time.
func saltHealthy(ctx context.Context, env *Env) bool {
	if env == nil || env.Registry == nil {
		return false
	}
	b, ok := env.Registry.Get(saltBackendName)
	if !ok || b == nil {
		return false
	}
	return b.Health(ctx).State == backend.HealthOK
}

// countOf renders "14 minions" / "1 minion" — the noun agreeing with n.
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// countWithCommas renders 1512 as 1,512.
func countWithCommas(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre == 0 {
		pre = 3
	}
	b.WriteString(s[:pre])
	for i := pre; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
