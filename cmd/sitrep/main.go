// Command sitrep is the client binary: a terminal UI for operating fleets of
// servers from the laptop (see docs/SPEC.md). This entry point only handles
// version reporting for now; task 016 replaces it with the real app.
package main

import (
	"fmt"
	"os"
)

// version is set at build time with
//
//	go build -ldflags "-X main.version=1.2.3"
//
// and falls back to "dev" for local builds.
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Printf("sitrep %s\n", version)
		return
	}
	fmt.Fprintln(os.Stderr, "sitrep: not implemented yet")
	os.Exit(2)
}
