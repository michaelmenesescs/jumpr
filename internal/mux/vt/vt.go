// Package vt wraps a terminal emulator behind jumper's own small interface so
// the implementation can be swapped without touching the mux.
//
// Nothing from github.com/charmbracelet/x/vt or ultraviolet may appear in this
// package's exported API.
package vt

import (
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/x/ansi"
	vtx "github.com/charmbracelet/x/vt"

	"jumper/internal/render"
)

// Emulator turns a terminal byte stream into a screen of cells.
type Emulator interface {
	io.Writer // feed remote output
	Resize(cols, rows int)
	Render() string // rows joined by "\n", with SGR colours, exactly rows x cols
	Cursor() (x, y int, visible bool)
	ScrollbackLines() []string // plain text, oldest first
	// Responses returns bytes the emulator wants to send back to the remote side (DA, cursor reports).
	// It must be drained continuously by the owner.
	Responses() io.Reader
	Close() error
}

const reset = "\x1b[0m"

// emulator is the x/vt-backed Emulator.
type emulator struct {
	emu *vtx.SafeEmulator

	mu         sync.RWMutex
	cols, rows int
	closed     atomic.Bool
}

// New returns an Emulator of cols x rows with up to scrollback lines of
// scrollback history. Non-positive sizes fall back to 80x24; a non-positive
// scrollback leaves the library default in place.
func New(cols, rows, scrollback int) Emulator {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	e := vtx.NewSafeEmulator(cols, rows)
	if scrollback > 0 {
		e.SetScrollbackSize(scrollback)
	}
	return &emulator{emu: e, cols: cols, rows: rows}
}

// Write feeds remote output to the emulator.
func (e *emulator) Write(p []byte) (int, error) {
	if e.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	return e.emu.Write(p)
}

// Resize changes the emulated screen size.
func (e *emulator) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	e.mu.Lock()
	e.cols, e.rows = cols, rows
	e.mu.Unlock()
	e.emu.Resize(cols, rows)
}

// Render returns the visible screen as exactly rows lines of exactly cols
// cells, joined by "\n", with SGR colours preserved.
func (e *emulator) Render() string {
	e.mu.RLock()
	cols, rows := e.cols, e.rows
	e.mu.RUnlock()

	lines := strings.Split(e.emu.Render(), "\n")
	if len(lines) > rows {
		lines = lines[:rows]
	}
	out := make([]string, 0, rows)
	for _, line := range lines {
		out = append(out, fitRow(line, cols))
	}
	blank := strings.Repeat(" ", cols)
	for len(out) < rows {
		out = append(out, blank)
	}
	return strings.Join(out, "\n")
}

// fitRow makes one rendered row exactly cols cells. Padding is written after
// any SGR reset the line already carries so a background colour never bleeds
// into the pane border.
func fitRow(line string, cols int) string {
	if w := render.Width(line); w > cols {
		line = ansi.Truncate(line, cols, "")
		if strings.ContainsRune(line, '\x1b') && !strings.HasSuffix(line, reset) {
			line += reset
		}
	}
	if pad := cols - render.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return line
}

// Cursor returns the cursor position and whether it is visible.
func (e *emulator) Cursor() (x, y int, visible bool) {
	p := e.emu.CursorPosition()
	return p.X, p.Y, !e.emu.CursorHidden()
}

// ScrollbackLines returns the scrollback buffer as plain text lines, oldest
// first, with trailing spaces trimmed. The visible screen is not included.
func (e *emulator) ScrollbackLines() []string {
	n := e.emu.ScrollbackLen()
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for y := 0; y < n; y++ {
		var b strings.Builder
		for x := 0; ; x++ {
			c := e.emu.ScrollbackCellAt(x, y)
			if c == nil {
				break
			}
			b.WriteString(c.Content)
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return out
}

// Responses returns the stream of replies the emulator wants sent back to the
// remote side. The owner must drain it, or Write can block.
func (e *emulator) Responses() io.Reader {
	return e.emu
}

// Close releases the emulator and ends Responses with io.EOF, which unblocks a
// drain goroutine sitting in Read.
//
// x/vt's own Close writes a flag that its Read reads without a lock, so calling
// it while the mandatory drain goroutine is inside Read is a data race. Closing
// the response pipe (the only thing after which Read returns) has the same
// effect and is race-free; Write is refused here once closed.
func (e *emulator) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
	if c, ok := e.emu.InputPipe().(interface{ CloseWithError(error) error }); ok {
		return c.CloseWithError(io.EOF)
	}
	return e.emu.Close()
}

// PlainScreen returns the visible screen as plain text lines with ANSI
// sequences stripped, one entry per row.
func PlainScreen(e Emulator) []string {
	if e == nil {
		return nil
	}
	return strings.Split(ansi.Strip(e.Render()), "\n")
}
