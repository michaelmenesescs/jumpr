package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/bus"
)

// loadTestPolicy writes body to a temp policy.yaml and loads it.
func loadTestPolicy(t *testing.T, body string) *bus.Policy {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := bus.LoadPolicy(path)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	return p
}

// TestPolicyCommandShowsEffectivePolicy checks that `:policy` shows the
// switches and the deny rules of the loaded policy, built-ins included.
func TestPolicyCommandShowsEffectivePolicy(t *testing.T) {
	env := newTestEnv(t)
	env.Policy = loadTestPolicy(t, `
readonly: false
max_targets: 12
protected: ["env=prod"]
redact: ['lab-[0-9]{4}']
deny:
  - pattern: '\bmkfs\b'
    reason: never format a lab disk
`)
	cmds := newTestCommands(t)
	if err := RegisterPolicyCommands(cmds); err != nil {
		t.Fatalf("register: %v", err)
	}
	cmd, ok := cmds.Lookup("policy")
	if !ok {
		t.Fatal(":policy is not registered")
	}
	msg := cmd.Run(env, "")()
	push, ok := msg.(PushViewMsg)
	if !ok {
		t.Fatalf(":policy returned %T, want PushViewMsg", msg)
	}
	text := strings.Join(push.View.CopyLines(), "\n")
	for _, want := range []string{
		"readonly: false",
		"max_targets: 12",
		"protected: env=prod",
		"redact: 1 extra pattern",
		"deny rules (",
		"systemctl",          // a built-in rule
		"never format a lab", // the file's rule
	} {
		if !strings.Contains(text, want) {
			t.Errorf(":policy text is missing %q:\n%s", want, text)
		}
	}
	if push.View.Crumb() != "policy" {
		t.Errorf("crumb = %q, want policy", push.View.Crumb())
	}
}

// TestPolicyCommandWithoutFile checks that the view says the built-in policy is
// what applies when no policy.yaml was loaded.
func TestPolicyCommandWithoutFile(t *testing.T) {
	env := newTestEnv(t)
	lines := strings.Join(policyLines(env), "\n")
	if !strings.Contains(lines, "no policy.yaml loaded") {
		t.Errorf("lines do not say the file is missing:\n%s", lines)
	}
	if !strings.Contains(lines, "built-ins first") {
		t.Errorf("lines do not list the built-in rules:\n%s", lines)
	}
	if !strings.Contains(policyLines(env)[0], "no policy.yaml") {
		t.Error("the note must be the first line")
	}
}

// TestReadOnlyCommand checks the three answers `:readonly` gives: on is a
// message, off asks for confirmation, and off is refused when the file says
// readonly.
func TestReadOnlyCommand(t *testing.T) {
	t.Run("on", func(t *testing.T) {
		env := newTestEnv(t)
		msg := runReadOnly(env, "on")()
		changed, ok := msg.(readOnlyChangedMsg)
		if !ok || !changed.On {
			t.Fatalf(":readonly on returned %#v, want readOnlyChangedMsg{On:true}", msg)
		}
	})

	t.Run("off asks", func(t *testing.T) {
		env := newTestEnv(t)
		env.Session = &SessionState{Name: "main", ReadOnly: true}
		msg := runReadOnly(env, "off")()
		open, ok := msg.(OpenDialogMsg)
		if !ok {
			t.Fatalf(":readonly off returned %T, want OpenDialogMsg", msg)
		}
		// The dialog is a y/N question; y runs the switch.
		if _, cmd := open.Dialog.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}); cmd == nil {
			t.Error("confirming the dialog returns no command")
		}
	})

	t.Run("off refused by policy", func(t *testing.T) {
		env := newTestEnv(t)
		env.Policy = loadTestPolicy(t, "readonly: true\n")
		env.Session = &SessionState{Name: "main", ReadOnly: true}
		msg := runReadOnly(env, "off")()
		flash, ok := msg.(FlashMsg)
		if !ok || flash.Level != FlashError {
			t.Fatalf(":readonly off returned %#v, want an error flash", msg)
		}
		if !strings.Contains(flash.Text, "policy.yaml") {
			t.Errorf("flash %q does not name the policy file", flash.Text)
		}
	})

	t.Run("off when already writable", func(t *testing.T) {
		env := newTestEnv(t)
		env.Session = &SessionState{Name: "main"}
		msg := runReadOnly(env, "off")()
		flash, ok := msg.(FlashMsg)
		if !ok || flash.Level != FlashInfo {
			t.Fatalf(":readonly off returned %#v, want an info flash", msg)
		}
	})

	t.Run("bad argument", func(t *testing.T) {
		env := newTestEnv(t)
		msg := runReadOnly(env, "maybe")()
		flash, ok := msg.(FlashMsg)
		if !ok || flash.Level != FlashError {
			t.Fatalf(":readonly maybe returned %#v, want an error flash", msg)
		}
	})
}

// TestApplyReadOnly checks that the root's handler is what moves the switch the
// status line's RO badge reads.
func TestApplyReadOnly(t *testing.T) {
	env := newTestEnv(t)
	env.Session = &SessionState{Name: "main"}
	r := &Root{env: env}

	if cmd := r.applyReadOnly(true); cmd == nil {
		t.Error("applyReadOnly(true) returned no flash")
	}
	if !env.Session.ReadOnly {
		t.Error("applyReadOnly(true) left the session writable")
	}
	// A policy that says readonly keeps the switch on: the session may not
	// overrule the file.
	env.Policy = loadTestPolicy(t, "readonly: true\n")
	if !sessionReadOnly(env) {
		t.Error("sessionReadOnly ignores the policy switch")
	}
	r.applyReadOnly(false)
	if env.Session.ReadOnly {
		t.Error("applyReadOnly(false) did not clear the session switch")
	}
}

// TestReadOnlyBadge checks that the switch reaches the status line: the RO
// badge appears while the session is read-only and goes away when it is not.
func TestReadOnlyBadge(t *testing.T) {
	h := newSyncHarness(t, 0)

	h.do(func() { h.root.applyReadOnly(true) })
	var data StatusData
	h.do(func() { data = h.root.statusData() })
	if !data.ReadOnly {
		t.Fatal("statusData().ReadOnly is false after read-only on")
	}
	if got := RenderStatus(themeOf(h.root.env), data, 120); !strings.Contains(got, "RO") {
		t.Errorf("status line has no RO badge: %q", got)
	}

	h.do(func() { h.root.applyReadOnly(false) })
	h.do(func() { data = h.root.statusData() })
	if data.ReadOnly {
		t.Error("statusData().ReadOnly is true after read-only off")
	}
	if got := RenderStatus(themeOf(h.root.env), data, 120); strings.Contains(got, "RO") {
		t.Errorf("status line still shows the RO badge: %q", got)
	}
}
