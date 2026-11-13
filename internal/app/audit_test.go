package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// bareGoroutineAllowlist names the files under internal/ that still start a
// goroutine with a bare `go` statement, and how many they start. Every entry
// is a goroutine whose panic is not recovered at its top: the process would die
// with it. New code uses internal/safe (safe.Go, safe.GoHook), and the tasks
// that own these packages move them over one at a time; the number only ever
// goes down (task 126 requirement 1).
var bareGoroutineAllowlist = map[string]int{
	"internal/backend/chef/backend.go":         1,
	"internal/backend/elastic/backend.go":      2,
	"internal/backend/fake/fake.go":            3,
	"internal/backend/grafana/backend.go":      1,
	"internal/backend/k8s/exec.go":             1,
	"internal/backend/k8s/informers.go":        4,
	"internal/backend/k8s/logs.go":             4,
	"internal/backend/k8s/portforward.go":      7,
	"internal/backend/linux/backend.go":        4,
	"internal/backend/linux/journal.go":        1,
	"internal/backend/prom/alertmanager.go":    1,
	"internal/backend/prom/backend.go":         1,
	"internal/backend/salt/backend.go":         1,
	"internal/backend/salt/events.go":          2,
	"internal/backend/salt/executor.go":        2,
	"internal/backend/salt/jobs.go":            1,
	"internal/backend/salt/kind_jobs.go":       2,
	"internal/backend/salt/minions.go":         5,
	"internal/bus/bus.go":                      1,
	"internal/bus/run.go":                      1,
	"internal/fanout/fanout.go":                2,
	"internal/inventory/inventory.go":          1,
	"internal/mux/termpane/termpane.go":        3,
	"internal/plugin/backend.go":               1,
	"internal/server/client.go":                3,
	"internal/server/conn.go":                  5,
	"internal/server/server.go":                8,
	"internal/transport/ssh/dial.go":           4,
	"internal/transport/ssh/exec.go":           1,
	"internal/transport/ssh/pool.go":           2,
	"internal/transport/ssh/shell.go":          1,
	"internal/transport/ssh/sshtest/server.go": 6,
	"internal/transport/ssh/stream.go":         2,
	"internal/transport/sysssh/sysssh.go":      3,
	"internal/transport/transporttest/fake.go": 1,
	"internal/tui/commands_health.go":          1,
	"internal/tui/statusline.go":               1,
	"internal/tui/views/dashboard.go":          2,
	"internal/tui/views/eslogs.go":             2,
	"internal/tui/views/fanout.go":             1,
	"internal/tui/views/saltstate.go":          1,
}

// repoRoot is the directory the audit walks: the module root, found from this
// package's own directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// bareGoroutines lists every `go` statement in the non-test sources under
// internal/, keyed by the file's path relative to the module root.
func bareGoroutines(t *testing.T, root string) map[string][]token.Position {
	t.Helper()
	found := make(map[string][]token.Position)
	fset := token.NewFileSet()
	err := filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" || info.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// internal/safe is the recovery primitive itself: its one `go` is
		// the goroutine that recovers.
		if strings.HasSuffix(filepath.ToSlash(path), "internal/safe/safe.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if _, ok := n.(*ast.GoStmt); !ok {
				return true
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			found[filepath.ToSlash(rel)] = append(found[filepath.ToSlash(rel)], fset.Position(n.Pos()))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	return found
}

// TestAuditGoroutinesAreRecovered is requirement 1's floor: every goroutine the
// product starts either goes through internal/safe or is on the explicit
// allowlist of the ones that still do not. A new bare `go` in internal/ fails
// this test, which is the point: a panic in it would take the session with it.
func TestAuditGoroutinesAreRecovered(t *testing.T) {
	root := repoRoot(t)
	found := bareGoroutines(t, root)
	if len(found) == 0 {
		t.Fatal("the audit found no goroutines at all: it is not looking where it should")
	}

	var problems []string
	for file, positions := range found {
		allowed := bareGoroutineAllowlist[file]
		for i, pos := range positions {
			if i < allowed {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: %d: bare go statement: use safe.Go or safe.GoHook", file, pos.Line))
		}
	}
	for file, allowed := range bareGoroutineAllowlist {
		if len(found[file]) > allowed {
			continue
		}
		if len(found[file]) < allowed {
			problems = append(problems, fmt.Sprintf("%s: allowlist says %d bare go statements, found %d: lower it",
				file, allowed, len(found[file])))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("goroutines without panic recovery:\n%s", strings.Join(problems, "\n"))
	}
}

// TestAuditAppStartsNoBareGoroutines is the other half: this task's own package
// starts every goroutine through internal/safe, so it has no allowlist entry at
// all and the count is zero.
func TestAuditAppStartsNoBareGoroutines(t *testing.T) {
	root := repoRoot(t)
	found := bareGoroutines(t, root)
	for file, positions := range found {
		if !strings.HasPrefix(file, "internal/app/") {
			continue
		}
		for _, pos := range positions {
			t.Errorf("%s: %d: internal/app starts a goroutine without safe.Go", file, pos.Line)
		}
	}
	if _, ok := bareGoroutineAllowlist["internal/app/log.go"]; ok {
		t.Error("internal/app is in the allowlist: it should need no exemption")
	}
}
