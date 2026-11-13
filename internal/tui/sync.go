package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/investigation"
	"jumper/internal/mux"
	"jumper/internal/mux/termpane"
	"jumper/internal/resource"
)

// Synchronized input and the shell guard rails (SPEC §11.1, task 064).
//
// Two ways to type into several panes, and both are guarded:
//
//   - a broadcast line collects one line, plans it as mux.sync.line through the
//     action bus (so policy sees it, the user confirms it, a canary pane gets it
//     first and the record keeps it);
//   - raw synchronized input sends every keystroke to the terminal panes of the
//     window, after the bus confirmed that the mode may be turned on.
//
// Nothing here does I/O: every remote write runs in a tea.Cmd and the mode is
// switched on by the bus step telling the root through syncMode.

const (
	// syncIdleTimeout is how long raw synchronized input survives without a
	// key press (SPEC §11.1).
	syncIdleTimeout = 5 * time.Minute
	// defaultMaxSyncPanes is mux.max_sync_panes when the environment names no
	// other limit (BUILD-PLAN §11).
	defaultMaxSyncPanes = 8
	// syncModeBuffer is how many mode changes may wait for the root.
	syncModeBuffer = 8
)

// syncBroadcastMsg is the line the prompt collected, ready to be planned.
type syncBroadcastMsg struct{ Line string }

// syncModeMsg reports that a bus step switched raw synchronized input on or off.
type syncModeMsg struct{ On bool }

// syncIdleMsg asks the root whether the mode has been idle too long.
type syncIdleMsg struct{}

// syncState is the root's side of a raw synchronized session: the line being
// reconstructed and when the last key arrived.
type syncState struct {
	line    termpane.LineTracker
	lastKey time.Time
}

// terminalPane is the part of a terminal pane content synchronized input and
// the shell guard rails need. *termpane.Pane implements it.
type terminalPane interface {
	Host() resource.HostID
	Busy() bool
	Protected() bool
	SetProtected(on bool)
	SendLine(line string) error
}

// syncTarget is one live terminal pane of the active window.
type syncTarget struct {
	ID     mux.PaneID
	Host   resource.HostID
	Pane   terminalPane
	labels map[string]string
}

// syncIdleTick asks for an idle check after the whole timeout.
func syncIdleTick() tea.Cmd {
	return tea.Tick(syncIdleTimeout, func(time.Time) tea.Msg { return syncIdleMsg{} })
}

// now is the environment's clock.
func (r *Root) now() time.Time {
	if r.env != nil && r.env.Now != nil {
		return r.env.Now()
	}
	return time.Now()
}

// policy is the policy the guard rails ask. Without one the built-in rules
// apply, which is what the bus does too.
func (r *Root) policy() *bus.Policy {
	if r.guard != nil {
		return r.guard
	}
	return bus.DefaultPolicy()
}

// SetPolicy installs the policy the shell guard rails and the sync mode ask.
// Without one the built-in policy applies, which is what the bus does too.
func (r *Root) SetPolicy(p *bus.Policy) { r.guard = p }

// SetMaxSyncPanes installs mux.max_sync_panes; 0 keeps the default of 8.
func (r *Root) SetMaxSyncPanes(n int) { r.maxSync = n }

// readOnly reports the session-level read-only switch, which the policy switch
// also sets.
func (r *Root) readOnly() bool {
	if r.env != nil && r.env.Session != nil && r.env.Session.ReadOnly {
		return true
	}
	return r.policy().ReadOnly()
}

// maxSyncPanes is mux.max_sync_panes, or its default.
func (r *Root) maxSyncPanes() int {
	if r.maxSync > 0 {
		return r.maxSync
	}
	return defaultMaxSyncPanes
}

// hostLabels are the labels policy matches on for a host, when the inventory
// knows it.
func (r *Root) hostLabels(host resource.HostID) map[string]string {
	if r.env == nil || r.env.Inventory == nil {
		return nil
	}
	h, ok := r.env.Inventory.Resolve(string(host))
	if !ok {
		return nil
	}
	return h.Labels
}

// connectedTerminals lists the live terminal panes of the active window, in
// pane order, with the focused pane first when it is one: that is the canary of
// a broadcast (task 064 requirement 2).
func (r *Root) connectedTerminals() []syncTarget {
	if r.mux == nil || r.mux.Session() == nil {
		return nil
	}
	w := r.mux.Session().ActiveWindow()
	if w == nil {
		return nil
	}
	contents := r.mux.Contents()
	out := make([]syncTarget, 0, len(contents))
	for _, id := range w.Layout.Panes() {
		pane, ok := contents[id].(terminalPane)
		if !ok || !pane.Busy() {
			continue
		}
		out = append(out, syncTarget{ID: id, Host: pane.Host(), Pane: pane})
	}
	for i, t := range out {
		if t.ID != w.Focus || i == 0 {
			continue
		}
		first := out[i]
		copy(out[1:i+1], out[:i])
		out[0] = first
		break
	}
	return out
}

// syncTargetsOf turns the panes into bus targets. The pane ID travels in the
// ref, so two panes on one host stay two targets.
func syncTargetsOf(targets []syncTarget) []action.Target {
	out := make([]action.Target, 0, len(targets))
	for _, t := range targets {
		out = append(out, action.Target{
			Host:   t.Host,
			Ref:    resource.Ref{Kind: "pane", Host: t.Host, Name: strconv.Itoa(int(t.ID))},
			Labels: t.labels,
		})
	}
	return out
}

// syncOp handles prefix B and `:sync [raw|off]`.
func (r *Root) syncOp(arg string) tea.Cmd {
	switch strings.TrimSpace(arg) {
	case "", "line", "broadcast":
		return r.broadcastPrompt()
	case "raw", "on":
		return r.syncRaw(true)
	case "off":
		return r.syncRaw(false)
	}
	return Flash(FlashError, "usage: :sync [raw|off]")
}

// broadcastPrompt asks for the one line sent to the terminal panes of the
// window (SPEC §11.1, `prefix B`).
func (r *Root) broadcastPrompt() tea.Cmd {
	targets := r.connectedTerminals()
	if len(targets) == 0 {
		return Flash(FlashInfo, "no terminal panes in this window")
	}
	label := fmt.Sprintf("broadcast to %d panes ❯", len(targets))
	r.dialog = NewPromptDialog(themeOf(r.env), "broadcast line", label, "", func(text string) tea.Cmd {
		// The prompt closes and submits in one batch; the root swallows the
		// close so it cannot tear down the plan dialog the line asks for.
		r.syncClose++
		return func() tea.Msg { return syncBroadcastMsg{Line: text} }
	})
	return nil
}

// broadcastLine plans one line as mux.sync.line over the terminal panes: the
// plan dialog lists the panes and the exact line, policy checks the displays,
// the bus runs the canary first and the record keeps the result.
func (r *Root) broadcastLine(line string) tea.Cmd {
	line = strings.TrimSpace(line)
	if line == "" {
		return Flash(FlashInfo, "nothing to broadcast")
	}
	targets := r.connectedTerminals()
	if len(targets) == 0 {
		return Flash(FlashInfo, "no terminal panes in this window")
	}
	for i := range targets {
		targets[i].labels = r.hostLabels(targets[i].Host)
	}
	return r.planRequest(action.Request{
		SpecID:  SpecSyncLine,
		Targets: syncTargetsOf(targets),
		Params:  map[string]string{"line": line},
		Actor:   action.Human,
	})
}

// syncRaw plans turning raw synchronized input on or off. Turning it on is
// itself a confirmed, recorded action (SPEC §11.1).
func (r *Root) syncRaw(on bool) tea.Cmd {
	if on {
		if reason := r.syncRefusal(); reason != "" {
			return Flash(FlashError, "%s", reason)
		}
	}
	targets := r.connectedTerminals()
	if len(targets) == 0 {
		return Flash(FlashInfo, "no terminal panes in this window")
	}
	params := map[string]string{
		"on":    strconv.FormatBool(on),
		"panes": strconv.Itoa(len(targets)),
	}
	// The mode is one decision about one window, so the plan has one step.
	return r.planRequest(action.Request{
		SpecID:  SpecSyncRaw,
		Targets: syncTargetsOf(targets)[:1],
		Params:  params,
		Actor:   action.Human,
	})
}

// syncRefusal reports why raw synchronized input may not be turned on, or "".
func (r *Root) syncRefusal() string {
	targets := r.connectedTerminals()
	if len(targets) == 0 {
		return "no terminal panes in this window"
	}
	if maxPanes := r.maxSyncPanes(); maxPanes > 0 && len(targets) > maxPanes {
		return fmt.Sprintf("raw sync is limited to %d panes; this window has %d", maxPanes, len(targets))
	}
	if r.readOnly() {
		return "read-only mode: raw synchronized input is off limits"
	}
	if r.policy().SyncRaw() == "deny" {
		return "policy sync.raw is deny"
	}
	for _, t := range targets {
		if t.Pane.Protected() {
			return fmt.Sprintf("%s is a protected host: raw sync is refused", t.Host)
		}
	}
	return ""
}

// planRequest shows the planning spinner and asks the bus for the plan of req.
// It is the sync flow's own entry point: a broadcast line is not a table
// selection, so it cannot go through the actions menu.
func (r *Root) planRequest(req action.Request) tea.Cmd {
	b := r.busOf()
	if b == nil {
		return Flash(FlashError, "no action bus configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), actionProposeTimeout)
	if r.planCancel != nil {
		r.planCancel()
	}
	r.planCancel = cancel
	r.dialog = newPlanningDialog(themeOf(r.env))
	return proposeCmd(ctx, b, req)
}

// waitSyncMode waits for a bus step to switch the mode on or off. The root
// re-arms it after every message, so the mode always follows the bus that was
// confirmed.
func (r *Root) waitSyncMode() tea.Cmd {
	ch := r.syncMode
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		on, ok := <-ch
		if !ok {
			return nil
		}
		return syncModeMsg{On: on}
	}
}

// applySyncMode switches the mode after the bus confirmed it.
func (r *Root) applySyncMode(on bool) tea.Cmd {
	if !on {
		return r.stopSync("requested")
	}
	targets := r.connectedTerminals()
	if len(targets) == 0 {
		r.stopSync("")
		return Flash(FlashWarn, "no terminal panes left to sync")
	}
	ids := make([]mux.PaneID, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.ID)
	}
	r.mux.SetSync(ids)
	r.sync = &syncState{lastKey: r.now()}
	return tea.Batch(
		Flash(FlashWarn, "synchronized input on: %d panes · :sync off to stop", len(ids)),
		syncIdleTick(),
	)
}

// stopSync switches raw synchronized input off.
func (r *Root) stopSync(why string) tea.Cmd {
	if r.mux != nil {
		r.mux.SetSync(nil)
	}
	r.sync = nil
	if why == "" {
		return nil
	}
	return Flash(FlashInfo, "sync off: %s", why)
}

// checkSyncIdle turns the mode off when no key arrived for syncIdleTimeout, and
// otherwise asks again when the deadline moves.
func (r *Root) checkSyncIdle() tea.Cmd {
	if !r.mux.SyncOn() {
		if r.sync != nil {
			r.sync = nil
		}
		return nil
	}
	if d := r.now().Sub(r.sync.lastKey); d < syncIdleTimeout {
		return tea.Tick(syncIdleTimeout-d, func(time.Time) tea.Msg { return syncIdleMsg{} })
	}
	return r.stopSync("5 minutes idle")
}

// routeKey hands a key to the mux, handling the Enter of a raw synchronized
// session itself: the reconstructed line has to pass policy before it goes out.
func (r *Root) routeKey(k tea.KeyMsg) tea.Cmd {
	if !r.syncKeyApplies(k) {
		if r.sync != nil && !r.mux.SyncOn() {
			// A window change or a pane add/remove ended the mode.
			r.sync = nil
		}
		return r.routeMux(k)
	}
	line, enter := r.sync.line.Key(k)
	r.sync.lastKey = r.now()
	if !enter {
		return r.routeMux(k)
	}
	return r.syncEnter(line)
}

// syncKeyApplies reports whether the key would have gone to a terminal pane of
// the synchronized window. The prefix and the keys after it never count.
func (r *Root) syncKeyApplies(k tea.KeyMsg) bool {
	if r.sync == nil || r.mux == nil || !r.mux.SyncOn() {
		return false
	}
	if r.mux.PrefixArmed() || k.String() == r.prefix {
		return false
	}
	_, c := r.mux.Focused()
	return c != nil && c.RawKeys()
}

// syncEnter sends the finished line to every synchronized pane, or holds the
// Enter back and clears the line when policy denies it (SPEC §11.1: a seat
// belt, not a boundary).
func (r *Root) syncEnter(line string) tea.Cmd {
	n := r.mux.SyncCount()
	if d := r.policy().CheckLine(line); d != nil {
		cmd := r.mux.SyncTo(tea.KeyMsg{Type: tea.KeyCtrlU})
		return tea.Batch(cmd, Flash(FlashError, "sync: %s", denialText(d)))
	}
	cmd := r.mux.SyncTo(tea.KeyMsg{Type: tea.KeyEnter})
	var cmds []tea.Cmd
	if r.env != nil {
		if c := r.env.AppendInvestigation(investigation.EntryInput, fmt.Sprintf("sync ×%d: %s", n, line), ""); c != nil {
			cmds = append(cmds, c)
		}
	}
	cmds = append(cmds, cmd)
	return tea.Batch(cmds...)
}

// denialText is a denial's reason and the rule that fired, on one line.
func denialText(d *bus.Denial) string {
	switch {
	case d == nil:
		return "denied by policy"
	case d.Reason != "" && d.Rule != "":
		return d.Reason + " (" + d.Rule + ")"
	case d.Reason != "":
		return d.Reason
	default:
		return d.Rule
	}
}

// shellRefusal reports why an interactive shell may not be opened, or "". A
// read-only session refuses shells unless policy allows them (SPEC §11.1).
func (r *Root) shellRefusal() string {
	if !r.readOnly() {
		return ""
	}
	if r.policy().ShellInReadOnly() {
		return ""
	}
	return "read-only mode: shells are disabled (policy.shell_in_readonly)"
}

// recordShellOpened appends the `input` entry that a shell is a deliberate
// escape hatch worth recording. It lands in the open investigation, or in the
// audit file when none is open (SPEC §10, §11.1).
func (r *Root) recordShellOpened(host resource.HostID) tea.Cmd {
	if r.env == nil {
		return nil
	}
	return r.env.AppendInvestigation(investigation.EntryInput, "shell opened on "+string(host), "")
}

// markProtected flags a terminal pane whose host policy protects, which its
// border title then shows.
func (r *Root) markProtected(pane terminalPane) {
	target := action.Target{Host: pane.Host(), Labels: r.hostLabels(pane.Host())}
	if r.policy().Protected(target) {
		pane.SetProtected(true)
	}
}
