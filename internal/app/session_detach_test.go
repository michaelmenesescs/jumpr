package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/interact"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
	"jumpr/internal/tui"
	"jumpr/internal/tui/theme"
)

// The session UX of task 082, from the process's side: work keeps running while
// detached, the attach summary, the prompt that nobody could answer, and the
// rename that moves the autosave file.

// TestDetachKeepsWorkRunning is requirement 1's promise: detaching only unplugs
// the terminal. A fan-out that was running is still running, and the session is
// not done.
func TestDetachKeepsWorkRunning(t *testing.T) {
	ex := transporttest.New().OnFunc(func(resource.HostID, transport.Command) (transporttest.Reply, bool) {
		return transporttest.Reply{Stdout: "up\n", Delay: 150 * time.Millisecond}, true
	})
	core := testCore(t, []string{"web-01", "web-02"}, ex)
	sess := testSession(t, core)

	c := attach(t, sess, "web-01")
	typeKeys(t, c.in, ":targets role=web\r")
	waitForText(t, c.out, "targets: 2 hosts")
	typeKeys(t, c.in, ":probe uptime\r")
	time.Sleep(100 * time.Millisecond)

	sess.Detach("detached")
	if sess.Info().Attached {
		t.Fatal("the session still reports a client")
	}
	// The host work finishes with nobody attached.
	deadline := time.Now().Add(10 * time.Second)
	for len(ex.Calls()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(ex.Calls()); got < 2 {
		t.Fatalf("%d hosts probed while detached, want 2", got)
	}
	select {
	case <-sess.Done():
		t.Fatal("the session ended on a detach")
	default:
	}
}

// TestWhileDetachedSummary is requirement 8 seen from the terminal: the next
// attach says what happened while nobody was looking.
func TestWhileDetachedSummary(t *testing.T) {
	ex := transporttest.New().OnFunc(func(resource.HostID, transport.Command) (transporttest.Reply, bool) {
		return transporttest.Reply{Stdout: "up\n", Delay: 120 * time.Millisecond}, true
	})
	core := testCore(t, []string{"web-01", "web-02", "web-03"}, ex)
	sess := testSession(t, core)

	c := attach(t, sess, "web-01")
	typeKeys(t, c.in, ":targets role=web\r")
	waitForText(t, c.out, "targets: 3 hosts")
	typeKeys(t, c.in, ":probe uptime\r")
	time.Sleep(100 * time.Millisecond)
	sess.Detach("detached")

	deadline := time.Now().Add(10 * time.Second)
	for len(ex.Calls()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(ex.Calls()); got != 3 {
		t.Fatalf("%d hosts probed while detached, want 3", got)
	}
	// The view has to have seen the closed stream for the counter to be
	// meaningful: the probe batch arrives at the session's own program.
	time.Sleep(200 * time.Millisecond)

	b := attach(t, sess, "")
	waitForText(t, b.out, "while detached:")
	waitForText(t, b.out, "1 fan-out finished")
}

// TestPromptFailsWhenDetached is requirement 6: a question asked while nobody is
// attached waits for interact.timeout and then fails with errs.Interaction, and
// the next attach says what was skipped.
func TestPromptFailsWhenDetached(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	skips := newPromptSkips()
	tap := &promptTap{skips: skips}
	tap.setInner(tui.NewPrompter(core.bridge.Send, 80*time.Millisecond))
	core.Prompter = tap

	sess := testSession(t, core)
	sess.SetPromptSkips(skips)
	attach(t, sess, "web-01")

	sess.Detach("detached")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := core.Prompter.Ask(ctx, interact.Prompt{
		Kind:    interact.HostKeyUnknown,
		Title:   "Unknown host key",
		Body:    "web-01 ssh-ed25519 SHA256:test",
		Choices: []string{"yes", "no"},
	})
	if err == nil {
		t.Fatal("a detached prompt was answered")
	}
	if got := errsKind(err); got != "needs-interaction" {
		t.Fatalf("prompt error kind = %q, want needs-interaction (%v)", got, err)
	}

	b := attach(t, sess, "")
	waitForText(t, b.out, "1 host needs a host-key decision: :health")
}

// TestRenameSessionRenamesFile is requirement 4: the autosave file follows the
// session's new name.
func TestRenameSessionRenamesFile(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)
	attach(t, sess, "web-01")

	if err := sess.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	oldPath := sess.path
	if oldPath == "" {
		t.Fatal("the session has no autosave path")
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("the autosave file was not written: %v", err)
	}

	if err := sess.Rename("db-incident"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := sess.Info().Name; got != "db-incident" {
		t.Fatalf("session name = %q", got)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("the old session file is still there (err = %v)", err)
	}
	newPath := filepath.Join(filepath.Dir(oldPath), "db-incident.json")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("the renamed session file is missing: %v", err)
	}
	// Later saves go to the new file.
	if err := sess.save(); err != nil {
		t.Fatalf("save after rename: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("the new session file is missing after a save: %v", err)
	}
	if err := sess.Rename("bad name"); err == nil {
		t.Fatal("an invalid session name was accepted")
	}
}

// TestSwitchToRaisesTheServerRequest is requirement 3's callback: the session
// hands its switch request to the server and reports what came back.
func TestSwitchToRaisesTheServerRequest(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)

	if err := sess.SwitchTo("other"); err == nil {
		t.Fatal("a session with no switch hook switched anyway")
	}
	var asked []string
	sess.SetSwitchHook(func(name string) error {
		asked = append(asked, name)
		return nil
	})
	if err := sess.SwitchTo("db-incident"); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if len(asked) != 1 || asked[0] != "db-incident" {
		t.Fatalf("switch requests = %v, want [db-incident]", asked)
	}
}

// TestSavedSessionsListsFiles is the dormant half of requirement 5: the server
// is told what is on disk, and a broken file is skipped rather than fatal.
func TestSavedSessionsListsFiles(t *testing.T) {
	theme.SetProfile("none")
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	good := filepath.Join(sessions, "db-incident.json")
	if err := os.WriteFile(good, []byte(`{"version":1,"name":"db-incident","active":0,`+
		`"windows":[{"name":"","layout":{"pane":1},"focus":1,"panes":{"1":{}}}]}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := savedSessions(sessions)
	if len(got) != 1 {
		t.Fatalf("savedSessions = %+v, want the one readable file", got)
	}
	if got[0].Name != "db-incident" || got[0].Windows != 1 {
		t.Fatalf("saved session = %+v", got[0])
	}
	if got[0].Activity.IsZero() {
		t.Fatal("the saved session has no activity time")
	}
}

// errsKind is the error kind as a string, for the message in a failure.
func errsKind(err error) string {
	return string(errs.KindOf(err))
}
