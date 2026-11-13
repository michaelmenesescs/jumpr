package app

import (
	"log/slog"
	"os"
	"path/filepath"
)

// newLogger returns the application logger: JSON to <cache>/jumper.log at
// Debug level when debug is on, a discard handler otherwise. A log file that
// cannot be opened degrades to the discard handler; logging never stops the
// program.
func newLogger(cacheDir string, debug bool) *slog.Logger {
	if !debug {
		return slog.New(slog.DiscardHandler)
	}
	if cacheDir == "" {
		return slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return slog.New(slog.DiscardHandler)
	}
	f, err := os.OpenFile(filepath.Join(cacheDir, "jumper.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
