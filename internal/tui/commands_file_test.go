package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend"
	"jumpr/internal/backend/linux"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport/transporttest"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// fileTestEnv wires a real Linux backend over a scripted transport into an Env,
// which is how the file commands reach the fleet in production.
func fileTestEnv(t *testing.T, ex *transporttest.Fake) *Env {
	t.Helper()
	b := linux.New(linux.Options{
		Exec:    ex,
		Timeout: time.Second,
		Nonce:   func() string { return "deadbeef" },
		Now:     func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	})
	reg := backend.NewRegistry()
	if err := reg.Register(b); err != nil {
		t.Fatalf("register linux backend: %v", err)
	}
	return &Env{Registry: reg, Keys: keymap.Default(), Theme: theme.Dark(), Now: time.Now}
}

// fileTestCommands is the registry `:` lines run against.
func fileTestCommands(t *testing.T) *Commands {
	t.Helper()
	c := NewCommands()
	if err := RegisterFileCommands(c); err != nil {
		t.Fatalf("RegisterFileCommands: %v", err)
	}
	return c
}

// runLine executes one `:` line and returns the message its tea.Cmd produced.
func runLine(t *testing.T, c *Commands, env *Env, line string) tea.Msg {
	t.Helper()
	cmd := c.Exec(env, line)
	if cmd == nil {
		t.Fatalf("%s: no command", line)
	}
	return cmd()
}

// viewLines returns the pushed viewer's lines, or fails the test.
func viewLines(t *testing.T, msg tea.Msg) []string {
	t.Helper()
	push, ok := msg.(PushViewMsg)
	if !ok {
		t.Fatalf("message = %T (%v), want PushViewMsg", msg, msg)
	}
	page, ok := push.View.(*textPage)
	if !ok {
		t.Fatalf("view = %T, want *textPage", push.View)
	}
	return page.CopyLines()
}

func TestFileCommandQuoting(t *testing.T) {
	ex := transporttest.New()
	env := fileTestEnv(t, ex)
	ex.On("stat -c", transporttest.Reply{Stdout: "2048 root:adm 640 2026-11-12 10:31:02.123456789 -0500\n"})
	ex.On("head -c", transporttest.Reply{Stdout: "first line\nsecond line\n"})

	// Spaces, a single quote, a double quote and a shell metacharacter: all of
	// it must reach the remote side as one quoted argument.
	path := `/var/log/my app's "logs" $HOME; rm -rf x.log`
	line := `file "` + strings.ReplaceAll(path, `"`, `\"`) + `" --host web-01`
	msg := runLine(t, fileTestCommands(t), env, line)
	lines := viewLines(t, msg)

	calls := ex.Calls()
	if len(calls) != 2 {
		t.Fatalf("exec calls = %d, want 2 (stat then head)", len(calls))
	}
	if got, want := calls[0].Command.Script, fileStatScript(path); got != want {
		t.Errorf("stat script = %q, want %q", got, want)
	}
	if got, want := calls[1].Command.Script, fileBodyScript(path, 0); got != want {
		t.Errorf("body script = %q, want %q", got, want)
	}
	if !strings.Contains(calls[0].Command.Script, shq.Quote(path)) {
		t.Errorf("path is not shq-quoted: %s", calls[0].Command.Script)
	}
	if strings.Contains(calls[0].Command.Script, "; rm -rf") && !strings.Contains(calls[0].Command.Script, shq.Quote(path)) {
		t.Errorf("path escaped its quotes: %s", calls[0].Command.Script)
	}
	if !calls[0].Command.Sudo || !calls[1].Command.Sudo {
		t.Error("file reads must run under sudo")
	}
	if got, want := calls[0].Host, resource.HostID("web-01"); got != want {
		t.Errorf("host = %q, want %q", got, want)
	}

	// The header carries size, owner, mode and mtime; the body follows.
	joined := strings.Join(lines, "\n")
	for _, want := range []string{path, "size 2.0 KiB", "owner root:adm", "mode 640", "mtime 2026-11-12 10:31:02 -0500", "first line", "second line"} {
		if !strings.Contains(joined, want) {
			t.Errorf("viewer is missing %q:\n%s", want, joined)
		}
	}
}

func TestFileCommandTailAndUsage(t *testing.T) {
	ex := transporttest.New()
	env := fileTestEnv(t, ex)
	ex.On("stat -c", transporttest.Reply{Stdout: "10 root:root 644 2026-11-12 10:31:02.000000000 +0000\n"})
	ex.On("tail -n", transporttest.Reply{Stdout: "last line\n"})

	msg := runLine(t, fileTestCommands(t), env, `file /var/log/syslog --host web-01 --tail 5`)
	lines := viewLines(t, msg)
	if got, want := ex.Calls()[1].Command.Script, "tail -n 5 "+shq.Quote("/var/log/syslog"); got != want {
		t.Errorf("tail script = %q, want %q", got, want)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "last line") {
		t.Errorf("viewer has no tail content: %v", lines)
	}

	cases := []struct{ line, want string }{
		{"file", "usage: :file"},
		{"file /etc/a b", "quote a path with spaces"},
		{"file /etc/hosts", "no host"},
		{"file /etc/hosts --host web-01 --tail 0", "positive line count"},
		{"file /etc/hosts --host web-01 --tail", "line count"},
		{"file /etc/hosts --host", "comma-separated host list"},
	}
	c := fileTestCommands(t)
	for _, tc := range cases {
		msg := runLine(t, c, env, tc.line)
		flash, ok := msg.(FlashMsg)
		if !ok {
			t.Fatalf("%s: message = %T, want FlashMsg", tc.line, msg)
		}
		if flash.Level != FlashError || !strings.Contains(flash.Text, tc.want) {
			t.Errorf("%s: flash = %q (%d), want an error containing %q", tc.line, flash.Text, flash.Level, tc.want)
		}
	}
}

func TestFileCommandSensitiveAsks(t *testing.T) {
	ex := transporttest.New()
	env := fileTestEnv(t, ex)
	ex.On("stat -c", transporttest.Reply{Stdout: "512 root:shadow 640 2026-11-12 10:31:02.000000000 +0000\n"})
	ex.On("head -c", transporttest.Reply{Stdout: "root:$6$hash:19000:0:99999:7:::\npassword = hunter2\n"})

	c := fileTestCommands(t)
	msg := runLine(t, c, env, `file /etc/shadow --host db-01`)
	dialog, ok := msg.(OpenDialogMsg)
	if !ok {
		t.Fatalf("message = %T (%v), want OpenDialogMsg (the y/N question)", msg, msg)
	}
	if got := ex.Calls(); len(got) != 0 {
		t.Fatalf("a sensitive path read %d times before confirmation", len(got))
	}

	// The n answer closes without reading.
	_, noCmd := dialog.Dialog.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if noCmd == nil {
		t.Fatal("n produced no command")
	}
	if _, ok := noCmd().(CloseDialogMsg); !ok {
		t.Fatalf("n produced %T, want CloseDialogMsg", noCmd())
	}
	if got := ex.Calls(); len(got) != 0 {
		t.Fatalf("n still read the file")
	}

	// The y answer runs the read, and the content is redacted.
	_, yesCmd := dialog.Dialog.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if yesCmd == nil {
		t.Fatal("y produced no command")
	}
	var push tea.Msg
	switch m := yesCmd().(type) {
	case tea.BatchMsg:
		for _, sub := range m {
			if got := sub(); got != nil {
				if _, ok := got.(PushViewMsg); ok {
					push = got
				}
			}
		}
	case PushViewMsg:
		push = m
	}
	if push == nil {
		t.Fatal("y did not open the viewer")
	}
	joined := strings.Join(viewLines(t, push), "\n")
	if strings.Contains(joined, "hunter2") {
		t.Errorf("sensitive content was not redacted:\n%s", joined)
	}
	if !strings.Contains(joined, "redacted") {
		t.Errorf("expected a redaction marker:\n%s", joined)
	}
	if got := len(ex.Calls()); got != 2 {
		t.Errorf("exec calls after y = %d, want 2", got)
	}
}

func TestFileCommandBinaryRefused(t *testing.T) {
	ex := transporttest.New()
	env := fileTestEnv(t, ex)
	ex.On("stat -c", transporttest.Reply{Stdout: "16 root:root 644 2026-11-12 10:31:02.000000000 +0000\n"})
	ex.On("head -c", transporttest.Reply{Stdout: "\x7fELF\x02\x01\x01\x00binary body"})

	msg := runLine(t, fileTestCommands(t), env, `file /usr/bin/jumpr --host web-01`)
	flash, ok := msg.(FlashMsg)
	if !ok {
		t.Fatalf("message = %T, want FlashMsg", msg)
	}
	if flash.Level != FlashWarn || !strings.Contains(flash.Text, "refusing to display binary file") {
		t.Errorf("flash = %q (%d), want a binary refusal", flash.Text, flash.Level)
	}
}

func TestFileCommandMultiHostChecksum(t *testing.T) {
	ex := transporttest.New()
	env := fileTestEnv(t, ex)
	sumA := strings.Repeat("a", 64)
	sumB := strings.Repeat("b", 64)
	ex.OnHost("web-01", "sha256sum", transporttest.Reply{Stdout: sumA + "  /etc/nginx/nginx.conf\n"})
	ex.OnHost("web-02", "sha256sum", transporttest.Reply{Stdout: sumA + "  /etc/nginx/nginx.conf\n"})
	ex.OnHost("web-03", "sha256sum", transporttest.Reply{Stdout: sumB + "  /etc/nginx/nginx.conf\n"})

	msg := runLine(t, fileTestCommands(t), env, `file /etc/nginx/nginx.conf --host web-01,web-02,web-03`)
	lines := viewLines(t, msg)
	joined := strings.Join(lines, "\n")

	calls := ex.Calls()
	if len(calls) != 3 {
		t.Fatalf("exec calls = %d, want 3 (one sha256sum per host)", len(calls))
	}
	for _, call := range calls {
		if got, want := call.Command.Script, checksumScript("/etc/nginx/nginx.conf"); got != want {
			t.Errorf("script = %q, want %q", got, want)
		}
	}
	if !strings.Contains(joined, "3 hosts") || !strings.Contains(joined, "2 sums") {
		t.Errorf("headline is not grouped:\n%s", joined)
	}
	first := strings.Index(joined, sumA)
	second := strings.Index(joined, sumB)
	if first < 0 || second < 0 {
		t.Fatalf("digests missing from the fan-out:\n%s", joined)
	}
	if first > second {
		t.Errorf("bigger group must come first:\n%s", joined)
	}
	if !strings.Contains(joined, "web-01, web-02") || !strings.Contains(joined, "web-03") {
		t.Errorf("host groups are wrong:\n%s", joined)
	}
}

func TestFileCommandProbePkg(t *testing.T) {
	ex := transporttest.New()
	env := fileTestEnv(t, ex)
	ex.OnHost("web-01", "rpm -q", transporttest.Reply{Stdout: "@@family rpm\n1.2.3-1\n"})
	ex.OnHost("web-02", "rpm -q", transporttest.Reply{Stdout: "@@family rpm\n1.2.3-1\n"})
	ex.OnHost("web-03", "rpm -q", transporttest.Reply{Stdout: "@@family rpm\n1.2.4-1\n"})

	c := fileTestCommands(t)
	msg := runLine(t, c, env, `probe pkg openssl --host web-01,web-02,web-03`)
	joined := strings.Join(viewLines(t, msg), "\n")
	if !strings.Contains(joined, "pkg openssl") || !strings.Contains(joined, "2 versions") {
		t.Errorf("probe headline is wrong:\n%s", joined)
	}
	if !strings.Contains(joined, "1.2.3-1  2 hosts  web-01, web-02") {
		t.Errorf("identical versions are not grouped:\n%s", joined)
	}
	calls := ex.Calls()
	if len(calls) != 3 {
		t.Fatalf("exec calls = %d, want 3", len(calls))
	}
	if got, want := calls[0].Command.Script, linux.PackageVersionCommand("openssl"); got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
	if calls[0].Command.Sudo {
		t.Error("a package version probe needs no sudo")
	}

	// Misuse is a flash, never a panic.
	for _, line := range []string{"probe", "probe kernel", "probe pkg", "probe pkg bash", "probe pkg bash --host a --extra"} {
		if msg := runLine(t, c, env, line); msg == nil {
			t.Fatalf("%s: no message", line)
		}
	}
}

func TestFileCommandSensitivePatterns(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/etc/shadow", true},
		{"/etc/ssl/private/site.key", true},
		{"/etc/ssl/certs/ca.pem", true},
		{"/home/ops/.ssh/id_ed25519", true},
		{"/home/ops/.ssh/config", true},
		{"/etc/app/secret.yaml", true},
		{"/run/app/token", true},
		{"/etc/hosts", false},
		{"/var/log/nginx/access.log", false},
		{"/opt/keychain", false},
	}
	for _, tc := range cases {
		if _, got := sensitivePath(tc.path); got != tc.want {
			t.Errorf("sensitivePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestFileCommandNoBackendNeverPanics keeps a pane-less env safe.
func TestFileCommandNoBackendNeverPanics(t *testing.T) {
	c := fileTestCommands(t)
	msg := runLine(t, c, &Env{Keys: keymap.Default()}, `file /etc/hosts --host web-01`)
	flash, ok := msg.(FlashMsg)
	if !ok || flash.Level != FlashError {
		t.Fatalf("message = %T (%v), want an error flash", msg, msg)
	}
}

// TestFileCommandTransportErrorFlashes reports a remote failure instead of
// opening an empty view.
func TestFileCommandTransportErrorFlashes(t *testing.T) {
	ex := transporttest.New()
	env := fileTestEnv(t, ex)
	ex.On("stat -c", transporttest.Reply{RC: 1, Stderr: "stat: cannot statx '/nope': No such file or directory"})

	msg := runLine(t, fileTestCommands(t), env, `file /nope --host web-01`)
	flash, ok := msg.(FlashMsg)
	if !ok || flash.Level != FlashError {
		t.Fatalf("message = %T (%v), want an error flash", msg, msg)
	}
	if !strings.Contains(flash.Text, "No such file") {
		t.Errorf("flash = %q, want the remote message", flash.Text)
	}
}

// TestFileCommandViewScrolls checks the pushed viewer is a real View: it renders
// the exact frame and pops on esc.
func TestFileCommandViewScrolls(t *testing.T) {
	page := newTextPage(nil, "file /x", []string{"one", "two", "three"})
	frame := page.Render(10, 2)
	if got, want := len(strings.Split(frame, "\n")), 2; got != want {
		t.Fatalf("frame has %d lines, want %d", got, want)
	}
	if !strings.HasPrefix(strings.Split(frame, "\n")[0], "one") {
		t.Errorf("first line = %q", strings.Split(frame, "\n")[0])
	}
	if _, cmd := page.Update(tea.KeyMsg{Type: tea.KeyDown}); cmd != nil {
		t.Errorf("down produced %v", cmd())
	}
	if !strings.Contains(page.Render(10, 2), "two") {
		t.Errorf("down did not scroll: %q", page.Render(10, 2))
	}
	_, cmd := page.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(PopViewMsg); !ok {
		t.Errorf("esc produced %T, want PopViewMsg", cmd())
	}
	if page.Capturing() {
		t.Error("the viewer must not capture the keyboard")
	}
	if sub := page.Subject(); sub.Kind != "" || sub.Unit != "" || sub.Alert != "" || len(sub.Hosts) != 0 {
		t.Errorf("subject = %+v, want empty", sub)
	}
}
