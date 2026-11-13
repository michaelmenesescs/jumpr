// Command jumper is the client binary: a terminal UI for operating fleets of
// servers from the laptop (see docs/SPEC.md). This file is the only one that
// touches os.Stdin, os.Stdout, os.Stderr and os.Exit (BUILD-PLAN §6.1); every
// other package gets readers, writers and values passed in.
//
// It is subcommand dispatch only (BUILD-PLAN §3): parsing lives in
// internal/app/cli.go, the server in internal/app/serve.go, the on-demand start
// in internal/app/spawn.go.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/term"

	"jumper/internal/app"
	"jumper/internal/errs"
	"jumper/internal/interact"
	"jumper/internal/server"
)

// version is set at build time with
//
//	go build -ldflags "-X main.version=1.2.3"
//
// and falls back to "dev" for local builds.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses the command line and runs the command. It returns the process exit
// code: 0 on success, 1 for a failed attach or control command, 2 for a bad
// command line or a command that needs a terminal and has none.
func run(args []string) int {
	inv, err := app.ParseCLI(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %s\n", errorText(err))
		if errs.KindOf(err) == errs.Config {
			fmt.Fprint(os.Stderr, app.Usage())
		}
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch inv.Command {
	case app.CommandHelp:
		fmt.Print(app.Usage())
		return 0
	case app.CommandVersion:
		fmt.Printf("jumper %s\n", version)
		return 0
	case app.CommandInit:
		dir, code := initArgs(inv.Args, inv.ConfigDir)
		if code >= 0 {
			return code
		}
		return app.Init(app.Options{
			Out:       os.Stdout,
			ConfigDir: dir,
			Home:      home,
		}, os.Stdout)
	case app.CommandDoctor:
		// Both doctor subcommands live here: `ssh <host>` probes a real host,
		// `conn <profile> <url>` probes an HTTP connection profile, and no
		// argument runs the full report.
		if len(inv.Args) > 0 && inv.Args[0] == "conn" {
			return runDoctor(ctx, inv.Args, home, inv.ConfigDir)
		}
		pos, dir, code := doctorArgs(inv.Args, inv.ConfigDir)
		if code >= 0 {
			return code
		}
		return app.Doctor(ctx, app.Options{
			In:        os.Stdin,
			Out:       os.Stdout,
			ConfigDir: dir,
			Home:      home,
			Debug:     inv.Debug,
			Version:   version,
			Env:       environ(),
			Prompter:  newTermPrompter(os.Stdin, os.Stderr),
		}, pos, os.Stdout)
	case app.CommandServer:
		return runServer(ctx, inv, home)
	case app.CommandLs, app.CommandKillSession, app.CommandKillServer:
		// Control commands read the server; they never start one, because
		// "nothing is running" is a real answer.
		return runControl(ctx, inv, home, false)
	case app.CommandNew:
		if inv.Detached {
			// `new -d` needs a server but never a terminal.
			return runControl(ctx, inv, home, true)
		}
		return runAttach(ctx, inv, home)
	case app.CommandAttach:
		return runAttach(ctx, inv, home)
	}
	fmt.Fprintf(os.Stderr, "jumper: unknown command %q\n", inv.Command)
	return 2
}

// errorText is the user-facing text of an error: an *errs.Error's message
// without the operation prefix, since the CLI's lines are read by people.
func errorText(err error) string {
	var ee *errs.Error
	if errors.As(err, &ee) && ee.Msg != "" {
		return ee.Msg
	}
	return err.Error()
}

// runServer runs the session server in this process (BUILD-PLAN §6).
func runServer(ctx context.Context, inv app.Invocation, home string) int {
	paths := app.Paths(inv, home)
	socket := app.SocketPath(inv, os.Getenv("JUMPER_SOCKET"), home)
	if err := app.CheckSocketPath(socket); err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	err := app.RunServer(ctx, app.ServerOptions{
		Options: app.Options{
			Demo:      inv.Demo,
			ConfigDir: paths.ConfigDir,
			Home:      home,
			Debug:     inv.Debug,
			ReadOnly:  inv.ReadOnly,
			Fresh:     inv.Fresh,
			Version:   version,
			Env:       environ(),
		},
		SocketPath: socket,
		Foreground: inv.Foreground,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	return 0
}

// runAttach attaches the terminal to a session, standalone or through the
// session server.
func runAttach(ctx context.Context, inv app.Invocation, home string) int {
	// A jumper inside a jumper terminal pane would fight the outer one for the
	// keyboard (SPEC §21.4); local panes export $SITREP.
	if os.Getenv("SITREP") != "" {
		fmt.Fprintln(os.Stderr, "jumper: already inside jumper")
		return 1
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, "jumper: not a terminal")
		return 2
	}

	env := clientEnv()
	if inv.Standalone {
		return runStandalone(ctx, inv, home, env)
	}

	paths := app.Paths(inv, home)
	socket := app.SocketPath(inv, os.Getenv("JUMPER_SOCKET"), home)
	if err := app.CheckSocketPath(socket); err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	if err := app.EnsureServer(ctx, socket, exe, serverArgs(inv, socket), os.Environ(),
		app.LogPath(paths.CacheDir)); err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	if msg := app.ConfigWarning(app.StatePath(paths.CacheDir), paths.ConfigDir); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}

	resizes, stopResizes := signalEvents(syscall.SIGWINCH)
	defer stopResizes()
	hangups, stopHangups := signalEvents(syscall.SIGHUP, syscall.SIGTERM)
	defer stopHangups()

	cols, rows := sizeOf()
	exit, err := server.Attach(ctx, server.ClientOptions{
		SocketPath: socket,
		Hello:      inv.Hello(version, cols, rows, env),
		In:         os.Stdin,
		Out:        os.Stdout,
		MakeRaw:    makeRaw,
		Size:       sizeOf,
		Resizes:    resizes,
		Hangup:     hangups,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	if msg := exit.UserMessage(); msg != "" {
		fmt.Fprintln(os.Stdout, msg)
	}
	if exit.Reason == "error" {
		return 1
	}
	return 0
}

// runStandalone runs one process with one session, wired to the real terminal:
// the debugging mode, and the only mode there was before M5 (BUILD-PLAN §6).
func runStandalone(ctx context.Context, inv app.Invocation, home string, env map[string]string) int {
	paths := app.Paths(inv, home)
	size, resizes, stopSizes := watchSize(os.Stdout)
	defer stopSizes()

	err := app.Run(ctx, app.Options{
		In:        os.Stdin,
		Out:       os.Stdout,
		Demo:      inv.Demo,
		ConfigDir: paths.ConfigDir,
		Home:      home,
		Debug:     inv.Debug,
		Fresh:     inv.Fresh,
		ReadOnly:  inv.ReadOnly,
		Version:   version,
		Env:       env,
		Prompter:  newTermPrompter(os.Stdin, os.Stderr),
		Size:      size,
		Resizes:   resizes,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	return 0
}

// runControl runs a control command over the socket (ls, kill-session,
// kill-server, new -d). ensure says whether the server may be started first.
func runControl(ctx context.Context, inv app.Invocation, home string, ensure bool) int {
	paths := app.Paths(inv, home)
	socket := app.SocketPath(inv, os.Getenv("JUMPER_SOCKET"), home)
	if err := app.CheckSocketPath(socket); err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	if ensure {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
			return 1
		}
		if err := app.EnsureServer(ctx, socket, exe, serverArgs(inv, socket), os.Environ(),
			app.LogPath(paths.CacheDir)); err != nil {
			fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
			return 1
		}
	}

	res, err := server.Control(ctx, socket, inv.Hello(version, 0, 0, nil))
	if err != nil {
		if errs.KindOf(err) == errs.Unreachable {
			fmt.Fprintln(os.Stderr, "jumper: no server running")
			return 1
		}
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	if !res.OK {
		msg := res.Error
		if msg == "" {
			msg = "server refused the command"
		}
		fmt.Fprintf(os.Stderr, "jumper: %s\n", msg)
		return 1
	}
	if inv.Command == app.CommandLs {
		for _, s := range res.Sessions {
			fmt.Println(app.FormatSessionLine(s))
		}
	}
	return 0
}

// serverArgs is how a client starts the server: the same binary with the
// client's configuration directory, its debug flag and the resolved socket, so
// both sides agree on where the socket is.
func serverArgs(inv app.Invocation, socket string) []string {
	args := []string{"server"}
	if inv.ConfigDir != "" {
		args = append(args, "--config-dir", inv.ConfigDir)
	}
	if inv.Debug {
		args = append(args, "--debug")
	}
	if inv.Demo {
		args = append(args, "--demo")
	}
	args = append(args, "-S", socket)
	return args
}

// clientEnv is the environment an attach carries to the server: TERM,
// COLORTERM, SSH_AUTH_SOCK and TMUX only (BUILD-PLAN §6.2).
func clientEnv() map[string]string {
	env := make(map[string]string, 4)
	for _, key := range []string{"TERM", "COLORTERM", "SSH_AUTH_SOCK", "TMUX"} {
		if v := os.Getenv(key); v != "" {
			env[key] = v
		}
	}
	return env
}

// sizeOf reads the terminal size. It is handed to the client as its Size func,
// so the size sent with every RESIZE is current.
func sizeOf() (cols, rows int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0
	}
	return w, h
}

// makeRaw puts the terminal in raw mode and returns the restore function.
func makeRaw() (func(), error) {
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(int(os.Stdin.Fd()), state) }, nil
}

// signalEvents turns the given signals into one channel of events, coalescing
// repeats: a resize the client has not sent yet is already stale. The returned
// stop function unregisters the signals.
func signalEvents(sigs ...os.Signal) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, sigs...)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sig:
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	stop := func() {
		signal.Stop(sig)
		close(done)
	}
	return ch, stop
}

// watchSize reads the terminal size once and reports every later SIGWINCH as
// one app.Size, dropping values the app has not taken yet. A stdout that is not
// a terminal yields no size, which leaves the app on its default. The returned
// stop function unregisters the signal.
func watchSize(f *os.File) (*app.Size, <-chan app.Size, func()) {
	w, h, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return nil, nil, func() {}
	}
	size := &app.Size{Cols: w, Rows: h}
	ch := make(chan app.Size, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sig:
				w, h, err := term.GetSize(int(f.Fd()))
				if err != nil {
					continue
				}
				select {
				case ch <- app.Size{Cols: w, Rows: h}:
				default:
					// A resize the app has not read yet is already stale.
				}
			}
		}
	}()
	stop := func() {
		signal.Stop(sig)
		close(done)
	}
	return size, ch, stop
}

// configDirFlags are the spellings a subcommand may use for the configuration
// directory. They are accepted before or after the positional arguments
// because the flag package stops at the first non-flag word.
var configDirFlags = []string{"--config-dir", "-config-dir"}

// doctorArgs pulls the flags out of a `jumper doctor …` invocation and returns
// the positional arguments and the effective configuration directory. code is
// -1 when the command should run, else it is the exit code to return: 0 after
// help, 2 for a bad command line.
func doctorArgs(args []string, configDir string) (pos []string, dir string, code int) {
	dir = configDir
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Print("usage: jumper doctor [--config-dir DIR]\n" +
				"       jumper doctor ssh <host> [--config-dir DIR]\n\n" +
				"  doctor          check the configuration, SSH, agent, bastions, samples,\n" +
				"                  backends, connection profiles, knowledge root and terminal\n" +
				"  ssh <host>      resolve, dial, authenticate and run a few read-only probes\n")
			return nil, dir, 0
		case matchesFlag(a, configDirFlags):
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "jumper: %s needs a directory\n", a)
				return nil, dir, 2
			}
			i++
			dir = args[i]
		case strings.HasPrefix(a, "--config-dir=") || strings.HasPrefix(a, "-config-dir="):
			dir = a[strings.IndexByte(a, '=')+1:]
		default:
			pos = append(pos, a)
		}
	}
	return pos, dir, -1
}

// initArgs pulls the flags out of a `jumper init` invocation and returns the
// effective configuration directory. code is -1 when init should run, else the
// exit code to return: 0 after help, 2 for a bad command line.
func initArgs(args []string, configDir string) (dir string, code int) {
	const usage = "usage: jumper init [--config-dir DIR]\n\n" +
		"  writes a commented config.yaml, connections.yaml, policy.yaml and keymap.yaml\n" +
		"  plus an empty plugins/ into the configuration directory; never overwrites\n"
	dir = configDir
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Print(usage)
			return dir, 0
		case matchesFlag(a, configDirFlags):
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "jumper: %s needs a directory\n", a)
				return dir, 2
			}
			i++
			dir = args[i]
		case strings.HasPrefix(a, "--config-dir=") || strings.HasPrefix(a, "-config-dir="):
			dir = a[strings.IndexByte(a, '=')+1:]
		default:
			fmt.Fprintf(os.Stderr, "jumper: init: unexpected argument %q\n", a)
			fmt.Fprint(os.Stderr, usage)
			return dir, 2
		}
	}
	return dir, -1
}

// matchesFlag reports whether a is exactly one of the flags.
func matchesFlag(a string, flags []string) bool {
	for _, f := range flags {
		if a == f {
			return true
		}
	}
	return false
}

// termPrompter answers prompts on the terminal: the question goes to out, one
// line is read from in. It is line-based on purpose — doctor is a plain CLI, not
// the TUI — and it is the only Prompter that touches stdin.
type termPrompter struct {
	in  *bufio.Reader
	out io.Writer
	mu  sync.Mutex
}

// newTermPrompter returns a Prompter over the given streams. It never echoes a
// secret answer.
func newTermPrompter(in io.Reader, out io.Writer) *termPrompter {
	if in == nil {
		in = strings.NewReader("")
	}
	if out == nil {
		out = io.Discard
	}
	return &termPrompter{in: bufio.NewReader(in), out: out}
}

// Ask implements interact.Prompter. The choice (or the secret text) is the line
// the user types; an empty line cancels a choice question, and a closed stdin
// fails with errs.Interaction.
func (p *termPrompter) Ask(ctx context.Context, q interact.Prompt) (interact.Answer, error) {
	if err := ctxErr(ctx); err != nil {
		return interact.Answer{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	fmt.Fprintf(p.out, "%s\n", q.Title)
	if q.Body != "" {
		fmt.Fprintf(p.out, "%s\n", q.Body)
	}
	if len(q.Choices) > 0 {
		fmt.Fprintf(p.out, "[%s] ", strings.Join(q.Choices, "/"))
	} else if q.Secret {
		fmt.Fprint(p.out, "passphrase: ")
	} else {
		fmt.Fprint(p.out, "> ")
	}
	line, err := p.in.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		if err == io.EOF {
			return interact.Answer{}, errs.New(errs.Interaction, "prompt",
				fmt.Sprintf("%s: no answer (stdin closed)", q.Title))
		}
		return interact.Answer{}, errs.Wrap(errs.Interaction, "prompt", err)
	}
	ans := interact.Answer{Choice: strings.TrimSpace(line), Text: line}
	if len(q.Choices) > 0 && ans.Choice == "" {
		ans.Canceled = true
	}
	return ans, nil
}

// ctxErr turns a canceled or expired context into the interaction error the
// Prompter contract asks for.
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errs.Wrap(errs.Interaction, "prompt", err)
	}
	return nil
}

// runDoctor implements `jumper doctor conn <profile> <url> [--verbose]` (SPEC
// §7.2). It returns the process exit code: 0 only when the endpoint answered 2xx
// and did not redirect to a login page.
func runDoctor(ctx context.Context, args []string, home, configDir string) int {
	const usage = "usage: jumper doctor conn <profile> <url> [--verbose]"
	if len(args) == 0 || args[0] != "conn" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("jumper doctor conn", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	verbose := fs.Bool("verbose", false, "also show the response headers, redacted")
	// Flags may come before or after the two positionals; flag.Parse stops at
	// the first non-flag, so consume one positional at a time.
	var positional []string
	rest := args[1:]
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			if err == flag.ErrHelp {
				return 0
			}
			return 2
		}
		left := fs.Args()
		if len(left) == 0 {
			break
		}
		positional = append(positional, left[0])
		rest = left[1:]
	}
	if len(positional) != 2 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	profile, target := positional[0], positional[1]

	res, err := app.DoctorConn(ctx, os.Stdout, profile, target, app.DoctorOptions{
		ConfigDir: configDir,
		Home:      home,
		Env:       environ(),
		Verbose:   *verbose,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	if !res.OK {
		return 1
	}
	return 0
}

// environ copies the process environment into a map for the `env` proxy type.
// cmd/jumper is where the environment is read; internal/app never is.
func environ() map[string]string {
	env := make(map[string]string, len(os.Environ()))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	return env
}
