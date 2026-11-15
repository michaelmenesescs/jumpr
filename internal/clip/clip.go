// Package clip puts text on the user's clipboard. It never touches the
// process's terminal itself: pbcopy runs through an injected function, OSC 52
// writes to an injected writer, so tests, the server and the app all share one
// code path (BUILD-PLAN §6.1 rule 6).
package clip

import (
	"encoding/base64"
	"fmt"
	"io"

	"jumpr/internal/errs"
)

// maxOSC52 is the largest base64 payload sent as one OSC 52 sequence. tmux and
// several terminals drop or truncate anything bigger.
const maxOSC52 = 74000

// Clipboard is one way of putting text on the user's clipboard.
type Clipboard interface {
	Copy(text string) error
}

// pbcopy is the macOS clipboard, driven by an injected command runner.
type pbcopy struct {
	run func(name string, stdin []byte) error
}

// NewPbcopy returns a Clipboard that feeds text to pbcopy through run. run is
// injected so this package never builds a process; app supplies one built on
// os/exec with a 2 s timeout.
func NewPbcopy(run func(name string, stdin []byte) error) Clipboard {
	return &pbcopy{run: run}
}

// Copy runs pbcopy with text on stdin.
func (p *pbcopy) Copy(text string) error {
	if p.run == nil {
		return errs.New(errs.Internal, "clip.pbcopy", "no command runner")
	}
	if err := p.run("pbcopy", []byte(text)); err != nil {
		return errs.Wrap(errs.Internal, "clip.pbcopy", err)
	}
	return nil
}

// osc52 writes the selection as an OSC 52 escape sequence.
type osc52 struct {
	w    io.Writer
	tmux bool
}

// NewOSC52 returns a Clipboard that writes ESC ] 52 ; c ; <base64> BEL to w.
// When inTmux is true the sequence is wrapped in the tmux passthrough form so
// the outer tmux forwards it (SPEC §21.3, §21.4).
func NewOSC52(w io.Writer, inTmux bool) Clipboard {
	return &osc52{w: w, tmux: inTmux}
}

// Copy writes the encoded selection. More than 74,000 bytes of base64 is an
// error rather than a silently dropped paste.
func (o *osc52) Copy(text string) error {
	if o.w == nil {
		return errs.New(errs.Internal, "clip.OSC52", "no writer")
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	if len(b64) > maxOSC52 {
		return errs.New(errs.Protocol, "clip.OSC52",
			fmt.Sprintf("selection is %d base64 bytes, over the %d byte limit", len(b64), maxOSC52))
	}
	seq := "\x1b]52;c;" + b64 + "\a"
	if o.tmux {
		seq = "\x1bPtmux;\x1b" + seq + "\x1b\\"
	}
	if _, err := io.WriteString(o.w, seq); err != nil {
		return errs.Wrap(errs.Internal, "clip.OSC52", err)
	}
	return nil
}

// multi tries each clipboard in turn.
type multi []Clipboard

// Multi returns a Clipboard that tries cs in order and stops at the first
// success. A nil entry is skipped.
func Multi(cs ...Clipboard) Clipboard {
	m := make(multi, 0, len(cs))
	for _, c := range cs {
		if c != nil {
			m = append(m, c)
		}
	}
	return m
}

// Copy returns nil as soon as one clipboard accepts text; otherwise the last
// error.
func (m multi) Copy(text string) error {
	var last error
	for _, c := range m {
		err := c.Copy(text)
		if err == nil {
			return nil
		}
		last = err
	}
	if last != nil {
		return last
	}
	return errs.New(errs.Internal, "clip.Multi", "no clipboard available")
}
