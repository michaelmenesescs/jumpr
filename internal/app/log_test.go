package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
)

// The debug log of task 126: one redacted line per remote command, and a file
// that does not grow without a limit.

// stubExecutor is a transport.Executor with a scripted answer.
type stubExecutor struct {
	res transport.ExecResult
	err error
}

func (s stubExecutor) Name() string { return "stub" }

func (s stubExecutor) Exec(_ context.Context, host resource.HostID, _ transport.Command) (transport.ExecResult, error) {
	res := s.res
	res.Host = host
	return res, s.err
}

// TestLogRemoteCommandRedacted is requirement 3: a command that carries a
// secret and output that carries one leave a log line with the host, the
// redacted command line, the duration, the exit code and the byte counts — and
// neither secret, and no output text at all.
func TestLogRemoteCommandRedacted(t *testing.T) {
	dir := t.TempDir()
	logger := newLogger(dir, true)
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redactor: %v", err)
	}

	const (
		token  = "Bearer s3cr3t-t0ken-abcdef"
		stdout = "TOPSECRET-OUTPUT-VALUE\n"
		stdin  = "TOPSECRET-STDIN-PASSWORD"
	)
	ex := newLoggingExecutor(stubExecutor{res: transport.ExecResult{
		Stdout:   []byte(stdout),
		Stderr:   []byte("warning: nothing to see"),
		RC:       3,
		Duration: 12 * time.Millisecond, // the line reports what the command said
		Via:      "ssh",
	}}, logger, red)

	cmd := transport.Command{
		Script: "curl -sS -H 'Authorization: " + token + "' https://api.example.internal/v1/status",
		Stdin:  []byte(stdin),
	}
	res, exErr := ex.Exec(context.Background(), "web-01.example.internal", cmd)
	if exErr != nil {
		t.Fatalf("Exec: %v", exErr)
	}
	if res.RC != 3 {
		t.Fatalf("Exec returned rc %d, want the scripted 3", res.RC)
	}

	raw, err := os.ReadFile(filepath.Join(dir, jumprLogName))
	if err != nil {
		t.Fatalf("read %s: %v", jumprLogName, err)
	}
	text := string(raw)
	for _, want := range []string{
		`"msg":"remote command"`,
		"web-01.example.internal",
		`"sudo":false`,
		`"duration":"12ms"`,
		`"rc":3`,
		`"out_bytes":23`,
		`"err_bytes":23`,
		`"truncated":false`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log line is missing %s:\n%s", want, text)
		}
	}
	for _, secret := range []string{"s3cr3t-t0ken-abcdef", "TOPSECRET-OUTPUT-VALUE", stdin, "nothing to see"} {
		if strings.Contains(text, secret) {
			t.Errorf("the log contains %q; it may carry the command, never a secret, the output or stdin:\n%s", secret, text)
		}
	}
	// The command itself is still there, so the line is worth reading.
	if !strings.Contains(text, "curl -sS") {
		t.Errorf("the log line has no command in it:\n%s", text)
	}
}

// TestLogLineIsCapped is the other side of "the command is in the log": a
// command larger than the cap is cut, never written whole into every line.
func TestLogLineIsCapped(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ex := newLoggingExecutor(stubExecutor{}, logger, nil)

	huge := "echo " + strings.Repeat("a", commandLineCap*4)
	if _, err := ex.Exec(context.Background(), "web-01", transport.Command{Script: huge}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	line := buf.String()
	if len(line) > commandLineCap*2 {
		t.Fatalf("one command wrote %d bytes into the log, want it capped", len(line))
	}
	if strings.Contains(line, strings.Repeat("a", commandLineCap+1)) {
		t.Fatal("the command text was not cut")
	}
}

// TestLogRotationKeepsThree is requirement 4 for the client log: jumpr.log
// rotates at 10 MiB and three older files are kept.
func TestLogRotationKeepsThree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, jumprLogName)
	w, err := openRotating(path, logRotateBytes, logRotateKeep, 0o644)
	if err != nil {
		t.Fatalf("openRotating: %v", err)
	}
	defer w.Close()

	// Enough to rotate more than three times, in chunks a log handler would
	// write.
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	chunk[len(chunk)-1] = '\n'
	for written := 0; written < 4*logRotateBytes+(logRotateBytes/2); written += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write after %d bytes: %v", written, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for i := 1; i <= logRotateKeep; i++ {
		info, err := os.Stat(path + "." + strconv.Itoa(i))
		if err != nil {
			t.Fatalf("rotated file %d is missing: %v", i, err)
		}
		if info.Size() > logRotateBytes {
			t.Errorf("rotated file %d is %d bytes, want at most %d", i, info.Size(), int64(logRotateBytes))
		}
	}
	if _, err := os.Stat(path + "." + strconv.Itoa(logRotateKeep+1)); !os.IsNotExist(err) {
		t.Fatalf("a fourth rotated file was kept (err = %v), want three", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the live log is missing: %v", err)
	}
	if info.Size() > logRotateBytes {
		t.Errorf("the live log is %d bytes, want at most %d", info.Size(), int64(logRotateBytes))
	}
}

// TestLogServerLogRotates is requirement 4 for the daemon: server.log is opened
// through the same rotating writer and not as a plain append-only file.
func TestLogServerLogRotates(t *testing.T) {
	dir := t.TempDir()
	logger, closer := newServerLogger(dir)
	if closer == nil {
		t.Fatal("newServerLogger returned no file to close")
	}
	defer closer.Close()
	logger.Info("hello", "where", "server.log")

	raw, err := os.ReadFile(filepath.Join(dir, serverLogName))
	if err != nil {
		t.Fatalf("read %s: %v", serverLogName, err)
	}
	if !strings.Contains(string(raw), "hello") {
		t.Fatalf("the server log did not get the line: %s", raw)
	}

	// The writer behind the handler is the rotating one, at the documented
	// size, so a daemon running for weeks does not fill the disk.
	rf, ok := closer.(*rotatingFile)
	if !ok {
		t.Fatalf("server log closer = %T, want a rotating file", closer)
	}
	if rf.max != logRotateBytes || rf.keep != logRotateKeep {
		t.Fatalf("server log rotates at %d keeping %d, want %d and %d", rf.max, rf.keep, int64(logRotateBytes), logRotateKeep)
	}
}

// TestLogDisabledWithoutDebug is the other half of the contract: without
// --debug nothing is written at all.
func TestLogDisabledWithoutDebug(t *testing.T) {
	dir := t.TempDir()
	logger := newLogger(dir, false)
	logger.Debug("this must not reach a file")
	if _, err := os.Stat(filepath.Join(dir, jumprLogName)); !os.IsNotExist(err) {
		t.Fatalf("a log file was created without --debug (err = %v)", err)
	}
}
