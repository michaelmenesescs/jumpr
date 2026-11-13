package keymap

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jumper/internal/mux"
)

var update = flag.Bool("update", false, "rewrite docs/KEYS.md from the defaults")

// writeFile writes a keymap file into a temporary directory and returns its path.
func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keymap.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write keymap: %v", err)
	}
	return path
}

// loadOK loads a file that must be valid.
func loadOK(t *testing.T, body string) *Overrides {
	t.Helper()
	o, probs := Load(writeFile(t, body))
	if len(probs) > 0 {
		t.Fatalf("Load reported %v, want no problems", probs)
	}
	return o
}

// loadBad loads a file that must be rejected and returns the problems.
func loadBad(t *testing.T, body string) []Problem {
	t.Helper()
	_, probs := Load(writeFile(t, body))
	if len(probs) == 0 {
		t.Fatal("Load accepted the file, want problems")
	}
	return probs
}

// wantKeys fails unless the binding's keys are exactly want.
func wantKeys(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s keys = %v, want %v", name, got, want)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keymap.yaml")
	o, probs := Load(path)
	if len(probs) > 0 {
		t.Fatalf("Load of a missing file reported %v", probs)
	}
	if len(o.Global) != 0 || len(o.Prefix) != 0 {
		t.Fatalf("overrides = %+v, want none", o)
	}
	got, want := o.Map(Default()), Default()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effective map differs from the defaults:\n%+v\n%+v", got, want)
	}
}

func TestLoadEmptyFileUsesDefaults(t *testing.T) {
	o, probs := Load(writeFile(t, "# nothing here\n"))
	if len(probs) > 0 {
		t.Fatalf("Load reported %v", probs)
	}
	wantKeys(t, "open", o.Map(Default()).Open.Keys, "enter")
}

func TestLoadGlobalOverrideReplacesKeys(t *testing.T) {
	o := loadOK(t, "open: [o, enter]\nquit: [Q]\n")
	m := o.Map(Default())
	wantKeys(t, "open", m.Open.Keys, "o", "enter")
	wantKeys(t, "quit", m.Quit.Keys, "Q")
	// Untouched bindings keep the defaults.
	wantKeys(t, "help", m.Help.Keys, "?")
	if m.Open.Help != "open" {
		t.Errorf("open help = %q, want the default text", m.Open.Help)
	}
	// The base map is not modified.
	wantKeys(t, "default open", Default().Open.Keys, "enter")
}

func TestLoadPrefixOverrideReplacesKeys(t *testing.T) {
	o := loadOK(t, "prefix:\n  split-h: [\"|\"]\n  focus-left: [h, left]\n")
	table := o.KeyTable(mux.DefaultKeyTable())
	if got := table["|"].Op; got != mux.OpSplitH {
		t.Errorf(`table["|"] = %q, want split-h`, got)
	}
	if _, ok := table["%"]; ok {
		t.Error(`table still binds "%": the override replaces the op's whole key list`)
	}
	if got := table["left"]; got.Op != mux.OpFocus || got.Arg != "left" {
		t.Errorf(`table["left"] = %+v, want focus/left`, got)
	}
	if got := table["h"]; got.Op != mux.OpFocus || got.Arg != "left" {
		t.Errorf(`table["h"] = %+v, want focus/left`, got)
	}
	// Untouched prefix bindings keep the defaults.
	if got := table["z"].Op; got != mux.OpZoom {
		t.Errorf(`table["z"] = %q, want zoom`, got)
	}
}

func TestLoadPrefixArgNames(t *testing.T) {
	o := loadOK(t, "prefix:\n  select-window-3: [\"3\", F3]\n  resize-down: [J]\n")
	table := o.KeyTable(mux.DefaultKeyTable())
	if got := table["F3"]; got.Op != mux.OpSelectWindow || got.Arg != "3" {
		t.Errorf("F3 = %+v, want select-window/3", got)
	}
	if got := table["J"]; got.Op != mux.OpResize || got.Arg != "down" {
		t.Errorf("J = %+v, want resize/down", got)
	}
}

func TestLoadUnknownNameReportsLine(t *testing.T) {
	probs := loadBad(t, "open: [enter]\n\nbogus_binding: [x]\n")
	if len(probs) != 1 {
		t.Fatalf("problems = %v, want one", probs)
	}
	p := probs[0]
	if p.Line != 3 {
		t.Errorf("line = %d, want 3", p.Line)
	}
	if !strings.Contains(p.Msg, `unknown binding "bogus_binding"`) {
		t.Errorf("msg = %q, want the unknown name", p.Msg)
	}
	if !strings.Contains(p.String(), "keymap.yaml:3:") {
		t.Errorf("String() = %q, want file:line", p.String())
	}
}

func TestLoadPrefixNameAtTopLevelIsGuided(t *testing.T) {
	probs := loadBad(t, "split-h: [\"|\"]\n")
	if len(probs) != 1 || !strings.Contains(probs[0].Msg, `belongs under "prefix"`) {
		t.Fatalf("problems = %v, want a hint to use the prefix section", probs)
	}
	if probs[0].Line != 1 {
		t.Errorf("line = %d, want 1", probs[0].Line)
	}
}

func TestLoadGlobalNameUnderPrefixIsGuided(t *testing.T) {
	probs := loadBad(t, "prefix:\n  open: [enter]\n")
	if len(probs) != 1 || !strings.Contains(probs[0].Msg, "global binding") {
		t.Fatalf("problems = %v, want a hint that open is a global binding", probs)
	}
	if probs[0].Line != 2 {
		t.Errorf("line = %d, want 2", probs[0].Line)
	}
}

func TestLoadConflictWithDefaultReportsLine(t *testing.T) {
	// G is bottom's default key; binding it to quit makes it ambiguous.
	probs := loadBad(t, "open: [enter]\nquit: [G]\n")
	if len(probs) != 1 {
		t.Fatalf("problems = %v, want one", probs)
	}
	p := probs[0]
	if p.Line != 2 {
		t.Errorf("line = %d, want 2 (the override that introduced the clash)", p.Line)
	}
	if !strings.Contains(p.Msg, `key "G"`) || !strings.Contains(p.Msg, "bottom") || !strings.Contains(p.Msg, "quit") {
		t.Errorf("msg = %q, want the key and both bindings", p.Msg)
	}
}

func TestLoadConflictBetweenOverrides(t *testing.T) {
	probs := loadBad(t, "open: [enter]\nquit: [x]\nopen: [enter]\n")
	if len(probs) == 0 {
		t.Fatal("a binding set twice must be reported")
	}
	if !strings.Contains(probs[0].Msg, `"open" is set twice`) {
		t.Errorf("msg = %q, want the duplicate binding", probs[0].Msg)
	}
	if probs[0].Line != 3 {
		t.Errorf("line = %d, want 3", probs[0].Line)
	}
}

func TestLoadPrefixConflictReportsLine(t *testing.T) {
	// z is zoom's default key.
	probs := loadBad(t, "prefix:\n  split-h: [z]\n")
	if len(probs) != 1 {
		t.Fatalf("problems = %v, want one", probs)
	}
	p := probs[0]
	if p.Line != 2 {
		t.Errorf("line = %d, want 2", p.Line)
	}
	if !strings.Contains(p.Msg, `prefix key "z"`) || !strings.Contains(p.Msg, "split-h") || !strings.Contains(p.Msg, "zoom") {
		t.Errorf("msg = %q, want the key and both operations", p.Msg)
	}
}

func TestLoadBadKeyLists(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"scalar", "open: enter\n", "must be a list of keys"},
		{"empty", "open: []\n", "binds no keys"},
		{"nested", "open:\n  - [o]\n", "not a name"},
		{"blank", "open: [\"\"]\n", "empty key"},
		{"prefix section scalar", "prefix: split-h\n", "must be a mapping"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probs := loadBad(t, c.body)
			if !strings.Contains(probs[0].Msg, c.want) {
				t.Errorf("msg = %q, want %q", probs[0].Msg, c.want)
			}
		})
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	probs := loadBad(t, "open: [enter\n")
	if !strings.Contains(probs[0].Msg, "invalid YAML") {
		t.Fatalf("msg = %q, want an invalid YAML report", probs[0].Msg)
	}
	if probs[0].Err().Kind != "config" {
		t.Errorf("kind = %q, want config", probs[0].Err().Kind)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	probs := loadBad(t, "bogus_one: [a]\nbogus_two: [b]\n")
	if len(probs) != 2 {
		t.Fatalf("problems = %v, want one per mistake", probs)
	}
	if err := Error(probs); err == nil || !strings.Contains(err.Error(), "bogus_two") {
		t.Errorf("joined error = %v, want every problem", err)
	}
	if Error(nil) != nil {
		t.Error("Error(nil) must be nil")
	}
}

// TestLoadShippedTemplate loads the keymap.yaml `jumper init` writes: it must be
// valid and must spell every default, so the template cannot drift from the map.
func TestLoadShippedTemplate(t *testing.T) {
	o, probs := Load(filepath.Join("..", "..", "app", "templates", "keymap.yaml"))
	if len(probs) > 0 {
		t.Fatalf("the shipped keymap.yaml is rejected: %v", probs)
	}
	got, want := o.Map(Default()), Default()
	for _, b := range globalBindings(want) {
		gotKeys := bindingOf(got, b.Name)
		if !reflect.DeepEqual(gotKeys, b.Bind.Keys) {
			t.Errorf("%s = %v in the template, want the default %v", b.Name, gotKeys, b.Bind.Keys)
		}
	}
	if len(o.Prefix) != 0 {
		t.Errorf("the template overrides %d prefix bindings, want none", len(o.Prefix))
	}
}

// bindingOf returns the keys of a named global binding.
func bindingOf(m *Map, name string) []string {
	for _, b := range globalBindings(m) {
		if b.Name == name {
			return b.Bind.Keys
		}
	}
	return nil
}

func TestRowsCoverEveryBinding(t *testing.T) {
	rows := Rows(Default())
	if len(rows) != 24 {
		t.Fatalf("rows = %d, want one per binding (24)", len(rows))
	}
	if rows[0].Name != "up" || rows[len(rows)-1].Name != "errors" {
		t.Errorf("rows start %q and end %q, want declaration order", rows[0].Name, rows[len(rows)-1].Name)
	}
	if rows[0].Help != "up" {
		t.Errorf("help = %q, want the binding's help text", rows[0].Help)
	}
}

func TestPrefixRowsAreSortedAndGrouped(t *testing.T) {
	rows := PrefixRows(mux.DefaultKeyTable())
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if len(names) != len(rows) {
		t.Fatal("duplicate names")
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("names are not sorted: %q before %q", names[i-1], names[i])
		}
	}
	for _, r := range rows {
		if r.Name == "split-h" {
			if got := strings.Join(r.Keys, " "); got != "% |" {
				t.Errorf("split-h keys = %q, want the two default keys", got)
			}
		}
	}
}

func TestTextShowsEffectiveMap(t *testing.T) {
	o := loadOK(t, "open: [o]\nprefix:\n  split-h: [\"|\"]\n")
	text := Text(o.Map(Default()), o.KeyTable(mux.DefaultKeyTable()), "ctrl+a")
	for _, want := range []string{"global", "prefix C-a", "open", "o", "split-h"} {
		if !strings.Contains(text, want) {
			t.Errorf(":keys text is missing %q:\n%s", want, text)
		}
	}
}

func TestDocMentionsOverrides(t *testing.T) {
	doc := Doc(Default(), mux.DefaultKeyTable(), "ctrl+a")
	for _, want := range []string{"# Key bindings", "keymap.yaml", "## Prefix bindings", "`C-a`"} {
		if !strings.Contains(doc, want) {
			t.Errorf("KEYS doc is missing %q", want)
		}
	}
}

// TestKEYSGolden compares docs/KEYS.md with the defaults, so the document the
// user reads cannot drift from the bindings. Run with -update to regenerate.
func TestKEYSGolden(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "KEYS.md")
	want := Doc(Default(), mux.DefaultKeyTable(), "ctrl+a")
	if *update {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run with -update to create it)", path, err)
	}
	if string(got) != want {
		t.Errorf("%s is out of date; run: go test ./internal/tui/keymap/ -update", path)
	}
}
