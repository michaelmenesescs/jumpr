package salt

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
)

// ---- fixtures ----

const (
	treeEnvsJSON  = `["base","dev"]`
	treeBaseFiles = `["top.sls","common/init.sls","nginx/init.sls","nginx/conf.sls","notes.md","_modules/helpers.py"]`
	treeDevFiles  = `["site/init.sls"]`

	treeTopJSON = `{"web-01":{"base":["common","nginx"]},` +
		`"web-02":{"base":["common","nginx"]},` +
		`"db-01":{"base":["common"]}}`

	treeSlsNginxJSON = `{
  "web-01": {
    "nginx": {
      "__sls__": "nginx",
      "__env__": "base",
      "pkg": ["installed", {"name": "nginx"}],
      "service": ["running", {"name": "nginx"}, {"require": [{"pkg": "nginx"}]}],
      "file": ["managed", {"name": "/etc/nginx/nginx.conf"},
        {"require": [{"pkg": "nginx"}, {"service": "nginx"}],
         "watch": [{"file": "/etc/nginx/sites-available/default"}]}]
    }
  }
}`

	treeSlsErrorJSON = `{"web-01": ["Rendering SLS 'base:nginx' failed: Jinja error"]}`

	treeHighstateJSON = `{"web-01": {
    "nginx": {"__sls__": "nginx", "__env__": "base",
      "service": ["running", {"name": "nginx"}]},
    "common": {"__sls__": "common", "__env__": "base",
      "pkg": ["installed", {"name": "ca-certificates"}]}
  }}`

	treeSource = "managed:\n  file.managed:\n    - name: /etc/nginx/nginx.conf\n"
)

// treeMaster is a scripted Salt master answering the state-tree calls.
type treeMaster struct {
	fake   *transporttest.Fake
	source string
	// failFileList makes fileserver.file_list fail.
	failFileList bool
}

func newTreeMaster() *treeMaster {
	m := &treeMaster{source: treeSource}
	m.fake = transporttest.New().OnFunc(m.reply)
	return m
}

func (m *treeMaster) reply(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
	script := cmd.Script
	switch {
	case strings.Contains(script, "fileserver.envs"):
		return transporttest.Reply{Stdout: treeEnvsJSON}, true
	case strings.Contains(script, "fileserver.file_list"):
		if m.failFileList {
			return transporttest.Reply{Stderr: "fileserver: No such environment", RC: 1}, true
		}
		if strings.Contains(script, "saltenv=dev") {
			return transporttest.Reply{Stdout: treeDevFiles}, true
		}
		return transporttest.Reply{Stdout: treeBaseFiles}, true
	case strings.Contains(script, "state.show_top"):
		return transporttest.Reply{Stdout: treeTopJSON}, true
	case strings.Contains(script, "state.show_sls"):
		return transporttest.Reply{Stdout: treeSlsNginxJSON}, true
	case strings.Contains(script, "state.show_highstate"):
		return transporttest.Reply{Stdout: treeHighstateJSON}, true
	case strings.Contains(script, "cp.get_file_str"):
		out, _ := json.Marshal(map[string]string{"web-01": m.source})
		return transporttest.Reply{Stdout: string(out)}, true
	}
	return transporttest.Reply{}, false
}

// treeCalls returns the script lines the fake master saw.
func (m *treeMaster) calls() []string {
	cs := m.fake.Calls()
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Command.Script)
	}
	return out
}

// newTreeBackend wires a Salt backend over the scripted master with minions
// mapped through the salt.id label.
func newTreeBackend(t *testing.T, m *treeMaster, minions ...string) *Backend {
	t.Helper()
	r := NewRunner(m.fake, resource.HostID("saltmaster.example.internal"), RunnerOptions{})
	return New(Options{
		Runner:    r,
		Inventory: newExeInv(minions...),
		Master:    "saltmaster.example.internal",
	})
}

// treeHosts is the host list of the given minions.
func treeHosts(minions ...string) []resource.HostID {
	out := make([]resource.HostID, 0, len(minions))
	for _, m := range minions {
		out = append(out, exeHost(m))
	}
	return out
}

// ---- parser tests ----

// TestSlsNameDerivation covers the sls names Salt addressable from file paths.
func TestSlsNameDerivation(t *testing.T) {
	cases := []struct {
		path string
		name string
		ok   bool
	}{
		{"nginx/init.sls", "nginx", true},
		{"nginx/conf.sls", "nginx.conf", true},
		{"common/init.sls", "common", true},
		{"init.sls", "init", true},
		{"nginx/sub/deep.sls", "nginx.sub.deep", true},
		{"top.sls", "", false},
		{"notes.md", "", false},
		{"_modules/helpers.py", "", false},
		{"/nginx/init.sls", "nginx", true},
		{"", "", false},
	}
	for _, c := range cases {
		name, ok := SlsName(c.path)
		if ok != c.ok || name != c.name {
			t.Errorf("SlsName(%q) = %q, %v; want %q, %v", c.path, name, ok, c.name, c.ok)
		}
	}
}

// TestSlsParseFileServerEnvs: fileserver.envs answers with a bare list, and the
// object form is accepted too.
func TestSlsParseFileServerEnvs(t *testing.T) {
	envs, err := ParseFileServerEnvs([]byte("WARNING: banner\n" + treeEnvsJSON))
	if err != nil {
		t.Fatalf("ParseFileServerEnvs: %v", err)
	}
	if strings.Join(envs, ",") != "base,dev" {
		t.Errorf("envs = %v, want base,dev", envs)
	}
	// The sources=True form is an object of env -> directories.
	envs, err = ParseFileServerEnvs([]byte(`{"base":["/srv/salt"],"dev":["/srv/salt-dev"]}`))
	if err != nil {
		t.Fatalf("ParseFileServerEnvs object: %v", err)
	}
	if strings.Join(envs, ",") != "base,dev" {
		t.Errorf("object envs = %v, want base,dev", envs)
	}
	if _, err := ParseFileServerEnvs([]byte("not json")); err == nil {
		t.Error("ParseFileServerEnvs accepted garbage")
	}
}

// TestSlsParseFileList: fileserver.file_list answers with a bare list.
func TestSlsParseFileList(t *testing.T) {
	files, err := ParseFileServerFiles([]byte(treeBaseFiles))
	if err != nil {
		t.Fatalf("ParseFileServerFiles: %v", err)
	}
	if len(files) != 6 || files[0] != "top.sls" {
		t.Errorf("files = %v", files)
	}
}

// TestShowTopParse: the per-minion top file comes back as env -> sls.
func TestShowTopParse(t *testing.T) {
	top, err := ParseShowTop([]byte(treeTopJSON))
	if err != nil {
		t.Fatalf("ParseShowTop: %v", err)
	}
	if got := strings.Join(top.SLS("web-01"), ","); got != "common,nginx" {
		t.Errorf("web-01 top = %q, want common,nginx", got)
	}
	if got := strings.Join(top.SLS("db-01"), ","); got != "common" {
		t.Errorf("db-01 top = %q, want common", got)
	}
	if len(top.Errors) != 0 {
		t.Errorf("errors = %v", top.Errors)
	}

	// A minion that did not answer is reported, not dropped.
	top, err = ParseShowTop([]byte(`{"web-01":{"base":["common"]},"web-09":false}`))
	if err != nil {
		t.Fatalf("ParseShowTop no-return: %v", err)
	}
	if got := strings.Join(top.Errors["web-09"], ";"); !strings.Contains(got, "no return") {
		t.Errorf("web-09 error = %q, want no return", got)
	}
}

// TestShowHighstateParse flattens rendered states into rows.
func TestShowHighstateParse(t *testing.T) {
	st, err := ParseShowState([]byte(treeHighstateJSON))
	if err != nil {
		t.Fatalf("ParseShowState: %v", err)
	}
	if len(st.Defs) != 2 {
		t.Fatalf("defs = %d, want 2 (%+v)", len(st.Defs), st.Defs)
	}
	// State ids are sorted, so common comes before nginx.
	first := st.Defs[0]
	if first.ID != "common" || first.Module != "pkg" || first.Fun != "installed" {
		t.Errorf("first def = %+v, want common pkg.installed", first)
	}
	if first.SLS != "common" || first.Env != "base" || first.Name != "ca-certificates" {
		t.Errorf("first def fields = %+v", first)
	}
	second := st.Defs[1]
	if second.Function() != "service.running" || second.Name != "nginx" {
		t.Errorf("second def = %+v", second)
	}
}

// TestSlsShowRowsFlattensRequisites: show_sls rows carry module.fun, name and
// the requisites summarized.
func TestSlsShowRowsFlattensRequisites(t *testing.T) {
	st, err := ParseShowState([]byte(treeSlsNginxJSON))
	if err != nil {
		t.Fatalf("ParseShowState: %v", err)
	}
	if len(st.Defs) != 3 {
		t.Fatalf("defs = %d, want 3 (%+v)", len(st.Defs), st.Defs)
	}
	// One row per state module; every row of one state carries its state id.
	byID := map[string]StateDef{}
	for _, d := range st.Defs {
		if d.ID != "nginx" {
			t.Fatalf("def id = %q, want nginx", d.ID)
		}
		byID[d.Module] = d
	}
	if got := byID["pkg"].Function(); got != "pkg.installed" {
		t.Errorf("pkg function = %q", got)
	}
	if got := byID["pkg"].Requisites; got != "" {
		t.Errorf("pkg requisites = %q, want none", got)
	}
	if got := byID["service"].Requisites; got != "require pkg:nginx" {
		t.Errorf("service requisites = %q", got)
	}
	if got := byID["file"].Requisites; got != "require pkg:nginx, service:nginx; watch file:/etc/nginx/sites-available/default" {
		t.Errorf("file requisites = %q", got)
	}
	if got := byID["file"].Name; got != "/etc/nginx/nginx.conf" {
		t.Errorf("file name = %q", got)
	}
}

// TestShowHighstateErrors: a render error is shown, never returned as failure.
func TestShowHighstateErrors(t *testing.T) {
	st, err := ParseShowState([]byte(treeSlsErrorJSON))
	if err != nil {
		t.Fatalf("ParseShowState returned an error for a render error: %v", err)
	}
	if len(st.Errors) != 1 || !strings.Contains(st.Errors[0].Text, "Jinja error") {
		t.Fatalf("errors = %+v", st.Errors)
	}
	if st.Errors[0].Minion != "web-01" {
		t.Errorf("error minion = %q", st.Errors[0].Minion)
	}
	// A bare list (no minion) is a render error too.
	st, err = ParseShowState([]byte(`["Rendering SLS failed"]`))
	if err != nil {
		t.Fatalf("ParseShowState bare list: %v", err)
	}
	if len(st.Errors) != 1 || !strings.Contains(st.Errors[0].Text, "Rendering SLS failed") {
		t.Errorf("bare-list errors = %+v", st.Errors)
	}
}

// ---- backend tests ----

// TestTreeBuildsFilesAndAssignments: List(salt-sls) derives sls names, keeps
// the path and counts the targeted minions whose top includes each sls.
func TestTreeBuildsFilesAndAssignments(t *testing.T) {
	m := newTreeMaster()
	b := newTreeBackend(t, m, "web-01", "web-02", "db-01")

	q := resource.Query{Kind: KindSls, Hosts: treeHosts("web-01", "web-02", "db-01")}
	tree, err := b.Tree(context.Background(), q)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if strings.Join(tree.Envs, ",") != "base,dev" {
		t.Errorf("envs = %v", tree.Envs)
	}
	// base: common, nginx, nginx.conf; dev: site.
	if len(tree.Files) != 4 {
		t.Fatalf("files = %+v, want 4", tree.Files)
	}
	found := map[string]SlsFile{}
	for _, f := range tree.Files {
		found[f.Env+"/"+f.Name] = f
	}
	if f, ok := found["base/nginx"]; !ok || f.Path != "nginx/init.sls" {
		t.Errorf("base/nginx = %+v", f)
	}
	if f, ok := found["base/nginx.conf"]; !ok || f.Path != "nginx/conf.sls" {
		t.Errorf("base/nginx.conf = %+v", f)
	}
	if _, ok := found["base/top"]; ok {
		t.Error("the top file became an sls")
	}
	if !tree.AssignedOK {
		t.Fatalf("assigned was not computed: %v", tree.TopErr)
	}
	if got := tree.Assigned[SlsKey("base", "nginx")]; got != 2 {
		t.Errorf("nginx assigned = %d, want 2", got)
	}
	if got := tree.Assigned[SlsKey("base", "common")]; got != 3 {
		t.Errorf("common assigned = %d, want 3", got)
	}
	if got := tree.Assigned[SlsKey("base", "nginx.conf")]; got != 0 {
		t.Errorf("nginx.conf assigned = %d, want 0", got)
	}
}

// TestTreeRows: the registry kind is served by List with one row per sls.
func TestTreeRows(t *testing.T) {
	m := newTreeMaster()
	b := newTreeBackend(t, m, "web-01", "web-02", "db-01")

	ch, err := b.List(context.Background(), resource.Query{Kind: KindSls, Hosts: treeHosts("web-01")})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var items []resource.Resource
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("result error: %v", res.Err)
		}
		items = append(items, res.Items...)
	}
	if len(items) != 4 {
		t.Fatalf("rows = %d, want 4", len(items))
	}
	for _, r := range items {
		if r.Ref.Kind != KindSls {
			t.Fatalf("ref kind = %q", r.Ref.Kind)
		}
		if r.Fields["sls"] == "" || r.Fields["env"] == "" || r.Fields["path"] == "" {
			t.Errorf("row %v is missing a field", r.Fields)
		}
		if r.Ref.Namespace != r.Fields["env"] {
			t.Errorf("row %v namespace = %q, want the env", r.Fields, r.Ref.Namespace)
		}
		if _, ok := r.Fields["assigned"]; !ok {
			t.Errorf("row %v has no assigned count", r.Fields)
		}
	}
	// Unknown kind still errors.
	if _, err := b.List(context.Background(), resource.Query{Kind: "nope"}); err == nil {
		t.Error("List accepted an unknown kind")
	}
}

// TestTreeEnvFilter: q.Args["env"] narrows the listing to one environment.
func TestTreeEnvFilter(t *testing.T) {
	m := newTreeMaster()
	b := newTreeBackend(t, m, "web-01")
	q := resource.Query{Kind: KindSls, Args: map[string]string{"env": "dev"}, Hosts: treeHosts("web-01")}
	tree, err := b.Tree(context.Background(), q)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree.Files) != 1 || tree.Files[0].Env != "dev" || tree.Files[0].Name != "site" {
		t.Fatalf("files = %+v, want only dev/site", tree.Files)
	}
	if got := tree.Assigned[SlsKey("dev", "site")]; got != 0 {
		t.Errorf("dev/site assigned = %d, want 0", got)
	}
}

// TestTreeWithoutTargetsSkipsShowTop: no session targets means no top file job.
func TestTreeWithoutTargetsSkipsShowTop(t *testing.T) {
	m := newTreeMaster()
	b := newTreeBackend(t, m, "web-01")
	tree, err := b.Tree(context.Background(), resource.Query{Kind: KindSls})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if tree.AssignedOK {
		t.Error("assigned was computed without targets")
	}
	for _, c := range m.calls() {
		if strings.Contains(c, "state.show_top") {
			t.Errorf("state.show_top ran without targets: %s", c)
		}
	}
}

// TestShowTopTargetsTheMinions: the top file is one targeted call for every
// session minion.
func TestShowTopTargetsTheMinions(t *testing.T) {
	m := newTreeMaster()
	b := newTreeBackend(t, m, "web-01", "db-01")
	top, err := b.TopFile(context.Background(), []string{"db-01", "web-01"})
	if err != nil {
		t.Fatalf("TopFile: %v", err)
	}
	if got := strings.Join(top.SLS("web-01"), ","); got != "common,nginx" {
		t.Errorf("web-01 top = %q", got)
	}
	var line string
	for _, c := range m.calls() {
		if strings.Contains(c, "state.show_top") {
			line = c
		}
	}
	if !strings.Contains(line, "-L") || !strings.Contains(line, "db-01,web-01") {
		t.Errorf("show_top call = %q, want one -L call for both minions", line)
	}
}

// TestSlsSource: the raw sls source comes from cp.get_file_str on the master's
// own minion.
func TestSlsSource(t *testing.T) {
	m := newTreeMaster()
	b := newTreeBackend(t, m, "web-01")
	src, err := b.SlsSource(context.Background(), "web-01", "nginx/init.sls")
	if err != nil {
		t.Fatalf("SlsSource: %v", err)
	}
	if !strings.Contains(src, "file.managed") {
		t.Errorf("source = %q", src)
	}
	var line string
	for _, c := range m.calls() {
		if strings.Contains(c, "cp.get_file_str") {
			line = c
		}
	}
	if !strings.Contains(line, "salt://nginx/init.sls") {
		t.Errorf("source call = %q", line)
	}
}

// TestSlsSourceNotReadable: a false return is an NotAvailable error.
func TestSlsSourceNotReadable(t *testing.T) {
	m := &treeMaster{fake: transporttest.New()}
	m.fake.OnFunc(func(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if strings.Contains(cmd.Script, "cp.get_file_str") {
			return transporttest.Reply{Stdout: `{"web-01": false}`}, true
		}
		return transporttest.Reply{}, false
	})
	b := newTreeBackend(t, m, "web-01")
	_, err := b.SlsSource(context.Background(), "web-01", "nginx/init.sls")
	if err == nil {
		t.Fatal("SlsSource accepted a false return")
	}
	if kind := errs.KindOf(err); kind != errs.NotAvailable {
		t.Errorf("kind = %q, want %q", kind, errs.NotAvailable)
	}
}

// TestSlsSourceRejectsBadPaths: user text never reaches the command line
// unvalidated.
func TestSlsSourceRejectsBadPaths(t *testing.T) {
	m := newTreeMaster()
	b := newTreeBackend(t, m, "web-01")
	for _, p := range []string{"", "../etc/passwd", "/etc/passwd", "nginx/../../etc/passwd"} {
		_, err := b.SlsSource(context.Background(), "web-01", p)
		if err == nil {
			t.Errorf("SlsSource accepted path %q", p)
			continue
		}
		if kind := errs.KindOf(err); kind != errs.Config {
			t.Errorf("path %q kind = %q, want %q", p, kind, errs.Config)
		}
	}
}

// TestTreeFileListErrorIsReported: one failing environment does not hide the
// others, and the failure is carried for the view.
func TestTreeFileListErrorIsReported(t *testing.T) {
	m := newTreeMaster()
	m.failFileList = true
	b := newTreeBackend(t, m, "web-01")
	tree, err := b.Tree(context.Background(), resource.Query{Kind: KindSls, Hosts: treeHosts("web-01")})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree.Files) != 0 {
		t.Errorf("files = %+v, want none", tree.Files)
	}
	if len(tree.Errors) == 0 {
		t.Error("the file list failure was dropped")
	}
}

// TestSlsGoldens parses the captured lab output when the capture exists.
func TestSlsGoldens(t *testing.T) {
	type golden struct {
		name  string
		parse func([]byte) error
	}
	goldens := []golden{
		{"fileserver-envs.json", func(b []byte) error { _, err := ParseFileServerEnvs(b); return err }},
		{"fileserver-file-list.json", func(b []byte) error { _, err := ParseFileServerFiles(b); return err }},
		{"show-top.json", func(b []byte) error { _, err := ParseShowTop(b); return err }},
		{"show-sls-nginx.json", func(b []byte) error { _, err := ParseShowState(b); return err }},
		{"show-highstate.json", func(b []byte) error { _, err := ParseShowState(b); return err }},
	}
	for _, g := range goldens {
		g := g
		t.Run(g.name, func(t *testing.T) {
			p := filepath.Join("testdata", "salt", g.name)
			b, err := os.ReadFile(p)
			if err != nil {
				if os.IsNotExist(err) {
					t.Skip("golden not captured")
				}
				t.Fatalf("read %s: %v", p, err)
			}
			if err := g.parse(b); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
		})
	}
}

// TestSlsKindSpec: the salt-sls kind is global, aliased and has the four
// columns the browser shows.
func TestSlsKindSpec(t *testing.T) {
	var spec resource.KindSpec
	for _, s := range kindSpecs() {
		if s.Name == KindSls {
			spec = s
		}
	}
	if spec.Name == "" {
		t.Fatal("no salt-sls kind")
	}
	if spec.Scope != resource.ScopeGlobal {
		t.Errorf("scope = %v, want global", spec.Scope)
	}
	if strings.Join(spec.Aliases, ",") != "states,sls" {
		t.Errorf("aliases = %v", spec.Aliases)
	}
	for _, col := range []string{"sls", "env", "path", "assigned"} {
		if _, ok := spec.Column(col); !ok {
			t.Errorf("salt-sls lacks column %q", col)
		}
	}
	if kind, ok := normalizeKind("states"); !ok || kind != KindSls {
		t.Errorf("normalizeKind(states) = %q, %v", kind, ok)
	}
}

// TestTreeRunnerless checks the guards: no runner is a Config error, not a
// panic.
func TestTreeRunnerless(t *testing.T) {
	b := New(Options{})
	if _, err := b.Tree(context.Background(), resource.Query{Kind: KindSls}); err == nil {
		t.Fatal("Tree without a runner succeeded")
	}
	if _, err := b.SlsSource(context.Background(), "", ""); err == nil {
		t.Fatal("SlsSource without a runner succeeded")
	}
}
