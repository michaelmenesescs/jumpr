// Command jumper is the client binary: a terminal UI for operating fleets of
// servers from the laptop (see docs/SPEC.md). This file is the only one that
// touches os.Stdin, os.Stdout, os.Stderr and os.Exit (BUILD-PLAN §6.1); every
// other package gets readers, writers and values passed in.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"jumper/internal/app"
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
	if rest := fs.Args(); len(rest) > 0 {
		switch rest[0] {
		case "version":
			fmt.Printf("jumper %s\n", version)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "jumper: unknown subcommand %q\n", rest[0])
			return 2
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = app.Run(ctx, app.Options{
		In:        os.Stdin,
		Out:       os.Stdout,
		Demo:      *demo,
		ConfigDir: *configDir,
		Home:      home,
		Debug:     *debug,
		Version:   version,
		Env:       nil,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "jumper: %v\n", err)
		return 1
	}
	return 0
}
