// Package svcmap is the service map: who talks to whom. It holds one small
// graph that every collector fills in its own vocabulary — Kubernetes objects
// keep their Kubernetes names (Ingress, Service, Deployment, Pod), machines
// reached over SSH are hosts, and what runs on them is a systemd unit or, when
// the unit is unknown, a process — and it renders that graph as text.
//
// An edge always says where it came from. Observed edges are traffic that was
// seen (Hubble flows on a Cilium cluster, established sockets on a host).
// Declared edges are configuration that says traffic should flow (an Ingress
// rule, a Service selector, an environment variable naming a Service). The
// two are never mixed up in the output.
package svcmap

import (
	"fmt"
	"sort"
	"strings"
)

// Kind is what a node is, in the words of the system it belongs to.
type Kind string

const (
	Ingress     Kind = "Ingress"
	Service     Kind = "Service" // a Kubernetes Service object, never a systemd unit
	Deployment  Kind = "Deployment"
	StatefulSet Kind = "StatefulSet"
	DaemonSet   Kind = "DaemonSet"
	Job         Kind = "Job"
	Pod         Kind = "Pod"  // a pod no workload owns
	Node        Kind = "Node" // a Kubernetes node
	Host        Kind = "Host" // a machine reached over SSH
	Unit        Kind = "Unit" // a systemd unit on a host
	Process     Kind = "Process"
	External    Kind = "External" // outside the cluster or the inventory
)

// Source says how an edge is known.
type Source string

const (
	// Observed sources: traffic that was seen.
	Hubble Source = "hubble"
	Socket Source = "socket"
	// Declared sources: configuration that implies traffic.
	IngressRule Source = "ingress rule"
	Selector    Source = "selector"
	EnvRef      Source = "env reference"
)

// Observed reports whether the source is seen traffic rather than config.
func (s Source) Observed() bool { return s == Hubble || s == Socket }

// Vertex is one node of the map. Scope is the namespace of a Kubernetes
// object or the host of a unit or process; it is empty for a host itself.
type Vertex struct {
	Kind   Kind
	Scope  string
	Name   string
	Detail string // "2/2 ready", "shop.example.internal"
}

// ID is the vertex's identity inside one graph.
func (v Vertex) ID() string { return string(v.Kind) + "\x00" + v.Scope + "\x00" + v.Name }

// Label is how the vertex is written: its kind first, then scope/name.
func (v Vertex) Label() string {
	name := v.Name
	if v.Scope != "" {
		switch v.Kind {
		case Unit, Process:
			name = v.Name + " on " + v.Scope
		default:
			name = v.Scope + "/" + v.Name
		}
	}
	return string(v.Kind) + " " + name
}

// Edge is "From talks to To on Port". Forwarded and Dropped are flow counts
// for observed edges; Note carries what a declared edge was derived from.
type Edge struct {
	From, To  string // vertex IDs
	Port      string // "443/TCP", "5432"
	Source    Source
	Forwarded int
	Dropped   int
	Note      string
}

// Graph is the map. The zero value is not usable; call New.
type Graph struct {
	vertices map[string]Vertex
	edges    map[string]*Edge
	order    []string
	// Notes are caveats the reader needs: what could not be collected and
	// what kind of evidence the edges are.
	Notes []string
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{vertices: map[string]Vertex{}, edges: map[string]*Edge{}}
}

// Add puts v in the graph and returns its ID. A later Add of the same vertex
// keeps the first Detail unless it was empty.
func (g *Graph) Add(v Vertex) string {
	id := v.ID()
	if old, ok := g.vertices[id]; ok {
		if old.Detail == "" && v.Detail != "" {
			g.vertices[id] = v
		}
		return id
	}
	g.vertices[id] = v
	return id
}

// Connect adds an edge, merging counts into an existing edge with the same
// ends, port and source.
func (g *Graph) Connect(e Edge) {
	if e.From == "" || e.To == "" || e.From == e.To {
		return
	}
	key := e.From + "\x01" + e.To + "\x01" + e.Port + "\x01" + string(e.Source)
	if old, ok := g.edges[key]; ok {
		old.Forwarded += e.Forwarded
		old.Dropped += e.Dropped
		if old.Note == "" {
			old.Note = e.Note
		}
		return
	}
	c := e
	g.edges[key] = &c
	g.order = append(g.order, key)
}

// Vertices returns every vertex, sorted by label.
func (g *Graph) Vertices() []Vertex {
	out := make([]Vertex, 0, len(g.vertices))
	for _, v := range g.vertices {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label() < out[j].Label() })
	return out
}

// Edges returns every edge, sorted by the labels of its ends.
func (g *Graph) Edges() []Edge {
	out := make([]Edge, 0, len(g.edges))
	for _, k := range g.order {
		out = append(out, *g.edges[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := g.vertices[out[i].From].Label(), g.vertices[out[j].From].Label()
		if a != b {
			return a < b
		}
		return g.vertices[out[i].To].Label() < g.vertices[out[j].To].Label()
	})
	return out
}

// Merge copies other into g.
func (g *Graph) Merge(other *Graph) {
	if other == nil {
		return
	}
	for _, v := range other.vertices {
		g.Add(v)
	}
	for _, k := range other.order {
		g.Connect(*other.edges[k])
	}
	g.Notes = append(g.Notes, other.Notes...)
}

// kindRank orders roots: traffic enters at the top.
func kindRank(k Kind) int {
	switch k {
	case External:
		return 0
	case Ingress:
		return 1
	case Service:
		return 2
	case Host:
		return 4
	default:
		return 3
	}
}

// evidence writes how an edge is known.
func evidence(e Edge, sep string) string {
	switch {
	case e.Source.Observed() && e.Dropped > 0:
		return fmt.Sprintf("observed"+sep+"%s"+sep+"%d forwarded, %d DROPPED", e.Source, e.Forwarded, e.Dropped)
	case e.Source.Observed() && e.Forwarded > 0:
		return fmt.Sprintf("observed"+sep+"%s"+sep+"%d forwarded", e.Source, e.Forwarded)
	case e.Source.Observed():
		return fmt.Sprintf("observed"+sep+"%s", e.Source)
	case e.Note != "":
		return fmt.Sprintf("declared"+sep+"%s"+sep+"%s", e.Source, e.Note)
	default:
		return fmt.Sprintf("declared"+sep+"%s", e.Source)
	}
}

// Lines renders the map as an indented tree per entry point: every vertex
// nothing talks to starts a tree, and each edge is drawn once. A vertex that
// was already expanded is named again with "↑ above" instead of repeated.
// ascii switches the drawing characters to plain ASCII.
func (g *Graph) Lines(ascii bool) []string {
	branch, arrow, again := "└─▶ ", "▶", "↑ shown above"
	if ascii {
		branch, arrow, again = "`-> ", ">", "^ shown above"
	}
	_ = arrow
	sep := " · "
	if ascii {
		sep = " | "
	}
	out := make(map[string][]Edge)
	inbound := make(map[string]int)
	for _, e := range g.Edges() {
		out[e.From] = append(out[e.From], e)
		inbound[e.To]++
	}
	var roots []Vertex
	for _, v := range g.Vertices() {
		if inbound[v.ID()] == 0 && len(out[v.ID()]) > 0 {
			roots = append(roots, v)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return kindRank(roots[i].Kind) < kindRank(roots[j].Kind) })

	var lines []string
	expanded := make(map[string]bool)
	head := func(v Vertex) string {
		if v.Detail != "" {
			return v.Label() + "  " + v.Detail
		}
		return v.Label()
	}
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		expanded[id] = true
		for _, e := range out[id] {
			to := g.vertices[e.To]
			text := strings.Repeat("    ", depth) + branch + head(to)
			if e.Port != "" {
				text += "  :" + e.Port
			}
			text += "   [" + evidence(e, sep) + "]"
			if expanded[e.To] && len(out[e.To]) > 0 {
				lines = append(lines, text+"  "+again)
				continue
			}
			lines = append(lines, text)
			walk(e.To, depth+1)
		}
	}
	for _, r := range roots {
		if expanded[r.ID()] {
			continue
		}
		lines = append(lines, head(r))
		walk(r.ID(), 0)
		lines = append(lines, "")
	}
	// Cycles have no root: start them at their first vertex by label.
	for _, v := range g.Vertices() {
		if !expanded[v.ID()] && len(out[v.ID()]) > 0 {
			lines = append(lines, head(v))
			walk(v.ID(), 0)
			lines = append(lines, "")
		}
	}
	var lonely []string
	for _, v := range g.Vertices() {
		if inbound[v.ID()] == 0 && len(out[v.ID()]) == 0 {
			lonely = append(lonely, "  "+head(v))
		}
	}
	if len(lonely) > 0 {
		lines = append(lines, "no connections found for:")
		lines = append(lines, lonely...)
		lines = append(lines, "")
	}
	if len(lines) == 0 {
		lines = append(lines, "nothing to map", "")
	}
	for _, n := range g.Notes {
		lines = append(lines, "note: "+n)
	}
	return lines
}
