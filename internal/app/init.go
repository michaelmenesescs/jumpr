package app

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"jumpr/internal/config"
	"jumpr/internal/errs"
)

// initTemplates holds the commented example configuration `jumpr init`
// writes. The files are the source of truth for a first run: the config
// template lists every key of BUILD-PLAN §11 at its default value, and
// init_test.go compares it against config.Default().
//
//go:embed templates/*.yaml
var initTemplates embed.FS

// initFile is one file init writes: the name in the config directory and the
// mode it is created with.
type initFile struct {
	Name string
	Mode os.FileMode
}

// initFiles are the files written, in the order the report lists them. The
// policy and keymap files can carry more than the config, so they are private
// to the user.
var initFiles = []initFile{
	{Name: "config.yaml", Mode: 0o600},
	{Name: "connections.yaml", Mode: 0o600},
	{Name: "policy.yaml", Mode: 0o600},
	{Name: "keymap.yaml", Mode: 0o600},
}

// initPluginsDir is the plugin directory init creates empty; plugins live in
// its own files and are hot-reloaded (SPEC §12).
const initPluginsDir = "plugins"

// Init writes the commented example configuration into the config directory
// and returns the process exit code. It never overwrites a file: an existing
// file is reported as skipped and left exactly as it is, so `jumpr init` is
// safe to run again after an upgrade.
//
// It writes only to out (never to stdout/stderr) and reads nothing from the
// environment; cmd/jumpr supplies Home and ConfigDir.
func Init(o Options, out io.Writer) int {
	if out == nil {
		out = io.Discard
	}
	paths := config.ResolvePaths(o.Home, o.ConfigDir)
	if paths.ConfigDir == "" {
		fmt.Fprintln(out, "jumpr init: no configuration directory")
		return 1
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		fmt.Fprintf(out, "jumpr init: %v\n", initError(paths.ConfigDir, err))
		return 1
	}

	created := 0
	for _, f := range initFiles {
		data, err := initTemplates.ReadFile("templates/" + f.Name)
		if err != nil {
			fmt.Fprintf(out, "jumpr init: %v\n", initError("templates/"+f.Name, err))
			return 1
		}
		path := filepath.Join(paths.ConfigDir, f.Name)
		switch err := writeIfAbsent(path, data, f.Mode); {
		case err == nil:
			created++
			fmt.Fprintf(out, "created  %s\n", path)
		case errors.Is(err, fs.ErrExist):
			fmt.Fprintf(out, "skipped  %s (already exists)\n", path)
		default:
			fmt.Fprintf(out, "jumpr init: %v\n", initError(path, err))
			return 1
		}
	}

	pluginsPath := filepath.Join(paths.ConfigDir, initPluginsDir)
	if err := os.MkdirAll(pluginsPath, 0o755); err != nil {
		fmt.Fprintf(out, "jumpr init: %v\n", initError(pluginsPath, err))
		return 1
	}
	fmt.Fprintf(out, "created  %s/\n", pluginsPath)

	if created > 0 {
		fmt.Fprintf(out, "\nnext: edit %s, then run `jumpr doctor`\n",
			filepath.Join(paths.ConfigDir, "config.yaml"))
	}
	return 0
}

// writeIfAbsent creates path with data, failing with fs.ErrExist when the file
// is already there. O_EXCL makes the check and the create one operation, so a
// concurrent run cannot overwrite the other one's file.
func writeIfAbsent(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// initError wraps a write failure as the configuration error it is.
func initError(path string, err error) error {
	return errs.Wrap(errs.Config, "app.Init", fmt.Errorf("%s: %w", path, err))
}
