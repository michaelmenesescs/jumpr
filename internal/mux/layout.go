package mux

import (
	"fmt"
	"math"

	"jumper/internal/errs"
)

const (
	minPaneW = 10
	minPaneH = 3
	minRatio = 0.1
	maxRatio = 0.9
)

// node is a leaf (a == nil) or a split.
type node struct {
	pane       PaneID
	horizontal bool    // children side by side
	ratio      float64 // share of the first child
	a, b       *node
}

func (n *node) leaf() bool { return n.a == nil }

// Layout is a binary tree of splits. Pure data, no I/O, no tea.
type Layout struct{ root *node }

// clone returns a detached copy of the tree. A snapshot hands one to the save
// command, which marshals it on its own goroutine while the live layout keeps
// splitting and closing panes (BUILD-PLAN §6.1: persistence must never share
// mutable state with Update).
func (l *Layout) clone() *Layout {
	if l == nil {
		return nil
	}
	return &Layout{root: l.root.clone()}
}

// clone copies one node and its subtree.
func (n *node) clone() *node {
	if n == nil {
		return nil
	}
	c := *n
	c.a = n.a.clone()
	c.b = n.b.clone()
	return &c
}

// NewLayout returns a layout holding a single pane.
func NewLayout(first PaneID) *Layout { return &Layout{root: &node{pane: first}} }

func (n *node) find(id PaneID) *node {
	if n == nil {
		return nil
	}
	if n.leaf() {
		if n.pane == id {
			return n
		}
		return nil
	}
	if f := n.a.find(id); f != nil {
		return f
	}
	return n.b.find(id)
}

func (n *node) first() PaneID {
	for !n.leaf() {
		n = n.a
	}
	return n.pane
}

func (n *node) walk(f func(*node)) {
	if n.leaf() {
		f(n)
		return
	}
	n.a.walk(f)
	n.b.walk(f)
}

// minW and minH are the smallest size the subtree can be given.
func (n *node) minW() int {
	switch {
	case n.leaf():
		return minPaneW
	case n.horizontal:
		return n.a.minW() + n.b.minW()
	}
	return max(n.a.minW(), n.b.minW())
}

func (n *node) minH() int {
	switch {
	case n.leaf():
		return minPaneH
	case n.horizontal:
		return max(n.a.minH(), n.b.minH())
	}
	return n.a.minH() + n.b.minH()
}

func clamp(v, lo, hi float64) float64 { return math.Min(hi, math.Max(lo, v)) }

func (l *Layout) split(target, nw PaneID, horizontal bool) error {
	const op = "mux.split"
	if l == nil || l.root == nil {
		return errs.New(errs.Internal, op, "empty layout")
	}
	if l.root.find(nw) != nil {
		return errs.New(errs.Internal, op, fmt.Sprintf("pane %d already exists", nw))
	}
	t := l.root.find(target)
	if t == nil {
		return errs.New(errs.Internal, op, fmt.Sprintf("unknown pane %d", target))
	}
	t.a = &node{pane: target}
	t.b = &node{pane: nw}
	t.horizontal = horizontal
	t.ratio = 0.5
	t.pane = 0
	return nil
}

// SplitH puts new to the right of target, 50/50.
func (l *Layout) SplitH(target, new PaneID) error { return l.split(target, new, true) }

// SplitV puts new below target, 50/50.
func (l *Layout) SplitV(target, new PaneID) error { return l.split(target, new, false) }

// Close removes id; its sibling takes the space. next is the pane to focus.
// Closing the only pane reports empty. An unknown id is a no-op returning (0, false).
func (l *Layout) Close(id PaneID) (next PaneID, empty bool) {
	if l == nil || l.root == nil {
		return 0, true
	}
	if l.root.leaf() {
		if l.root.pane == id {
			l.root = nil
			return 0, true
		}
		return 0, false
	}
	var parent *node
	var rec func(n *node) bool
	rec = func(n *node) bool {
		if n.leaf() {
			return false
		}
		for _, c := range []*node{n.a, n.b} {
			if c.leaf() && c.pane == id {
				sib := n.b
				if c == n.b {
					sib = n.a
				}
				*n = *sib
				parent = n
				return true
			}
		}
		return rec(n.a) || rec(n.b)
	}
	if !rec(l.root) {
		return 0, false
	}
	return parent.first(), false
}

// splitSize divides total along an axis between the two children of n.
func splitSize(n *node, total int, minA, minB int) int {
	first := int(math.Round(float64(total) * n.ratio))
	if minA+minB <= total {
		first = min(max(first, minA), total-minB)
	}
	return min(max(first, 0), total)
}

func (n *node) rects(r Rect, out map[PaneID]Rect) {
	if n.leaf() {
		out[n.pane] = r
		return
	}
	n.a.rects(n.childRect(r, true), out)
	n.b.rects(n.childRect(r, false), out)
}

func (n *node) childRect(r Rect, first bool) Rect {
	if n.horizontal {
		w := splitSize(n, r.W, n.a.minW(), n.b.minW())
		if first {
			return Rect{r.X, r.Y, w, r.H}
		}
		return Rect{r.X + w, r.Y, r.W - w, r.H}
	}
	h := splitSize(n, r.H, n.a.minH(), n.b.minH())
	if first {
		return Rect{r.X, r.Y, r.W, h}
	}
	return Rect{r.X, r.Y + h, r.W, r.H - h}
}

// Rects tiles total exactly.
func (l *Layout) Rects(total Rect) map[PaneID]Rect {
	out := map[PaneID]Rect{}
	if l != nil && l.root != nil {
		l.root.rects(total, out)
	}
	return out
}

// Panes lists panes depth-first, first child first.
func (l *Layout) Panes() []PaneID {
	var ids []PaneID
	if l != nil && l.root != nil {
		l.root.walk(func(n *node) { ids = append(ids, n.pane) })
	}
	return ids
}

// Contains reports whether id is a pane of the layout.
func (l *Layout) Contains(id PaneID) bool { return l != nil && l.root.find(id) != nil }

// Count is the number of panes.
func (l *Layout) Count() int { return len(l.Panes()) }

func overlap(a0, a1, b0, b1 int) int { return min(a1, b1) - max(a0, b0) }

// Neighbor finds the pane touching id's edge in direction d, by rectangles.
func (l *Layout) Neighbor(id PaneID, d Direction, total Rect) (PaneID, bool) {
	rects := l.Rects(total)
	r, ok := rects[id]
	if !ok {
		return 0, false
	}
	var best PaneID
	bestOv := 0
	var bestR Rect
	found := false
	for oid, o := range rects {
		if oid == id {
			continue
		}
		var touch bool
		var ov int
		switch d {
		case Left:
			touch, ov = o.X+o.W == r.X, overlap(r.Y, r.Y+r.H, o.Y, o.Y+o.H)
		case Right:
			touch, ov = o.X == r.X+r.W, overlap(r.Y, r.Y+r.H, o.Y, o.Y+o.H)
		case Up:
			touch, ov = o.Y+o.H == r.Y, overlap(r.X, r.X+r.W, o.X, o.X+o.W)
		case Down:
			touch, ov = o.Y == r.Y+r.H, overlap(r.X, r.X+r.W, o.X, o.X+o.W)
		}
		if !touch || ov <= 0 {
			continue
		}
		better := !found || ov > bestOv ||
			(ov == bestOv && (o.X < bestR.X || (o.X == bestR.X && o.Y < bestR.Y)))
		if better {
			best, bestOv, bestR, found = oid, ov, o, true
		}
	}
	return best, found
}

// Resize moves the divider of the nearest ancestor split on d's axis by cells
// in direction d. No such ancestor: no-op.
func (l *Layout) Resize(id PaneID, d Direction, cells int, total Rect) {
	if l == nil || l.root == nil || cells == 0 {
		return
	}
	horizontal := d == Left || d == Right
	var path []*node
	var rects []Rect
	var rec func(n *node, r Rect) bool
	rec = func(n *node, r Rect) bool {
		if n.leaf() {
			return n.pane == id
		}
		path = append(path, n)
		rects = append(rects, r)
		if rec(n.a, n.childRect(r, true)) || rec(n.b, n.childRect(r, false)) {
			return true
		}
		path = path[:len(path)-1]
		rects = rects[:len(rects)-1]
		return false
	}
	if !rec(l.root, total) {
		return
	}
	for i := len(path) - 1; i >= 0; i-- {
		n := path[i]
		if n.horizontal != horizontal {
			continue
		}
		size, minA, minB := rects[i].H, n.a.minH(), n.b.minH()
		if horizontal {
			size, minA, minB = rects[i].W, n.a.minW(), n.b.minW()
		}
		if size <= 0 {
			return
		}
		delta := float64(cells) / float64(size)
		if d == Left || d == Up {
			delta = -delta
		}
		lo, hi := minRatio, maxRatio
		if minA+minB <= size {
			lo = math.Max(lo, float64(minA)/float64(size))
			hi = math.Min(hi, 1-float64(minB)/float64(size))
		}
		if lo > hi {
			return
		}
		n.ratio = clamp(n.ratio+delta, lo, hi)
		return
	}
}

// Swap exchanges two leaves.
func (l *Layout) Swap(a, b PaneID) {
	if l == nil || l.root == nil {
		return
	}
	na, nb := l.root.find(a), l.root.find(b)
	if na == nil || nb == nil {
		return
	}
	na.pane, nb.pane = nb.pane, na.pane
}

// Even equalizes sizes along one axis: every leaf in a maximal chain of
// splits along that axis gets the same share.
func (l *Layout) Even(horizontal bool) {
	if l == nil || l.root == nil {
		return
	}
	var items func(n *node) int
	items = func(n *node) int {
		if n.leaf() || n.horizontal != horizontal {
			return 1
		}
		return items(n.a) + items(n.b)
	}
	var rec func(n *node)
	rec = func(n *node) {
		if n.leaf() {
			return
		}
		if n.horizontal == horizontal {
			ca, cb := items(n.a), items(n.b)
			n.ratio = clamp(float64(ca)/float64(ca+cb), minRatio, maxRatio)
		}
		rec(n.a)
		rec(n.b)
	}
	rec(l.root)
}
