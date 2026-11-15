package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/linux"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

// unitOp is the Op of every error built here.
const unitOp = "tui.unit"

// RegisterUnitCommands registers `:unitdiff` and `:unit` on c. open builds the
// grouped fan-out view `:unitdiff` pushes; it is injected because package tui
// must not import package views (views.NewFanout is the production one). A nil
// open still registers the commands but refuses to start a diff.
func RegisterUnitCommands(c *Commands, open FanoutOpener) error {
	if c == nil {
		return nil
	}
	for _, cmd := range unitCommands(open) {
		if err := c.Register(cmd); err != nil {
			return err
		}
	}
	return nil
}

// unitCommands is the command table: `:unitdiff <unit>` (the unit files across
// the targets, grouped) and `:unit <unit>` (the same unit on every target host,
// as one table).
func unitCommands(open FanoutOpener) []Command {
	return []Command{
		{
			Name:  "unitdiff",
			Usage: ":unitdiff <unit>",
			Help:  "diff one unit's files across the targets",
			Run: func(env *Env, args string) tea.Cmd {
				return runUnitDiff(env, args, open)
			},
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:  "unit",
			Usage: ":unit <unit>",
			Help:  "the same unit on every target host",
			Run: func(env *Env, args string) tea.Cmd {
				return runUnitFleet(env, args)
			},
			Complete: func(*Env, string) []string { return nil },
		},
	}
}

// unitArg takes the single unit name of a command line. Anything but exactly
// one word, or a name that could not survive as one shell argument, is an
// error: the name reaches a remote command line, so it is never guessed at.
func unitArg(usage, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", errs.New(errs.Config, unitOp, usage)
	}
	if len(fields) > 1 {
		return "", errs.New(errs.Config, unitOp, "one unit only: "+usage)
	}
	unit := fields[0]
	if !shq.ValidUnitName(unit) {
		return "", errs.New(errs.Config, unitOp, "bad unit name "+strconv.Quote(unit))
	}
	return unit, nil
}

// runUnitDiff pushes the fan-out of `systemctl cat <unit> --no-pager` over the
// session targets. The work itself starts in the fan-out view's Init, so
// nothing here blocks.
func runUnitDiff(env *Env, args string, open FanoutOpener) tea.Cmd {
	unit, err := unitArg("usage: :unitdiff <unit>", args)
	if err != nil {
		return Flash(FlashError, "%v", err)
	}
	hosts := sessionHosts(env)
	if len(hosts) == 0 {
		return Flash(FlashError, "no targets; try :hosts <selector> first")
	}
	if open == nil {
		return Flash(FlashError, "no fan-out view for units")
	}
	p, err := linux.UnitFileProbe(unit)
	if err != nil {
		return Flash(FlashError, "%v", err)
	}
	cmd := transport.Command{Script: p.Cmd}
	return func() tea.Msg {
		return PushViewMsg{View: open(env, "unit-file "+unit, cmd, hosts, p.Normalize)}
	}
}

// runUnitFleet opens the unit-fleet table: one row per target host, so
// "restart nginx on these 12" starts here (`x` acts on the marked rows).
func runUnitFleet(env *Env, args string) tea.Cmd {
	unit, err := unitArg("usage: :unit <unit>", args)
	if err != nil {
		return Flash(FlashError, "%v", err)
	}
	hosts := sessionHosts(env)
	if len(hosts) == 0 {
		return Flash(FlashError, "no targets; try :hosts <selector> first")
	}
	return OpenQuery(resource.Query{
		Kind:  linux.KindUnitFleet,
		Hosts: hosts,
		Args:  map[string]string{"unit": unit},
	})
}
