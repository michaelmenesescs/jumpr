// Package transport defines how jumper runs commands on remote hosts.
package transport

import (
	"context"
	"io"
	"time"

	"jumper/internal/resource"
)

// Command is one remote shell invocation. It runs as: sh -c <Script>,
// or `sudo -n sh -c <Script>` when Sudo is set. Script is passed as a single quoted argument (see internal/shq).
type Command struct {
	Script    string
	Sudo      bool
	Stdin     []byte        // optional
	Timeout   time.Duration // 0 = executor default
	MaxOutput int           // bytes per stream; 0 = executor default (2 MiB)
}

// ExecResult is what came back. RC is the remote exit code (-1 if the command never ran).
// A non-zero RC is NOT an error at this layer.
type ExecResult struct {
	Host      resource.HostID
	Stdout    []byte
	Stderr    []byte
	RC        int
	Truncated bool // output hit MaxOutput
	Duration  time.Duration
	Via       string // "ssh", "ssh via bastion.lab.example.internal", "salt:salt.lab.example.internal"
}

// Executor runs commands on hosts. Implementations: native SSH, Salt, system ssh, fake.
// Exec must respect ctx, must never block past the timeout, and must return a *errs.Error
// (Timeout, Unreachable, Auth, HostKey, Sudo, Canceled) for transport-level failures.
// Safe for concurrent use.
type Executor interface {
	Name() string
	Exec(ctx context.Context, host resource.HostID, cmd Command) (ExecResult, error)
}

// Streamer starts a long-running command and delivers stdout line by line (journal follow, salt events).
type Streamer interface {
	Start(ctx context.Context, host resource.HostID, cmd Command) (Stream, error)
}

type Stream interface {
	// Lines is closed when the command ends or the stream is closed. Lines exclude the trailing newline.
	Lines() <-chan []byte
	// Wait blocks until the command ends and returns its result (Stdout empty; Stderr is the capped tail).
	Wait() (ExecResult, error)
	Close() error
}

type PTYRequest struct {
	Term    string // e.g. "xterm-256color"
	Cols    int
	Rows    int
	Command string // empty = login shell
}

// HostResult is one host's outcome in a multi-host run.
type HostResult struct {
	Host   resource.HostID
	Result ExecResult
	Err    error // *errs.Error for transport failures
}

// BatchExecutor is optional: executors that can run one command on many hosts as a single operation
// (Salt). fanout.Run uses it when present. Every host yields exactly one HostResult; the channel is closed.
type BatchExecutor interface {
	ExecBatch(ctx context.Context, hosts []resource.HostID, cmd Command) <-chan HostResult
}

// ShellOpener opens an interactive PTY session on a host.
type ShellOpener interface {
	OpenShell(ctx context.Context, host resource.HostID, req PTYRequest) (Shell, error)
}

// Shell is the byte stream of a remote PTY. Read returns terminal output; Write sends keystrokes.
type Shell interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Wait() error // returns when the remote side exits
}
