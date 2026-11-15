package server

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"jumpr/internal/errs"
)

// fileLock is the per-user server lock: an exclusive, non-blocking flock on
// ~/.cache/jumpr/server.lock. Holding it proves that no other server owns the
// socket, which is why it is taken before a stale socket file is removed.
type fileLock struct {
	f *os.File
}

// acquireLock takes the exclusive non-blocking lock at path. A lock already
// held by another process is an errs.Config error: exactly one server per user.
func acquireLock(path string) (*fileLock, error) {
	if path == "" {
		return nil, errs.New(errs.Config, "server.lock", "empty lock path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, errs.Wrap(errs.Config, "server.lock", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errs.Wrap(errs.Config, "server.lock", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errs.New(errs.Config, "server.lock", "server already running")
		}
		return nil, errs.Wrap(errs.Config, "server.lock", err)
	}
	return &fileLock{f: f}, nil
}

// release unlocks and closes the lock file. Safe on a nil receiver so Serve can
// defer it unconditionally.
func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
