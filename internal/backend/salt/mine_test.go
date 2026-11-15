package salt

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport/transporttest"
)

// mineReply builds a `salt-run cache.mine` reply from minion → function →
// value JSON.
func mineReply(t *testing.T, byMinion map[string]map[string]string) transporttest.Reply {
	t.Helper()
	out := make(map[string]map[string]json.RawMessage, len(byMinion))
	for minion, funcs := range byMinion {
		inner := make(map[string]json.RawMessage, len(funcs))
		for fun, v := range funcs {
			if !json.Valid([]byte(v)) {
				t.Fatalf("invalid mine fixture %s/%s: %s", minion, fun, v)
			}
			inner[fun] = json.RawMessage(v)
		}
		out[minion] = inner
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal mine reply: %v", err)
	}
	return transporttest.Reply{Stdout: string(b) + "\n"}
}

// newMineBackend builds a Backend over a scripted master.
func newMineBackend(t *testing.T, f *transporttest.Fake) *Backend {
	t.Helper()
	return New(Options{
		Runner: NewRunner(f, master, RunnerOptions{}),
		Master: master,
	})
}

// mineFixture is a two-minion mine cache.
func mineFixture(t *testing.T) *transporttest.Fake {
	t.Helper()
	return transporttest.New().On("cache.mine", mineReply(t, map[string]map[string]string{
		"web-01": {
			"network.ip_addrs": `["10.0.0.5", "10.0.0.6"]`,
			"grains.item":      `"web-01"`,
		},
		"db-01": {
			"network.ip_addrs": `["10.0.0.9"]`,
		},
	}))
}

// TestMineRows: the mine cache becomes one sorted row per minion and function,
// with a compact value and a size.
func TestMineRows(t *testing.T) {
	b := newMineBackend(t, mineFixture(t))
	entries, err := b.Mine(context.Background())
	if err != nil {
		t.Fatalf("Mine: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	if entries[0].Minion != "db-01" || entries[0].Function != "network.ip_addrs" {
		t.Errorf("first entry = %+v, want db-01/network.ip_addrs", entries[0])
	}
	if got := entries[0].Value; got != "10.0.0.9" {
		t.Errorf("db-01 value = %q, want 10.0.0.9", got)
	}
	if entries[0].Size == 0 {
		t.Errorf("db-01 size = 0, want the cached value's byte length")
	}
	if entries[1].Minion != "web-01" || entries[1].Function != "grains.item" {
		t.Errorf("second entry = %+v, want web-01/grains.item", entries[1])
	}
	if got := entries[2].Value; got != "10.0.0.5,10.0.0.6" {
		t.Errorf("web-01 ip_addrs = %q, want a comma-joined list", got)
	}
}

// TestMineListRows: kind salt-mine lists the four columns through List.
func TestMineListRows(t *testing.T) {
	b := newMineBackend(t, mineFixture(t))
	rows := collectRows(t, b, resource.Query{Kind: KindMine})
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	row := rows[0]
	if row.Ref.Kind != KindMine || row.Ref.Namespace != "db-01" || row.Ref.Name != "network.ip_addrs" {
		t.Errorf("ref = %s, want salt/salt-mine/db-01//network.ip_addrs", row.Ref.String())
	}
	for _, col := range []string{"minion", "function", "value", "size"} {
		if row.Fields[col] == nil {
			t.Errorf("row lacks field %q: %+v", col, row.Fields)
		}
	}
	if _, ok := row.Fields["size"].(int64); !ok {
		t.Errorf("size = %T, want int64", row.Fields["size"])
	}
}

// TestMineCompactValue: a nested, multi-line value is one line of at most 80
// cells.
func TestMineCompactValue(t *testing.T) {
	long := strings.Repeat("abcdefghij", 20)
	f := transporttest.New().On("cache.mine", mineReply(t, map[string]map[string]string{
		"web-01": {
			"nested": `{"a": {"b": [1, 2, 3]}, "c": "d"}`,
			"long":   `"` + long + `"`,
		},
	}))
	b := newMineBackend(t, f)
	entries, err := b.Mine(context.Background())
	if err != nil {
		t.Fatalf("Mine: %v", err)
	}
	byFun := map[string]MineEntry{}
	for _, e := range entries {
		byFun[e.Function] = e
	}
	nested := byFun["nested"].Value
	if strings.ContainsAny(nested, "\n\r\t") {
		t.Errorf("nested value %q is not one line", nested)
	}
	if nested == "" || !strings.Contains(nested, `"c":"d"`) {
		t.Errorf("nested value = %q, want compact JSON", nested)
	}
	if got := len([]rune(byFun["long"].Value)); got > mineValueWidth {
		t.Errorf("long value is %d cells, want at most %d", got, mineValueWidth)
	}
	if !strings.HasSuffix(byFun["long"].Value, "…") {
		t.Errorf("long value = %q, want an ellipsis tail", byFun["long"].Value)
	}
}

// TestMineValuePrettyRedacted: the full value is pretty-printed and a secret in
// it is redacted before it is returned.
func TestMineValuePrettyRedacted(t *testing.T) {
	f := transporttest.New().On("cache.mine", mineReply(t, map[string]map[string]string{
		"web-01": {
			"config": `{"password": "hunter2", "port": 8080}`,
		},
	}))
	b := newMineBackend(t, f)
	value, err := b.MineValue(context.Background(), "web-01", "config")
	if err != nil {
		t.Fatalf("MineValue: %v", err)
	}
	if strings.Contains(value, "hunter2") {
		t.Fatalf("the value was not redacted: %s", value)
	}
	if !strings.Contains(value, "‹redacted:") {
		t.Errorf("value = %q, want a redaction marker", value)
	}
	if !strings.Contains(value, "\n  \"port\": 8080") {
		t.Errorf("value = %q, want two-space indented JSON", value)
	}
}

// TestMineValueCapped: a value larger than 256 KiB is cut, not returned whole.
func TestMineValueCapped(t *testing.T) {
	big := `"` + strings.Repeat("x", mineCap+2048) + `"`
	f := transporttest.New().On("cache.mine", mineReply(t, map[string]map[string]string{
		"web-01": {"huge": big},
	}))
	b := newMineBackend(t, f)
	value, err := b.MineValue(context.Background(), "web-01", "huge")
	if err != nil {
		t.Fatalf("MineValue: %v", err)
	}
	if len(value) > mineCap+64 {
		t.Errorf("value is %d bytes, want it capped near %d", len(value), mineCap)
	}
	if !strings.Contains(value, "more bytes") && !strings.Contains(value, "…") {
		t.Errorf("capped value lacks a cut notice: %q", value[len(value)-60:])
	}
}

// TestMineValueUnknownFunction: a function the cache does not hold is
// NotAvailable, never an empty string.
func TestMineValueUnknownFunction(t *testing.T) {
	b := newMineBackend(t, mineFixture(t))
	_, err := b.MineValue(context.Background(), "web-01", "no.such.function")
	if err == nil {
		t.Fatal("MineValue: want an error for an unknown function")
	}
	if kind := errs.KindOf(err); kind != errs.NotAvailable {
		t.Errorf("kind = %q, want %q", kind, errs.NotAvailable)
	}
}

// TestMineValueBadInput: an unsafe minion id or function never reaches a
// command line.
func TestMineValueBadInput(t *testing.T) {
	b := newMineBackend(t, mineFixture(t))
	for _, tc := range []struct{ minion, function string }{
		{"", "network.ip_addrs"},
		{"web-01; rm -rf /", "network.ip_addrs"},
		{"web-01", "-rf"},
		{"web-01", ""},
	} {
		if _, err := b.MineValue(context.Background(), tc.minion, tc.function); err == nil {
			t.Errorf("MineValue(%q, %q): want an error", tc.minion, tc.function)
		}
	}
}

// TestMineKindSpec: salt-mine is global, aliased as mine and carries the four
// columns of the browser.
func TestMineKindSpec(t *testing.T) {
	var spec resource.KindSpec
	for _, s := range kindSpecs() {
		if s.Name == KindMine {
			spec = s
		}
	}
	if spec.Name == "" {
		t.Fatal("no salt-mine kind")
	}
	if spec.Scope != resource.ScopeGlobal {
		t.Errorf("scope = %v, want global", spec.Scope)
	}
	if strings.Join(spec.Aliases, ",") != "mine" {
		t.Errorf("aliases = %v, want mine", spec.Aliases)
	}
	for _, col := range []string{"minion", "function", "value", "size"} {
		if _, ok := spec.Column(col); !ok {
			t.Errorf("salt-mine lacks column %q", col)
		}
	}
	if kind, ok := normalizeKind("mine"); !ok || kind != KindMine {
		t.Errorf("normalizeKind(mine) = %q, %v", kind, ok)
	}
}

// TestMineMinionGrains: one minion's grains come from the cache, and a minion
// the cache does not know names the live read.
func TestMineMinionGrains(t *testing.T) {
	f := transporttest.New().On("cache.grains", cacheReply(t, map[string]string{
		"web-01": `{"id": "web-01", "role": "web", "num_cpus": 4}`,
	}))
	b := newMineBackend(t, f)
	g, err := b.MinionGrains(context.Background(), "web-01")
	if err != nil {
		t.Fatalf("MinionGrains: %v", err)
	}
	if g.Str("role") != "web" || g.Str("num_cpus") != "4" {
		t.Errorf("grains = %v", g)
	}

	empty := newMineBackend(t, transporttest.New().On("cache.grains",
		transporttest.Reply{Stdout: "{}\n"}))
	_, err = empty.MinionGrains(context.Background(), "web-01")
	if err == nil {
		t.Fatal("MinionGrains: want an error for a minion the cache lacks")
	}
	if kind := errs.KindOf(err); kind != errs.NotAvailable {
		t.Errorf("kind = %q, want %q", kind, errs.NotAvailable)
	}
	if !strings.Contains(err.Error(), ":grains --live web-01") {
		t.Errorf("error = %q, want it to name the live read", err)
	}
}

// TestMineLiveGrains: the live read runs grains.items on the minion as a job,
// and answers with that minion's grains.
func TestMineLiveGrains(t *testing.T) {
	const live = `{"id": "web-01", "role": "web", "dc": "lab1"}`
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return pillarJobReply(t, jid, "grains.items", "web-01", live)
	})
	b := New(Options{Runner: NewRunner(m.fake, master, RunnerOptions{}), Master: master})

	ctx, cancel := context.WithTimeout(context.Background(), scaledWait(5*time.Second))
	defer cancel()
	g, err := b.LiveGrains(ctx, "web-01")
	if err != nil {
		t.Fatalf("LiveGrains: %v", err)
	}
	if g.Str("dc") != "lab1" || g.Str("role") != "web" {
		t.Errorf("grains = %v", g)
	}
	if m.submits != 1 {
		t.Errorf("submissions = %d, want 1", m.submits)
	}
	if m.pollCount(jidA) < 1 {
		t.Errorf("the job was never polled")
	}
}

// TestMineLiveGrainsNoReturn: a minion that never answered is a timeout, never
// an empty grain set that looks like a minion without grains.
func TestMineLiveGrainsNoReturn(t *testing.T) {
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return pillarJobReply(t, jid, "grains.items", "web-01", `false`)
	})
	b := New(Options{Runner: NewRunner(m.fake, master, RunnerOptions{}), Master: master})

	ctx, cancel := context.WithTimeout(context.Background(), scaledWait(5*time.Second))
	defer cancel()
	if _, err := b.LiveGrains(ctx, "web-01"); err == nil {
		t.Fatal("LiveGrains: want an error for a minion that did not return")
	} else if kind := errs.KindOf(err); kind != errs.Timeout {
		t.Errorf("kind = %q, want %q", kind, errs.Timeout)
	}
}

// TestMineGrainsDiff: a live read that differs from the cache is reported per
// key, sorted, with missing sides left empty.
func TestMineGrainsDiff(t *testing.T) {
	cached := grainsFrom(t, `{"id": "web-01", "role": "web", "num_cpus": 4, "gone": "x"}`)
	live := grainsFrom(t, `{"id": "web-01", "role": "web", "num_cpus": 8, "fresh": "y"}`)
	diffs := DiffGrains(cached, live)
	if len(diffs) != 3 {
		t.Fatalf("got %d diffs, want 3: %+v", len(diffs), diffs)
	}
	if diffs[0].Key != "fresh" || diffs[0].Cached != "" || diffs[0].Live != "y" {
		t.Errorf("diff[0] = %+v, want fresh added live", diffs[0])
	}
	if diffs[1].Key != "gone" || diffs[1].Cached != "x" || diffs[1].Live != "" {
		t.Errorf("diff[1] = %+v, want gone only cached", diffs[1])
	}
	if diffs[2].Key != "num_cpus" || diffs[2].Cached != "4" || diffs[2].Live != "8" {
		t.Errorf("diff[2] = %+v, want num_cpus 4 → 8", diffs[2])
	}
	if same := DiffGrains(cached, cached); len(same) != 0 {
		t.Errorf("DiffGrains(same, same) = %+v, want no diffs", same)
	}
}

// TestMineGrainsDistribution: the distribution counts distinct values across
// the fleet, most common first, and lists the minions that hold one value.
func TestMineGrainsDistribution(t *testing.T) {
	all := map[string]Grains{
		"web-01": grainsFrom(t, `{"role": "web", "ipv4": ["10.0.0.5", "10.0.0.6"]}`),
		"web-02": grainsFrom(t, `{"role": "web", "ipv4": ["10.0.0.7"]}`),
		"db-01":  grainsFrom(t, `{"role": "db"}`),
		"db-02":  grainsFrom(t, `{"role": "db", "zone": "b"}`),
	}
	dist := GrainDistribution(all, "role")
	if len(dist) != 2 {
		t.Fatalf("got %d values, want 2: %+v", len(dist), dist)
	}
	if dist[0].Value != "db" || dist[0].Count != 2 || dist[1].Value != "web" || dist[1].Count != 2 {
		t.Errorf("distribution = %+v, want db 2 then web 2 (value order)", dist)
	}
	ips := GrainDistribution(all, "ipv4")
	if len(ips) != 3 || ips[0].Count != 1 {
		t.Errorf("ipv4 distribution = %+v, want one row per address", ips)
	}
	if got := MinionsWithGrain(all, "role", "web"); strings.Join(got, ",") != "web-01,web-02" {
		t.Errorf("MinionsWithGrain(role=web) = %v", got)
	}
	if got := MinionsWithGrain(all, "ipv4", "10.0.0.6"); strings.Join(got, ",") != "web-01" {
		t.Errorf("MinionsWithGrain(ipv4 contains 10.0.0.6) = %v", got)
	}
	if got := MinionsWithGrain(all, "role", "nope"); len(got) != 0 {
		t.Errorf("MinionsWithGrain(role=nope) = %v, want none", got)
	}
	if got := GrainDistribution(all, "missing"); len(got) != 0 {
		t.Errorf("GrainDistribution(missing) = %+v, want none", got)
	}
}

// TestMineRunnerless: the reads guard on a missing runner instead of panicking.
func TestMineRunnerless(t *testing.T) {
	b := New(Options{Master: master})
	if _, err := b.Mine(context.Background()); err == nil {
		t.Error("Mine: want an error without a runner")
	}
	if _, err := b.AllGrains(context.Background()); err == nil {
		t.Error("AllGrains: want an error without a runner")
	}
	if _, err := b.LiveGrains(context.Background(), "web-01"); err == nil {
		t.Error("LiveGrains: want an error without a runner")
	}
}
