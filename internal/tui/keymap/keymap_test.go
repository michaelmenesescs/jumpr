package keymap

import (
	"reflect"
	"testing"

	"jumper/internal/errs"
)

func TestParsePrefix(t *testing.T) {
	ok := []struct{ in, want string }{
		{"C-a", "ctrl+a"},
		{"C-b", "ctrl+b"},
		{"C-z", "ctrl+z"},
		{"C-\\", "ctrl+\\"},
		{"C-@", "ctrl+@"},
		{"C-]", "ctrl+]"},
		{"C-^", "ctrl+^"},
		{"C-_", "ctrl+_"},
	}
	for _, c := range ok {
		got, err := ParsePrefix(c.in)
		if err != nil {
			t.Errorf("ParsePrefix(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	bad := []string{"", "a", "C", "C-", "C-A", "C-ab", "C-1", "C-[", "ctrl+a", "c-a", "C- "}
	for _, in := range bad {
		got, err := ParsePrefix(in)
		if err == nil {
			t.Errorf("ParsePrefix(%q) = %q, want an error", in, got)
			continue
		}
		if kind := errs.KindOf(err); kind != errs.Config {
			t.Errorf("ParsePrefix(%q) kind = %q, want %q", in, kind, errs.Config)
		}
		if got != "" {
			t.Errorf("ParsePrefix(%q) returned %q with the error", in, got)
		}
	}
}

func TestMatches(t *testing.T) {
	b := Binding{Keys: []string{"j", "down"}, Help: "down"}
	for _, k := range []string{"j", "down"} {
		if !b.Matches(k) {
			t.Errorf("Matches(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"J", "up", "", "ctrl+d"} {
		if b.Matches(k) {
			t.Errorf("Matches(%q) = true, want false", k)
		}
	}
	if (Binding{}).Matches("") {
		t.Error("zero Binding matched an empty key")
	}
}

func TestDisplayKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ctrl+a", "C-a"},
		{"ctrl+u", "C-u"},
		{"ctrl+\\", "C-\\"},
		{"ctrl+@", "C-@"},
		{"enter", "enter"},
		{"esc", "esc"},
		{"pgup", "pgup"},
		{"G", "G"},
		{"?", "?"},
		{"", ""},
	}
	for _, c := range cases {
		if got := DisplayKey(c.in); got != c.want {
			t.Errorf("DisplayKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDisplayKeyRoundTrip(t *testing.T) {
	ctrlKeys := []string{
		"ctrl+a", "ctrl+b", "ctrl+c", "ctrl+d", "ctrl+e", "ctrl+f", "ctrl+g",
		"ctrl+h", "ctrl+j", "ctrl+k", "ctrl+l", "ctrl+n", "ctrl+o", "ctrl+p",
		"ctrl+q", "ctrl+r", "ctrl+s", "ctrl+t", "ctrl+u", "ctrl+v", "ctrl+w",
		"ctrl+x", "ctrl+y", "ctrl+z", "ctrl+@", "ctrl+\\", "ctrl+]", "ctrl+^",
		"ctrl+_",
	}
	for _, k := range ctrlKeys {
		display := DisplayKey(k)
		got, err := ParsePrefix(display)
		if err != nil {
			t.Errorf("ParsePrefix(DisplayKey(%q)) = %q: %v", k, display, err)
			continue
		}
		if got != k {
			t.Errorf("round trip %q -> %q -> %q", k, display, got)
		}
	}
}

func TestDefault(t *testing.T) {
	want := map[string][]string{
		"Up":         {"k", "up"},
		"Down":       {"j", "down"},
		"PageUp":     {"pgup", "ctrl+u"},
		"PageDown":   {"pgdown", "ctrl+d"},
		"Top":        {"g", "home"},
		"Bottom":     {"G", "end"},
		"Open":       {"enter"},
		"Back":       {"esc"},
		"Command":    {":"},
		"Filter":     {"/"},
		"FuzzyFind":  {"ctrl+f"},
		"Help":       {"?"},
		"Refresh":    {"r"},
		"Quit":       {"q"},
		"Actions":    {"x"},
		"Note":       {"n"},
		"Finding":    {"F"},
		"Knowledge":  {"K"},
		"Timeline":   {"T"},
		"Yank":       {"y"},
		"Follow":     {"w"},
		"TimeWindow": {"t"},
		"Diff":       {"d"},
		"Errors":     {"e"},
	}
	m := Default()
	if m == nil {
		t.Fatal("Default() returned nil")
	}
	v := reflect.ValueOf(*m)
	typ := v.Type()
	if typ.NumField() != len(want) {
		t.Fatalf("Map has %d fields, want %d", typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		b, ok := v.Field(i).Interface().(Binding)
		if !ok {
			t.Errorf("field %s is not a Binding", name)
			continue
		}
		exp, ok := want[name]
		if !ok {
			t.Errorf("unexpected binding field %s", name)
			continue
		}
		if !reflect.DeepEqual(b.Keys, exp) {
			t.Errorf("%s.Keys = %v, want %v", name, b.Keys, exp)
		}
		if b.Help == "" {
			t.Errorf("%s.Help is empty", name)
		}
		for _, k := range exp {
			if !b.Matches(k) {
				t.Errorf("%s does not match %q", name, k)
			}
		}
	}
}
