package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"jumper/internal/errs"
)

// crashTimeLayout names crash files: <cache>/crash/<UTC timestamp>.txt.
const crashTimeLayout = "2006-01-02T15-04-05Z"

// recoverCrash is the deferred top-level recover: it writes the panic value and
// goroutine stack to <cache>/crash/<timestamp>.txt and stores an errs.Internal
// error in *err. Bubble Tea restores the terminal on its own when a panic
// escapes Update or View.
func recoverCrash(cacheDir string, logger *slog.Logger, err *error) {
	v := recover()
	if v == nil {
		return
	}
	path, werr := writeCrashFile(cacheDir, v)
	if logger != nil {
		logger.Error("panic recovered", "value", fmt.Sprint(v), "file", path, "error", werr)
	}
	msg := "panic: " + fmt.Sprint(v)
	if path != "" {
		msg += " (crash log: " + path + ")"
	}
	if err != nil {
		*err = errs.New(errs.Internal, "app.Run", msg)
	}
}

// writeCrashFile writes the panic and stack to the crash directory and returns
// the file path. When the directory cannot be written the path is empty and
// the error says why.
func writeCrashFile(cacheDir string, v any) (string, error) {
	if cacheDir == "" {
		return "", fmt.Errorf("no cache directory")
	}
	dir := filepath.Join(cacheDir, "crash")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := time.Now().UTC().Format(crashTimeLayout) + ".txt"
	path := filepath.Join(dir, name)
	body := fmt.Sprintf("panic: %v\n\nwhen: %s\n\n%s",
		v, time.Now().UTC().Format(time.RFC3339Nano), debug.Stack())
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
