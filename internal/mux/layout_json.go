package mux

import (
	"encoding/json"
	"fmt"

	"jumper/internal/errs"
)

type jsonNode struct {
	Pane  *PaneID   `json:"pane,omitempty"`
	Dir   string    `json:"dir,omitempty"`
	Ratio float64   `json:"ratio,omitempty"`
	A     *jsonNode `json:"a,omitempty"`
	B     *jsonNode `json:"b,omitempty"`
}

func toJSON(n *node) *jsonNode {
	if n.leaf() {
		p := n.pane
		return &jsonNode{Pane: &p}
	}
	dir := "v"
	if n.horizontal {
		dir = "h"
	}
	return &jsonNode{Dir: dir, Ratio: n.ratio, A: toJSON(n.a), B: toJSON(n.b)}
}

func fromJSON(j *jsonNode, seen map[PaneID]bool) (*node, error) {
	if j == nil {
		return nil, fmt.Errorf("missing node")
	}
	if j.Pane != nil {
		if j.A != nil || j.B != nil || j.Dir != "" {
			return nil, fmt.Errorf("node is both leaf and split")
		}
		if seen[*j.Pane] {
			return nil, fmt.Errorf("duplicate pane %d", *j.Pane)
		}
		seen[*j.Pane] = true
		return &node{pane: *j.Pane}, nil
	}
	if j.Dir != "h" && j.Dir != "v" {
		return nil, fmt.Errorf("bad dir %q", j.Dir)
	}
	if !(j.Ratio >= minRatio && j.Ratio <= maxRatio) {
		return nil, fmt.Errorf("ratio %v out of range", j.Ratio)
	}
	a, err := fromJSON(j.A, seen)
	if err != nil {
		return nil, err
	}
	b, err := fromJSON(j.B, seen)
	if err != nil {
		return nil, err
	}
	return &node{horizontal: j.Dir == "h", ratio: j.Ratio, a: a, b: b}, nil
}

// MarshalJSON encodes the tree: leaf {"pane":3}; split {"dir":"h","ratio":0.5,"a":…,"b":…}.
func (l *Layout) MarshalJSON() ([]byte, error) {
	if l == nil || l.root == nil {
		return nil, errs.New(errs.Internal, "mux.layout.marshal", "empty layout")
	}
	return json.Marshal(toJSON(l.root))
}

// UnmarshalJSON decodes and validates ratios and pane uniqueness.
func (l *Layout) UnmarshalJSON(data []byte) error {
	var j jsonNode
	if err := json.Unmarshal(data, &j); err != nil {
		return errs.Wrap(errs.Parse, "mux.layout.unmarshal", err)
	}
	root, err := fromJSON(&j, map[PaneID]bool{})
	if err != nil {
		return errs.Wrap(errs.Parse, "mux.layout.unmarshal", err)
	}
	l.root = root
	return nil
}
