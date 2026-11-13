package views

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/elastic"
	"jumper/internal/backend/linux"
	"jumper/internal/errs"
	"jumper/internal/probe"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/transport/transporttest"
	"jumper/internal/tui"
)

// hostTestNow is the clock every host test uses.
var hostTestNow = time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)

// hostSectionIDs are the host batch's section markers, in probe order. The
// backend mints its own batch; the test only needs matching marker names.
var hostSectionIDs = []string{
	"loadavg", "meminfo", "psi", "os", "kernel", "uptime", "nproc", "sysstate", "failed",
}

// hostProbeBatch is a marker-compatible stand-in for the backend's batch.
func hostProbeBatch(t *testing.T) probe.Batch {
	t.Helper()
	sections := make([]probe.Section, len(hostSectionIDs))
	for i, id := range hostSectionIDs {
		sections[i] = probe.Section{ID: id, Cmd: "true"}
	}
	batch, err := probe.NewBatch("deadbeef", sections...)
	if err != nil {
		t.Fatalf("probe.NewBatch: %v", err)
	}
	return batch
}

// hostMark writes one marker pair, exactly like a real node's script does.
func hostMark(sb *strings.Builder, nonce, id string, rc int, body string) {
	fmt.Fprintf(sb, "\n@@SITREP:%s:%s:B@@\n%s\n@@SITREP:%s:%s:E:%d@@\n", nonce, id, body, nonce, id, rc)
}

// hostReply scripts the overview batch reply from per-section stdout.
func hostReply(t *testing.T, bodies map[string]string) transporttest.Reply {
	t.Helper()
	batch := hostProbeBatch(t)
	var out, errs2 strings.Builder
	for _, id := range hostSectionIDs {
		hostMark(&out, batch.Nonce, id, 0, bodies[id])
		hostMark(&errs2, batch.Nonce, id, 0, "")
	}
	return transporttest.Reply{Stdout: out.String(), Stderr: errs2.String()}
}

// hostBodies is a Rocky 9 host with one failed nginx unit.
func hostBodies() map[string]string {
	return map[string]string{
		"loadavg":  "4.12 3.98 2.10 2/413 99123",
		"meminfo":  "MemTotal:       32768000 kB\nMemAvailable:    9502720 kB\nMemFree: 100 kB\nBuffers: 300 kB\nCached: 400 kB",
		"psi":      "== cpu\nsome avg10=1.20 avg60=0.80 avg300=0.40 total=123456\n== memory\nsome avg10=0.00 avg60=0.00 avg300=0.00 total=0\n== io\nsome avg10=3.20 avg60=1.10 avg300=0.50 total=987654\nfull avg10=2.90 avg60=0.90 avg300=0.40 total=876543",
		"os":       "ID=\"rocky\"\nVERSION_ID=\"9.4\"\nPRETTY_NAME=\"Rocky Linux 9.4 (Blue Onyx)\"",
		"kernel":   "5.14.0-427.el9.x86_64",
		"uptime":   "3553200.00 28012345.11",
		"nproc":    "8",
		"sysstate": "degraded",
		"failed":   "● nginx.service loaded failed failed A high performance web server",
	}
}

// hostJournalOut is two journalctl JSON lines inside the session window.
func hostJournalOut() string {
	mk := func(at time.Time, pri int, src, msg string) string {
		return fmt.Sprintf(
			`{"__REALTIME_TIMESTAMP":"%d","PRIORITY":"%d","_COMM":"%s","MESSAGE":"%s"}`,
			at.UnixMicro(), pri, src, msg)
	}
	return mk(hostTestNow.Add(-7*time.Minute), 3, "nginx", "bind() to 0.0.0.0:443 failed (98: Address already in use)") +
		"\n" + mk(hostTestNow.Add(-6*time.Minute), 2, "kernel", "oom-kill: constraint=CONSTRAINT_NONE") + "\n"
}

// newHostBackend builds a real Linux backend over the scripted transport.
func newHostBackend(t *testing.T, ex *transporttest.Fake) *linux.Backend {
	t.Helper()
	return linux.New(linux.Options{
		Exec:    ex,
		Timeout: time.Second,
		Nonce:   func() string { return "deadbeef" },
		Now:     func() time.Time { return hostTestNow },
	})
}

// hostEnv is the environment a host view runs in: no colour, fixed clock, a
// half-full session window.
func hostEnv() *tui.Env {
	env := testEnv()
	env.Now = func() time.Time { return hostTestNow }
	env.Session = &tui.SessionState{
		Name:   "main",
		Window: resource.TimeWindow{From: hostTestNow.Add(-time.Hour), ToNow: true, Label: "-1h"},
	}
	return env
}

// drainHost runs Init and every follow-up command by hand until both streams
// close, like the root would.
func drainHost(t *testing.T, h *Host) *Host {
	t.Helper()
	drainHostCmd(t, h, h.Init())
	return h
}

// drainHostCmd applies one command and everything it asks for next. The view is
// a pointer, so Update's in-place changes stick.
func drainHostCmd(t *testing.T, h *Host, cmd tea.Cmd) {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i > 5000 {
			t.Fatal("streams did not close")
		}
		msg := cmd()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if c != nil {
					drainHostCmd(t, h, c)
				}
			}
			return
		}
		_, next := h.Update(msg)
		cmd = next
	}
}

// sendHost routes one message into the view and returns its command.
func sendHost(t *testing.T, h *Host, msg tea.Msg) tea.Cmd {
	t.Helper()
	v, cmd := h.Update(msg)
	if _, ok := v.(*Host); !ok {
		t.Fatalf("Update returned %T, want *Host", v)
	}
	return cmd
}

// hostFixture builds a loaded view over the scripted backend.
func hostFixture(t *testing.T) *Host {
	t.Helper()
	ex := transporttest.New()
	// The journal rule goes first: the first matching rule wins and the empty
	// match would otherwise answer the journalctl call with the batch reply.
	ex.On("journalctl", transporttest.Reply{Stdout: hostJournalOut()})
	ex.On("", hostReply(t, hostBodies()))
	b := newHostBackend(t, ex)
	return drainHost(t, NewHost(hostEnv(), b, "web-01.lab.example.internal"))
}

func TestHostRendersSections(t *testing.T) {
	h := hostFixture(t)
	frame := stripHostANSI(h.Render(120, 30))

	for _, want := range []string{
		"web-01.lab.example.internal",
		"SYSTEM", "PRESSURE",
		"Rocky Linux 9.4",
		"kernel 5.14.0-427",
		"uptime 41d 3h",
		"systemd degraded",
		"1 failed unit",
		"LOAD", "4.12 3.98 2.10", "(8 cpu)",
		"MEMORY",
		"FAILED UNITS",
		"nginx.service",
		"JOURNAL",
		"bind() to 0.0.0.0:443 failed",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame does not contain %q:\n%s", want, frame)
		}
	}
}

func TestHostPartialFailure(t *testing.T) {
	ex := transporttest.New()
	ex.On("journalctl", transporttest.Reply{
		Stderr: "sudo: a password is required",
		RC:     1,
	})
	ex.On("", hostReply(t, hostBodies()))
	b := newHostBackend(t, ex)
	h := drainHost(t, NewHost(hostEnv(), b, "web-01.lab.example.internal"))
	frame := stripHostANSI(h.Render(120, 30))

	if !strings.Contains(frame, "needs-privilege") && !strings.Contains(frame, "sudo") {
		t.Errorf("journal failure is not labelled:\n%s", frame)
	}
	for _, want := range []string{"SYSTEM", "PRESSURE", "FAILED UNITS", "nginx.service"} {
		if !strings.Contains(frame, want) {
			t.Errorf("overview dropped %q when the journal failed:\n%s", want, frame)
		}
	}
}

func TestHostExactSize(t *testing.T) {
	h := hostFixture(t)
	for _, size := range [][2]int{{80, 24}, {132, 40}} {
		w, ht := size[0], size[1]
		frame := h.Render(w, ht)
		lines := strings.Split(frame, "\n")
		if len(lines) != ht {
			t.Fatalf("%dx%d: %d lines, want %d", w, ht, len(lines), ht)
		}
		for i, line := range lines {
			if got := render.Width(line); got != w {
				t.Fatalf("%dx%d: line %d is %d cells, want %d: %q", w, ht, i, got, w, line)
			}
		}
	}
	// Under 100 columns the right column is dropped; at 132 it is there.
	if strings.Contains(stripHostANSI(h.Render(80, 24)), "MEMORY") {
		t.Error("80 columns kept the MEMORY block")
	}
	if !strings.Contains(stripHostANSI(h.Render(132, 40)), "MEMORY") {
		t.Error("132 columns dropped the MEMORY block")
	}
}

func TestHostKeysOpenQueries(t *testing.T) {
	h := hostFixture(t)
	want := map[string]resource.KindName{
		"u": linux.KindUnit,
		"j": linux.KindJournal,
		"p": linux.KindProcess,
		"s": linux.KindSocket,
		"l": elastic.KindLog,
	}
	for key, kind := range want {
		cmd := sendHost(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd == nil {
			t.Fatalf("key %q returned no command", key)
		}
		msg, ok := cmd().(tui.OpenQueryMsg)
		if !ok {
			t.Fatalf("key %q produced %T, want OpenQueryMsg", key, cmd())
		}
		if msg.Query.Kind != kind {
			t.Errorf("key %q opened kind %q, want %q", key, msg.Query.Kind, kind)
		}
		if len(msg.Query.Hosts) != 1 || msg.Query.Hosts[0] != "web-01.lab.example.internal" {
			t.Errorf("key %q opened hosts %v", key, msg.Query.Hosts)
		}
	}

	// r refreshes both parts, esc goes back.
	if cmd := sendHost(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd == nil {
		t.Error("refresh returned no command")
	}
	cmd := sendHost(t, h, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc returned no command")
	}
	if _, ok := cmd().(tui.PopViewMsg); !ok {
		t.Errorf("esc produced %T, want PopViewMsg", cmd())
	}

	hints := h.Hints()
	for _, key := range []string{"u", "j", "p", "s", "l", "r", "esc"} {
		found := false
		for _, hint := range hints {
			if hint.Key == key {
				found = true
			}
		}
		if !found {
			t.Errorf("Hints() has no %q", key)
		}
	}
}

func TestHostWindowReloadsJournal(t *testing.T) {
	h := hostFixture(t)
	// The journal part is loaded before the window changes.
	if len(h.journalRows) == 0 {
		t.Fatal("journal part did not load")
	}
	_, cmd := h.Update(tui.WindowChangedMsg{
		Window: resource.TimeWindow{From: hostTestNow.Add(-2 * time.Hour), ToNow: true, Label: "-2h"},
	})
	if cmd == nil {
		t.Fatal("window change did not reload the journal")
	}
	h = drainHost(t, h)
	if h.journalErr != nil {
		t.Fatalf("journal reload failed: %v", h.journalErr)
	}
	if len(h.journalRows) == 0 {
		t.Error("journal part is empty after the window change")
	}
}

func TestHostCrumbSubjectAndCommandLine(t *testing.T) {
	h := hostFixture(t)
	if got := h.Crumb(); got != "web-01" {
		t.Errorf("Crumb() = %q, want web-01", got)
	}
	if got := h.CommandLine(); got != "overview --host web-01.lab.example.internal" {
		t.Errorf("CommandLine() = %q", got)
	}
	subj := h.Subject()
	if len(subj.Hosts) != 1 || subj.Hosts[0] != "web-01.lab.example.internal" {
		t.Errorf("Subject hosts = %v", subj.Hosts)
	}
	if subj.Unit != "nginx.service" {
		t.Errorf("Subject unit = %q, want the first failed unit", subj.Unit)
	}
	if subj.Kind != linux.KindHostOverview {
		t.Errorf("Subject kind = %q", subj.Kind)
	}
}

func TestHostFactory(t *testing.T) {
	reg := backend.NewRegistry()
	b := newHostBackend(t, transporttest.New())
	if err := reg.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}
	env := hostEnv()
	env.Registry = reg
	env.Factories = Factories()

	f, ok := Factories()[linux.KindHostOverview]
	if !ok {
		t.Fatal("Factories() has no host-overview view")
	}
	v, err := f(env, resource.Query{Kind: linux.KindHostOverview, Args: map[string]string{"host": "web-01"}})
	if err != nil {
		t.Fatalf("hostFactory: %v", err)
	}
	h, ok := v.(*Host)
	if !ok {
		t.Fatalf("factory built %T", v)
	}
	if h.host != "web-01" {
		t.Errorf("factory host = %q", h.host)
	}
	if _, err := f(env, resource.Query{Kind: linux.KindHostOverview}); errs.KindOf(err) != errs.Config {
		t.Errorf("factory without a host: kind = %s, want config", errs.KindOf(err))
	}
}

// stripHostANSI removes the colour escape sequences the theme adds; tests
// assert on text, never on ANSI.
func stripHostANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
