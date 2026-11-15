package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/server"
)

// TestCLIParseTable covers every command and every flag: the parsing is pure,
// so the whole grammar is a table (task 081 requirement 1).
func TestCLIParseTable(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want Invocation
	}{
		{
			name: "no arguments attaches",
			args: nil,
			want: Invocation{Command: CommandAttach},
		},
		{
			name: "attach",
			args: []string{"attach"},
			want: Invocation{Command: CommandAttach},
		},
		{
			name: "attach with target",
			args: []string{"attach", "-t", "main"},
			want: Invocation{Command: CommandAttach, Target: "main"},
		},
		{
			name: "attach with long target",
			args: []string{"attach", "--target", "main"},
			want: Invocation{Command: CommandAttach, Target: "main"},
		},
		{
			name: "attach with equals target",
			args: []string{"attach", "-t=db-incident"},
			want: Invocation{Command: CommandAttach, Target: "db-incident"},
		},
		{
			name: "new detached",
			args: []string{"new", "-d", "-s", "db-incident"},
			want: Invocation{Command: CommandNew, Detached: true, Session: "db-incident"},
		},
		{
			name: "new with no name",
			args: []string{"new"},
			want: Invocation{Command: CommandNew},
		},
		{
			name: "new with session equals",
			args: []string{"new", "--session=work"},
			want: Invocation{Command: CommandNew, Session: "work"},
		},
		{
			name: "ls",
			args: []string{"ls"},
			want: Invocation{Command: CommandLs},
		},
		{
			name: "kill-session",
			args: []string{"kill-session", "-t", "main"},
			want: Invocation{Command: CommandKillSession, Target: "main"},
		},
		{
			name: "kill-server",
			args: []string{"kill-server"},
			want: Invocation{Command: CommandKillServer},
		},
		{
			name: "server",
			args: []string{"server"},
			want: Invocation{Command: CommandServer},
		},
		{
			name: "server foreground",
			args: []string{"server", "--foreground"},
			want: Invocation{Command: CommandServer, Foreground: true},
		},
		{
			name: "version command",
			args: []string{"version"},
			want: Invocation{Command: CommandVersion},
		},
		{
			name: "version flag",
			args: []string{"--version"},
			want: Invocation{Command: CommandVersion},
		},
		{
			name: "help flag",
			args: []string{"--help"},
			want: Invocation{Command: CommandHelp},
		},
		{
			name: "help short flag",
			args: []string{"-h"},
			want: Invocation{Command: CommandHelp},
		},
		{
			name: "help after the command",
			args: []string{"attach", "--help"},
			want: Invocation{Command: CommandHelp},
		},
		{
			name: "global flags before the command",
			args: []string{"--config-dir", "/tmp/cfg", "--debug", "-S", "/tmp/s.sock",
				"--readonly", "--fresh", "attach", "-t", "x"},
			want: Invocation{Command: CommandAttach, ConfigDir: "/tmp/cfg", Debug: true,
				Socket: "/tmp/s.sock", ReadOnly: true, Fresh: true, Target: "x"},
		},
		{
			name: "global flags after the command",
			args: []string{"ls", "--debug", "--config-dir=/tmp/cfg2"},
			want: Invocation{Command: CommandLs, Debug: true, ConfigDir: "/tmp/cfg2"},
		},
		{
			name: "socket equals",
			args: []string{"-S=/tmp/eq.sock", "ls"},
			want: Invocation{Command: CommandLs, Socket: "/tmp/eq.sock"},
		},
		{
			name: "standalone",
			args: []string{"--standalone"},
			want: Invocation{Command: CommandAttach, Standalone: true},
		},
		{
			name: "demo implies standalone",
			args: []string{"--demo"},
			want: Invocation{Command: CommandAttach, Demo: true, Standalone: true},
		},
		{
			name: "demo on new",
			args: []string{"--demo", "new", "-s", "x"},
			want: Invocation{Command: CommandNew, Demo: true, Standalone: true, Session: "x"},
		},
		{
			name: "doctor with no arguments",
			args: []string{"doctor"},
			want: Invocation{Command: CommandDoctor},
		},
		{
			name: "doctor ssh keeps its own arguments",
			args: []string{"doctor", "ssh", "web.example.internal", "--config-dir", "/tmp/d"},
			want: Invocation{Command: CommandDoctor, Args: []string{"ssh", "web.example.internal", "--config-dir", "/tmp/d"}},
		},
		{
			name: "doctor conn keeps its flags",
			args: []string{"doctor", "conn", "prod", "https://api.example.internal", "--verbose"},
			want: Invocation{Command: CommandDoctor,
				Args: []string{"conn", "prod", "https://api.example.internal", "--verbose"}},
		},
		{
			name: "init keeps its arguments",
			args: []string{"init", "--config-dir", "/tmp/init"},
			want: Invocation{Command: CommandInit, Args: []string{"--config-dir", "/tmp/init"}},
		},
		{
			name: "terminator ends the flags",
			args: []string{"--debug", "--"},
			want: Invocation{Command: CommandAttach, Debug: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCLI(tc.args)
			if err != nil {
				t.Fatalf("ParseCLI(%q) = error %v", tc.args, err)
			}
			checkInvocation(t, got, tc.want)
		})
	}
}

// TestCLIParseErrors checks that everything the grammar does not allow is a
// config error, which cmd/jumpr turns into exit code 2.
func TestCLIParseErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // substring of the message
	}{
		{name: "unknown command", args: []string{"attatch"}, want: "unknown command"},
		{name: "unknown global flag", args: []string{"--nope"}, want: "unknown flag"},
		{name: "unknown command flag", args: []string{"ls", "-z"}, want: "unknown flag"},
		{name: "target on ls", args: []string{"ls", "-t", "x"}, want: "unknown flag"},
		{name: "target on new", args: []string{"new", "-t", "x"}, want: "unknown flag"},
		{name: "detached on attach", args: []string{"attach", "-d"}, want: "unknown flag"},
		{name: "session on attach", args: []string{"attach", "-s", "x"}, want: "unknown flag"},
		{name: "foreground on attach", args: []string{"--foreground"}, want: "unknown flag"},
		{name: "kill-session without a target", args: []string{"kill-session"}, want: "kill-session needs -t"},
		{name: "missing flag value", args: []string{"attach", "-t"}, want: "needs a value"},
		{name: "missing config dir", args: []string{"--config-dir"}, want: "needs a value"},
		{name: "missing socket", args: []string{"-S"}, want: "needs a value"},
		{name: "bad session name", args: []string{"kill-session", "-t", "bad/name"}, want: "invalid session name"},
		{name: "bad new name", args: []string{"new", "-s", "two words"}, want: "invalid session name"},
		{name: "extra argument", args: []string{"attach", "extra"}, want: "unexpected argument"},
		{name: "argument after the terminator", args: []string{"--", "ls"}, want: "unexpected argument"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCLI(tc.args)
			if err == nil {
				t.Fatalf("ParseCLI(%q) accepted a bad command line", tc.args)
			}
			if kind := errs.KindOf(err); kind != errs.Config {
				t.Fatalf("kind = %q, want %q", kind, errs.Config)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestCLIHello checks the first frame each command builds (BUILD-PLAN §6.2).
func TestCLIHello(t *testing.T) {
	env := map[string]string{"TERM": "xterm-256color"}
	cases := []struct {
		args []string
		want server.Hello
	}{
		{
			args: []string{"attach", "-t", "main"},
			want: server.Hello{Command: "attach", Session: "main"},
		},
		{
			args: []string{"new", "-s", "work"},
			want: server.Hello{Command: "new", Session: "work"},
		},
		{
			args: []string{"new", "-d", "-s", "work"},
			want: server.Hello{Command: "new", Session: "work", Detached: true},
		},
		{
			args: []string{"ls"},
			want: server.Hello{Command: "ls"},
		},
		{
			args: []string{"kill-session", "-t", "work"},
			want: server.Hello{Command: "kill-session", Session: "work"},
		},
		{
			args: []string{"kill-server"},
			want: server.Hello{Command: "kill-server"},
		},
	}

	for _, tc := range cases {
		inv, err := ParseCLI(tc.args)
		if err != nil {
			t.Fatalf("ParseCLI(%q): %v", tc.args, err)
		}
		got := inv.Hello("1.2.3", 132, 40, env)
		if got.Protocol != server.ProtocolVersion {
			t.Fatalf("%q: protocol = %d", tc.args, got.Protocol)
		}
		if got.Version != "1.2.3" || got.Cols != 132 || got.Rows != 40 {
			t.Fatalf("%q: hello = %+v", tc.args, got)
		}
		if got.Command != tc.want.Command || got.Session != tc.want.Session || got.Detached != tc.want.Detached {
			t.Fatalf("%q: hello = %+v, want %+v", tc.args, got, tc.want)
		}
		if got.Env["TERM"] != "xterm-256color" {
			t.Fatalf("%q: env = %v", tc.args, got.Env)
		}
	}
}

// TestCLISocketPath checks the resolution order: -S, then $JUMPR_SOCKET, then
// the cache directory.
func TestCLISocketPath(t *testing.T) {
	inv, err := ParseCLI([]string{"-S", "/tmp/flag.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if got := SocketPath(inv, "/tmp/env.sock", "/home/tester"); got != "/tmp/flag.sock" {
		t.Fatalf("with -S: %q", got)
	}

	inv, err = ParseCLI(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := SocketPath(inv, "/tmp/env.sock", "/home/tester"); got != "/tmp/env.sock" {
		t.Fatalf("with $JUMPR_SOCKET: %q", got)
	}
	if got, want := SocketPath(inv, "", "/home/tester"), "/home/tester/.cache/jumpr/jumpr.sock"; got != want {
		t.Fatalf("default = %q, want %q", got, want)
	}

	inv, err = ParseCLI([]string{"--config-dir", "~/cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if got := Paths(inv, "/home/tester").ConfigDir; got != "/home/tester/cfg" {
		t.Fatalf("config dir = %q", got)
	}
}

// TestCLICheckSocketPath checks the macOS length limit.
func TestCLICheckSocketPath(t *testing.T) {
	if err := CheckSocketPath(""); err == nil {
		t.Fatal("an empty path was accepted")
	}
	if err := CheckSocketPath("/tmp/jumpr.sock"); err != nil {
		t.Fatalf("a normal path was rejected: %v", err)
	}
	long := "/tmp/" + strings.Repeat("x", MaxSocketPath)
	if err := CheckSocketPath(long); err == nil {
		t.Fatal("an over-long path was accepted")
	} else if kind := errs.KindOf(err); kind != errs.Config {
		t.Fatalf("kind = %q, want config", kind)
	}
}

// TestCLISessionLines checks the `jumpr ls` output, which is tmux's:
// main: 2 windows (created Nov 12 14:02) (attached)
func TestCLISessionLines(t *testing.T) {
	created := time.Date(2026, 11, 12, 14, 2, 0, 0, time.Local)
	cases := []struct {
		info server.SessionInfo
		want string
	}{
		{
			info: server.SessionInfo{Name: "main", Windows: 2, Attached: true, Created: created},
			want: "main: 2 windows (created Nov 12 14:02) (attached)",
		},
		{
			info: server.SessionInfo{Name: "main", Windows: 1, Created: created},
			want: "main: 1 window (created Nov 12 14:02)",
		},
		{
			info: server.SessionInfo{Name: "db-incident", Windows: 0, Attached: true, Created: created},
			want: "db-incident: 0 windows (created Nov 12 14:02) (attached)",
		},
		{
			info: server.SessionInfo{Name: "main", Windows: 3, Created: time.Date(2026, 11, 2, 9, 5, 0, 0, time.Local)},
			want: "main: 3 windows (created Nov  2 09:05)",
		},
	}
	for _, tc := range cases {
		if got := FormatSessionLine(tc.info); got != tc.want {
			t.Fatalf("FormatSessionLine(%+v) = %q, want %q", tc.info, got, tc.want)
		}
	}
}

// TestCLIUsageCoversCommands checks the usage text names every command and flag
// the parser accepts: a command nobody documents is a command nobody finds.
func TestCLIUsageCoversCommands(t *testing.T) {
	usage := Usage()
	for _, c := range AllCommands() {
		if !strings.Contains(usage, string(c)) {
			t.Errorf("usage does not mention the %q command", c)
		}
	}
	for _, flag := range []string{"--config-dir", "--debug", "--socket", "--standalone",
		"--demo", "--readonly", "--fresh", "--version", "-t", "-s", "-d", "--foreground"} {
		if !strings.Contains(usage, flag) {
			t.Errorf("usage does not mention %s", flag)
		}
	}
}

// checkInvocation compares every parsed field.
func checkInvocation(t *testing.T, got, want Invocation) {
	t.Helper()
	if got.Command != want.Command {
		t.Fatalf("command = %q, want %q", got.Command, want.Command)
	}
	if got.ConfigDir != want.ConfigDir {
		t.Fatalf("config dir = %q, want %q", got.ConfigDir, want.ConfigDir)
	}
	if got.Socket != want.Socket {
		t.Fatalf("socket = %q, want %q", got.Socket, want.Socket)
	}
	if got.Debug != want.Debug || got.Standalone != want.Standalone || got.Demo != want.Demo ||
		got.ReadOnly != want.ReadOnly || got.Fresh != want.Fresh {
		t.Fatalf("flags = %+v, want %+v", got, want)
	}
	if got.Target != want.Target || got.Session != want.Session ||
		got.Detached != want.Detached || got.Foreground != want.Foreground {
		t.Fatalf("command flags = %+v, want %+v", got, want)
	}
	if !slices.Equal(got.Args, want.Args) {
		t.Fatalf("args = %q, want %q", got.Args, want.Args)
	}
}
