package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loadGood loads the good testdata directory once per test that needs it.
func loadGood(t *testing.T) map[string]Definition {
	t.Helper()
	ok, problems := LoadDir(filepath.Join("testdata", "good"))
	if len(problems) > 0 {
		t.Fatalf("testdata/good has problems: %v", problems)
	}
	byName := make(map[string]Definition, len(ok))
	for _, d := range ok {
		byName[d.Name] = d
	}
	return byName
}

func TestLoadSpecExamples(t *testing.T) {
	defs := loadGood(t)
	if len(defs) != 3 {
		t.Fatalf("loaded %d definitions, want 3: %v", len(defs), defs)
	}

	nginx, ok := defs["nginx-status"]
	if !ok {
		t.Fatal("nginx-status (SPEC §12 first example) did not load")
	}
	if nginx.APIVersion != "jumpr/v1" || nginx.Kind != "Plugin" {
		t.Errorf("nginx-status header = %q/%q", nginx.APIVersion, nginx.Kind)
	}
	if nginx.Target != TargetHost || !nginx.Privileged {
		t.Errorf("nginx-status target/privileged = %q/%v", nginx.Target, nginx.Privileged)
	}
	if !strings.Contains(nginx.Command, "nginx -T") || !strings.Contains(nginx.Command, "sport = :443") {
		t.Errorf("nginx-status command lost text: %q", nginx.Command)
	}
	if nginx.Parse.Type != ParseRegex || !strings.Contains(nginx.Parse.Pattern, "(?P<directive>") {
		t.Errorf("nginx-status parse = %+v", nginx.Parse)
	}
	if got, want := nginx.View.Columns, []string{"directive", "value"}; !equalStrings(got, want) {
		t.Errorf("nginx-status columns = %v, want %v", got, want)
	}
	if !nginx.View.GroupByOutput {
		t.Error("nginx-status group_by_output did not decode")
	}
	if len(nginx.Links) != 1 || nginx.Links[0].Label != "journal" || nginx.Links[0].To != "journal" {
		t.Errorf("nginx-status links = %+v", nginx.Links)
	}
	if nginx.Links[0].Args["unit"] != "nginx.service" {
		t.Errorf("nginx-status link args = %v", nginx.Links[0].Args)
	}
	if len(nginx.Actions) != 1 {
		t.Fatalf("nginx-status actions = %+v", nginx.Actions)
	}
	act := nginx.Actions[0]
	if act.Name != "reload" || act.Command != "systemctl reload nginx" || !act.Privileged || !act.Confirm {
		t.Errorf("nginx-status action = %+v", act)
	}
	if nginx.File == "" || nginx.Line != 3 {
		t.Errorf("nginx-status source = %q:%d, want the name on line 3", nginx.File, nginx.Line)
	}

	lb, ok := defs["lb-pool-members"]
	if !ok {
		t.Fatal("lb-pool-members (SPEC §12 second example) did not load")
	}
	if lb.Target != TargetHTTP || lb.Connection != "corp-sso" {
		t.Errorf("lb-pool-members target/connection = %q/%q", lb.Target, lb.Connection)
	}
	if lb.Request == nil {
		t.Fatal("lb-pool-members request missing")
	}
	if lb.Request.Method != "GET" {
		t.Errorf("lb-pool-members method = %q", lb.Request.Method)
	}
	if want := `https://lb-api.example.internal/v1/pools/{{ .Param "pool" }}/members`; lb.Request.URL != want {
		t.Errorf("lb-pool-members url = %q, want %q", lb.Request.URL, want)
	}
	if lb.Parse.Type != ParseJQ || !strings.Contains(lb.Parse.Expr, ".members[]") {
		t.Errorf("lb-pool-members parse = %+v", lb.Parse)
	}
	if got, want := lb.View.Columns, []string{"host", "state", "conns"}; !equalStrings(got, want) {
		t.Errorf("lb-pool-members columns = %v, want %v", got, want)
	}
	if lb.EmitsHosts != TargetHost {
		t.Errorf("lb-pool-members emits_hosts = %q", lb.EmitsHosts)
	}
	if p, ok := lb.Param("pool"); !ok || !p.Required || p.Description == "" {
		t.Errorf("lb-pool-members param pool = %+v, ok=%v", p, ok)
	}
}

// TestLoadBrokenFilesEachGiveOneProblem is the acceptance test of task 115: the
// twelve broken files load into twelve problems, one per file, each naming the
// file, the line and the field.
func TestLoadBrokenFilesEachGiveOneProblem(t *testing.T) {
	dir := filepath.Join("testdata", "broken")
	want := map[string]struct{ field, msg string }{
		"01-missing-api-version.yaml":    {"apiVersion", "apiVersion must be"},
		"02-wrong-kind.yaml":             {"kind", "kind must be"},
		"03-bad-name.yaml":               {"name", "must match ^[a-z][a-z0-9-]*$"},
		"04-unknown-target.yaml":         {"target", "must be one of host, k8s, http, inventory"},
		"05-no-command.yaml":             {"command", "needs a command"},
		"06-unknown-parse-type.yaml":     {"parse.type", "parse type \"csv\" must be one of"},
		"07-bad-regex.yaml":              {"parse.pattern", "regex does not compile"},
		"08-regex-without-names.yaml":    {"parse.pattern", "named group"},
		"09-bad-jq.yaml":                 {"parse.expr", "jq expression does not compile"},
		"10-unknown-column.yaml":         {"view.columns[1]", `column "size" is not produced`},
		"11-undeclared-param.yaml":       {"command", `undeclared param "pool"`},
		"12-action-without-confirm.yaml": {"actions[0].confirm", "confirm: true"},
	}

	ok, problems := LoadDir(dir)
	if len(ok) != 0 {
		t.Fatalf("broken directory produced %d good definitions: %v", len(ok), ok)
	}
	if len(problems) != len(want) {
		for _, p := range problems {
			t.Logf("problem: %s", p)
		}
		t.Fatalf("got %d problems, want %d (one per broken file)", len(problems), len(want))
	}

	seen := make(map[string]bool, len(problems))
	for _, p := range problems {
		base := filepath.Base(p.File)
		expect, known := want[base]
		if !known {
			t.Errorf("unexpected problem for %s: %s", base, p)
			continue
		}
		if seen[base] {
			t.Errorf("%s produced more than one problem", base)
		}
		seen[base] = true
		if p.Line < 1 {
			t.Errorf("%s: problem has no line: %s", base, p)
		}
		if p.Field != expect.field {
			t.Errorf("%s: field = %q, want %q (%s)", base, p.Field, expect.field, p)
		}
		if !strings.Contains(p.Msg, expect.msg) {
			t.Errorf("%s: message %q does not mention %q", base, p.Msg, expect.msg)
		}
		if !strings.HasPrefix(p.String(), p.File+":") {
			t.Errorf("%s: String() = %q, want it to start with the file and line", base, p.String())
		}
	}
	for base := range want {
		if !seen[base] {
			t.Errorf("%s produced no problem", base)
		}
	}
}

func TestLoadDirKeepsGoodFiles(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, filepath.Join("testdata", "good", "nginx-status.yaml"), filepath.Join(dir, "nginx-status.yaml"))
	copyFile(t, filepath.Join("testdata", "broken", "10-unknown-column.yaml"), filepath.Join(dir, "10-unknown-column.yaml"))

	ok, problems := LoadDir(dir)
	if len(ok) != 1 || ok[0].Name != "nginx-status" {
		t.Fatalf("ok = %v, want only nginx-status", ok)
	}
	if len(problems) != 1 || problems[0].Field != "view.columns[1]" {
		t.Fatalf("problems = %v, want one about view.columns[1]", problems)
	}
}

func TestLoadMissingDirIsEmpty(t *testing.T) {
	ok, problems := LoadDir(filepath.Join(t.TempDir(), "no-such-dir"))
	if len(ok) != 0 || len(problems) != 0 {
		t.Fatalf("missing dir = %v, %v; want nothing", ok, problems)
	}
}

func TestLoadSkipsNonPluginFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "not a plugin\n")
	writeFile(t, dir, ".hidden.yaml", "not: [valid\n")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "subdir"), "broken.yaml", "kind: nope\n")
	writeFile(t, dir, "ok.yaml", validHostPlugin) // .yaml loads, .yml would too

	ok, problems := LoadDir(dir)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(ok) != 1 || ok[0].Name != "ok-plugin" {
		t.Fatalf("ok = %v", ok)
	}
}

func TestLoadDuplicateName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.yaml", validHostPlugin)
	writeFile(t, dir, "b.yaml", validHostPlugin)
	ok, problems := LoadDir(dir)
	if len(ok) != 1 {
		t.Fatalf("ok = %d definitions, want 1", len(ok))
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want 1", problems)
	}
	if problems[0].Field != "name" || !strings.Contains(problems[0].Msg, "already defined in") {
		t.Errorf("duplicate problem = %s", problems[0])
	}
}

func TestLoadUnknownFieldIsStrict(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "typo.yaml", strings.Replace(validHostPlugin, "target: host", "targt: host", 1))
	_, problems := LoadDir(dir)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want 1", problems)
	}
	if problems[0].Field != "targt" {
		t.Errorf("field = %q, want targt (%s)", problems[0].Field, problems[0])
	}
	if problems[0].Line != 5 {
		t.Errorf("line = %d, want 5", problems[0].Line)
	}
}

func TestLoadSyntaxErrorHasLine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "syntax.yaml", "apiVersion: jumpr/v1\nkind: Plugin\nname: [oops\n")
	_, problems := LoadDir(dir)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want 1", problems)
	}
	if problems[0].Line < 1 || !strings.Contains(problems[0].Msg, "did not find expected") {
		t.Errorf("problem = %s, want a YAML syntax message with a line", problems[0])
	}
}

func TestLoadEmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "empty.yaml", "   \n")
	_, problems := LoadDir(dir)
	if len(problems) != 1 || !strings.Contains(problems[0].Msg, "empty") {
		t.Fatalf("problems = %v, want one about an empty file", problems)
	}
}

func TestValidateProblemIsConfigError(t *testing.T) {
	p := Problem{File: "x.yaml", Line: 7, Field: "name", Msg: "bad"}
	if got, want := p.String(), "x.yaml:7: name: bad"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if kind := p.Err().Kind; kind != "config" {
		t.Errorf("Err kind = %q, want config", kind)
	}
}

func TestValidateParseRules(t *testing.T) {
	tests := []struct {
		name  string
		parse string
		field string // "" means valid
		msg   string
	}{
		{"regex ok", "type: regex\npattern: '(?P<a>\\w+) \\s+(?P<b>\\w+)'", "", ""},
		{"regex empty", "type: regex", "parse.pattern", "needs pattern"},
		{"regex broken", "type: regex\npattern: '(?P<a>'", "parse.pattern", "does not compile"},
		{"regex unnamed", "type: regex\npattern: '(\\w+)'", "parse.pattern", "named group"},
		{"jq ok", "type: jq\nexpr: '.members[]'", "", ""},
		{"jq empty", "type: jq", "parse.expr", "needs expr"},
		{"jq broken", "type: jq\nexpr: '{'", "parse.expr", "does not compile"},
		{"json", "type: json", "", ""},
		{"lines", "type: lines", "", ""},
		{"raw", "type: raw", "", ""},
		{"kv ok", "type: kv", "", ""},
		{"kv colon", "type: kv\nseparator: ':'", "", ""},
		{"kv bad separator", "type: kv\nseparator: '|'", "parse.separator", "must be"},
		{"columns names", "type: columns\nnames: [a, b]", "", ""},
		{"columns header", "type: columns\nheader: true", "", ""},
		{"columns empty", "type: columns", "parse.names", "needs header: true or names"},
		{"columns dup", "type: columns\nnames: [a, a]", "parse.names[1]", "listed twice"},
		{"columns both", "type: columns\nheader: true\nnames: [a]", "parse.names", "not both"},
		{"columns negative skip", "type: columns\nheader: true\nskip: -1", "parse.skip", "negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := "apiVersion: jumpr/v1\nkind: Plugin\nname: p\ntarget: host\ncommand: uptime\nparse:\n  " +
				strings.ReplaceAll(tc.parse, "\n", "\n  ") + "\n"
			writeFile(t, dir, "p.yaml", body)
			_, problems := LoadDir(dir)
			if tc.field == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want one about %s", problems, tc.field)
			}
			if problems[0].Field != tc.field || !strings.Contains(problems[0].Msg, tc.msg) {
				t.Errorf("problem = %s, want field %q mentioning %q", problems[0], tc.field, tc.msg)
			}
		})
	}
}

func TestValidateCommandTemplateQuotes(t *testing.T) {
	got, err := RenderCommand(`lbctl members {{ .Param "pool" }} on {{ .Host }}`, map[string]string{"pool": "a; rm -rf /"}, "web1.example.internal")
	if err != nil {
		t.Fatalf("RenderCommand: %v", err)
	}
	want := `lbctl members 'a; rm -rf /' on web1.example.internal`
	if got != want {
		t.Fatalf("RenderCommand = %q, want %q", got, want)
	}
	if strings.Contains(got, "; rm -rf / ") || !strings.Contains(got, "'a; rm -rf /'") {
		t.Fatalf("the value was not quoted into one word: %q", got)
	}

	// A value with a single quote must not escape the quoting either.
	got, err = RenderCommand(`echo {{ .Param "x" }}`, map[string]string{"x": "it's; id"}, "")
	if err != nil {
		t.Fatalf("RenderCommand: %v", err)
	}
	if want := `echo 'it'\''s; id'`; got != want {
		t.Errorf("RenderCommand = %q, want %q", got, want)
	}

	if _, err := RenderCommand(`echo {{ .Param "missing" }}`, nil, ""); err == nil {
		t.Error("a parameter without a value must be an error")
	}
}

func TestValidateTemplateRejectsOtherConstructs(t *testing.T) {
	for _, text := range []string{
		`cmd {{ .Env "HOME" }}`,
		`cmd {{ .Host | upper }}`,
		`cmd {{ if .Host }}x{{ end }}`,
		`cmd {{ .Param }}`,
		`cmd {{ Param "x" }}`,
		`cmd {{ .Param "x" "y" }}`,
		`cmd {{ .Host.Foo }}`,
		`cmd {{`,
	} {
		if _, err := RenderCommand(text, map[string]string{"x": "1"}, "h"); err == nil {
			t.Errorf("RenderCommand(%q) accepted a construct that is not {{ .Param }} or {{ .Host }}", text)
		}
	}
}

func TestValidateTemplateForms(t *testing.T) {
	parts, err := parseTemplate(`a{{.Param "p"}}b{{ .Host }}c`)
	if err != nil {
		t.Fatalf("parseTemplate: %v", err)
	}
	if got, want := strings.Join(refs(parts), ","), "p"; got != want {
		t.Errorf("refs = %q, want %q", got, want)
	}
	if !usesHost(parts) {
		t.Error("usesHost = false, want true")
	}
	got, err := RenderCommand(`a{{.Param "p"}}b`, map[string]string{"p": "x y"}, "")
	if err != nil {
		t.Fatalf("RenderCommand: %v", err)
	}
	if want := `a'x y'b`; got != want {
		t.Errorf("RenderCommand = %q, want %q", got, want)
	}
}

func TestValidateActionRules(t *testing.T) {
	base := "apiVersion: jumpr/v1\nkind: Plugin\nname: a\ntarget: host\ncommand: uptime\nparse:\n  type: lines\n"
	tests := []struct {
		name  string
		body  string
		field string
		msg   string
	}{
		{"confirmed", "actions:\n  - name: reload\n    command: systemctl reload nginx\n    confirm: true\n", "", ""},
		{"unconfirmed", "actions:\n  - name: reload\n    command: systemctl reload nginx\n", "actions[0].confirm", "confirm: true"},
		{"no command", "actions:\n  - name: reload\n    confirm: true\n", "actions[0].command", "needs a command"},
		{"bad name", "actions:\n  - name: Reload!\n    command: true\n    confirm: true\n", "actions[0].name", "must match"},
		{"duplicate", "actions:\n  - name: reload\n    command: a\n    confirm: true\n  - name: reload\n    command: b\n    confirm: true\n", "actions[1].name", "twice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "a.yaml", base+tc.body)
			_, problems := LoadDir(dir)
			if tc.field == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want one about %s", problems, tc.field)
			}
			if problems[0].Field != tc.field || !strings.Contains(problems[0].Msg, tc.msg) {
				t.Errorf("problem = %s, want field %q mentioning %q", problems[0], tc.field, tc.msg)
			}
		})
	}
}

func TestValidateRequestRules(t *testing.T) {
	base := "apiVersion: jumpr/v1\nkind: Plugin\nname: h\ntarget: http\nparse:\n  type: json\n"
	tests := []struct {
		name  string
		body  string
		field string
		msg   string
	}{
		{"get", "request:\n  method: GET\n  url: https://api.example.internal/v1\n", "", ""},
		{"head", "request:\n  method: HEAD\n  url: https://api.example.internal/v1\n", "", ""},
		{"post without safe", "request:\n  method: POST\n  url: https://api.example.internal/v1\n", "request.method", "changes state"},
		{"post safe", "request:\n  method: POST\n  url: https://api.example.internal/v1\n  safe: true\n", "", ""},
		{"no method", "request:\n  url: https://api.example.internal/v1\n", "request.method", "needs request.method"},
		{"no url", "request:\n  method: GET\n", "request.url", "needs request.url"},
		{"bad scheme", "request:\n  method: GET\n  url: ftp://api.example.internal/v1\n", "request.url", "http:// or https://"},
		{"missing request", "", "request", "needs a request"},
		{"url template with undeclared param", "request:\n  method: GET\n  url: https://api.example.internal/{{ .Param \"pool\" }}\n", "request.url", `undeclared param "pool"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := base + tc.body
			if tc.name == "missing request" {
				body += "view: { columns: [a] }\n"
			}
			writeFile(t, dir, "h.yaml", body)
			_, problems := LoadDir(dir)
			if tc.field == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want one about %s", problems, tc.field)
			}
			if problems[0].Field != tc.field || !strings.Contains(problems[0].Msg, tc.msg) {
				t.Errorf("problem = %s, want field %q mentioning %q", problems[0], tc.field, tc.msg)
			}
		})
	}
}

func TestWatchCallsOnChange(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.yaml", validHostPlugin)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, dir, 10*time.Millisecond, func() {
			select {
			case called <- struct{}{}:
			default:
			}
		})
	}()

	// Let Watch take its first snapshot before changing anything.
	time.Sleep(60 * time.Millisecond)

	// A change to an existing file.
	writeFile(t, dir, "a.yaml", validHostPlugin+"# edited\n")
	waitCall(t, called, "an edit to an existing file")

	// A new file.
	writeFile(t, dir, "b.yaml", validHostPlugin)
	waitCall(t, called, "a new file")

	// A removal.
	if err := os.Remove(filepath.Join(dir, "b.yaml")); err != nil {
		t.Fatal(err)
	}
	waitCall(t, called, "a removed file")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after its context was canceled")
	}
}

func TestWatchStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	called := 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, dir, 5*time.Millisecond, func() { called++ })
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after its context was canceled")
	}
	if called != 0 {
		t.Errorf("onChange called %d times with no change", called)
	}
}

const validHostPlugin = `apiVersion: jumpr/v1
kind: Plugin
name: ok-plugin
description: a valid host plugin
target: host
command: uptime
parse:
  type: lines
view:
  columns: [line]
`

// waitCall waits for one Watch callback, failing after two seconds.
func waitCall(t *testing.T, called <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatalf("Watch never reported %s", what)
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
