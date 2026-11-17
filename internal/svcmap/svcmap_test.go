package svcmap

import (
	"strings"
	"testing"
)

func TestLinesTreeAndEvidence(t *testing.T) {
	g := New()
	ing := g.Add(Vertex{Kind: Ingress, Scope: "shop", Name: "shop", Detail: "shop.example.internal"})
	svc := g.Add(Vertex{Kind: Service, Scope: "shop", Name: "web"})
	web := g.Add(Vertex{Kind: Deployment, Scope: "shop", Name: "web", Detail: "2/2 ready"})
	cart := g.Add(Vertex{Kind: Deployment, Scope: "shop", Name: "cart"})
	g.Connect(Edge{From: ing, To: svc, Port: "80", Source: IngressRule})
	g.Connect(Edge{From: svc, To: web, Source: Selector, Note: "app=web"})
	g.Connect(Edge{From: web, To: cart, Port: "8080/TCP", Source: Hubble, Forwarded: 3})
	g.Connect(Edge{From: web, To: cart, Port: "8080/TCP", Source: Hubble, Forwarded: 2, Dropped: 1})
	g.Connect(Edge{From: cart, To: web, Port: "80/TCP", Source: Hubble, Forwarded: 1}) // a cycle
	g.Notes = append(g.Notes, "caveat")

	got := strings.Join(g.Lines(false), "\n")
	for _, want := range []string{
		"Ingress shop/shop  shop.example.internal",
		"└─▶ Service shop/web  :80   [declared · ingress rule]",
		"    └─▶ Deployment shop/web  2/2 ready   [declared · selector · app=web]",
		"Deployment shop/cart  :8080/TCP   [observed · hubble · 5 forwarded, 1 DROPPED]",
		"↑ shown above",
		"note: caveat",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if n := len(g.Edges()); n != 4 {
		t.Errorf("edges = %d, want 4 (the two hubble edges merge)", n)
	}
	for _, l := range g.Lines(true) {
		for _, r := range l {
			if r > 127 {
				t.Fatalf("ascii rendering has %q in %q", r, l)
			}
		}
	}
}

func TestLabelsUseTheRightWords(t *testing.T) {
	cases := map[string]Vertex{
		"Unit nginx.service on web-01": {Kind: Unit, Scope: "web-01", Name: "nginx.service"},
		"Service shop/web":             {Kind: Service, Scope: "shop", Name: "web"},
		"Host db-01":                   {Kind: Host, Name: "db-01"},
		"Node k3s-1":                   {Kind: Node, Name: "k3s-1"},
	}
	for want, v := range cases {
		if got := v.Label(); got != want {
			t.Errorf("Label() = %q, want %q", got, want)
		}
	}
}

func TestEmptyGraphAndSelfEdge(t *testing.T) {
	g := New()
	a := g.Add(Vertex{Kind: Host, Name: "a"})
	g.Connect(Edge{From: a, To: a, Source: Socket})
	if len(g.Edges()) != 0 {
		t.Fatal("a self edge was kept")
	}
	if got := strings.Join(g.Lines(false), "\n"); !strings.Contains(got, "no connections found for:") {
		t.Fatalf("lonely vertex not listed:\n%s", got)
	}
}
