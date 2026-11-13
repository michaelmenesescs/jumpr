package app

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/server"
)

// TestSpawnHelperProcess is the process EnsureServer starts in these tests: the
// test binary itself, told by the environment what to do. It is a no-op in a
// normal run (the child is started with -test.run=TestSpawnHelperProcess).
func TestSpawnHelperProcess(t *testing.T) {
	switch os.Getenv("JUMPER_TEST_HELPER") {
	case "sleep":
		time.Sleep(5 * time.Second)
	case "fail":
		fmt.Fprintln(os.Stdout, "boom: the server refused to start")
	}
}

// TestSpawnEnsureServerSeesRunningServer checks the first half of EnsureServer:
// a socket that answers is used and nothing is started. The executable does not
// exist, so a start attempt would fail loudly.
func TestSpawnEnsureServerSeesRunningServer(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := EnsureServer(ctx, sock, filepath.Join(dir, "no-such-server"), []string{"server"}, nil,
		filepath.Join(dir, "server.log")); err != nil {
		t.Fatalf("EnsureServer with a live socket: %v", err)
	}
}

// TestSpawnMissingExecutable checks that a server that cannot be started at all
// is an unreachable error, not a five second wait.
func TestSpawnMissingExecutable(t *testing.T) {
	dir := shortDir(t)
	err := EnsureServer(context.Background(), filepath.Join(dir, "absent.sock"),
		filepath.Join(dir, "no-such-server"), []string{"server"}, nil,
		filepath.Join(dir, "server.log"))
	if err == nil {
		t.Fatal("a missing executable was accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Unreachable {
		t.Fatalf("kind = %q, want %q: %v", kind, errs.Unreachable, err)
	}
}

// TestSpawnCanceled checks that a canceled context stops the wait at once.
func TestSpawnCanceled(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := EnsureServer(ctx, sock, os.Args[0], []string{"-test.run=TestSpawnHelperProcess"},
		helperEnv("sleep"), filepath.Join(dir, "server.log"))
	if err == nil {
		t.Fatal("a canceled EnsureServer returned nil")
	}
	if kind := errs.KindOf(err); kind != errs.Canceled {
		t.Fatalf("kind = %q, want %q: %v", kind, errs.Canceled, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the wait took %s", elapsed)
	}
}

// TestSpawnExitedBeforeListening checks the early-exit path: the error carries
// the tail of the server's log, which is where the reason is.
func TestSpawnExitedBeforeListening(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	logPath := filepath.Join(dir, "server.log")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := EnsureServer(ctx, sock, os.Args[0], []string{"-test.run=TestSpawnHelperProcess"},
		helperEnv("fail"), logPath)
	if err == nil {
		t.Fatal("a server that exited was accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Unreachable {
		t.Fatalf("kind = %q, want %q: %v", kind, errs.Unreachable, err)
	}
	for _, want := range []string{"exited", "boom: the server refused to start", logPath} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestSpawnLogTail checks the tail an error carries: the last lines of the log,
// or nothing at all when there is no log.
func TestSpawnLogTail(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "server.log")
	var b strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := tailLines(path, 20)
	lines := strings.Split(tail, "\n")
	if len(lines) != 21 { // the "server log (...)" header plus twenty lines
		t.Fatalf("tail has %d lines: %q", len(lines), tail)
	}
	if !strings.Contains(tail, "line 25") || strings.Contains(tail, "line 5\n") {
		t.Fatalf("tail = %q", tail)
	}
	if tailLines(filepath.Join(dir, "absent.log"), 20) != "" {
		t.Fatal("a missing log produced a tail")
	}
	if tailLines(path, 0) != "" {
		t.Fatal("zero lines produced a tail")
	}
}

// TestSpawnServerState checks the record a server leaves behind, and the warning
// a client gets when its --config-dir differs from it.
func TestSpawnServerState(t *testing.T) {
	dir := shortDir(t)
	path := StatePath(dir)
	if got, want := path, filepath.Join(dir, "server.json"); got != want {
		t.Fatalf("state path = %q, want %q", got, want)
	}
	if got, want := LogPath(dir), filepath.Join(dir, "server.log"); got != want {
		t.Fatalf("log path = %q, want %q", got, want)
	}

	// Nothing has been recorded yet: no warning, no error to show the user.
	if w := ConfigWarning(path, "/tmp/cfg"); w != "" {
		t.Fatalf("warning without a state file: %q", w)
	}

	st := ServerState{ConfigDir: "/home/tester/.config/jumper", Version: "1.2.3", PID: 42}
	if err := WriteServerState(path, st); err != nil {
		t.Fatalf("WriteServerState: %v", err)
	}
	got, err := ReadServerState(path)
	if err != nil {
		t.Fatalf("ReadServerState: %v", err)
	}
	if got != st {
		t.Fatalf("state = %+v, want %+v", got, st)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %v", perm)
	}

	if w := ConfigWarning(path, "/home/tester/.config/jumper"); w != "" {
		t.Fatalf("warning for the same directory: %q", w)
	}
	w := ConfigWarning(path, "/home/tester/other")
	if !strings.Contains(w, "warning") || !strings.Contains(w, st.ConfigDir) {
		t.Fatalf("warning = %q", w)
	}
}

// TestSpawnServerEnv checks the environment of the started server: the client's,
// with the marker set exactly once.
func TestSpawnServerEnv(t *testing.T) {
	env := serverEnv([]string{"HOME=/tmp/h", "JUMPER_SERVER=0", "TERM=dumb"})
	var markers, homes int
	for _, kv := range env {
		switch {
		case kv == "JUMPER_SERVER=1":
			markers++
		case strings.HasPrefix(kv, "JUMPER_SERVER="):
			t.Fatalf("a second marker survived: %q", kv)
		case kv == "HOME=/tmp/h":
			homes++
		}
	}
	if markers != 1 || homes != 1 {
		t.Fatalf("env = %q", env)
	}
}

// TestSpawnEnsureServerStartsAndConnects is the whole on-demand start: it builds
// the binary, lets EnsureServer spawn `jumper server` on a temp socket in --demo,
// asks it for `ls`, creates a detached session, kills the server and checks that
// the process is gone.
//
// It runs `go build`, so it is skipped with -short (task 081 pitfall).
func TestSpawnEnsureServerStartsAndConnects(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts the real server")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDir(t)
	exe := filepath.Join(dir, "jumper")
	var build bytes.Buffer
	cmd := exec.Command("go", "build", "-o", exe, "./cmd/jumper")
	cmd.Dir = root
	cmd.Stdout = &build
	cmd.Stderr = &build
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v\n%s", err, build.String())
	}

	home := filepath.Join(dir, "home")
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(home, ".cache", "jumper")
	sock := filepath.Join(dir, "j.sock")
	logPath := LogPath(cache)

	// A private HOME keeps the server's cache, lock and log out of the real one.
	env := replaceEnv(os.Environ(), "HOME", home)
	env = replaceEnv(env, "JUMPER_SOCKET", "")
	env = replaceEnv(env, "SITREP", "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	args := []string{"server", "--config-dir", cfg, "--demo", "-S", sock}
	if err := EnsureServer(ctx, sock, exe, args, env, logPath); err != nil {
		t.Fatalf("EnsureServer: %v\n%s", err, readFile(logPath))
	}

	// ls answers: the server is up and speaking the protocol.
	res, err := server.Control(ctx, sock, server.Hello{Command: "ls"})
	if err != nil {
		t.Fatalf("ls: %v\n%s", err, readFile(logPath))
	}
	if !res.OK {
		t.Fatalf("ls refused: %q", res.Error)
	}
	if len(res.Sessions) != 0 {
		t.Fatalf("a fresh server reports sessions: %+v", res.Sessions)
	}

	// The server recorded itself, which is what the client's warning reads.
	st, err := ReadServerState(StatePath(cache))
	if err != nil {
		t.Fatalf("state: %v\n%s", err, readFile(logPath))
	}
	if st.ConfigDir != cfg {
		t.Fatalf("state config dir = %q, want %q", st.ConfigDir, cfg)
	}
	if st.PID <= 0 {
		t.Fatalf("state pid = %d", st.PID)
	}

	// `new -d -s test` creates a session without attaching to it.
	res, err = server.Control(ctx, sock, server.Hello{Command: "new", Session: "test", Detached: true})
	if err != nil {
		t.Fatalf("new -d: %v\n%s", err, readFile(logPath))
	}
	if !res.OK {
		t.Fatalf("new -d refused: %q", res.Error)
	}
	res, err = server.Control(ctx, sock, server.Hello{Command: "ls"})
	if err != nil {
		t.Fatalf("ls after new: %v", err)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].Name != "test" {
		t.Fatalf("sessions = %+v", res.Sessions)
	}

	// kill-server: the answer comes first, then the process and its socket go.
	res, err = server.Control(ctx, sock, server.Hello{Command: "kill-server"})
	if err != nil {
		t.Fatalf("kill-server: %v\n%s", err, readFile(logPath))
	}
	if !res.OK {
		t.Fatalf("kill-server refused: %q", res.Error)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := dialServer(sock); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the socket still answers after kill-server\n%s", readFile(logPath))
		}
		time.Sleep(20 * time.Millisecond)
	}
	// ... and the process exited, not just its listener.
	for {
		if perr := syscall.Kill(st.PID, 0); perr == syscall.ESRCH {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is still alive after kill-server\n%s", st.PID, readFile(logPath))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// shortDir is a temporary directory with a path short enough for a unix socket
// (macOS `sun_path` is 104 bytes) and cleaned up with the test.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "js")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// helperEnv is the environment of the helper process: ours plus the marker that
// decides what it does.
func helperEnv(mode string) []string {
	env := replaceEnv(os.Environ(), "JUMPER_TEST_HELPER", mode)
	// -test.* flags a normal `go test` sets would make the child behave like
	// the parent; only the helper test is asked for explicitly.
	return replaceEnv(env, "JUMPER_SERVER", "")
}

// replaceEnv sets key=value in env, dropping every earlier value of key.
func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	if value == "" {
		return out
	}
	return append(out, prefix+value)
}

// readFile is the log of a failed start, for the failure message.
func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	return string(data)
}
