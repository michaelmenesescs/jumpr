package plugin

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
)

// checkRows compares a parse result with the expected rows, row by row, so a
// failure names the row that differs.
func checkRows(t *testing.T, got, want []map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d:\n got %#v\nwant %#v", len(got), len(want), got, want)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("row %d:\n got %#v\nwant %#v", i, got[i], want[i])
		}
	}
}

// parseCase is one row of a per-type table test.
type parseCase struct {
	name string
	def  ParseDef
	out  string
	want []map[string]any
}

func runParseCases(t *testing.T, cases []parseCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.def, []byte(tc.out))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			checkRows(t, got, tc.want)
		})
	}
}

func TestParseJSON(t *testing.T) {
	runParseCases(t, []parseCase{
		{
			name: "object is one row",
			def:  ParseDef{Type: ParseJSON},
			out:  `{"host": "web-01", "up": true, "conns": 3, "load": 1.5}`,
			want: []map[string]any{{"host": "web-01", "up": true, "conns": int64(3), "load": 1.5}},
		},
		{
			name: "array of objects is one row each",
			def:  ParseDef{Type: ParseJSON},
			out:  `[{"host": "web-01", "conns": 3}, {"host": "web-02", "conns": 0}]`,
			want: []map[string]any{
				{"host": "web-01", "conns": int64(3)},
				{"host": "web-02", "conns": int64(0)},
			},
		},
		{
			name: "nested numbers are typed too",
			def:  ParseDef{Type: ParseJSON},
			out:  `{"host": "web-01", "meta": {"workers": 4, "load": 0.25}, "tags": [1, 2]}`,
			want: []map[string]any{{
				"host": "web-01",
				"meta": map[string]any{"workers": int64(4), "load": 0.25},
				"tags": []any{int64(1), int64(2)},
			}},
		},
		{
			name: "array of scalars becomes value rows",
			def:  ParseDef{Type: ParseJSON},
			out:  `["web-01", 2, null]`,
			want: []map[string]any{
				{"value": "web-01"},
				{"value": int64(2)},
				{"value": nil},
			},
		},
		{
			name: "empty output is no rows",
			def:  ParseDef{Type: ParseJSON},
			out:  " \n",
			want: nil,
		},
	})
}

func TestParseJSONErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
	}{
		{"truncated", `{"host": `},
		{"trailing data", `{"a": 1} {"b": 2}`},
		{"bare word", `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(ParseDef{Type: ParseJSON}, []byte(tc.out))
			if err == nil {
				t.Fatal("Parse() returned no error")
			}
			if kind := errs.KindOf(err); kind != errs.Parse {
				t.Fatalf("kind = %q, want %q (%v)", kind, errs.Parse, err)
			}
		})
	}
}

func TestParseJQ(t *testing.T) {
	members := `{"pool": "web", "members": [` +
		`{"address": "web-01", "state": "up", "active": 3},` +
		`{"address": "web-02", "state": "down", "active": 0}]}`

	runParseCases(t, []parseCase{
		{
			name: "lb-pool-members example",
			def:  ParseDef{Type: ParseJQ, Expr: `.members[] | {host: .address, state: .state, conns: .active}`},
			out:  members,
			want: []map[string]any{
				{"host": "web-01", "state": "up", "conns": int64(3)},
				{"host": "web-02", "state": "down", "conns": int64(0)},
			},
		},
		{
			name: "scalar is wrapped in value",
			def:  ParseDef{Type: ParseJQ, Expr: `.members[] | .address`},
			out:  members,
			want: []map[string]any{
				{"value": "web-01"},
				{"value": "web-02"},
			},
		},
		{
			name: "empty output is no rows",
			def:  ParseDef{Type: ParseJQ, Expr: `.members[]`},
			out:  "",
			want: nil,
		},
	})

	t.Run("row cap", func(t *testing.T) {
		got, err := Parse(ParseDef{Type: ParseJQ, Expr: `range(0; 25000)`}, []byte(`{}`))
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		if len(got) != parseMaxRows {
			t.Fatalf("got %d rows, want the cap of %d", len(got), parseMaxRows)
		}
		if last := got[len(got)-1]["value"]; last != parseMaxRows-1 {
			t.Fatalf("last row value = %v, want %d", last, parseMaxRows-1)
		}
	})
}

func TestParseJQErrors(t *testing.T) {
	t.Run("expression does not compile", func(t *testing.T) {
		_, err := Parse(ParseDef{Type: ParseJQ, Expr: `.members[`}, []byte(`{}`))
		if err == nil {
			t.Fatal("Parse() returned no error")
		}
		if kind := errs.KindOf(err); kind != errs.Parse {
			t.Fatalf("kind = %q, want %q (%v)", kind, errs.Parse, err)
		}
		if !strings.Contains(err.Error(), "jq") {
			t.Fatalf("error %q does not name jq", err)
		}
	})

	t.Run("expression fails at run time", func(t *testing.T) {
		_, err := Parse(ParseDef{Type: ParseJQ, Expr: `error("boom")`}, []byte(`{}`))
		if err == nil {
			t.Fatal("Parse() returned no error")
		}
		if kind := errs.KindOf(err); kind != errs.Parse {
			t.Fatalf("kind = %q, want %q (%v)", kind, errs.Parse, err)
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Fatalf("error %q lost the expression message", err)
		}
	})

	t.Run("input is not JSON", func(t *testing.T) {
		_, err := Parse(ParseDef{Type: ParseJQ, Expr: `.`}, []byte(`nope`))
		if err == nil {
			t.Fatal("Parse() returned no error")
		}
		if kind := errs.KindOf(err); kind != errs.Parse {
			t.Fatalf("kind = %q, want %q (%v)", kind, errs.Parse, err)
		}
	})

	t.Run("expression cannot run forever", func(t *testing.T) {
		if testing.Short() {
			t.Skip("takes parseTimeout")
		}
		start := time.Now()
		_, err := Parse(ParseDef{Type: ParseJQ, Expr: `reduce range(1e9) as $i (0; . + $i)`}, []byte(`{}`))
		if err == nil {
			t.Fatal("Parse() returned no error")
		}
		if kind := errs.KindOf(err); kind != errs.Timeout {
			t.Fatalf("kind = %q, want %q (%v)", kind, errs.Timeout, err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("took %s, want at most %s", took, parseTimeout+time.Second)
		}
	})
}

func TestParseRegex(t *testing.T) {
	// nginx-status (SPEC §12): the worker/listen directives, followed by the
	// established-connection count, which the pattern does not match.
	nginx := "worker_processes  auto;\n" +
		"listen 443 ssl;\n" +
		"listen 80;\n" +
		"# configuration file /etc/nginx/nginx.conf:\n" +
		"12\n"

	runParseCases(t, []parseCase{
		{
			name: "nginx example",
			def:  ParseDef{Type: ParseRegex, Pattern: `^\s*(?P<directive>\w+)\s+(?P<value>[^;]+);`},
			out:  nginx,
			want: []map[string]any{
				{"directive": "worker_processes", "value": "auto"},
				{"directive": "listen", "value": "443 ssl"},
				{"directive": "listen", "value": int64(80)},
			},
		},
		{
			name: "values are typed",
			def:  ParseDef{Type: ParseRegex, Pattern: `^(?P<host>\S+) +(?P<conns>\d+) +(?P<load>[0-9.]+)$`},
			out:  "web-01 42 0.5\nweb-02 0 3\n",
			want: []map[string]any{
				{"host": "web-01", "conns": int64(42), "load": 0.5},
				{"host": "web-02", "conns": int64(0), "load": int64(3)},
			},
		},
		{
			name: "strings keeps every value a string",
			def:  ParseDef{Type: ParseRegex, Pattern: `^(?P<host>\S+) +(?P<conns>\d+)$`, Strings: true},
			out:  "web-01 42\n",
			want: []map[string]any{{"host": "web-01", "conns": "42"}},
		},
		{
			name: "multiline runs the pattern over the whole output",
			def:  ParseDef{Type: ParseRegex, Pattern: `(?s)(?P<top>\w)\n(?P<bottom>\w)`, Multiline: true},
			out:  "a\nb\nc\nd\n",
			want: []map[string]any{
				{"top": "a", "bottom": "b"},
				{"top": "c", "bottom": "d"},
			},
		},
		{
			name: "without multiline a line pattern cannot span lines",
			def:  ParseDef{Type: ParseRegex, Pattern: `(?s)(?P<top>\w)\n(?P<bottom>\w)`},
			out:  "a\nb\nc\nd\n",
			want: nil,
		},
		{
			name: "several matches on one line are several rows",
			def:  ParseDef{Type: ParseRegex, Pattern: `(?P<k>\w+)=(?P<v>\w+)`},
			out:  "a=1 b=2\nc=3\n",
			want: []map[string]any{
				{"k": "a", "v": int64(1)},
				{"k": "b", "v": int64(2)},
				{"k": "c", "v": int64(3)},
			},
		},
	})

	t.Run("pattern without named groups is a config error", func(t *testing.T) {
		_, err := Parse(ParseDef{Type: ParseRegex, Pattern: `^(\w+)$`}, []byte("web-01\n"))
		if err == nil {
			t.Fatal("Parse() returned no error")
		}
		if kind := errs.KindOf(err); kind != errs.Config {
			t.Fatalf("kind = %q, want %q (%v)", kind, errs.Config, err)
		}
	})
}

func TestParseColumns(t *testing.T) {
	runParseCases(t, []parseCase{
		{
			name: "header from the first line, last column takes the rest",
			def:  ParseDef{Type: ParseColumns, Header: true},
			out: "PID USER COMMAND\n" +
				"1 root /sbin/init\n" +
				"2 app /usr/bin/app --flag\n",
			want: []map[string]any{
				{"PID": int64(1), "USER": "root", "COMMAND": "/sbin/init"},
				{"PID": int64(2), "USER": "app", "COMMAND": "/usr/bin/app --flag"},
			},
		},
		{
			name: "names, skip and typed values",
			def:  ParseDef{Type: ParseColumns, Names: []string{"host", "conns", "load"}, Skip: 1},
			out:  "host conns load\nweb-01 42 0.5\n",
			want: []map[string]any{{"host": "web-01", "conns": int64(42), "load": 0.5}},
		},
		{
			name: "strings keeps every value a string",
			def:  ParseDef{Type: ParseColumns, Names: []string{"host", "conns"}, Strings: true},
			out:  "web-01 42\n",
			want: []map[string]any{{"host": "web-01", "conns": "42"}},
		},
		{
			name: "one column takes the whole line",
			def:  ParseDef{Type: ParseColumns, Names: []string{"line"}},
			out:  "  nginx: worker process\nnginx: master process\n",
			want: []map[string]any{
				{"line": "nginx: worker process"},
				{"line": "nginx: master process"},
			},
		},
		{
			name: "a line with too few fields is skipped",
			def:  ParseDef{Type: ParseColumns, Names: []string{"host", "conns"}},
			out:  "web-01 42\nbadline\n",
			want: []map[string]any{{"host": "web-01", "conns": int64(42)}},
		},
		{
			name: "skip past the output is no rows",
			def:  ParseDef{Type: ParseColumns, Names: []string{"host"}, Skip: 5},
			out:  "a\nb\n",
			want: nil,
		},
		{
			name: "empty output is no rows even with a header",
			def:  ParseDef{Type: ParseColumns, Header: true},
			out:  "",
			want: nil,
		},
	})
}

func TestParseKV(t *testing.T) {
	runParseCases(t, []parseCase{
		{
			name: "equals by default, one row",
			def:  ParseDef{Type: ParseKV},
			out:  "MemTotal=16000\nMemFree=200\nswappiness=10\n",
			want: []map[string]any{{"MemTotal": int64(16000), "MemFree": int64(200), "swappiness": int64(10)}},
		},
		{
			name: "colon separator keeps the rest of the line",
			def:  ParseDef{Type: ParseKV, Separator: ":"},
			out:  "MemTotal:       16000 kB\nMemFree:         200 kB\n",
			want: []map[string]any{{"MemTotal": "16000 kB", "MemFree": "200 kB"}},
		},
		{
			name: "strings keeps every value a string",
			def:  ParseDef{Type: ParseKV, Strings: true},
			out:  "a=1\n",
			want: []map[string]any{{"a": "1"}},
		},
		{
			name: "lines without a separator are skipped",
			def:  ParseDef{Type: ParseKV},
			out:  "# comment\nno separator here\na=1\n",
			want: []map[string]any{{"a": int64(1)}},
		},
		{
			name: "nothing parsed is no rows",
			def:  ParseDef{Type: ParseKV},
			out:  "just text\n",
			want: nil,
		},
	})
}

func TestParseLines(t *testing.T) {
	runParseCases(t, []parseCase{
		{
			name: "one row per line",
			def:  ParseDef{Type: ParseLines},
			out:  "nginx.service\nhaproxy.service\n",
			want: []map[string]any{
				{"line": "nginx.service"},
				{"line": "haproxy.service"},
			},
		},
		{
			name: "blank lines are dropped",
			def:  ParseDef{Type: ParseLines},
			out:  "a\n\nb\n",
			want: []map[string]any{{"line": "a"}, {"line": "b"}},
		},
		{
			name: "empty output is no rows",
			def:  ParseDef{Type: ParseLines},
			out:  "\n",
			want: nil,
		},
	})
}

func TestParseRaw(t *testing.T) {
	runParseCases(t, []parseCase{
		{
			name: "whole output in one row for group_by_output",
			def:  ParseDef{Type: ParseRaw},
			out:  "worker_processes  auto;\n12\n\n",
			want: []map[string]any{{"output": "worker_processes  auto;\n12"}},
		},
		{
			name: "empty output still yields the row",
			def:  ParseDef{Type: ParseRaw},
			out:  "",
			want: []map[string]any{{"output": ""}},
		},
	})
}

func TestParseUnknownType(t *testing.T) {
	_, err := Parse(ParseDef{Type: "yaml"}, []byte("a: 1\n"))
	if err == nil {
		t.Fatal("Parse() returned no error")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Fatalf("kind = %q, want %q (%v)", kind, errs.Config, err)
	}
}

func TestParseGarbageOutput(t *testing.T) {
	// Whatever the type, arbitrary bytes must not panic and must not return
	// both rows and an error.
	outputs := []string{
		"", "\x00\x01\x02", "\xff\xfe", "{\"a\":", strings.Repeat("x", 4096),
		"\r\n\r\n", "a=b\nc", "   \t  ", `[1,2,{"a":1}]`,
	}
	defs := []ParseDef{
		{Type: ParseJSON},
		{Type: ParseJQ, Expr: ".a"},
		{Type: ParseRegex, Pattern: `(?P<word>\w+)`},
		{Type: ParseColumns, Header: true},
		{Type: ParseColumns, Names: []string{"a", "b"}},
		{Type: ParseKV},
		{Type: ParseLines},
		{Type: ParseRaw},
	}
	for _, def := range defs {
		for _, out := range outputs {
			rows, err := Parse(def, []byte(out))
			if err != nil && rows != nil {
				t.Fatalf("%s on %q: rows and error both non-nil", def.Type, out)
			}
		}
	}
}
