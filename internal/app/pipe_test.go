package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumpr/internal/tui/theme"
)

// syncBuffer is the frame sink for a run with no TTY. Bubble Tea writes frames
// from its renderer goroutine while the test polls, so a plain bytes.Buffer
// would be a data race under -race.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

// paneTitleRE matches the label the multiplexer draws in a pane's top border
// (" 1:host (1512) "), so counting it counts panes.
var paneTitleRE = regexp.MustCompile(`[0-9]+:[a-z]+[^()\n]*\([0-9]+\)`)

// frameLines splits output into the lines each frame painted. Every frame starts
// with a cursor-home escape; lines the renderer could skip are left out, so a
// frame is a delta and never a screenshot of the whole screen.
func frameLines(out string) [][]string {
	var frames [][]string
	for _, f := range strings.Split(out, ansi.CursorHomePosition) {
		raw := strings.Split(ansi.Strip(f), "\n")
		lines := make([]string, len(raw))
		for i, l := range raw {
			// The renderer frames lines with \r\n and may start a frame with a
			// \r; neither is a cell.
			lines[i] = strings.Trim(l, "\r")
		}
		frames = append(frames, lines)
	}
	return frames
}

// waitForText waits until the output contains want once ANSI is stripped.
func waitForText(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(ansi.Strip(out.String()), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output never contained %q\n%s", want, ansi.Strip(out.String()))
}

// waitForFrame waits until one frame written after byte offset off satisfies
// cond. Frames arrive about 60 times a second, so nothing may be asserted right
// after a write to the input pipe.
func waitForFrame(t *testing.T, out *syncBuffer, off int, what string, cond func([]string) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		text := out.String()
		if off <= len(text) {
			for _, lines := range frameLines(text[off:]) {
				if cond(lines) {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no frame drawn after byte %d with %s", off, what)
}

// typeKeys writes s one byte at a time. Bubble Tea decodes whatever one Read
// returns as a single key message, so a whole line written in one go ("...\r")
// would arrive as one impossible key instead of keystrokes. An io.Pipe delivers
// one Write per Read, so a byte per Write is one key per message.
func typeKeys(t *testing.T, w io.Writer, s string) {
	t.Helper()
	for _, b := range []byte(s) {
		if _, err := w.Write([]byte{b}); err != nil {
			t.Fatalf("write %q: %v", string(b), err)
		}
	}
}

// TestRunOverPipes runs the whole app with pipes instead of a terminal: input is
// a pipe, output a goroutine-safe buffer, the size is injected and resizes come
// from a channel. It proves the detach path (M5) is only transport, and that
// canceling the context stops the program and every goroutine writing to it.
func TestRunOverPipes(t *testing.T) {
	// A profile with no colour keeps the assertions readable; "auto" in the
	// config does not override an explicit profile.
	theme.SetProfile("none")

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cfgDir := filepath.Join(dir, "config")
	for _, d := range []string{home, cfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	out := &syncBuffer{}
	pr, pw := io.Pipe()
	defer pw.Close()
	resizes := make(chan Size, 1)
	ready := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, Options{
			In:        pr,
			Out:       out,
			Demo:      true,
			Home:      home,
			ConfigDir: cfgDir,
			Size:      &Size{Cols: 120, Rows: 36},
			Resizes:   resizes,
			Ready:     ready,
		})
	}()

	select {
	case <-ready:
	case err := <-errc:
		t.Fatalf("Run returned before it was ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run never signalled ready")
	}

	// The demo fleet renders although no byte ever came from a terminal. The
	// list is sorted by load, so one host is opened explicitly instead of
	// relying on which row happens to be on screen.
	typeKeys(t, pw, ":host host=web-ewr-001\r")
	waitForText(t, out, "web-ewr-001")

	// A command typed into the pipe reaches the view stack. Enter in raw
	// terminal input is \r, never \n.
	typeKeys(t, pw, ":alerts\r")
	waitForText(t, out, "HighErrorRate")

	// The prefix byte (a raw ctrl+a is 0x01) plus "%" splits the window, so one
	// frame has to show two pane titles.
	off := out.Len()
	if _, err := pw.Write([]byte{0x01}); err != nil {
		t.Fatalf("write prefix: %v", err)
	}
	typeKeys(t, pw, "%")
	waitForFrame(t, out, off, "two pane titles", func(lines []string) bool {
		n := 0
		for _, l := range lines {
			n += len(paneTitleRE.FindAllString(l, -1))
		}
		return n >= 2
	})

	// A resize offered on the channel redraws: the status line is fitted to the
	// full width, so a later frame has an 80-cell one.
	off = out.Len()
	resizes <- Size{Cols: 80, Rows: 24}
	waitForFrame(t, out, off, "an 80-column status line", func(lines []string) bool {
		for _, l := range lines {
			if strings.HasPrefix(l, "jumpr") && ansi.StringWidth(l) == 80 {
				return true
			}
		}
		return false
	})

	// Canceling the context ends the program, and nothing writes to the buffer
	// afterwards.
	cancel()
	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, tea.ErrProgramKilled) {
			t.Fatalf("Run after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2 s of the cancel")
	}
	settled := out.Len()
	time.Sleep(200 * time.Millisecond)
	if now := out.Len(); now != settled {
		t.Fatalf("output grew from %d to %d bytes after Run returned", settled, now)
	}
}
