package grafana

import (
	"encoding/json"
	"reflect"
	"testing"

	"jumper/internal/resource"
)

func TestDatasourceShapes(t *testing.T) {
	cases := []struct {
		name string
		json string
		want Datasource
	}{
		{"object", `{"uid":"prom-1","type":"prometheus"}`, Datasource{UID: "prom-1", Type: "prometheus"}},
		{"legacy type name", `"Prometheus"`, Datasource{Name: "Prometheus", Type: "prometheus"}},
		{"legacy uid", `"prom-1"`, Datasource{Name: "prom-1", UID: "prom-1"}},
		{"null", `null`, Datasource{}},
		{"garbage", `[1,2]`, Datasource{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Datasource
			if err := json.Unmarshal([]byte(tc.json), &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got != tc.want {
				t.Errorf("Datasource = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDatasourceMarshalUsesLegacyNameAsUID(t *testing.T) {
	out, err := json.Marshal(Datasource{Name: "Prometheus", Type: "prometheus"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := map[string]string{"uid": "Prometheus", "type": "prometheus"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("marshalled datasource = %v, want %v", got, want)
	}
}

func TestVariableShapes(t *testing.T) {
	body := `{
	  "name": "instance",
	  "type": "query",
	  "datasource": "Prometheus",
	  "query": {"query": "label_values(node_load1, instance)", "refId": "A"},
	  "current": {"text": ["web-01", "web-02"], "value": ["web-01", "web-02"]},
	  "options": ["web-01", {"text": "web-02", "value": "web-02", "selected": true}]
	}`
	var v Variable
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if v.Name != "instance" || v.Type != "query" {
		t.Errorf("variable = %+v, want instance/query", v)
	}
	if want := []string{"web-01", "web-02"}; !reflect.DeepEqual(v.Current, want) {
		t.Errorf("current = %v, want %v", v.Current, want)
	}
	if len(v.Options) != 2 || v.Options[1].Value != "web-02" || !v.Options[1].Selected {
		t.Errorf("options = %+v, want a string and an object", v.Options)
	}
	if got, want := v.Query.String(), "label_values(node_load1, instance)"; got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
	if got, want := v.Query.RefID, "A"; got != want {
		t.Errorf("refId = %q, want %q", got, want)
	}

	// A single object current value and a string query.
	body = `{"name":"host","current":{"text":"web-01","value":"web-01"},"query":"label_values(up, instance)"}`
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if v.Value() != "web-01" {
		t.Errorf("value = %q, want web-01", v.Value())
	}
	if _, ok := v.LabelValuesExpr(); !ok {
		t.Errorf("query %q is not recognised as label_values", v.Query.String())
	}

	// A list current value without an object wrapper.
	body = `{"name":"multi","current":["a","b"],"options":[]}`
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(v.Current, want) {
		t.Errorf("current = %v, want %v", v.Current, want)
	}
}

func TestAutoFill(t *testing.T) {
	d := &Dashboard{Variables: []Variable{
		{Name: "host"},
		{Name: "instance"},
		{Name: "node"},
		{Name: "env", Current: []string{"prod"}},
		{Name: "host", Current: []string{"db-01"}, Auto: true},
	}}
	filled := d.AutoFill([]resource.HostID{"web-01.example.internal", "web-02.example.internal"})
	want := []string{"host", "instance", "node", "host"}
	if !reflect.DeepEqual(filled, want) {
		t.Fatalf("filled = %v, want %v", filled, want)
	}
	for _, i := range []int{0, 1, 2, 4} {
		v := d.Variables[i]
		if !v.Auto || v.Value() != "web-01.example.internal" {
			t.Errorf("variable %q = %+v, want web-01 marked auto", v.Name, v)
		}
	}
	if env := d.Variables[3]; env.Auto || env.Value() != "prod" {
		t.Errorf("env = %+v, want the user's value untouched", env)
	}

	// No hosts changes nothing.
	if got := d.AutoFill(nil); got != nil {
		t.Errorf("AutoFill(nil) = %v, want nil", got)
	}
}

func TestAutoFillKeepsUserChoice(t *testing.T) {
	d := &Dashboard{Variables: []Variable{{Name: "host", Current: []string{"db-01"}}}}
	if got := d.AutoFill([]resource.HostID{"web-01"}); got != nil {
		t.Fatalf("AutoFill = %v, want no fill for a user-set host", got)
	}
	if got := d.Variables[0].Value(); got != "db-01" {
		t.Errorf("host = %q, want db-01", got)
	}
}

func TestLabelValuesExpr(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"label_values(node_load1, instance)", true},
		{" label_values( up , job ) ", true},
		{"label_values(node_load1{job=\"node\"}, instance)", false},
		{"query_result(up > 0)", false},
		{`label_values(rate(x[5m]), instance)`, false},
		{"", false},
	}
	for _, tc := range cases {
		v := Variable{Query: VariableQuery{Expr: tc.query}}
		if _, ok := v.LabelValuesExpr(); ok != tc.want {
			t.Errorf("LabelValuesExpr(%q) = %v, want %v", tc.query, ok, tc.want)
		}
	}
}

func TestPanelRowsDefensive(t *testing.T) {
	// Every shape a real dashboard carries: a row with no title, a row with no
	// children, targets absent, datasource null.
	body := `{
	  "templating": {"list": []},
	  "panels": [
	    {"id": "17", "type": "row", "panels": [
	      {"id": 1, "type": "stat", "title": "Up", "datasource": null}
	    ]},
	    {"type": "row"},
	    {"type": "timeseries", "title": "No id"}
	  ]
	}`
	var d Dashboard
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	rows := d.PanelRows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].ID != 1 || !rows[0].Supported || rows[0].Section != "" {
		t.Errorf("first row = %+v, want panel 1 under an untitled section", rows[0])
	}
	if rows[1].ID != 0 || !rows[1].Supported {
		t.Errorf("second row = %+v, want the id-less timeseries", rows[1])
	}
	if n := len(d.Variables); n != 0 {
		t.Errorf("variables = %d, want 0", n)
	}
}

func TestPanelRowsEmpty(t *testing.T) {
	var d *Dashboard
	if got := d.PanelRows(); got != nil {
		t.Errorf("PanelRows on nil = %v, want nil", got)
	}
	if got := (&Dashboard{}).PanelRows(); got != nil {
		t.Errorf("PanelRows on an empty dashboard = %v, want nil", got)
	}
}

func TestDashboardPaths(t *testing.T) {
	if got, want := DashboardPath("a1b2c3", "node-overview"), "/d/a1b2c3/node-overview"; got != want {
		t.Errorf("DashboardPath = %q, want %q", got, want)
	}
	if got, want := DashboardPath("a1b2c3", ""), "/d/a1b2c3/a1b2c3"; got != want {
		t.Errorf("DashboardPath without a slug = %q, want %q", got, want)
	}
	if got := DashboardPath("", "slug"); got != "" {
		t.Errorf("DashboardPath without a uid = %q, want empty", got)
	}
	if got, want := PanelPath("a1b2c3", "node-overview", 4), "/d/a1b2c3/node-overview?viewPanel=4"; got != want {
		t.Errorf("PanelPath = %q, want %q", got, want)
	}
}

func TestSetVariableAndValues(t *testing.T) {
	d := &Dashboard{Variables: []Variable{
		{Name: "env", Options: []VariableOption{{Text: "prod", Value: "prod"}}},
		{Name: "host"},
	}}
	if !d.SetVariable("$env", "staging") {
		t.Fatal("SetVariable did not find $env")
	}
	if got := d.Values(); got["env"] != "staging" || got["host"] != "" {
		t.Errorf("values = %v, want env=staging and an empty host", got)
	}
	if d.SetVariable("nope", "x") {
		t.Error("SetVariable accepted an unknown variable")
	}
}

func TestLabelString(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   string
	}{
		{nil, "{}"},
		{map[string]string{"alertname": "NodeDown"}, `{alertname="NodeDown"}`},
		{
			map[string]string{"severity": "critical", "alertname": "NodeDown"},
			`{alertname="NodeDown", severity="critical"}`,
		},
	}
	for _, tc := range cases {
		if got := LabelString(tc.labels); got != tc.want {
			t.Errorf("LabelString(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}
