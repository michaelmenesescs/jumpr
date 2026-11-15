package group

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

func hid(i int) resource.HostID { return resource.HostID(fmt.Sprintf("h%04d", i)) }

func build(total, same, other, nerr int) *Grouper {
	g := NewGrouper(total, nil)
	n := 0
	for i := 0; i < same; i++ {
		g.Add(hid(n), "ok\n", 0)
		n++
	}
	for i := 0; i < other; i++ {
		g.Add(hid(n), fmt.Sprintf("odd %d\n", i%3), 0)
		n++
	}
	for i := 0; i < nerr; i++ {
		g.AddError(hid(n), errs.New(errs.Timeout, "x", "t"))
		n++
	}
	return g
}

func TestGroupingBasic(t *testing.T) {
	g := NewGrouper(5, nil)
	g.Add("b", "x", 0)
	g.Add("a", "x", 0)
	g.Add("c", "y", 0)
	g.Add("d", "x", 1)
	s := g.Summary()
	if len(s.Groups) != 3 || s.Done != 4 || s.Total != 5 {
		t.Fatalf("%+v", s)
	}
	if !reflect.DeepEqual(s.Groups[0].Hosts, []resource.HostID{"a", "b"}) {
		t.Fatalf("hosts %v", s.Groups[0].Hosts)
	}
	m, ok := s.Majority()
	if !ok || len(m.Hosts) != 2 || len(s.Differing()) != 2 {
		t.Fatal("majority/differing")
	}
	if _, ok := (Summary{}).Majority(); ok {
		t.Fatal("empty majority")
	}
}

func TestHeadlineTable(t *testing.T) {
	cases := []struct {
		g    *Grouper
		want string
	}{
		{build(24, 24, 0, 0), "24 identical"},
		{build(1512, 1489, 19, 4), "1,489 identical · 19 differ · 4 errors"},
		{build(1512, 800, 12, 0), "812/1,512 · 800 identical · 12 differ"},
		{NewGrouper(1512, nil), "0/1,512"},
	}
	for _, c := range cases {
		if got := c.g.Summary().Headline(); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

func TestErrorsByKind(t *testing.T) {
	g := NewGrouper(3, nil)
	g.AddError("a", errs.New(errs.Timeout, "op", "m"))
	g.AddError("b", errs.New(errs.Timeout, "op", "m"))
	g.AddError("c", errs.New(errs.Auth, "op", "m"))
	s := g.Summary()
	if len(s.Errors[errs.Timeout]) != 2 || len(s.Errors[errs.Auth]) != 1 || s.Done != 3 {
		t.Fatalf("%+v", s)
	}
}

func TestNormalizeDefault(t *testing.T) {
	g := NewGrouper(3, nil)
	g.Add("a", "one  \ntwo\n\n", 0)
	g.Add("b", "one\ntwo", 0)
	g.Add("c", "one\ntwo", 1)
	if n := len(g.Summary().Groups); n != 2 {
		t.Fatalf("groups %d", n)
	}
}

func TestDeterministicOrder(t *testing.T) {
	type in struct {
		h   resource.HostID
		out string
	}
	var ins []in
	for i := 0; i < 30; i++ {
		ins = append(ins, in{hid(i), fmt.Sprintf("v%d", i%4)})
	}
	var first Summary
	for k := 0; k < 20; k++ {
		rand.Shuffle(len(ins), func(i, j int) { ins[i], ins[j] = ins[j], ins[i] })
		g := NewGrouper(30, nil)
		for _, x := range ins {
			g.Add(x.h, x.out, 0)
		}
		s := g.Summary()
		for i := range s.Groups {
			s.Groups[i].Output = ""
		}
		if k == 0 {
			first = s
		} else if !reflect.DeepEqual(first, s) {
			t.Fatal("order differs")
		}
	}
}
