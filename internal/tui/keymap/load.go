package keymap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"jumpr/internal/errs"
	"jumpr/internal/mux"
)

// prefixSection is the one reserved top-level key of keymap.yaml: it holds the
// overrides of the mux prefix key table. Every other top-level key names a
// binding of Map.
const prefixSection = "prefix"

// Overrides is a parsed keymap.yaml: the bindings it replaces, by name.
type Overrides struct {
	File   string              // path the overrides came from
	Global map[string][]string // Map binding name -> keys
	Prefix map[string][]string // prefix name ("split-h", "focus-left") -> keys
	lines  map[bindingKey]int  // where each override was written, for conflict reports
}

// bindingKey identifies one override for line lookups.
type bindingKey struct {
	section string // "global" or prefixSection
	name    string
}

// globalBinding is one named binding of the global map.
type globalBinding struct {
	Name string
	Bind *Binding
}

// globalBindings lists every binding of m in the canonical order used by the
// key help and docs/KEYS.md: the declaration order of Map.
func globalBindings(m *Map) []globalBinding {
	return []globalBinding{
		{"up", &m.Up},
		{"down", &m.Down},
		{"page_up", &m.PageUp},
		{"page_down", &m.PageDown},
		{"top", &m.Top},
		{"bottom", &m.Bottom},
		{"open", &m.Open},
		{"back", &m.Back},
		{"command", &m.Command},
		{"filter", &m.Filter},
		{"fuzzy_find", &m.FuzzyFind},
		{"help", &m.Help},
		{"refresh", &m.Refresh},
		{"quit", &m.Quit},
		{"actions", &m.Actions},
		{"note", &m.Note},
		{"finding", &m.Finding},
		{"knowledge", &m.Knowledge},
		{"timeline", &m.Timeline},
		{"yank", &m.Yank},
		{"follow", &m.Follow},
		{"time_window", &m.TimeWindow},
		{"diff", &m.Diff},
		{"errors", &m.Errors},
	}
}

// isGlobalName reports whether name is a binding of the global map.
func isGlobalName(name string) bool {
	for _, b := range globalBindings(Default()) {
		if b.Name == name {
			return true
		}
	}
	return false
}

// prefixName is the name a key table entry is overridden by: the operation, plus
// its argument when it has one ("focus-left", "select-window-1").
func prefixName(op mux.OpMsg) string {
	if op.Arg == "" {
		return op.Op
	}
	return op.Op + "-" + op.Arg
}

// prefixNames maps every name of the built-in prefix table to its operation.
func prefixNames() map[string]mux.OpMsg {
	t := mux.DefaultKeyTable()
	names := make(map[string]mux.OpMsg, len(t))
	for _, op := range t {
		names[prefixName(op)] = op
	}
	return names
}

// Load reads a keymap.yaml: the user's overrides of the global bindings and of
// the mux prefix key table (SPEC §13, §21.2). Every top-level key names a
// binding of Map (`up`, `page_up`, `fuzzy_find`, …); the single reserved key
// `prefix` holds a mapping of prefix names (`split-h`, `focus-left`, …) to key
// lists. A missing file yields empty overrides and no problems, so jumpr runs
// on its defaults without one.
//
// Load reports every problem it finds — an unknown name, a malformed key list,
// a name set twice, or one key bound to two actions — each with the line it was
// written on.
func Load(path string) (*Overrides, []Problem) {
	o := &Overrides{
		File:   path,
		Global: map[string][]string{},
		Prefix: map[string][]string{},
		lines:  map[bindingKey]int{},
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return o, nil
	case err != nil:
		return o, []Problem{{File: path, Msg: "cannot read keymap file: " + err.Error()}}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return o, []Problem{{File: path, Msg: "invalid YAML: " + err.Error()}}
	}
	if len(doc.Content) == 0 {
		return o, nil // empty file: defaults
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return o, nil
	}
	if root.Kind != yaml.MappingNode {
		return o, []Problem{{File: path, Line: root.Line,
			Msg: "keymap must be a mapping of binding names to key lists"}}
	}

	var probs []Problem
	for i := 0; i+1 < len(root.Content); i += 2 {
		nameNode, valNode := root.Content[i], root.Content[i+1]
		name := strings.TrimSpace(nameNode.Value)
		if name == prefixSection {
			probs = append(probs, o.readPrefix(path, valNode)...)
			continue
		}
		if !isGlobalName(name) {
			if _, isPrefix := prefixNames()[name]; isPrefix {
				probs = append(probs, Problem{File: path, Line: nameNode.Line,
					Msg: fmt.Sprintf("unknown binding %q: %q is a prefix binding, so it belongs under %q",
						name, name, prefixSection)})
				continue
			}
			probs = append(probs, Problem{File: path, Line: nameNode.Line,
				Msg: fmt.Sprintf("unknown binding %q", name)})
			continue
		}
		keys, bad := readKeys(path, nameNode, valNode)
		if bad != nil {
			probs = append(probs, *bad)
			continue
		}
		if _, dup := o.Global[name]; dup {
			probs = append(probs, Problem{File: path, Line: nameNode.Line,
				Msg: fmt.Sprintf("binding %q is set twice", name)})
			continue
		}
		o.Global[name] = keys
		o.lines[bindingKey{section: "global", name: name}] = nameNode.Line
	}
	probs = append(probs, o.conflicts(path)...)
	return o, probs
}

// readPrefix reads the `prefix:` section of the file.
func (o *Overrides) readPrefix(path string, node *yaml.Node) []Problem {
	if node.Kind != yaml.MappingNode {
		return []Problem{{File: path, Line: node.Line,
			Msg: fmt.Sprintf("%q must be a mapping of prefix names to key lists", prefixSection)}}
	}
	known := prefixNames()
	var probs []Problem
	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, valNode := node.Content[i], node.Content[i+1]
		name := strings.TrimSpace(nameNode.Value)
		if _, ok := known[name]; !ok {
			msg := fmt.Sprintf("unknown prefix binding %q", name)
			if isGlobalName(name) {
				msg = fmt.Sprintf("unknown prefix binding %q: %q is a global binding, so it belongs at the top level",
					name, name)
			}
			probs = append(probs, Problem{File: path, Line: nameNode.Line, Msg: msg})
			continue
		}
		keys, bad := readKeys(path, nameNode, valNode)
		if bad != nil {
			probs = append(probs, *bad)
			continue
		}
		if _, dup := o.Prefix[name]; dup {
			probs = append(probs, Problem{File: path, Line: nameNode.Line,
				Msg: fmt.Sprintf("prefix binding %q is set twice", name)})
			continue
		}
		o.Prefix[name] = keys
		o.lines[bindingKey{section: prefixSection, name: name}] = nameNode.Line
	}
	return probs
}

// readKeys reads one binding's key list. A binding must name at least one key:
// an action nobody can trigger is a mistake, not an instruction.
func readKeys(path string, nameNode, valNode *yaml.Node) ([]string, *Problem) {
	if valNode.Kind != yaml.SequenceNode {
		return nil, &Problem{File: path, Line: nameNode.Line,
			Msg: fmt.Sprintf("%q must be a list of keys, like [%s]", nameNode.Value,
				strings.Join(defaultKeys(nameNode.Value), ", "))}
	}
	keys := make([]string, 0, len(valNode.Content))
	for _, item := range valNode.Content {
		if item.Kind != yaml.ScalarNode {
			return nil, &Problem{File: path, Line: item.Line,
				Msg: fmt.Sprintf("%q has a key that is not a name", nameNode.Value)}
		}
		key := strings.TrimSpace(item.Value)
		if key == "" {
			return nil, &Problem{File: path, Line: item.Line,
				Msg: fmt.Sprintf("%q has an empty key", nameNode.Value)}
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, &Problem{File: path, Line: nameNode.Line,
			Msg: fmt.Sprintf("%q binds no keys", nameNode.Value)}
	}
	return keys, nil
}

// defaultKeys lists the built-in keys of a global binding, for error text.
func defaultKeys(name string) []string {
	for _, b := range globalBindings(Default()) {
		if b.Name == name {
			return b.Bind.Keys
		}
	}
	return nil
}

// conflicts reports keys bound to more than one action, on either table. A
// conflicting key always involves an override (the defaults are conflict-free),
// so the problem carries the line of the override that introduced it.
func (o *Overrides) conflicts(path string) []Problem {
	var probs []Problem
	for _, dup := range duplicates(o.Map(Default())) {
		probs = append(probs, Problem{File: path, Line: o.lineOf("global", dup.names...),
			Msg: fmt.Sprintf("key %q is bound to both %s and %s", dup.key, dup.names[0], dup.names[1])})
	}
	for _, dup := range o.duplicatesTable(mux.DefaultKeyTable()) {
		probs = append(probs, Problem{File: path, Line: o.lineOf(prefixSection, dup.names...),
			Msg: fmt.Sprintf("prefix key %q is bound to both %s and %s", dup.key, dup.names[0], dup.names[1])})
	}
	return probs
}

// lineOf returns the line of the last given override, or 0 when none of them
// was overridden in the file.
func (o *Overrides) lineOf(section string, names ...string) int {
	line := 0
	for _, name := range names {
		if l, ok := o.lines[bindingKey{section: section, name: name}]; ok && l > line {
			line = l
		}
	}
	return line
}

// Problem is one reason a key file was rejected: where it is (file and 1-based
// line) and what is wrong. Load returns every problem it finds, not only the
// first, so one edit round can fix the whole file.
type Problem struct {
	File string
	Line int // 0 when YAML could not place it
	Msg  string
}

// String renders the problem as "file:line: message", skipping what is unknown.
func (p Problem) String() string {
	var b strings.Builder
	b.WriteString(p.File)
	if p.Line > 0 {
		fmt.Fprintf(&b, ":%d", p.Line)
	}
	b.WriteString(": " + p.Msg)
	return b.String()
}

// Err returns the problem as the *errs.Error that may cross a package boundary.
func (p Problem) Err() *errs.Error {
	return &errs.Error{Kind: errs.Config, Op: "keymap.load", Msg: p.String()}
}

// Error joins every problem into the one error a caller returns, or nil when
// there is none.
func Error(probs []Problem) error {
	if len(probs) == 0 {
		return nil
	}
	msgs := make([]string, len(probs))
	for i, p := range probs {
		msgs[i] = p.String()
	}
	return &errs.Error{Kind: errs.Config, Op: "keymap.load", Msg: strings.Join(msgs, "; ")}
}

// dupKey is one key bound to several actions of the same table.
type dupKey struct {
	key   string
	names []string
}

// duplicates lists every key bound to more than one global binding, in binding
// order so the report is stable.
func duplicates(m *Map) []dupKey {
	owners := map[string][]string{}
	var order []string
	for _, b := range globalBindings(m) {
		for _, k := range b.Bind.Keys {
			if _, seen := owners[k]; !seen {
				order = append(order, k)
			}
			owners[k] = append(owners[k], b.Name)
		}
	}
	return dupsFrom(owners, order)
}

// duplicatesTable lists every prefix key bound to more than one operation once
// the overrides are applied. Ownership is tracked by operation name, not by the
// table map, so overriding one operation's keys does not silently hide a key it
// took from another one.
func (o *Overrides) duplicatesTable(base mux.KeyTable) []dupKey {
	owners := map[string][]string{}
	var order []string
	add := func(key, name string) {
		if _, seen := owners[key]; !seen {
			order = append(order, key)
		}
		for _, have := range owners[key] {
			if have == name {
				return // an operation may own several keys; it cannot own one twice
			}
		}
		owners[key] = append(owners[key], name)
	}
	for k, op := range base {
		name := prefixName(op)
		if _, overridden := o.Prefix[name]; overridden {
			continue // the file decides this operation's keys
		}
		add(k, name)
	}
	names := make([]string, 0, len(o.Prefix))
	for name := range o.Prefix {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, k := range o.Prefix[name] {
			add(k, name)
		}
	}
	sort.Strings(order)
	return dupsFrom(owners, order)
}

// dupsFrom keeps the keys with more than one owner, in the given order.
func dupsFrom(owners map[string][]string, order []string) []dupKey {
	var out []dupKey
	for _, k := range order {
		if names := owners[k]; len(names) > 1 {
			out = append(out, dupKey{key: k, names: names})
		}
	}
	return out
}

// Map returns a copy of base with these overrides applied. base is not
// modified; a nil Overrides yields the defaults unchanged.
func (o *Overrides) Map(base *Map) *Map {
	if base == nil {
		base = Default()
	}
	m := *base
	for _, b := range globalBindings(&m) {
		b.Bind.Keys = append([]string(nil), b.Bind.Keys...)
	}
	if o == nil {
		return &m
	}
	for _, b := range globalBindings(&m) {
		if keys, ok := o.Global[b.Name]; ok {
			if keys == nil {
				keys = []string{}
			}
			b.Bind.Keys = append([]string(nil), keys...)
		}
	}
	return &m
}

// KeyTable returns a copy of base with these overrides applied: an overridden
// name keeps its operation and takes the keys the file lists, and the keys it
// had in base are released. base is not modified.
func (o *Overrides) KeyTable(base mux.KeyTable) mux.KeyTable {
	if base == nil {
		base = mux.DefaultKeyTable()
	}
	t := make(mux.KeyTable, len(base))
	ops := make(map[string]mux.OpMsg, len(base))
	for k, v := range base {
		t[k] = v
		ops[prefixName(v)] = v
	}
	if o == nil {
		return t
	}
	names := make([]string, 0, len(o.Prefix))
	for name := range o.Prefix {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		op, ok := ops[name]
		if !ok {
			continue // validated at load time against the defaults
		}
		for k, v := range t {
			if v == op {
				delete(t, k)
			}
		}
		for _, k := range o.Prefix[name] {
			t[k] = op
		}
	}
	return t
}

// Row is one binding of the effective key map.
type Row struct {
	Name string   // "up", "page_up", or a prefix name like "split-h"
	Keys []string // Bubble Tea key strings, as bound
	Help string   // one-line description; empty for prefix bindings
}

// Rows returns the effective global bindings, in declaration order.
func Rows(m *Map) []Row {
	if m == nil {
		m = Default()
	}
	out := make([]Row, 0, 24)
	for _, b := range globalBindings(m) {
		out = append(out, Row{Name: b.Name, Keys: append([]string(nil), b.Bind.Keys...), Help: b.Bind.Help})
	}
	return out
}

// PrefixRows returns the effective prefix table, sorted by name.
func PrefixRows(t mux.KeyTable) []Row {
	byName := map[string][]string{}
	for k, op := range t {
		name := prefixName(op)
		byName[name] = append(byName[name], k)
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Row, 0, len(names))
	for _, name := range names {
		keys := byName[name]
		sort.SliceStable(keys, func(i, j int) bool { return keyOrder(keys[i]) < keyOrder(keys[j]) })
		out = append(out, Row{Name: name, Keys: keys})
	}
	return out
}

// keyOrder sorts keys for display: plain characters first, then named keys and
// control keys, each group alphabetically. It only has to be stable.
func keyOrder(k string) string {
	if len([]rune(k)) == 1 {
		return "0" + k
	}
	return "1" + k
}

// keyList renders keys for humans: `ctrl+a` as `C-a`, one per row separated by
// commas, an empty binding as `(none)`.
func keyList(keys []string) string {
	if len(keys) == 0 {
		return "(none)"
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = DisplayKey(k)
	}
	return strings.Join(out, ", ")
}

// keyListMarkdown renders keys for a Markdown table cell: a space key becomes
// `space` and a pipe is escaped, so neither can break the row.
func keyListMarkdown(keys []string) string {
	if len(keys) == 0 {
		return "(none)"
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		switch k {
		case " ":
			out[i] = "space"
		default:
			out[i] = strings.ReplaceAll(DisplayKey(k), "|", `\|`)
		}
	}
	return strings.Join(out, ", ")
}

// Doc renders the effective key map as Markdown. docs/KEYS.md is this function
// applied to the defaults, written by the test with -update, so the document
// cannot drift from the bindings.
func Doc(m *Map, table mux.KeyTable, prefixKey string) string {
	prefix := DisplayKey(prefixKey)
	var b strings.Builder
	b.WriteString("# Key bindings\n\n")
	b.WriteString("_Generated from the built-in defaults by `go test ./internal/tui/keymap/ -update`.\n")
	b.WriteString("Do not edit by hand: change the bindings, then regenerate._\n\n")
	b.WriteString("Every binding below can be overridden in `keymap.yaml` in the configuration\n")
	b.WriteString("directory (`~/.config/jumpr/keymap.yaml`) by its name, without rebuilding\n")
	b.WriteString("jumpr. The prefix key itself is `mux.prefix` in `config.yaml` (default ")
	b.WriteString("`" + prefix + "`).\n\n")
	b.WriteString("A key list replaces the default list completely, so name every key you want\n")
	b.WriteString("to keep. An unknown name or a key bound to two actions is a configuration\n")
	b.WriteString("error naming the line to fix; `:keys` shows the map jumpr is running with.\n\n")

	b.WriteString("## View and global bindings\n\n")
	b.WriteString("These work inside a view pane without the prefix.\n\n")
	b.WriteString("| Name | Keys | Action |\n|---|---|---|\n")
	for _, r := range Rows(m) {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.Name, keyListMarkdown(r.Keys), r.Help)
	}

	b.WriteString("\n## Prefix bindings\n\n")
	b.WriteString("These follow the prefix key `" + prefix + "` in the format `" + prefix + " <key>`,\n")
	b.WriteString("and are listed under `prefix:` in `keymap.yaml`.\n\n")
	b.WriteString("| Name | Keys |\n|---|---|\n")
	for _, r := range PrefixRows(table) {
		fmt.Fprintf(&b, "| `%s` | %s |\n", r.Name, keyListMarkdown(r.Keys))
	}
	return b.String()
}

// Text renders the effective key map for the `:keys` command: the same rows as
// Doc, one per line, without Markdown.
func Text(m *Map, table mux.KeyTable, prefixKey string) string {
	rows := Rows(m)
	width := 0
	for _, r := range rows {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	var b strings.Builder
	b.WriteString("global\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-*s  %-16s %s\n", width, r.Name, keyList(r.Keys), r.Help)
	}
	b.WriteString("\nprefix " + DisplayKey(prefixKey) + "\n")
	for _, r := range PrefixRows(table) {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, r.Name, keyList(r.Keys))
	}
	return b.String()
}
