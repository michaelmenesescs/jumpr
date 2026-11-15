// Package termpane is the mux.Content that hosts an interactive SSH shell: a
// local terminal emulator fed by the remote PTY, keystrokes encoded back to it,
// and a small connection state machine around the two.
package termpane

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumpr/internal/errs"
	"jumpr/internal/mux"
	"jumpr/internal/mux/vt"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
)

const (
	// connectTimeout bounds one OpenShell call.
	connectTimeout = 15 * time.Second
	// frameInterval is the shortest gap between two repaint notifications.
	frameInterval = 16 * time.Millisecond
	// defaultScrollback is the scrollback kept when Options names none.
	defaultScrollback = 10000
	// defaultTerm is the TERM value sent to the remote PTY.
	defaultTerm = "xterm-256color"
	// initialCols and initialRows are the interior size used when the pane is
	// connected before its first View call.
	initialCols = 80
	initialRows = 24
	// readBuf is the read buffer size of the reader goroutine.
	readBuf = 32 << 10
	// writeQueue is how many key batches may wait for the writer goroutine.
	writeQueue = 256
)

// Options are the pane's settings. Zero values mean the defaults.
type Options struct {
	Scrollback int              // lines of scrollback history
	Term       string           // TERM sent to the remote PTY
	Now        func() time.Time // clock, for the repaint throttle
}

// state is the connection state of the pane.
type state int

const (
	stateConnecting state = iota
	stateConnected
	stateClosed
	stateFailed
)

// Messages produced and consumed by this pane. They never leave it: every
// command is wrapped in mux.ForPane, so the mux routes them straight back.
type connectedMsg struct {
	gen   int
	shell transport.Shell
}

type failedMsg struct {
	gen int
	err error
}

type closedMsg struct {
	gen    int
	reason string
}

type dirtyMsg struct{ gen int }

// conn is one shell connection and everything that belongs to it. A reconnect
// builds a new conn; messages from an old one are dropped by generation.
type conn struct {
	gen    int
	shell  transport.Shell
	emu    vt.Emulator
	ctx    context.Context
	cancel context.CancelFunc
	writes chan []byte
	dirty  chan struct{}
	exits  chan tea.Msg
	wg     sync.WaitGroup
}

// OpenFunc opens one interactive shell. A pane does not care where the shell
// runs: an SSH host and a Kubernetes pod exec are two implementations.
type OpenFunc func(ctx context.Context, req transport.PTYRequest) (transport.Shell, error)

// Pane is a terminal pane: mux.Content over one interactive shell.
type Pane struct {
	id    mux.PaneID
	host  resource.HostID
	open  OpenFunc
	title string
	label string // ContentState.Host; "" means string(host)
	opts  Options

	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	conn       *conn
	st         state
	gen        int
	reason     string
	restored   bool
	focused    bool
	protected  bool
	closed     bool
	cols, rows int

	running atomic.Int32
}

var _ mux.Content = (*Pane)(nil)

// New builds a pane that connects to host through opener: the SSH constructor,
// a thin wrapper over NewShell. ctx bounds every connection the pane makes;
// Close cancels the context it derives from it.
func New(id mux.PaneID, ctx context.Context, opener transport.ShellOpener, host resource.HostID, o Options) *Pane {
	var open OpenFunc
	if opener != nil {
		open = func(ctx context.Context, req transport.PTYRequest) (transport.Shell, error) {
			return opener.OpenShell(ctx, host, req)
		}
	}
	p := newPane(id, ctx, open, o)
	p.host = host
	return p
}

// NewShell builds a pane over any shell factory and shows title in its border.
// host is what session persistence records for the pane; empty means the pane
// has no reconnect target of its own.
func NewShell(id mux.PaneID, ctx context.Context, open OpenFunc, title, host string, o Options) *Pane {
	p := newPane(id, ctx, open, o)
	p.title = title
	p.label = host
	return p
}

// NewPod builds a Kubernetes pod exec pane. The content state names the pod the
// way a restart needs it: k8s:<ctx>/<ns>/<pod>/<container>.
func NewPod(id mux.PaneID, ctx context.Context, open OpenFunc, kubeCtx, ns, pod, container string, o Options) *Pane {
	title := pod
	if ns != "" {
		title = ns + "/" + pod
	}
	if container != "" && container != pod {
		title += ":" + container
	}
	host := "k8s:" + kubeCtx + "/" + ns + "/" + pod + "/" + container
	return NewShell(id, ctx, open, "k8s "+title, host, o)
}

// newPane applies the option defaults and derives the pane's context.
func newPane(id mux.PaneID, ctx context.Context, open OpenFunc, o Options) *Pane {
	if ctx == nil {
		ctx = context.Background()
	}
	if o.Scrollback <= 0 {
		o.Scrollback = defaultScrollback
	}
	if o.Term == "" {
		o.Term = defaultTerm
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	pctx, cancel := context.WithCancel(ctx)
	return &Pane{
		id:     id,
		open:   open,
		opts:   o,
		ctx:    pctx,
		cancel: cancel,
		st:     stateConnecting,
		cols:   initialCols,
		rows:   initialRows,
	}
}

// MarkRestored makes a term pane come back from a session file without
// reconnecting: it starts closed with the reason "restored".
func (p *Pane) MarkRestored() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.restored = true
	p.st = stateClosed
	p.reason = "restored"
}

// Init starts the connection. A restored or closed pane does not reconnect by
// itself.
func (p *Pane) Init() tea.Cmd {
	p.mu.Lock()
	skip := p.restored || p.closed
	gen := p.gen
	p.mu.Unlock()
	if skip {
		return nil
	}
	return mux.ForPane(p.id, p.connect(gen))
}

// Update handles connection messages, the repaint tick and keys.
func (p *Pane) Update(msg tea.Msg) (mux.Content, tea.Cmd) {
	var cmd tea.Cmd
	switch m := msg.(type) {
	case connectedMsg:
		cmd = p.connected(m)
	case failedMsg:
		p.failed(m)
	case closedMsg:
		p.remoteExit(m)
	case dirtyMsg:
		p.mu.Lock()
		ok := !p.closed && p.st == stateConnected && m.gen == p.gen
		p.mu.Unlock()
		if ok {
			cmd = p.waitDirty(m.gen)
		}
	case tea.KeyMsg:
		cmd = p.key(m)
	}
	return p, mux.ForPane(p.id, cmd)
}

// connected installs a fresh shell and starts its goroutines.
func (p *Pane) connected(m connectedMsg) tea.Cmd {
	p.mu.Lock()
	if p.closed || m.gen != p.gen || p.st != stateConnecting {
		p.mu.Unlock()
		_ = m.shell.Close()
		return nil
	}
	c := &conn{
		gen:    m.gen,
		shell:  m.shell,
		emu:    vt.New(p.cols, p.rows, p.opts.Scrollback),
		writes: make(chan []byte, writeQueue),
		dirty:  make(chan struct{}, 1),
		exits:  make(chan tea.Msg, 1),
	}
	c.ctx, c.cancel = context.WithCancel(p.ctx)
	p.conn = c
	p.st = stateConnected
	p.reason = ""
	p.mu.Unlock()

	p.start(c)
	return tea.Batch(p.waitDirty(c.gen), p.waitExit(c.gen))
}

// failed marks the pane failed with the error kind as its reason.
func (p *Pane) failed(m failedMsg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || m.gen != p.gen {
		return
	}
	p.st = stateFailed
	p.reason = reasonFor(m.err)
}

// remoteExit records that the remote side ended the session. The emulator is
// kept so the last screen can be shown dimmed.
func (p *Pane) remoteExit(m closedMsg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || m.gen != p.gen || p.st != stateConnected {
		return
	}
	p.st = stateClosed
	p.reason = m.reason
	if p.conn != nil {
		p.conn.cancel()
	}
}

// key encodes one key press for the remote side, or answers the reconnect and
// close keys of a disconnected pane.
func (p *Pane) key(k tea.KeyMsg) tea.Cmd {
	p.mu.Lock()
	st := p.st
	c := p.conn
	p.mu.Unlock()
	switch st {
	case stateConnected:
		if c == nil {
			return nil
		}
		b := Encode(k)
		if len(b) == 0 {
			return nil
		}
		// A write to a stalled connection can block; the writer goroutine owns
		// the blocking call, Update only queues the bytes.
		select {
		case c.writes <- b:
		case <-c.ctx.Done():
		default:
		}
		return nil
	case stateConnecting:
		return nil
	}
	switch k.String() {
	case "r":
		return p.reconnect()
	case "x":
		return func() tea.Msg { return mux.OpMsg{Op: mux.OpKillPane} }
	}
	return nil
}

// reconnect drops the dead connection and opens a new one.
func (p *Pane) reconnect() tea.Cmd {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	old := p.conn
	p.conn = nil
	p.gen++
	gen := p.gen
	p.st = stateConnecting
	p.reason = ""
	p.mu.Unlock()

	if old != nil {
		old.cancel()
		_ = old.shell.Close()
		_ = old.emu.Close()
	}
	return p.connect(gen)
}

// connect is the tea.Cmd that opens the shell.
func (p *Pane) connect(gen int) tea.Cmd {
	return func() tea.Msg {
		if p.open == nil {
			return failedMsg{gen: gen, err: errs.New(errs.Config, "termpane.OpenShell", "no shell opener")}
		}
		p.mu.Lock()
		cols, rows := p.cols, p.rows
		open := p.open
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(p.ctx, connectTimeout)
		defer cancel()
		sh, err := open(ctx, transport.PTYRequest{
			Term: p.opts.Term,
			Cols: cols,
			Rows: rows,
		})
		if err != nil {
			return failedMsg{gen: gen, err: err}
		}
		return connectedMsg{gen: gen, shell: sh}
	}
}

// waitDirty blocks until the reader reports output, then asks for a repaint.
func (p *Pane) waitDirty(gen int) tea.Cmd {
	return func() tea.Msg {
		p.mu.Lock()
		c := p.conn
		p.mu.Unlock()
		if c == nil || c.gen != gen {
			return nil
		}
		select {
		case <-c.dirty:
			return dirtyMsg{gen: gen}
		case <-c.ctx.Done():
			return nil
		}
	}
}

// waitExit blocks until the reader reached the end of the stream.
func (p *Pane) waitExit(gen int) tea.Cmd {
	return func() tea.Msg {
		p.mu.Lock()
		c := p.conn
		p.mu.Unlock()
		if c == nil || c.gen != gen {
			return nil
		}
		select {
		case msg := <-c.exits:
			return msg
		case <-c.ctx.Done():
			return nil
		}
	}
}

// start runs the three goroutines of a connection: reader, responder and
// writer. Close stops all of them.
func (p *Pane) start(c *conn) {
	c.wg.Add(3)
	p.running.Add(3)
	go p.readLoop(c)
	go p.respondLoop(c)
	go p.writeLoop(c)
}

// readLoop feeds remote output to the emulator and notifies the UI, coalescing
// repaints to at most one per frameInterval.
func (p *Pane) readLoop(c *conn) {
	defer p.done(c)
	buf := make([]byte, readBuf)
	var last time.Time
	for {
		n, err := c.shell.Read(buf)
		if n > 0 {
			_, _ = c.emu.Write(buf[:n])
			now := p.opts.Now()
			if !last.IsZero() {
				if d := frameInterval - now.Sub(last); d > 0 {
					t := time.NewTimer(d)
					select {
					case <-t.C:
					case <-c.ctx.Done():
						t.Stop()
						return
					}
				}
			}
			last = p.opts.Now()
			select {
			case c.dirty <- struct{}{}:
			default:
			}
		}
		if err != nil {
			p.finish(c, err)
			return
		}
		if c.ctx.Err() != nil {
			return
		}
	}
}

// respondLoop drains the emulator's replies, so programs like vim that ask the
// terminal a question at startup do not hang.
func (p *Pane) respondLoop(c *conn) {
	defer p.done(c)
	_, _ = io.Copy(c.shell, c.emu.Responses())
}

// writeLoop sends queued keystrokes to the remote side. It is the only
// goroutine that writes, so a stalled connection can never block the UI.
func (p *Pane) writeLoop(c *conn) {
	defer p.done(c)
	for {
		select {
		case <-c.ctx.Done():
			return
		case b := <-c.writes:
			if _, err := c.shell.Write(b); err != nil {
				return
			}
		}
	}
}

// done marks one connection goroutine as finished.
func (p *Pane) done(c *conn) {
	c.wg.Done()
	p.running.Add(-1)
}

// finish reports the end of the stream unless the pane is being torn down.
func (p *Pane) finish(c *conn, err error) {
	p.mu.Lock()
	gone := p.closed
	p.mu.Unlock()
	if gone || c.ctx.Err() != nil {
		return
	}
	reason := "remote exit"
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		reason = reasonFor(err)
	}
	select {
	case c.exits <- closedMsg{gen: c.gen, reason: reason}:
	default:
	}
}

// View draws the emulated screen, the cursor and the connection status,
// exactly w by h cells.
func (p *Pane) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	p.mu.Lock()
	c := p.conn
	st := p.st
	focused := p.focused
	reason := p.reason
	closed := p.closed
	resized := w != p.cols || h != p.rows
	p.cols, p.rows = w, h
	p.mu.Unlock()
	if closed {
		return render.Fit("", w, h)
	}

	var lines []string
	if c != nil {
		if resized {
			c.emu.Resize(w, h)
		}
		if resized && st == stateConnected {
			_ = c.shell.Resize(w, h)
		}
		lines = strings.Split(c.emu.Render(), "\n")
		if st == stateConnected && focused {
			if x, y, visible := c.emu.Cursor(); visible && y >= 0 && y < len(lines) {
				lines[y] = reverseCell(lines[y], x, w)
			}
		}
	}
	switch st {
	case stateConnecting:
		lines = statusLine(lines, "connecting to "+p.shortHost()+"…", w, h)
	case stateClosed, stateFailed:
		for i := range lines {
			lines[i] = dim(lines[i])
		}
		lines = statusLine(lines, "[disconnected: "+reason+" — r reconnect · x close]", w, h)
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// Title is the pane border title. A pane whose host policy protects says so
// (SPEC §11.1).
func (p *Pane) Title() string {
	p.mu.Lock()
	title := p.title
	protected := p.protected
	p.mu.Unlock()
	if title == "" {
		title = "ssh " + p.shortHost()
	}
	if protected {
		title += " (protected)"
	}
	return title
}

// Host is the host the pane's shell runs on.
func (p *Pane) Host() resource.HostID { return p.host }

// SetProtected marks the pane's host as protected by policy, which the border
// title then shows.
func (p *Pane) SetProtected(on bool) {
	p.mu.Lock()
	p.protected = on
	p.mu.Unlock()
}

// Protected reports whether the pane was marked as a protected host.
func (p *Pane) Protected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.protected
}

// SendLine writes one line and a carriage return to the remote shell, the way
// a broadcast line reaches it. The bytes go through the writer goroutine, so a
// stalled connection cannot block the caller.
func (p *Pane) SendLine(line string) error {
	p.mu.Lock()
	st := p.st
	c := p.conn
	closed := p.closed
	p.mu.Unlock()
	if closed || st != stateConnected || c == nil {
		return errs.New(errs.NotAvailable, "termpane.SendLine", "pane is not connected").WithHost(string(p.host))
	}
	b := append([]byte(line), '\r')
	select {
	case c.writes <- b:
		return nil
	case <-c.ctx.Done():
		return errs.Wrap(errs.Canceled, "termpane.SendLine", c.ctx.Err()).WithHost(string(p.host))
	default:
		return errs.New(errs.Timeout, "termpane.SendLine", "write queue is full").WithHost(string(p.host))
	}
}

// SetFocus records whether the pane has the focus, which decides whether the
// cursor is drawn.
func (p *Pane) SetFocus(focused bool) {
	p.mu.Lock()
	p.focused = focused
	p.mu.Unlock()
}

// RawKeys is true while connected: every key except the prefix belongs to the
// remote side.
func (p *Pane) RawKeys() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st == stateConnected && !p.closed
}

// Busy is true while a shell is connected.
func (p *Pane) Busy() bool { return p.RawKeys() }

// CopyLines is the scrollback followed by the visible screen, plain text.
func (p *Pane) CopyLines() []string {
	p.mu.Lock()
	c := p.conn
	p.mu.Unlock()
	if c == nil {
		return nil
	}
	lines := c.emu.ScrollbackLines()
	lines = append(lines, vt.PlainScreen(c.emu)...)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// State is what session persistence records for this pane.
func (p *Pane) State() mux.ContentState {
	p.mu.Lock()
	label := p.label
	p.mu.Unlock()
	if label == "" {
		label = string(p.host)
	}
	return mux.ContentState{Type: "term", Host: label}
}

// Close releases the shell, the emulator and every goroutine. It is safe to
// call more than once.
func (p *Pane) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	if p.reason == "" {
		p.reason = "closed"
	}
	p.st = stateClosed
	c := p.conn
	p.mu.Unlock()

	p.cancel()
	if c != nil {
		c.cancel()
		_ = c.shell.Close()
		_ = c.emu.Close()
	}
	return nil
}

// shortHost is the host name without its domain.
func (p *Pane) shortHost() string {
	name := string(p.host)
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// reasonFor labels an error with its kind, which is what the pane shows.
func reasonFor(err error) string {
	if kind := errs.KindOf(err); kind != "" {
		return string(kind)
	}
	return "failed"
}

// reverseCell draws the cursor by reversing the cell at x.
func reverseCell(line string, x, w int) string {
	if x < 0 || x >= w {
		return line
	}
	cell := ansi.Cut(line, x, x+1)
	if cell == "" {
		cell = " "
	}
	return ansi.Cut(line, 0, x) + "\x1b[7m" + cell + "\x1b[0m" + ansi.Cut(line, x+1, w)
}

// dim renders text dimmed.
func dim(text string) string { return "\x1b[2m" + text + "\x1b[0m" }

// statusLine puts text on the last line of the pane, exactly w cells wide.
func statusLine(lines []string, text string, w, h int) []string {
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	lines[h-1] = dim(render.PadRight(text, w))
	return lines
}

// editedMark is what a line the tracker lost track of is recorded as. It is
// deliberately not the line, wrong records being worse than an honest "unknown"
// (SPEC §11.1, task 064 pitfalls).
const editedMark = "…(edited)"

// LineTracker reconstructs the line typed in a raw synchronized session, from
// the key presses the panes received. It is best effort by design: a key it
// cannot follow marks the line edited instead of guessing what the shell did.
type LineTracker struct {
	runes  []rune
	edited bool
}

// Reset forgets the line and the edited mark.
func (t *LineTracker) Reset() {
	t.runes = t.runes[:0]
	t.edited = false
}

// Line is the reconstructed line, or editedMark when the tracker lost track.
func (t *LineTracker) Line() string {
	if t.edited {
		return editedMark
	}
	return string(t.runes)
}

// Edited reports whether the tracker lost track of the line.
func (t *LineTracker) Edited() bool { return t.edited }

// Key applies one key press and reports the line it completed. enter is true
// when the key was enter; line is then the reconstructed line and the tracker
// starts over. line is "" for every other key.
func (t *LineTracker) Key(k tea.KeyMsg) (line string, enter bool) {
	if k.Paste {
		t.runes = append(t.runes, k.Runes...)
		return "", false
	}
	switch k.Type {
	case tea.KeyRunes:
		if k.Alt {
			t.edited = true
			return "", false
		}
		t.runes = append(t.runes, k.Runes...)
	case tea.KeySpace:
		t.runes = append(t.runes, ' ')
	case tea.KeyEnter:
		line := t.Line()
		t.Reset()
		return line, true
	case tea.KeyBackspace:
		if len(t.runes) > 0 {
			t.runes = t.runes[:len(t.runes)-1]
		}
	case tea.KeyCtrlU:
		t.runes = t.runes[:0]
	case tea.KeyCtrlW:
		t.dropWord()
	default:
		// An escape sequence, ctrl+r, a function key: the line changed in a
		// way this tracker cannot follow.
		t.edited = true
	}
	return "", false
}

// dropWord deletes the word before the cursor, the way ctrl+w does in a shell:
// trailing spaces first, then the word itself.
func (t *LineTracker) dropWord() {
	i := len(t.runes)
	for i > 0 && t.runes[i-1] == ' ' {
		i--
	}
	for i > 0 && t.runes[i-1] != ' ' {
		i--
	}
	t.runes = t.runes[:i]
}
