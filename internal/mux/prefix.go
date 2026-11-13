package mux

import "time"

const (
	armTimeout    = 2 * time.Second
	repeatTimeout = 600 * time.Millisecond
)

// KeyTable maps the key pressed after the prefix to an operation.
type KeyTable map[string]OpMsg

// DefaultKeyTable implements the key table of SPEC §21.2.
func DefaultKeyTable() KeyTable {
	t := KeyTable{
		"%":     {Op: OpSplitH},
		"|":     {Op: OpSplitH},
		"\"":    {Op: OpSplitV},
		"-":     {Op: OpSplitV},
		"left":  {Op: OpFocus, Arg: "left"},
		"right": {Op: OpFocus, Arg: "right"},
		"up":    {Op: OpFocus, Arg: "up"},
		"down":  {Op: OpFocus, Arg: "down"},
		"h":     {Op: OpFocus, Arg: "left"},
		"j":     {Op: OpFocus, Arg: "down"},
		"k":     {Op: OpFocus, Arg: "up"},
		"l":     {Op: OpFocus, Arg: "right"},
		"o":     {Op: OpFocus, Arg: "next"},
		";":     {Op: OpFocus, Arg: "last"},
		"H":     {Op: OpResize, Arg: "left"},
		"J":     {Op: OpResize, Arg: "down"},
		"K":     {Op: OpResize, Arg: "up"},
		"L":     {Op: OpResize, Arg: "right"},
		"z":     {Op: OpZoom},
		"x":     {Op: OpKillPane},
		"{":     {Op: OpSwap, Arg: "prev"},
		"}":     {Op: OpSwap, Arg: "next"},
		" ":     {Op: OpLayoutNext},
		"c":     {Op: OpNewWindow},
		"&":     {Op: OpKillWindow},
		"n":     {Op: OpNextWindow},
		"p":     {Op: OpPrevWindow},
		"w":     {Op: OpWindowList},
		",":     {Op: OpRenameWindow},
		"$":     {Op: OpRenameSession},
		"s":     {Op: OpSessionList},
		"S":     {Op: OpSSH},
		"[":     {Op: OpCopyMode},
		"B":     {Op: OpSync},
		":":     {Op: OpCommand},
		"d":     {Op: OpDetach},
		"?":     {Op: OpHelp},
	}
	for d := '0'; d <= '9'; d++ {
		t[string(d)] = OpMsg{Op: OpSelectWindow, Arg: string(d)}
	}
	return t
}

type prefixState int

const (
	stIdle prefixState = iota
	stArmed
	stRepeat
)

// Prefix is the tmux-style prefix key state machine. It is pure: time comes
// from the injected clock and nothing runs in the background.
type Prefix struct {
	prefix string
	table  KeyTable
	now    func() time.Time
	state  prefixState
	until  time.Time // when the armed or repeat state expires
}

// NewPrefix returns an idle state machine.
func NewPrefix(prefixKey string, table KeyTable, now func() time.Time) *Prefix {
	if now == nil {
		now = time.Now
	}
	return &Prefix{prefix: prefixKey, table: table, now: now}
}

// expire drops the armed or repeat state once its deadline has passed.
func (p *Prefix) expire() {
	if p.state != stIdle && p.now().After(p.until) {
		p.state = stIdle
	}
}

// Feed processes one key. consumed reports whether the key was taken by the
// machine (false means the caller should pass it on to the focused content).
func (p *Prefix) Feed(key string) (op *OpMsg, consumed bool) {
	p.expire()
	switch p.state {
	case stIdle:
		if key == p.prefix {
			p.state = stArmed
			p.until = p.now().Add(armTimeout)
			return nil, true
		}
		return nil, false
	case stRepeat:
		if o, ok := p.table[key]; ok && o.Op == OpResize {
			p.until = p.now().Add(repeatTimeout)
			return &o, true
		}
		p.state = stIdle
		return nil, false
	}
	// armed
	switch {
	case key == p.prefix:
		p.state = stIdle
		return &OpMsg{Op: OpSendPrefix}, true
	case key == "esc":
		p.state = stIdle
		return nil, true
	}
	o, ok := p.table[key]
	if !ok {
		p.state = stIdle
		return nil, true
	}
	if o.Op == OpResize {
		p.state = stRepeat
		p.until = p.now().Add(repeatTimeout)
	} else {
		p.state = stIdle
	}
	return &o, true
}

// Armed reports whether the prefix was pressed and a key is awaited.
func (p *Prefix) Armed() bool {
	p.expire()
	return p.state == stArmed
}

// SetPrefix changes the prefix key and returns the machine to idle.
func (p *Prefix) SetPrefix(key string) {
	p.prefix = key
	p.state = stIdle
}
