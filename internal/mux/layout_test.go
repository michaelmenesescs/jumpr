package mux

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"
)

var totals = []Rect{{0, 0, 80, 24}, {0, 0, 132, 40}, {5, 2, 203, 51}}

func TestLayoutRectsTile(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for seq := 0; seq < 200; seq++ {
		l := NewLayout(0)
		next := PaneID(1)
		for i := 0; i < 12; i++ {
			ps := l.Panes()
			switch op := rng.Intn(5); {
			case op < 2:
				_ = l.SplitH(ps[rng.Intn(len(ps))], next)
				next++
			case op < 4:
				_ = l.SplitV(ps[rng.Intn(len(ps))], next)
				next++
			default:
				if len(ps) > 1 {
					l.Close(ps[rng.Intn(len(ps))])
				}
			}
			if rng.Intn(3) == 0 {
				ps = l.Panes()
				l.Resize(ps[rng.Intn(len(ps))], Direction(rng.Intn(4)), rng.Intn(20), totals[0])
			}
		}
		for _, tot := range totals {
			rs := l.Rects(tot)
			if len(rs) != l.Count() {
				t.Fatalf("seq %d: %d rects for %d panes", seq, len(rs), l.Count())
			}
			grid := make([][]int, tot.H)
			for y := range grid {
				grid[y] = make([]int, tot.W)
			}
			fits := l.root.minW() <= tot.W && l.root.minH() <= tot.H
			for id, r := range rs {
				if fits && (r.W < minPaneW || r.H < minPaneH) {
					t.Fatalf("seq %d: pane %d too small %+v in %+v", seq, id, r, tot)
				}
				for y := r.Y - tot.Y; y < r.Y-tot.Y+r.H; y++ {
					for x := r.X - tot.X; x < r.X-tot.X+r.W; x++ {
						if y < 0 || x < 0 || y >= tot.H || x >= tot.W {
							t.Fatalf("seq %d: %+v outside %+v", seq, r, tot)
						}
						grid[y][x]++
					}
				}
			}
			for y := range grid {
				for x := range grid[y] {
					if grid[y][x] != 1 {
						t.Fatalf("seq %d: cell %d,%d covered %d times", seq, x, y, grid[y][x])
					}
				}
			}
		}
	}
}

func TestLayoutSplitClose(t *testing.T) {
	l := NewLayout(1)
	if err := l.SplitH(1, 2); err != nil {
		t.Fatal(err)
	}
	if err := l.SplitV(2, 3); err != nil {
		t.Fatal(err)
	}
	if l.SplitH(9, 4) == nil || l.SplitH(1, 3) == nil {
		t.Fatal("expected errors for unknown target / duplicate new")
	}
	if got := l.Panes(); !reflect.DeepEqual(got, []PaneID{1, 2, 3}) {
		t.Fatalf("panes %v", got)
	}
	rs := l.Rects(Rect{0, 0, 80, 24})
	if rs[1] != (Rect{0, 0, 40, 24}) || rs[2] != (Rect{40, 0, 40, 12}) || rs[3] != (Rect{40, 12, 40, 12}) {
		t.Fatalf("rects %+v", rs)
	}
	next, empty := l.Close(2)
	if empty || next != 3 || !l.Contains(3) || l.Contains(2) || l.Count() != 2 {
		t.Fatalf("close: next=%d empty=%v panes=%v", next, empty, l.Panes())
	}
	if rs := l.Rects(Rect{0, 0, 80, 24}); rs[3] != (Rect{40, 0, 40, 24}) {
		t.Fatalf("rects %+v", rs)
	}
}

func TestLayoutCloseLastPaneEmpty(t *testing.T) {
	l := NewLayout(7)
	if _, empty := l.Close(7); !empty {
		t.Fatal("want empty")
	}
}

func TestLayoutNeighborGrid(t *testing.T) {
	// 1 2
	// 3 4
	l := NewLayout(1)
	_ = l.SplitH(1, 2)
	_ = l.SplitV(1, 3)
	_ = l.SplitV(2, 4)
	tot := Rect{0, 0, 80, 24}
	want := map[PaneID]map[Direction]PaneID{
		1: {Right: 2, Down: 3},
		2: {Left: 1, Down: 4},
		3: {Right: 4, Up: 1},
		4: {Left: 3, Up: 2},
	}
	for id := PaneID(1); id <= 4; id++ {
		for d := Left; d <= Down; d++ {
			got, ok := l.Neighbor(id, d, tot)
			w, wok := want[id][d]
			if ok != wok || (ok && got != w) {
				t.Errorf("pane %d dir %d: got %d,%v want %d,%v", id, d, got, ok, w, wok)
			}
		}
	}
}

func TestLayoutResizeClamps(t *testing.T) {
	l := NewLayout(1)
	_ = l.SplitH(1, 2)
	tot := Rect{0, 0, 100, 24}
	l.Resize(1, Right, 10, tot)
	if r := l.Rects(tot)[1]; r.W != 60 {
		t.Fatalf("width %d", r.W)
	}
	l.Resize(1, Left, 15, tot)
	if r := l.Rects(tot)[1]; r.W != 45 {
		t.Fatalf("width %d", r.W)
	}
	l.Resize(1, Right, 500, tot)
	if r := l.Rects(tot)[1]; r.W != 90 {
		t.Fatalf("width %d", r.W)
	}
	l.Resize(1, Left, 500, tot)
	if r := l.Rects(tot)[1]; r.W != 10 {
		t.Fatalf("width %d", r.W)
	}
	before := l.Rects(tot)
	l.Resize(1, Down, 5, tot) // no vertical ancestor
	if !reflect.DeepEqual(before, l.Rects(tot)) {
		t.Fatal("no-op expected")
	}
}

func TestLayoutSwap(t *testing.T) {
	l := NewLayout(1)
	_ = l.SplitH(1, 2)
	l.Swap(1, 2)
	if got := l.Panes(); !reflect.DeepEqual(got, []PaneID{2, 1}) {
		t.Fatalf("%v", got)
	}
}

func TestLayoutEven(t *testing.T) {
	l := NewLayout(1)
	_ = l.SplitH(1, 2)
	_ = l.SplitH(2, 3)
	_ = l.SplitH(3, 4)
	l.Resize(1, Right, 20, Rect{0, 0, 120, 24})
	l.Even(true)
	for id, r := range l.Rects(Rect{0, 0, 120, 24}) {
		if r.W != 30 {
			t.Errorf("pane %d width %d", id, r.W)
		}
	}
}

func TestLayoutJSONRoundTrip(t *testing.T) {
	l := NewLayout(1)
	_ = l.SplitH(1, 2)
	_ = l.SplitV(2, 3)
	l.Resize(1, Right, 7, Rect{0, 0, 80, 24})
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	var m Layout
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.root, m.root) {
		t.Fatalf("round trip differs: %s", b)
	}
	if string(b[:6]) != `{"dir"` {
		t.Fatalf("form %s", b)
	}
	for _, bad := range []string{
		`{"dir":"h","ratio":0.95,"a":{"pane":1},"b":{"pane":2}}`,
		`{"dir":"h","ratio":0.5,"a":{"pane":1},"b":{"pane":1}}`,
		`{"dir":"x","ratio":0.5,"a":{"pane":1},"b":{"pane":2}}`,
		`{"dir":"h","ratio":0.5,"a":{"pane":1}}`,
	} {
		if json.Unmarshal([]byte(bad), &Layout{}) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	var leaf Layout
	if err := json.Unmarshal([]byte(`{"pane":3}`), &leaf); err != nil || leaf.Count() != 1 {
		t.Fatal(err)
	}
}
