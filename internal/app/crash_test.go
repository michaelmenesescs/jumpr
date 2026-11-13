package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/errs"
	"jumper/internal/safe"
)

// Crash safety from the process's side (task 126 requirements 1 and 2): a panic
// is recovered, written to <cache>/crash/ and turned into an exit reason or an
// error, and the layout is saved before the report.

// panicModel panics the first time its Update runs, which is inside the Bubble
// Tea goroutine.
type panicModel struct{}

func (panicModel) Init() tea.Cmd {
	return func() tea.Msg { return struct{}{} }
}

func (panicModel) Update(tea.Msg) (tea.Model, tea.Cmd) { panic("update exploded: nil map") }

func (panicModel) View() string { return "" }

// TestPanicInProgramRestoresTerminal is requirement 2, standalone half: a panic
// in Update is recovered by Bubble Tea, which leaves the alt screen and returns
// ErrProgramPanic, and app.recordProgramCrash turns that into a crash report.
func TestPanicInProgramRestoresTerminal(t *testing.T) {
	var out bytes.Buffer
	p := tea.NewProgram(panicModel{},
		tea.WithInput(strings.NewReader("")),
		tea.WithOutput(&out),
		tea.WithAltScreen(),
		tea.WithoutSignalHandler(),
	)

	_, err := p.Run()
	panicked, report := programEnded(err)
	if !panicked {
		t.Fatalf("program error = %v, want ErrProgramPanic", err)
	}
	if !errors.Is(report, tea.ErrProgramPanic) {
		t.Fatalf("report = %v, want the panic error", report)
	}
	if !strings.Contains(out.String(), "\x1b[?1049l") {
		t.Fatalf("the terminal was not restored (no leave-alt-screen in %q)", out.String())
	}

	dir := t.TempDir()
	cerr := recordProgramCrash(dir, nil, report)
	if got := errs.KindOf(cerr); got != errs.Internal {
		t.Fatalf("crash error kind = %q, want internal (%v)", got, cerr)
	}
	if !strings.Contains(cerr.Msg, "crash log:") {
		t.Errorf("crash error = %q, want the report's path in it", cerr.Msg)
	}
	path := crashFile(t, dir)
	body, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read crash file: %v", rerr)
	}
	for _, want := range []string{"panic:", "when:", "goroutine"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the crash report has no %q in it:\n%s", want, body)
		}
	}
}

// TestPanicInSessionSendsExitError is requirement 2, server half: the session
// saves its layout, writes the crash report, counts the panic, and detaches with
// reason "error" so the attached client's EXIT says error and not ended.
func TestPanicInSessionSendsExitError(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)

	reasons := make(chan string, 1)
	sess.SetOnDetach(func(reason string) { reasons <- reason })
	attach(t, sess, "web-01")

	cause := fmt.Errorf("%w: %w", tea.ErrProgramKilled, tea.ErrProgramPanic)
	panicked, report := programEnded(cause)
	if !panicked {
		t.Fatal("programEnded missed the panic in a killed-and-panicked program")
	}

	before := time.Now()
	sess.onProgramPanic(report)

	select {
	case reason := <-reasons:
		if reason != "error" {
			t.Fatalf("detach reason = %q, want error", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session did not detach after the panic")
	}

	if got := core.InternalErrors(); got != 1 {
		t.Fatalf("internal errors = %d, want the panic counted once", got)
	}

	path := crashFile(t, core.paths.CacheDir)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat crash file: %v", err)
	}
	if info.ModTime().Before(before.Add(-time.Second)) {
		t.Fatalf("crash file is older than the panic: %s", info.ModTime())
	}

	// The layout was saved before the report: the same name, and a file that
	// is not older than the crash.
	sessInfo, serr := os.Stat(sess.path)
	if serr != nil {
		t.Fatalf("the layout was not saved before the crash report: %v", serr)
	}
	if sessInfo.ModTime().After(info.ModTime().Add(time.Second)) {
		t.Fatalf("session file is %s newer than the crash file", sessInfo.ModTime().Sub(info.ModTime()))
	}
	if cerr := sess.CrashError(); errs.KindOf(cerr) != errs.Internal {
		t.Fatalf("CrashError = %v, want an internal error", cerr)
	}
}

// TestPanicInGoroutineIsFlashedAndCounted is requirement 1 seen from the UI: a
// goroutine started with the core's hook is logged, counted and flashed, and the
// session keeps running.
func TestPanicInGoroutineIsFlashedAndCounted(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)
	c := attach(t, sess, "web-01")

	safe.GoHook(core.log, "fan-out worker", core.panicHook(), func() {
		panic("worker exploded")
	})

	waitForText(t, c.out, "internal error in fan-out worker")

	if got := core.InternalErrors(); got != 1 {
		t.Fatalf("internal errors = %d, want 1", got)
	}
	if !hasWarning(core.Warnings(), "internal error in fan-out worker") {
		t.Fatalf("warnings = %v, want the flash text", core.Warnings())
	}
	select {
	case <-sess.Done():
		t.Fatal("a recovered panic ended the session")
	default:
	}
}

// TestPanicProgramWithoutPanicIsNormal is the other side of the switch: a
// killed program is a shutdown, not a crash, and an ordinary end says nothing.
func TestPanicProgramWithoutPanicIsNormal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		panicked bool
		report   bool
	}{
		{name: "nil", err: nil},
		{name: "killed", err: tea.ErrProgramKilled},
		{name: "panic", err: fmt.Errorf("%w: %w", tea.ErrProgramKilled, tea.ErrProgramPanic), panicked: true, report: true},
		{name: "other", err: errors.New("pipe closed"), report: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			panicked, report := programEnded(tc.err)
			if panicked != tc.panicked {
				t.Errorf("panicked = %v, want %v", panicked, tc.panicked)
			}
			if (report != nil) != tc.report {
				t.Errorf("report = %v, want one reported: %v", report, tc.report)
			}
		})
	}
}

// crashFile returns the one crash report in dir, failing the test when there is
// not exactly one.
func crashFile(t *testing.T, cacheDir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cacheDir, "crash"))
	if err != nil {
		t.Fatalf("read crash dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d crash reports, want 1", len(entries))
	}
	return filepath.Join(cacheDir, "crash", entries[0].Name())
}
