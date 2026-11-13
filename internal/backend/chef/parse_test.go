package chef

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/redact"
)

// testTime is the clock the fixtures are written against: web-01 checked in 5
// minutes before it, web-02 3 hours before it, db-01 30 hours before it.
var testTime = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// chefTestdata is where the capture tool writes the fixtures: the repository's
// testdata/chef, three levels above this package (task 113).
const chefTestdata = "../../../testdata/chef/"

// readFixture returns one file of testdata/chef.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(chefTestdata + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// TestParseStatus checks `knife status -F json` decoding: the array form every
// current knife prints, including the epoch ohai_time and a node without one.
func TestParseStatus(t *testing.T) {
	nodes, err := ParseStatus(readFixture(t, "status.json"))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if len(nodes) != 4 {
		t.Fatalf("nodes = %d, want 4", len(nodes))
	}
	byName := map[string]StatusNode{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	web := byName["web-01"]
	if web.Environment != "production" || web.Platform != "ubuntu" || web.PlatformVersion != "22.04" {
		t.Errorf("web-01 = %+v", web)
	}
	if got := strings.Join(web.RunList, ","); got != "role[base],role[web]" {
		t.Errorf("web-01 run list = %q", got)
	}
	if web.FQDN != "web-01.lab.example.internal" || web.IP != "10.0.0.11" {
		t.Errorf("web-01 fqdn/ip = %q/%q", web.FQDN, web.IP)
	}
	if !web.OhaiTime.Equal(testTime.Add(-5 * time.Minute)) {
		t.Errorf("web-01 ohai_time = %s, want %s", web.OhaiTime, testTime.Add(-5*time.Minute))
	}
	if !byName["web-03"].OhaiTime.IsZero() {
		t.Errorf("web-03 ohai_time = %s, want zero", byName["web-03"].OhaiTime)
	}
}

// TestParseStatusObjectForm checks the object-keyed form older knife prints,
// and that rows it cannot read are skipped rather than failing the batch.
func TestParseStatusObjectForm(t *testing.T) {
	body := `{
	  "web-02": {"chef_environment": "staging", "run_list": "role[base]", "ohai_time": "1794570900"},
	  "web-01": {"platform": "ubuntu"},
	  "broken": 42
	}`
	nodes, err := ParseStatus([]byte(body))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %+v, want web-01 and web-02", nodes)
	}
	if nodes[0].Name != "web-01" || nodes[1].Name != "web-02" {
		t.Errorf("order = %q, %q, want web-01, web-02", nodes[0].Name, nodes[1].Name)
	}
	if got := strings.Join(nodes[1].RunList, ","); got != "role[base]" {
		t.Errorf("run list = %q, want the single listed entry", got)
	}
	if nodes[1].OhaiTime.IsZero() {
		t.Error("numeric-string ohai_time not parsed")
	}
}

// TestParseStatusRejectsGarbage checks unparsable output is an errs.Parse error,
// never a panic.
func TestParseStatusRejectsGarbage(t *testing.T) {
	if _, err := ParseStatus([]byte("ERROR: no server configured\n")); errs.KindOf(err) != errs.Parse {
		t.Fatalf("kind = %q, want parse (%v)", errs.KindOf(err), err)
	}
}

// TestParseNodeDetail checks the long node form: the summary, the roles of the
// run list, the attributes grouped by precedence and the redaction of a secret
// attribute.
func TestParseNodeDetail(t *testing.T) {
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redactor: %v", err)
	}
	d, err := ParseNodeDetail(readFixture(t, "node-show.json"), red)
	if err != nil {
		t.Fatalf("ParseNodeDetail: %v", err)
	}
	if d.Name != "web-01" || d.Environment != "production" || d.Platform != "ubuntu" || d.PlatformVersion != "22.04" {
		t.Errorf("summary = %+v", d)
	}
	if got := strings.Join(d.Roles, ","); got != "base,web" {
		t.Errorf("roles = %q, want base,web", got)
	}
	if got := strings.Join(d.RunList, ","); got != "role[base],role[web],recipe[nginx::default]" {
		t.Errorf("run list = %q", got)
	}
	if len(d.Attributes) != 4 {
		t.Fatalf("attribute groups = %d, want normal, override, default, automatic: %+v", len(d.Attributes), d.Attributes)
	}
	wantPrec := []string{"normal", "override", "default", "automatic"}
	for i, want := range wantPrec {
		if d.Attributes[i].Precedence != want {
			t.Errorf("group %d = %q, want %q", i, d.Attributes[i].Precedence, want)
		}
	}
	normal := strings.Join(d.Attributes[0].Lines, "\n")
	if !strings.Contains(normal, "jumper.owner = platform") {
		t.Errorf("normal attributes missing:\n%s", normal)
	}
	automatic := d.Attributes[3]
	joined := strings.Join(automatic.Lines, "\n")
	if !strings.Contains(joined, "kernel.name = Linux") {
		t.Errorf("nested attribute missing:\n%s", joined)
	}
	if !strings.Contains(joined, "kernel.release = 5.15.0-105-generic") {
		t.Errorf("nested attribute missing:\n%s", joined)
	}
	if strings.Contains(joined, "hunter2-not-real") {
		t.Errorf("secret attribute shown:\n%s", joined)
	}
	if !strings.Contains(joined, "mysql.password =") {
		t.Errorf("secret key not kept readable:\n%s", joined)
	}
}

// TestParseNodeDetailCapsAttributes checks the attribute cap: a node with far
// more attributes than the cap keeps the first lines and counts the rest.
func TestParseNodeDetailCapsAttributes(t *testing.T) {
	attrs := make(map[string]any, maxAttrLines+50)
	for i := 0; i < maxAttrLines+50; i++ {
		attrs[fmt.Sprintf("key_%03d", i)] = "value"
	}
	body, err := json.Marshal(map[string]any{"name": "big", "normal": attrs})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	d, err := ParseNodeDetail(body, nil)
	if err != nil {
		t.Fatalf("ParseNodeDetail: %v", err)
	}
	if len(d.Attributes) != 1 {
		t.Fatalf("groups = %d, want 1", len(d.Attributes))
	}
	g := d.Attributes[0]
	if len(g.Lines) != maxAttrLines {
		t.Errorf("lines = %d, want %d", len(g.Lines), maxAttrLines)
	}
	if g.Dropped != 50 {
		t.Errorf("dropped = %d, want 50", g.Dropped)
	}
}

// TestParseNodeNames checks both shapes of a knife name list.
func TestParseNodeNames(t *testing.T) {
	names, err := ParseNodeNames(readFixture(t, "node-list.json"))
	if err != nil {
		t.Fatalf("ParseNodeNames: %v", err)
	}
	if len(names) != 4 || names[0] != "web-02" {
		t.Errorf("names = %v, want the array order kept", names)
	}
	obj, err := ParseNodeNames([]byte(`{"web-02":{},"web-01":{}}`))
	if err != nil {
		t.Fatalf("ParseNodeNames object: %v", err)
	}
	if len(obj) != 2 || obj[0] != "web-01" {
		t.Errorf("object names = %v, want sorted", obj)
	}
}

// TestParseSearch checks `knife search -a` rows, both the one-attribute scalar
// form and the several-attribute object form.
func TestParseSearch(t *testing.T) {
	rows, err := ParseSearch(readFixture(t, "search.json"))
	if err != nil {
		t.Fatalf("ParseSearch: %v", err)
	}
	if len(rows) != 3 || rows[0].Name != "web-01" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Attrs["value"] != "ubuntu" {
		t.Errorf("attrs = %v, want the scalar under value", rows[0].Attrs)
	}

	multi := `{"total":1,"start":0,"rows":[{"db-01":{"platform":"ubuntu","run_list":["role[base]"]}}]}`
	rows, err = ParseSearch([]byte(multi))
	if err != nil {
		t.Fatalf("ParseSearch multi: %v", err)
	}
	if len(rows) != 1 || rows[0].Attrs["platform"] != "ubuntu" {
		t.Errorf("multi rows = %+v", rows)
	}
	if _, ok := rows[0].Attrs["run_list"]; ok {
		t.Errorf("non-scalar attribute kept: %v", rows[0].Attrs)
	}
}

// TestFirstJSON checks the tolerant decode: banners are skipped, and output with
// no JSON at all is a parse error.
func TestFirstJSON(t *testing.T) {
	got, err := FirstJSON([]byte("warning: one\n\n  [1,2]\n"))
	if err != nil {
		t.Fatalf("FirstJSON: %v", err)
	}
	if string(got) != "[1,2]\n" {
		t.Errorf("got %q", got)
	}
	if _, err := FirstJSON([]byte("nothing here")); errs.KindOf(err) != errs.Parse {
		t.Errorf("kind = %q, want parse", errs.KindOf(err))
	}
}

// TestRolesFromRunList checks role extraction ignores recipes and duplicates.
func TestRolesFromRunList(t *testing.T) {
	got := rolesFromRunList([]string{"role[base]", "recipe[nginx]", "role[web]", "role[base]"})
	if strings.Join(got, ",") != "base,web" {
		t.Errorf("roles = %v, want [base web]", got)
	}
}
