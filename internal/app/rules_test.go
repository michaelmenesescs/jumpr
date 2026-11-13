package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// teaPath is the Bubble Tea v1 import path; only a few selectors on it are
// forbidden, because the rest of the package is fine to use.
const teaPath = "github.com/charmbracelet/bubbletea"

// rule is one detach-safety rule (BUILD-PLAN §6.1) over paths under internal/:
// the uses it forbids, and the directories exempt from it.
type rule struct {
	// what starts the failure message ("use of os.Getenv").
	what string
	// pkg is the import path the rule is about.
	pkg string
	// names are the forbidden selectors of pkg.
	names []string
	// imports forbids importing pkg at all.
	imports bool
	// calls flags the names only where they are called, not where they are
	// mentioned.
	calls bool
	// allow lists the directories under internal/ that may break the rule.
	allow []string
}

// detachRules is BUILD-PLAN §6.1 made executable: only internal/app may touch
// the process's standard streams or environment, or print; nothing may open a
// subprocess except the transports; and nothing may hand the terminal to
// another process.
var detachRules = []rule{
	{what: "use of", pkg: "os",
		names: []string{"Stdin", "Stdout", "Stderr", "Exit", "Getenv", "LookupEnv"},
		// labtest is the //go:build lab harness: it reads LAB_* variables the
		// lab sets for its own tests and is never linked into the binary.
		allow: []string{"app", "labtest"}},
	{what: "call to", pkg: "fmt", calls: true,
		names: []string{"Print", "Printf", "Println"}},
	{what: "call to", pkg: "log", calls: true,
		names: []string{"Print", "Printf", "Println", "Fatal", "Fatalf", "Fatalln",
			"Panic", "Panicf", "Panicln"}},
	{what: "import of", pkg: "os/exec", imports: true,
		// labtest runs the lab's provisioning scripts; it is //go:build lab
		// tooling, never part of the product binary.
		allow: []string{"app", "transport/sysssh", "transport/httpx", "labtest"}},
	{what: "call to", pkg: teaPath, calls: true,
		names: []string{"ExecProcess", "Suspend", "Printf", "Println"}},
}

// TestDetachSafetyRules walks every non-test .go file under internal/ and fails,
// listing file and line, on anything that would tie the app to a real terminal,
// a real stdout or a subprocess. It keeps the rules honest while the session
// server (M5) turns the app into a program over a byte stream.
func TestDetachSafetyRules(t *testing.T) {
	root := filepath.Join("..", "..")
	internal := filepath.Join(root, "internal")
	info, err := os.Stat(internal)
	if err != nil || !info.IsDir() {
		t.Fatalf("internal directory under %s: %v", root, err)
	}

	var problems []string
	walkErr := filepath.WalkDir(internal, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		problems = append(problems, checkDetachFile(root, path)...)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", internal, walkErr)
	}
	if len(problems) > 0 {
		t.Fatalf("detach-safety rules broken (BUILD-PLAN §6.1):\n\t%s", strings.Join(problems, "\n\t"))
	}
}

// checkDetachFile parses one file and returns one message per violation, each
// prefixed with file:line.
func checkDetachFile(root, path string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return []string{path + ": parse: " + err.Error()}
	}
	rel := relUnderInternal(root, path)

	// Import path by local name, so an alias cannot hide a package.
	type importEntry struct {
		name string
		path string
		pos  token.Pos
	}
	var entries []importEntry
	byPath := map[string]string{}
	for _, spec := range f.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := importPath[strings.LastIndex(importPath, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		entries = append(entries, importEntry{name: name, path: importPath, pos: spec.Pos()})
		byPath[name] = importPath
	}

	var problems []string
	report := func(pos token.Pos, what string) {
		problems = append(problems, fset.Position(pos).String()+": "+what)
	}

	for _, r := range detachRules {
		if !r.imports || r.exempt(rel) {
			continue
		}
		for _, e := range entries {
			if e.path == r.pkg {
				report(e.pos, "import of "+r.pkg)
			}
		}
	}

	check := func(sel *ast.SelectorExpr, inCall bool) {
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Obj != nil {
			// A field on a value, or a local that shadows the package name.
			return
		}
		pkgPath, ok := byPath[x.Name]
		if !ok {
			return
		}
		for _, r := range detachRules {
			if r.imports || r.pkg != pkgPath || r.exempt(rel) || (r.calls && !inCall) {
				continue
			}
			for _, name := range r.names {
				if sel.Sel.Name != name {
					continue
				}
				report(sel.Pos(), r.what+" "+x.Name+"."+name)
			}
		}
	}

	// Which selectors are the function of a call: "tea.Printf" as a value is
	// not the same as calling it.
	callees := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				callees[sel] = true
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			check(sel, callees[sel])
		}
		return true
	})
	return problems
}

// exempt reports whether rel (a path under internal/) may break rule r.
func (r rule) exempt(rel string) bool {
	for _, dir := range r.allow {
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

// relUnderInternal is path relative to internal/, slash-separated.
func relUnderInternal(root, path string) string {
	rel, err := filepath.Rel(filepath.Join(root, "internal"), path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
