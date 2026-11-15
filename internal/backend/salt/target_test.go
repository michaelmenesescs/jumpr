package salt

import (
	"fmt"
	"strings"
	"testing"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

func mustSelector(t *testing.T, s string) resource.Selector {
	t.Helper()
	sel, err := resource.ParseSelector(s)
	if err != nil {
		t.Fatalf("ParseSelector(%q): %v", s, err)
	}
	return sel
}

func TestCompileSelectorTable(t *testing.T) {
	grains := map[string]bool{"role": true, "dc": true, "maint": true, "name": true, "env": true}
	tests := []struct {
		sel  string
		keys map[string]bool
		want string
		ok   bool
	}{
		{"role=web dc~ewr|lax !maint", grains, "G@role:web and P@dc:ewr|lax and not G@maint:*", true},
		{"!maint", grains, "* and not G@maint:*", true},
		{"salt.role=web host~^web-0", grains, "G@role:web and E@^web-0", true},
		{"prom.job=node", grains, "", false},
		{"role=web @canary", grains, "", false},
		{`name="a b"`, grains, "", false},
		{"role!=web", grains, "* and not G@role:web", true},
		{"role", grains, "G@role:*", true},
		{"!role", grains, "* and not G@role:*", true},
		{"host=web-01", grains, "L@web-01", true},
		{"host!~^db", grains, "* and not E@^db", true},
		{"host~^web-0", grains, "E@^web-0", true},
		{"role=web dc=ewr", grains, "G@role:web and G@dc:ewr", true},
		{"dc~^(ewr|lax)$", grains, "", false},
		{"role=web name=it's", grains, "", false},
		{"role=", grains, "", false},
		{"", grains, "", false},
		{"host", grains, "", false},
		{"role=web", map[string]bool{"dc": true}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.sel, func(t *testing.T) {
			target, ok := CompileSelector(mustSelector(t, tt.sel), tt.keys)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (target %+v)", ok, tt.ok, target)
			}
			if !ok {
				return
			}
			if target.Type != "compound" {
				t.Fatalf("type = %q, want compound", target.Type)
			}
			if target.Expr != tt.want {
				t.Fatalf("expr = %q, want %q", target.Expr, tt.want)
			}
		})
	}
}

func TestCompileHostsChunking(t *testing.T) {
	ids := make([]string, 0, 450)
	for i := 0; i < 450; i++ {
		ids = append(ids, fmt.Sprintf("web-%03d", i))
	}
	targets := CompileHosts(ids, 200)
	if len(targets) != 3 {
		t.Fatalf("targets = %d, want 3", len(targets))
	}
	if got := len(strings.Split(targets[0].Expr, ",")); got != 200 {
		t.Fatalf("first chunk = %d ids, want 200", got)
	}
	if got := len(strings.Split(targets[2].Expr, ",")); got != 50 {
		t.Fatalf("last chunk = %d ids, want 50", got)
	}
	for _, tg := range targets {
		if tg.Type != "list" {
			t.Fatalf("type = %q, want list", tg.Type)
		}
	}
	if targets[0].Expr != strings.Join(ids[:200], ",") {
		t.Fatalf("first chunk not sorted as expected: %q", targets[0].Expr)
	}

	// De-duplication and sorting.
	dup := CompileHosts([]string{"b", "a", "b", "c"}, 2)
	if len(dup) != 2 {
		t.Fatalf("dedup chunks = %d, want 2", len(dup))
	}
	if dup[0].Expr != "a,b" || dup[1].Expr != "c" {
		t.Fatalf("dedup chunks = %q, %q", dup[0].Expr, dup[1].Expr)
	}

	// IDs that cannot live in a comma-joined list are rejected.
	bad := CompileHosts([]string{"ok", "a,b", "c d", ""}, 10)
	if len(bad) != 1 || bad[0].Expr != "ok" {
		t.Fatalf("bad ids not rejected: %+v", bad)
	}

	if got := CompileHosts(nil, 200); len(got) != 0 {
		t.Fatalf("empty input = %+v, want none", got)
	}
}

func TestCompile(t *testing.T) {
	grains := map[string]bool{"role": true, "dc": true}

	// Small sets are clearer as lists.
	small := []string{"web-01", "web-02", "web-03"}
	c := Compile(mustSelector(t, "role=web"), small, grains, 200)
	if c.Native {
		t.Fatal("small set compiled natively, want list")
	}
	if len(c.Targets) != 1 || c.Targets[0].Type != "list" {
		t.Fatalf("targets = %+v, want one list target", c.Targets)
	}
	if c.Targets[0].Expr != "web-01,web-02,web-03" {
		t.Fatalf("expr = %q", c.Targets[0].Expr)
	}
	if want := "list of 3 minions in 1 chunk"; c.Explain != want {
		t.Fatalf("explain = %q, want %q", c.Explain, want)
	}
	if strings.Join(c.Minions, ",") != strings.Join(small, ",") {
		t.Fatalf("minions = %v", c.Minions)
	}

	// Large sets with grain keys compile natively.
	large := make([]string, 30)
	for i := range large {
		large[i] = fmt.Sprintf("web-%02d", i)
	}
	c = Compile(mustSelector(t, "role=web dc=ewr"), large, grains, 200)
	if !c.Native {
		t.Fatalf("large set not native: %+v", c)
	}
	if len(c.Targets) != 1 || c.Targets[0].Type != "compound" {
		t.Fatalf("targets = %+v", c.Targets)
	}
	if want := "compound: G@role:web and G@dc:ewr"; c.Explain != want {
		t.Fatalf("explain = %q, want %q", c.Explain, want)
	}
	if len(c.Minions) != 30 {
		t.Fatalf("minions = %d, want 30", len(c.Minions))
	}

	// A non-grain key falls back to chunked lists and says why.
	c = Compile(mustSelector(t, "prom.job=node"), large, grains, 10)
	if c.Native {
		t.Fatalf("non-grain key compiled natively: %+v", c)
	}
	if len(c.Targets) != 3 {
		t.Fatalf("targets = %d, want 3", len(c.Targets))
	}
	if !strings.Contains(c.Explain, `selector uses non-grain key "prom.job"`) {
		t.Fatalf("explain = %q", c.Explain)
	}
	if !strings.HasPrefix(c.Explain, "list of 30 minions in 3 chunks") {
		t.Fatalf("explain = %q", c.Explain)
	}
}

func TestParseNative(t *testing.T) {
	tests := []struct {
		in   string
		typ  string
		expr string
	}{
		{"-C 'G@role:web and G@dc:ewr'", "compound", "G@role:web and G@dc:ewr"},
		{`-C "G@role:web"`, "compound", "G@role:web"},
		{"-C G@role:web and G@dc:ewr", "compound", "G@role:web and G@dc:ewr"},
		{"-G 'role:web'", "grain", "role:web"},
		{"-E '^web-0'", "pcre", "^web-0"},
		{"-L 'web-01,web-02'", "list", "web-01,web-02"},
		{"-N webservers", "nodegroup", "webservers"},
		{"-I 'env:prod'", "compound", "I@env:prod"},
		{"web-01*", "glob", "web-01*"},
		{"  'web-0?.example.internal' ", "glob", "web-0?.example.internal"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseNative(tt.in)
			if err != nil {
				t.Fatalf("ParseNative(%q): %v", tt.in, err)
			}
			if got.Type != tt.typ || got.Expr != tt.expr {
				t.Fatalf("ParseNative(%q) = %+v, want {%s %s}", tt.in, got, tt.typ, tt.expr)
			}
		})
	}

	for _, in := range []string{"", "   ", "-C", "-C ''", "-Z 'x'", "-L"} {
		if _, err := ParseNative(in); err == nil {
			t.Fatalf("ParseNative(%q) succeeded, want error", in)
		} else if k := errs.KindOf(err); k != errs.Config {
			t.Fatalf("ParseNative(%q) kind = %q, want config", in, k)
		}
	}
}

func TestParsePreview(t *testing.T) {
	got, err := ParsePreview([]byte(`["web-02", "web-01"]`))
	if err != nil {
		t.Fatalf("json preview: %v", err)
	}
	if strings.Join(got, ",") != "web-01,web-02" {
		t.Fatalf("json preview = %v", got)
	}

	got, err = ParsePreview([]byte("[DEPRECATION WARNING] noise\n- web-02\n- web-01\n- web-01\n"))
	if err != nil {
		t.Fatalf("text preview: %v", err)
	}
	if strings.Join(got, ",") != "web-01,web-02" {
		t.Fatalf("text preview = %v", got)
	}

	got, err = ParsePreview([]byte(""))
	if err != nil {
		t.Fatalf("empty preview: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty preview = %v, want none", got)
	}
}

func TestTargetString(t *testing.T) {
	tests := []struct {
		target Target
		want   string
	}{
		{Target{Type: "compound", Expr: "G@role:web and G@dc:ewr"}, "-C 'G@role:web and G@dc:ewr'"},
		{Target{Type: "list", Expr: "a,b"}, "-L a,b"},
		{Target{Type: "grain", Expr: "role:web"}, "-G role:web"},
		{Target{Type: "pcre", Expr: "^web-0[0-9]+$"}, "-E '^web-0[0-9]+$'"},
		{Target{Type: "nodegroup", Expr: "webservers"}, "-N webservers"},
		{Target{Type: "glob", Expr: "web-01*"}, "web-01*"},
	}
	for _, tt := range tests {
		if got := tt.target.String(); got != tt.want {
			t.Fatalf("%+v.String() = %q, want %q", tt.target, got, tt.want)
		}
	}
}
