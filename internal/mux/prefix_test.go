package mux

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) adv(d time.Duration) { c.t = c.t.Add(d) }
func newTest() (*Prefix, *clock) {
	c := &clock{t: time.Unix(1000, 0)}
	return NewPrefix("ctrl+a", DefaultKeyTable(), c.now), c
}

func TestPrefixDefaultBindings(t *testing.T) {
	want := map[string]OpMsg{
		"%": {OpSplitH, ""}, "|": {OpSplitH, ""}, "\"": {OpSplitV, ""}, "-": {OpSplitV, ""},
		"left": {OpFocus, "left"}, "right": {OpFocus, "right"}, "up": {OpFocus, "up"}, "down": {OpFocus, "down"},
		"h": {OpFocus, "left"}, "j": {OpFocus, "down"}, "k": {OpFocus, "up"}, "l": {OpFocus, "right"},
		"o": {OpFocus, "next"}, ";": {OpFocus, "last"},
		"H": {OpResize, "left"}, "J": {OpResize, "down"}, "K": {OpResize, "up"}, "L": {OpResize, "right"},
		"z": {OpZoom, ""}, "x": {OpKillPane, ""}, "{": {OpSwap, "prev"}, "}": {OpSwap, "next"},
		" ": {OpLayoutNext, ""}, "c": {OpNewWindow, ""}, "&": {OpKillWindow, ""},
		"n": {OpNextWindow, ""}, "p": {OpPrevWindow, ""}, "w": {OpWindowList, ""},
		",": {OpRenameWindow, ""}, "$": {OpRenameSession, ""}, "s": {OpSessionList, ""},
		"S": {OpSSH, ""}, "[": {OpCopyMode, ""}, "B": {OpSync, ""}, ":": {OpCommand, ""},
		"d": {OpDetach, ""}, "?": {OpHelp, ""},
	}
	for d := '0'; d <= '9'; d++ {
		want[string(d)] = OpMsg{OpSelectWindow, string(d)}
	}
	if len(DefaultKeyTable()) != len(want) {
		t.Fatalf("table has %d keys, want %d", len(DefaultKeyTable()), len(want))
	}
	for key, w := range want {
		p, _ := newTest()
		if op, c := p.Feed("ctrl+a"); op != nil || !c || !p.Armed() {
			t.Fatalf("prefix: op=%v consumed=%v armed=%v", op, c, p.Armed())
		}
		op, c := p.Feed(key)
		if op == nil || *op != w || !c {
			t.Errorf("key %q: got %v consumed=%v, want %v", key, op, c, w)
		}
	}
}

func TestPrefixIdleNotConsumed(t *testing.T) {
	p, _ := newTest()
	for _, k := range []string{"a", "%", "esc", "H", "ctrl+b"} {
		if op, c := p.Feed(k); op != nil || c {
			t.Errorf("idle %q: op=%v consumed=%v", k, op, c)
		}
	}
	if p.Armed() {
		t.Error("armed")
	}
}

func TestPrefixDouble(t *testing.T) {
	p, _ := newTest()
	p.Feed("ctrl+a")
	op, c := p.Feed("ctrl+a")
	if op == nil || op.Op != OpSendPrefix || !c || p.Armed() {
		t.Fatalf("op=%v c=%v armed=%v", op, c, p.Armed())
	}
	if _, c := p.Feed("z"); c {
		t.Error("back to idle expected")
	}
}

func TestPrefixRepeatResize(t *testing.T) {
	p, c := newTest()
	p.Feed("ctrl+a")
	op, _ := p.Feed("H")
	if op == nil || op.Op != OpResize || op.Arg != "left" {
		t.Fatal(op)
	}
	if p.Armed() {
		t.Error("repeat is not armed")
	}
	c.adv(500 * time.Millisecond)
	op, cons := p.Feed("L")
	if op == nil || op.Arg != "right" || !cons {
		t.Fatal("repeat within window failed")
	}
	c.adv(500 * time.Millisecond) // window refreshed by previous resize
	if op, cons = p.Feed("J"); op == nil || op.Arg != "down" || !cons {
		t.Fatal("repeat window should refresh")
	}
	c.adv(601 * time.Millisecond)
	if op, cons = p.Feed("K"); op != nil || cons {
		t.Fatalf("after window: op=%v consumed=%v", op, cons)
	}
}

func TestPrefixRepeatLeavesOnOtherKey(t *testing.T) {
	p, _ := newTest()
	p.Feed("ctrl+a")
	p.Feed("H")
	if op, c := p.Feed("x"); op != nil || c {
		t.Fatalf("op=%v consumed=%v", op, c)
	}
	if op, c := p.Feed("H"); op != nil || c {
		t.Fatal("should be idle")
	}
}

func TestPrefixArmedTimeout(t *testing.T) {
	p, c := newTest()
	p.Feed("ctrl+a")
	c.adv(1999 * time.Millisecond)
	if !p.Armed() {
		t.Fatal("still armed")
	}
	c.adv(2 * time.Millisecond)
	if p.Armed() {
		t.Fatal("should have timed out")
	}
	if op, cons := p.Feed("z"); op != nil || cons {
		t.Fatalf("op=%v consumed=%v", op, cons)
	}
}

func TestPrefixEscAndUnknown(t *testing.T) {
	p, _ := newTest()
	p.Feed("ctrl+a")
	if op, c := p.Feed("esc"); op != nil || !c || p.Armed() {
		t.Fatal("esc")
	}
	p.Feed("ctrl+a")
	if op, c := p.Feed("Q"); op != nil || !c || p.Armed() {
		t.Fatal("unknown")
	}
}

func TestPrefixSetPrefix(t *testing.T) {
	p, _ := newTest()
	p.SetPrefix("ctrl+b")
	if _, c := p.Feed("ctrl+a"); c {
		t.Error("old prefix still active")
	}
	if _, c := p.Feed("ctrl+b"); !c || !p.Armed() {
		t.Error("new prefix inactive")
	}
}
