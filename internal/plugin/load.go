package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"

	"jumpr/internal/errs"
)

var fieldNotFoundRe = regexp.MustCompile(`^field (\w+) not found in type `)

// Problem is one reason a plugin file was rejected: where it is (file, line,
// field) and what is wrong. LoadDir returns one Problem per distinct mistake and
// never stops at the first bad file.
type Problem struct {
	File  string // path of the plugin file
	Line  int    // 1-based line, 0 when YAML could not place it
	Field string // dotted field path, e.g. "parse.pattern" or "actions[0].confirm", when known
	Msg   string
}

// String renders the problem as "file:line: field: message", skipping the parts
// that are unknown.
func (p Problem) String() string {
	var b strings.Builder
	b.WriteString(p.File)
	if p.Line > 0 {
		fmt.Fprintf(&b, ":%d", p.Line)
	}
	if p.Field != "" {
		b.WriteString(": " + p.Field)
	}
	b.WriteString(": " + p.Msg)
	return b.String()
}

// Err returns the problem as the *errs.Error that may cross a package boundary.
func (p Problem) Err() *errs.Error {
	return &errs.Error{Kind: errs.Config, Op: "plugin.load", Msg: p.String()}
}

// LoadDir loads every *.yaml and *.yml file in dir, in file-name order. A
// missing directory is not an error (no plugins configured); every other
// problem is reported per file and never hides the other files.
func LoadDir(dir string) (ok []Definition, problems []Problem) {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, []Problem{{File: dir, Msg: "cannot read plugin directory: " + err.Error()}}
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isPluginFile(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	seen := make(map[string]string, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		def, probs := loadFile(path)
		if len(probs) > 0 {
			problems = append(problems, probs...)
			continue
		}
		if prev, dup := seen[def.Name]; dup {
			problems = append(problems, Problem{
				File:  path,
				Line:  def.Line,
				Field: "name",
				Msg:   fmt.Sprintf("plugin %q is already defined in %s", def.Name, prev),
			})
			continue
		}
		seen[def.Name] = path
		ok = append(ok, def)
	}
	return ok, problems
}

// isPluginFile reports whether name is a YAML plugin file. Dotfiles are
// skipped so editor swap files never load.
func isPluginFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// loadFile decodes and validates one file. A file is either fully valid or its
// problems are returned; the Definition is then unusable.
func loadFile(path string) (Definition, []Problem) {
	var def Definition
	data, err := os.ReadFile(path)
	if err != nil {
		return def, []Problem{{File: path, Msg: "cannot read plugin file: " + err.Error()}}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return def, []Problem{{File: path, Msg: "plugin file is empty"}}
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return def, yamlProblems(path, err)
	}
	lines := indexFile(&doc)

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&def); err != nil && !errors.Is(err, io.EOF) {
		return def, yamlProblems(path, err)
	}

	def.File = path
	def.Line = lines.line("name")
	if def.Line == 0 {
		def.Line = 1
	}
	c := &checker{file: path, lines: lines}
	c.validate(&def)
	return def, c.probs
}

// yamlProblems turns a yaml.v3 decoding failure into Problems, one per message,
// with the line number YAML reported.
func yamlProblems(path string, err error) []Problem {
	var te *yaml.TypeError
	if errors.As(err, &te) && len(te.Errors) > 0 {
		probs := make([]Problem, 0, len(te.Errors))
		for _, msg := range te.Errors {
			probs = append(probs, oneYAMLProblem(path, msg))
		}
		return probs
	}
	return []Problem{oneYAMLProblem(path, err.Error())}
}

func oneYAMLProblem(path, msg string) Problem {
	p := Problem{File: path, Msg: strings.TrimSpace(msg)}
	p.Msg = strings.TrimPrefix(p.Msg, "yaml: ")
	if m := lineRe.FindStringSubmatch(p.Msg); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			p.Line = n
		}
		p.Msg = m[2]
	}
	if m := fieldNotFoundRe.FindStringSubmatch(p.Msg); m != nil {
		p.Field = m[1]
		p.Msg = "unknown field"
	}
	if p.Line == 0 {
		p.Line = 1
	}
	return p
}

// nodeIndex remembers the YAML line of every field path in one file, so
// validation messages point at the line the author has to edit.
type nodeIndex map[string]*yaml.Node

func indexFile(doc *yaml.Node) nodeIndex {
	idx := make(nodeIndex)
	if doc == nil {
		return idx
	}
	root := doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	idx.index(root, "")
	return idx
}

func (idx nodeIndex) index(n *yaml.Node, path string) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			idx.index(c, path)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			p := key.Value
			if path != "" {
				p = path + "." + key.Value
			}
			idx[p] = key
			idx.index(val, p)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			idx.index(c, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

// line returns the line of path, or of its closest indexed parent.
func (idx nodeIndex) line(path string) int {
	for p := path; p != ""; p = parentPath(p) {
		if n, ok := idx[p]; ok {
			return n.Line
		}
	}
	return 0
}

// parentPath drops the last element of a dotted path ("a.b" → "a",
// "a[0].b" → "a[0]", "a[0]" → "a").
func parentPath(p string) string {
	if i := strings.LastIndexByte(p, '.'); i >= 0 {
		return p[:i]
	}
	if i := strings.IndexByte(p, '['); i >= 0 {
		return p[:i]
	}
	return ""
}

// checker collects the problems of one file.
type checker struct {
	file  string
	lines nodeIndex
	probs []Problem
}

func (c *checker) problem(field, format string, args ...any) {
	line := c.lines.line(field)
	if line == 0 {
		line = 1
	}
	c.probs = append(c.probs, Problem{
		File:  c.file,
		Line:  line,
		Field: field,
		Msg:   fmt.Sprintf(format, args...),
	})
}

// validate runs every rule of SPEC §12 against one decoded file.
func (c *checker) validate(d *Definition) {
	if d.APIVersion != APIVersion {
		c.problem("apiVersion", "apiVersion must be %q, not %q", APIVersion, d.APIVersion)
	}
	if d.Kind != KindPlugin {
		c.problem("kind", "kind must be %q, not %q", KindPlugin, d.Kind)
	}
	if !nameRe.MatchString(d.Name) {
		c.problem("name", "name %q must match %s", d.Name, nameRe.String())
	}
	switch d.Target {
	case TargetHost, TargetK8s, TargetHTTP, TargetInventory:
	default:
		c.problem("target", "target %q must be one of host, k8s, http, inventory", d.Target)
	}

	declared := c.params(d)
	c.command(d, declared)
	c.parse(d)
	c.view(d)
	c.request(d, declared)
	c.links(d, declared)
	c.actions(d, declared)

	switch d.EmitsHosts {
	case "", TargetHost:
	default:
		c.problem("emits_hosts", "emits_hosts %q must be %q or empty", d.EmitsHosts, TargetHost)
	}
}

// params validates the declared parameters and returns their names as a set.
func (c *checker) params(d *Definition) map[string]bool {
	declared := make(map[string]bool, len(d.Params))
	for i, p := range d.Params {
		field := fmt.Sprintf("params[%d]", i)
		if strings.TrimSpace(p.Name) == "" {
			c.problem(field, "a declared parameter needs a name")
			continue
		}
		if declared[p.Name] {
			c.problem(field, "parameter %q is declared twice", p.Name)
			continue
		}
		declared[p.Name] = true
	}
	return declared
}

func (c *checker) command(d *Definition, declared map[string]bool) {
	switch d.Target {
	case TargetHost, TargetK8s:
		if strings.TrimSpace(d.Command) == "" {
			c.problem("command", "a %s plugin needs a command", d.Target)
			return
		}
	case TargetInventory:
		if strings.TrimSpace(d.Command) == "" && d.Request == nil {
			c.problem("command", "an inventory plugin needs a command or a request")
			return
		}
	}
	if strings.TrimSpace(d.Command) == "" {
		return
	}
	c.template("command", d.Command, declared)
}

// template checks that every {{ .Param "x" }} of text is a declared parameter.
// The construct restriction itself lives in parseTemplate, so a command can
// only reference .Param and .Host and every substitution is shell-quoted.
func (c *checker) template(field, text string, declared map[string]bool) {
	parts, err := parseTemplate(text)
	if err != nil {
		c.problem(field, "%s", err.Error())
		return
	}
	seen := make(map[string]bool)
	for _, name := range refs(parts) {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !declared[name] {
			c.problem(field, "template references undeclared param %q; declare it under params", name)
		}
	}
}

func (c *checker) parse(d *Definition) {
	p := d.Parse
	switch p.Type {
	case ParseJSON, ParseLines, ParseRaw:
	case ParseJQ:
		if strings.TrimSpace(p.Expr) == "" {
			c.problem("parse.expr", "a jq parse needs expr")
			break
		}
		if _, err := gojq.Parse(p.Expr); err != nil {
			c.problem("parse.expr", "jq expression does not compile: %s", err)
		}
	case ParseRegex:
		re := c.regex("parse.pattern", p.Pattern)
		if re == nil {
			break
		}
		if len(namedGroups(re)) == 0 {
			c.problem("parse.pattern", "regex needs at least one named group, like (?P<name>...)")
		}
	case ParseColumns:
		if p.Skip < 0 {
			c.problem("parse.skip", "skip must not be negative")
		}
		if p.Header && len(p.Names) > 0 {
			c.problem("parse.names", "a columns parse takes header: true or names, not both")
		}
		if !p.Header && len(p.Names) == 0 {
			c.problem("parse.names", "a columns parse needs header: true or names")
		}
		seen := make(map[string]bool, len(p.Names))
		for i, n := range p.Names {
			field := fmt.Sprintf("parse.names[%d]", i)
			if strings.TrimSpace(n) == "" {
				c.problem(field, "column name is empty")
				continue
			}
			if seen[n] {
				c.problem(field, "column %q is listed twice", n)
				continue
			}
			seen[n] = true
		}
	case ParseKV:
		switch p.Separator {
		case "", "=", ":":
		default:
			c.problem("parse.separator", "separator %q must be \"=\" or \":\"", p.Separator)
		}
	default:
		c.problem("parse.type", "parse type %q must be one of json, jq, regex, columns, kv, lines, raw", p.Type)
	}
}

func (c *checker) regex(field, pattern string) *regexp.Regexp {
	if strings.TrimSpace(pattern) == "" {
		c.problem(field, "a regex parse needs pattern")
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		c.problem(field, "regex does not compile: %s", err)
		return nil
	}
	return re
}

// view checks that the declared columns exist in what the parse produces, when
// that is knowable without running the plugin. "host" is always allowed: the
// plugin backend adds it to every host table (task 117).
func (c *checker) view(d *Definition) {
	fields, known := parseFields(d.Parse)
	seen := make(map[string]bool, len(d.View.Columns))
	for i, col := range d.View.Columns {
		field := fmt.Sprintf("view.columns[%d]", i)
		if strings.TrimSpace(col) == "" {
			c.problem(field, "column name is empty")
			continue
		}
		if seen[col] {
			c.problem(field, "column %q is listed twice", col)
			continue
		}
		seen[col] = true
		if !known || col == "host" {
			continue
		}
		if !containsString(fields, col) {
			c.problem(field, "column %q is not produced by the %s parse (it has %s)",
				col, d.Parse.Type, strings.Join(fields, ", "))
		}
	}
}

// parseFields reports the column names a parse definition produces, when they
// are known statically. json and jq outputs are only known at run time.
func parseFields(d ParseDef) ([]string, bool) {
	switch d.Type {
	case ParseRegex:
		re, err := regexp.Compile(d.Pattern)
		if err != nil {
			return nil, false
		}
		names := namedGroups(re)
		return names, len(names) > 0
	case ParseColumns:
		if len(d.Names) > 0 {
			return d.Names, true
		}
		return nil, false
	case ParseLines:
		return []string{"line"}, true
	case ParseRaw:
		return []string{"output"}, true
	}
	return nil, false
}

func namedGroups(re *regexp.Regexp) []string {
	all := re.SubexpNames()
	out := make([]string, 0, len(all))
	for _, n := range all {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

func (c *checker) request(d *Definition, declared map[string]bool) {
	if d.Target != TargetHTTP && d.Target != TargetInventory {
		if d.Request != nil {
			c.problem("request", "request is only allowed for http and inventory plugins")
		}
		return
	}
	r := d.Request
	if r == nil {
		if d.Target == TargetHTTP {
			c.problem("request", "a http plugin needs a request")
		}
		return
	}
	switch method := strings.ToUpper(strings.TrimSpace(r.Method)); method {
	case "":
		c.problem("request.method", "a http plugin needs request.method")
	case "GET", "HEAD":
	case "POST", "PUT", "PATCH", "DELETE":
		if !r.Safe {
			c.problem("request.method", "method %s changes state; declare safe: true only if it cannot", method)
		}
	default:
		c.problem("request.method", "method %q is not allowed", r.Method)
	}

	if strings.TrimSpace(r.URL) == "" {
		c.problem("request.url", "a http plugin needs request.url")
		return
	}
	lower := strings.ToLower(strings.TrimSpace(r.URL))
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		c.problem("request.url", "request.url must start with http:// or https://")
	}
	c.template("request.url", r.URL, declared)
}

func (c *checker) links(d *Definition, declared map[string]bool) {
	for i, l := range d.Links {
		field := fmt.Sprintf("links[%d]", i)
		if strings.TrimSpace(l.Label) == "" {
			c.problem(field+".label", "a link needs a label")
		}
		if strings.TrimSpace(l.To) == "" {
			c.problem(field+".to", "a link needs to")
		}
		keys := make([]string, 0, len(l.Args))
		for k := range l.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c.template(field+".args."+k, l.Args[k], declared)
		}
	}
}

// actions validates the write the plugin offers. Every action runs through the
// action bus, which asks before it writes, so the file must say confirm: true.
func (c *checker) actions(d *Definition, declared map[string]bool) {
	seen := make(map[string]bool, len(d.Actions))
	for i, a := range d.Actions {
		field := fmt.Sprintf("actions[%d]", i)
		switch {
		case !nameRe.MatchString(a.Name):
			c.problem(field+".name", "action name %q must match %s", a.Name, nameRe.String())
		case seen[a.Name]:
			c.problem(field+".name", "action %q is declared twice", a.Name)
		default:
			seen[a.Name] = true
		}
		if strings.TrimSpace(a.Command) == "" {
			c.problem(field+".command", "an action needs a command")
		} else {
			c.template(field+".command", a.Command, declared)
		}
		if !a.Confirm {
			c.problem(field+".confirm", "an action must set confirm: true; every action runs through the bus and asks first")
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// fileStamp is the part of a file's identity Watch compares.
type fileStamp struct {
	mod  time.Time
	size int64
}

// Watch polls dir every `every` and calls onChange once per detected change of
// the plugin files (add, remove, mtime or size). It polls mtimes so it needs no
// fsnotify dependency, and returns when ctx is done. onChange runs on the
// caller's goroutine, so it must not block for long.
func Watch(ctx context.Context, dir string, every time.Duration, onChange func()) {
	if every <= 0 {
		every = time.Second
	}
	last := snapshot(dir)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur := snapshot(dir)
			if sameSnapshot(last, cur) {
				continue
			}
			last = cur
			if onChange != nil {
				onChange()
			}
		}
	}
}

// snapshot records the plugin files of dir and their stamps.
func snapshot(dir string) map[string]fileStamp {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return map[string]fileStamp{}
	}
	out := make(map[string]fileStamp, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isPluginFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[e.Name()] = fileStamp{mod: info.ModTime(), size: info.Size()}
	}
	return out
}

func sameSnapshot(a, b map[string]fileStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for name, x := range a {
		y, ok := b[name]
		if !ok || !x.mod.Equal(y.mod) || x.size != y.size {
			return false
		}
	}
	return true
}
