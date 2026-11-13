// Command jumper is the client binary: a terminal UI for operating fleets of
// servers from the laptop (see docs/SPEC.md). This file is the only one that
// touches os.Stdin, os.Stdout, os.Stderr and os.Exit (BUILD-PLAN §6.1); every
// other package gets readers, writers and values passed in.
package main

import (
	"bufio"
	"context"
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

// run parses the command line and runs the app. It returns the process exit
// code.
func run(args []string) int {
	fs := flag.NewFlagSet("jumper", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	demo := fs.Bool("demo", false, "run against the built-in fake world (no SSH)")
	configDir := fs.String("config-dir", "", "configuration directory (default ~/.config/jumper)")
	debug := fs.Bool("debug", false, "write a debug log to ~/.cache/jumper/jumper.log")
	fresh := fs.Bool("fresh", false, "start a fresh session instead of restoring the saved layout")
	readonly := fs.Bool("readonly", false, "start read-only: every write is denied until :readonly off")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Printf("jumper %s\n", version)
		return 0
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if rest := fs.Args(); len(rest) > 0 {
		switch rest[0] {
		case "version":
			fmt.Printf("jumper %s\n", version)
			return 0
		case "init":
			dir, code := initArgs(rest[1:], *configDir)
			if code >= 0 {
				return code
			}
			return app.Init(app.Options{
				Out:       os.Stdout,
				ConfigDir: dir,
				Home:      home,
			}, os.Stdout)
		case "doctor":
			// Both doctor subcommands live here: `ssh <host>` probes a real
			// host, `conn <profile> <url>` probes an HTTP connection profile,
			// and no argument runs the full report.
			if len(rest) > 1 && rest[1] == "conn" {
				return runDoctor(ctx, rest[1:], home, *configDir)
			}
			pos, dir, code := doctorArgs(rest[1:], *configDir)
			if code >= 0 {
				return code
			}
			return app.Doctor(ctx, app.Options{
				In:        os.Stdin,
				Out:       os.Stdout,
				ConfigDir: dir,
				Home:      home,
				Debug:     *debug,
				Version:   version,
				Env:       environ(),
				Prompter:  newTermPrompter(os.Stdin, os.Stderr),
			}, pos, os.Stdout)
		default:
			fmt.Fprintf(os.Stderr, "jumper: unknown subcommand %q\n", rest[0])
			return 2
		}
	}

	// The session's output is a switchable pipe, not the terminal, so Bubble
	// Tea can no longer measure it: the size and every SIGWINCH are read here
	// and handed to the app (task 078, BUILD-PLAN §6).
	size, resizes, stopSizes := watchSize(os.Stdout)
	defer stopSizes()

	err = app.Run(ctx, app.Options{
		In:        os.Stdin,
		Out:       os.Stdout,
		Demo:      *demo,
		ConfigDir: *configDir,
		Home:      home,
		Debug:     *debug,
		Fresh:     *fresh,
		ReadOnly:  *readonly,
		Version:   version,
		Env:       nil,
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
