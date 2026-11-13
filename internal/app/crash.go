package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
	body := fmt.Sprintf("panic: %v\n\nwhen: %s\n\n%s",
		v, time.Now().UTC().Format(time.RFC3339Nano), debug.Stack())
	return writeCrashReport(cacheDir, body)
}

// recordProgramCrash writes the crash report for a session whose tea.Program
// ended in a panic (task 126 requirement 2) and returns the error the caller
// reports. Bubble Tea has already restored the terminal and recovered the
// goroutine by the time this runs; what is left is to say it happened, in the
// log and in <cache>/crash/, and to tell an attached client EXIT{error}.
//
// The layout is not written here: the session saves it immediately before
// calling this, because the autosave tick is debounced by a second and the last
// change before a panic would otherwise be lost.
func recordProgramCrash(cacheDir string, logger *slog.Logger, cause error) *errs.Error {
	detail := "program experienced a panic"
	if cause != nil {
		detail = cause.Error()
	}
	body := fmt.Sprintf("panic: %s\n\nwhen: %s\nthe terminal was restored and the session ended\n\n%s",
		detail, time.Now().UTC().Format(time.RFC3339Nano), debug.Stack())
	path, werr := writeCrashReport(cacheDir, body)
	if logger != nil {
		logger.Error("session program panicked", "error", detail, "file", path, "write_error", werr)
	}
	err := errs.New(errs.Internal, "app.session", "panic: "+detail)
	if path != "" {
		err.Msg += " (crash log: " + path + ")"
	}
	return err
}

// writeCrashReport writes body to <cache>/crash/<timestamp>.txt and returns the
// path. The directory is created when it is missing; a cache directory that
// cannot be written is reported, never retried.
func writeCrashReport(cacheDir, body string) (string, error) {
	if cacheDir == "" {
		return "", fmt.Errorf("no cache directory")
	}
	dir := filepath.Join(cacheDir, "crash")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := time.Now().UTC().Format(crashTimeLayout) + ".txt"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// programEnded reports what a finished tea.Program means: a panic is a crash
// report plus an Internal error, a killed program is a normal shutdown, and
// anything else is only worth a debug line. The session handles the panic
// itself (see Session.onProgramPanic); this is the decision, kept pure so it
// can be tested without a terminal.
func programEnded(err error) (panicked bool, report error) {
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, tea.ErrProgramPanic):
		return true, err
	case errors.Is(err, tea.ErrProgramKilled):
		return false, nil
	default:
		return false, err
	}
}
