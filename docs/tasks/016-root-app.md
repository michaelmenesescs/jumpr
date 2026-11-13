# Task 016 — Root model, `app.Run`, `jumper --demo`

- **Milestone:** M0
- **Depends on:** 003, 007, 012, 013, 014, 015
- **Parallel group:** — (serial; M0 integration)
- **Status:** done

## Goal
A running program: `jumper --demo` shows the fake world in one full-screen view stack with status line, command
bar, hints, flash and help. This is the M0 exit.

## Context
BUILD-PLAN §2 (M0 demo), §5, §6.1 (detach-safety rules — they start here), §7 (panic recovery), §8.

## Files
- create `internal/tui/root.go` (the `tea.Model`), `internal/tui/root_test.go`
- create `internal/app/app.go` (`Options`, `Run`), `internal/app/log.go`, `internal/app/crash.go`
- modify `cmd/jumper/main.go`

## Requirements
1. `tui.NewRoot(env *Env, cmds *Commands, start resource.Query) *Root` implementing `tea.Model`.
   - Layout (top to bottom): status line (1), body (`h-3`), flash-or-command line (1), hints (1). In M0 the body
     is one `Stack`; a crumb line (`hosts (24) › web-ewr-007 › units`, last crumb bold) is the first body line.
   - Key routing order: open dialog → command bar if active → if top view `Capturing()` → view; else global keys:
     `:` opens the command bar, `?` opens help (sections: Global, then the view's `Hints()`), `ctrl+c` quits,
     `q` quits only when the stack has one view (otherwise it is passed to the view); everything else → stack.
   - Messages: `tea.WindowSizeMsg` stores size; `RunCommandMsg` → `cmds.Exec`; `OpenQueryMsg` → look up backend
     and kind, use `env.Factories[kind]` or `views.NewTable`, push; unknown kind → flash; `FlashMsg` shows for 5 s
     (`tea.Tick`, ignore expired ticks by sequence number); `HealthMsg` updates dots; `WindowChangedMsg` updates
     `env.Session.Window` and flashes; `HelpRequestedMsg`, `KindsRequestedMsg` (from 014); `OpenDialogMsg` /
     `CloseDialogMsg`.
   - `View()` returns exactly `width × height` (use `render.Fit` on the final string). Before the first
     `WindowSizeMsg` it returns `""`.
   - To avoid an import cycle (`views` imports `tui`), the root must not import `views`: `NewRoot` takes
     `defaultFactory Factory` as a parameter and `app` passes a closure that calls `views.NewTable`.
2. `app.Options { In io.Reader; Out io.Writer; Demo bool; ConfigDir, Home string; Debug bool; Version string;
   Env map[string]string }` and `func Run(ctx context.Context, o Options) error`:
   - loads config (003), sets the theme profile, builds the registry (fake backend when `Demo`; when not demo and
     nothing is configured, return `errs.Config` "no backends configured; try --demo"), builds `Env`, `Commands`,
     `Root`, then `tea.NewProgram(root, tea.WithInput(o.In), tea.WithOutput(o.Out), tea.WithAltScreen(),
     tea.WithContext(ctx))` and runs it.
   - `log.go`: `slog` JSON handler to `<cache>/jumper.log` at Debug level when `o.Debug`, else a discard handler.
   - `crash.go`: `Run` defers a recover that writes `<cache>/crash/<UTC timestamp>.txt` (panic value + stack) and
     returns `errs.Internal`. Bubble Tea restores the terminal itself on panic inside the program.
3. `main.go`: parse flags with `flag` (`--demo`, `--config-dir`, `--debug`, `--version`); subcommand `version`;
   call `app.Run` with `os.Stdin`, `os.Stdout`, `os.UserHomeDir()`; on error print `jumper: <error>` to stderr and
   exit 1. `main.go` is the only file that touches `os.Stdin/Stdout/Exit`.
4. `root_test.go` uses `teatest.NewTestModel(t, root, teatest.WithInitialTermSize(120, 36))` with the fake
   backend and `theme.SetProfile("none")`.

## Out of scope
Panes, windows, prefix key (M1). Real backends. Mouse.

## Acceptance
```
go test -race ./internal/tui/ -run Root -v
# TestRootDemoShowsHosts: final output contains "web-ewr-007" and "hosts (24)" when started with
#   Query{Kind:"host", Hosts: the 24 ewr hosts} … or start with selector dc=ewr and assert "(504)"
# TestRootCommandNavigates: type ":alerts", enter → output contains "HighErrorRate"
# TestRootFilter: "/", "007", enter → contains web-ewr-007, not web-ewr-008
# TestRootEnterEscStack: enter then esc returns to the list; crumbs shrink
# TestRootHelp: "?" shows "Global"; esc closes
# TestRootUnknownCommandFlashes
# TestRootQuit: ":q", enter → program finishes (tm.WaitFinished with a 2s timeout)
make check
./jumper --demo         # manual: BUILD-PLAN §2 M0 demo script works
```

## Pitfalls
- `teatest`: use `tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})` or `tm.Type(":alerts")`; use
  `teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte("HighErrorRate")) },
  teatest.WithDuration(3*time.Second))`. Do not compare whole frames.
- Do not use `tea.WithMouseCellMotion` yet. Do not call `tea.ExecProcess` ever (rule §6.1).
- `tea.WithInput(o.In)` with a non-TTY reader is intended; do not "fix" it by opening `/dev/tty`.
- The demo start query: `host` with no selector lists 1,512 hosts — fine, the table handles it.
