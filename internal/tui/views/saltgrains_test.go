package views

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/salt"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
)

// ---- fixtures ----

// fakeGrains is a Salt grains cache without a master.
type fakeGrains struct {
	cached   salt.Grains
	live     salt.Grains
	fleet    map[string]salt.Grains
	liveErr  error
	fleetErr error

	minion    string
	cacheRead int
	liveRead  int
}

func (f *fakeGrains) MinionGrains(_ context.Context, minion string) (salt.Grains, error) {
	f.minion = minion
	f.cacheRead++
	return f.cached, nil
}

func (f *fakeGrains) AllGrains(_ context.Context) (map[string]salt.Grains, error) {
	if f.fleetErr != nil {
		return nil, f.fleetErr
	}
	return f.fleet, nil
}

func (f *fakeGrains) LiveGrains(_ context.Context, minion string) (salt.Grains, error) {
	f.minion = minion
	f.liveRead++
	return f.live, f.liveErr
}

// MinionHost maps one fixture minion onto an inventory host; the others stay
// addressed by their own id.
func (f *fakeGrains) MinionHost(id string) (resource.HostID, bool) {
	if id == "web-01" {
		return "web-01.lab.example.internal", true
	}
	return "", false
}

// grainsFixture decodes one grains fixture the way the cache reply would.
func grainsFixture(t *testing.T, s string) salt.Grains {
	t.Helper()
	var g salt.Grains
	if err := json.Unmarshal([]byte(s), &g); err != nil {
		t.Fatalf("grains fixture: %v", err)
	}
	return g
}

// newGrainsFake is web-01 on the left and a three-minion fleet on the right.
func newGrainsFake(t *testing.T) *fakeGrains {
	t.Helper()
	web := grainsFixture(t, `{
		"id": "web-01",
		"role": "web",
		"num_cpus": 4,
		"ipv4": ["10.0.0.5", "10.0.0.6"],
		"nested": {"a": {"b": [1, 2]}}
	}`)
	return &fakeGrains{
		cached: web,
		live:   web,
		fleet: map[string]salt.Grains{
			"web-01": web,
			"web-02": grainsFixture(t, `{"role": "web", "num_cpus": 4}`),
			"db-01":  grainsFixture(t, `{"role": "db", "num_cpus": 8}`),
		},
	}
}

// grainsEnv is the environment the screens run in.
func grainsEnv() *tui.Env {
	return &tui.Env{
		Session: &tui.SessionState{Name: "test"},
		Now:     func() time.Time { return time.Unix(1730000000, 0) },
	}
}

// outward reports whether msg leaves the view (a dialog, a target change, a
// flash or navigation).
func outward(msg tea.Msg) bool {
	switch msg.(type) {
	case tui.OpenDialogMsg, tui.TargetsChangedMsg, tui.FlashMsg,
		tui.PopViewMsg, tui.PushViewMsg, tui.CloseDialogMsg:
		return true
	}
	return false
}

// settle feeds cmd and everything the view asks for back into the view and
// collects the messages it sends outwards.
func settle(t *testing.T, v tui.View, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	queue := []tea.Cmd{cmd}
	deadline := time.Now().Add(20 * time.Second)
	for len(queue) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the view did not settle")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if msg == nil {
			continue
		}
		if outward(msg) {
			out = append(out, msg)
			continue
		}
		if _, blink := msg.(cursor.BlinkMsg); blink {
			// The filter's cursor blink is not work: do not chase the ticker.
			continue
		}
		next, more := v.Update(msg)
		v = next
		queue = append(queue, more)
	}
	return out
}

// press sends one key press and settles what it started.
func pressGrains(t *testing.T, v tui.View, key string) []tea.Msg {
	t.Helper()
	next, cmd := v.Update(keyMsg(key))
	return settle(t, next, cmd)
}

// findTargets returns the target change among the messages, failing when there
// is none.
func findTargets(t *testing.T, msgs []tea.Msg) tui.TargetsChangedMsg {
	t.Helper()
	for _, msg := range msgs {
		if m, ok := msg.(tui.TargetsChangedMsg); ok {
			return m
		}
	}
	t.Fatalf("no TargetsChangedMsg among %d messages: %v", len(msgs), msgs)
	return tui.TargetsChangedMsg{}
}

// selectKey puts the grains cursor on one key.
func selectKey(t *testing.T, v *SaltGrains, key string) {
	t.Helper()
	for i, row := range v.rows {
		if row.Key == key {
			v.cursor = i
			return
		}
	}
	t.Fatalf("no grain %q in %v", key, v.rows)
}

// newGrainsView builds the view and settles its first load.
func newGrainsView(t *testing.T, f *fakeGrains, live bool) *SaltGrains {
	t.Helper()
	v := NewSaltGrains(grainsEnv(), f, "web-01", live)
	settle(t, v, v.Init())
	return v
}

// ---- grains ----

// TestGrainsViewLoadsCachedGrains: every grain of the minion is a row, sorted
// by key, with a one-line value.
func TestGrainsViewLoadsCachedGrains(t *testing.T) {
	f := newGrainsFake(t)
	v := newGrainsView(t, f, false)
	if f.minion != "web-01" {
		t.Errorf("read minion = %q, want web-01", f.minion)
	}
	if f.cacheRead != 1 {
		t.Errorf("cache reads = %d, want 1", f.cacheRead)
	}
	keys := make([]string, len(v.rows))
	for i, row := range v.rows {
		keys[i] = row.Key
	}
	if got, want := strings.Join(keys, ","), "id,ipv4,nested,num_cpus,role"; got != want {
		t.Errorf("keys = %s, want %s", got, want)
	}
	if v.rows[1].Value != "10.0.0.5,10.0.0.6" {
		t.Errorf("ipv4 = %q, want the list comma-joined", v.rows[1].Value)
	}
	if !strings.Contains(v.rows[2].Value, `"b":[1,2]`) {
		t.Errorf("nested = %q, want compact JSON", v.rows[2].Value)
	}
	if !strings.Contains(v.Crumb(), "web-01") {
		t.Errorf("crumb = %q, want the minion", v.Crumb())
	}
}

// TestGrainsViewRendersExactly: Render returns exactly w × h cells.
func TestGrainsViewRendersExactly(t *testing.T) {
	v := newGrainsView(t, newGrainsFake(t), false)
	out := v.Render(60, 8)
	lines := strings.Split(out, "\n")
	if len(lines) != 8 {
		t.Fatalf("Render gave %d lines, want 8", len(lines))
	}
	for i, ln := range lines {
		if got := len([]rune(ln)); got != 60 {
			t.Errorf("line %d is %d cells, want 60: %q", i, got, ln)
		}
	}
	body := strings.Join(lines, "\n")
	for _, want := range []string{"grains web-01", "GRAIN", "role"} {
		if !strings.Contains(body, want) {
			t.Errorf("screen lacks %q:\n%s", want, body)
		}
	}
}

// TestGrainsViewFilter: the filter narrows the rows by key and by value.
func TestGrainsViewFilter(t *testing.T) {
	v := newGrainsView(t, newGrainsFake(t), false)
	pressGrains(t, v, "/")
	if !v.Capturing() {
		t.Fatal("the filter should capture the keyboard")
	}
	for _, r := range "role" {
		// Typing starts the filter cursor's blink ticker; it is not work the
		// view waits for, so the command is dropped instead of run.
		_, _ = v.Update(keyMsg(string(r)))
	}
	_, _ = v.Update(keyMsg("enter"))
	if v.Capturing() {
		t.Error("enter should end the filter")
	}
	if len(v.rows) != 1 || v.rows[0].Key != "role" {
		t.Fatalf("filter role = %+v, want the role row", v.rows)
	}
	// A value filter works too: 10.0.0.6 is one element of the ipv4 list.
	v.setFilter("10.0.0.6")
	if len(v.rows) != 1 || v.rows[0].Key != "ipv4" {
		t.Errorf("filter 10.0.0.6 = %+v, want the ipv4 row", v.rows)
	}
	v.setFilter("")
	if len(v.rows) != 5 {
		t.Errorf("unfiltered rows = %d, want 5", len(v.rows))
	}
}

// TestGrainsViewDistribution: L reads the fleet cache and shows the count per
// value of the selected grain.
func TestGrainsViewDistribution(t *testing.T) {
	f := newGrainsFake(t)
	v := newGrainsView(t, f, false)
	selectKey(t, v, "role")
	var open tui.OpenDialogMsg
	for _, msg := range pressGrains(t, v, grainsDistKey) {
		if m, ok := msg.(tui.OpenDialogMsg); ok {
			open = m
		}
	}
	dist, ok := open.Dialog.(*grainsDistDialog)
	if !ok {
		t.Fatalf("dialog = %T, want the distribution box", open.Dialog)
	}
	if len(dist.values) != 2 {
		t.Fatalf("values = %+v, want web 2 and db 1", dist.values)
	}
	if dist.values[0].Value != "web" || dist.values[0].Count != 2 {
		t.Errorf("values[0] = %+v, want web 2", dist.values[0])
	}
	view := dist.View(40, 6)
	if !strings.Contains(view, "web") || !strings.Contains(view, "db") {
		t.Errorf("distribution box lacks its values:\n%s", view)
	}
	if len(v.all) != 3 {
		t.Errorf("fleet cache = %d minions, want 3", len(v.all))
	}
}

// TestGrainsViewEnterTargets: enter on a value makes the hosts with that value
// the session targets.
func TestGrainsViewEnterTargets(t *testing.T) {
	f := newGrainsFake(t)
	v := newGrainsView(t, f, false)
	selectKey(t, v, "role")
	changed := findTargets(t, pressGrains(t, v, "enter"))
	hosts := changed.Set.Hosts
	if len(hosts) != 2 {
		t.Fatalf("targets = %v, want the two web minions", hosts)
	}
	if hosts[0] != "web-01.lab.example.internal" || hosts[1] != "web-02" {
		t.Errorf("targets = %v, want the mapped web-01 and web-02", hosts)
	}
	if changed.Set.Label != "role=web" {
		t.Errorf("label = %q, want role=web", changed.Set.Label)
	}
}

// TestGrainsViewDistributionEnter: enter inside the distribution box sets the
// targets of the chosen value and closes the box.
func TestGrainsViewDistributionEnter(t *testing.T) {
	v := newGrainsView(t, newGrainsFake(t), false)
	selectKey(t, v, "role")
	var open tui.OpenDialogMsg
	for _, msg := range pressGrains(t, v, grainsDistKey) {
		if m, ok := msg.(tui.OpenDialogMsg); ok {
			open = m
		}
	}
	dist := open.Dialog.(*grainsDistDialog)
	next, cmd := dist.Update(keyMsg("j")) // db
	var ok bool
	dist, ok = next.(*grainsDistDialog)
	if !ok {
		t.Fatalf("dialog = %T, want the distribution box", next)
	}
	if dist.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", dist.cursor)
	}
	if cmd != nil {
		t.Fatalf("moving the cursor returned %T, want no command", cmd())
	}
	next, cmd = dist.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("enter on a value returned no command")
	}
	var (
		closed  bool
		targets []resource.HostID
	)
	for _, msg := range settle(t, v, cmd) {
		switch m := msg.(type) {
		case tui.CloseDialogMsg:
			closed = true
		case tui.TargetsChangedMsg:
			targets = m.Set.Hosts
		}
	}
	if !closed {
		t.Error("enter on a distribution value did not close the box")
	}
	if len(targets) != 1 || targets[0] != "db-01" {
		t.Errorf("targets = %v, want db-01 alone", targets)
	}
}

// TestGrainsViewLiveDiff: --live reads the minion, diffs it against the cache
// and marks the keys whose value is stale.
func TestGrainsViewLiveDiff(t *testing.T) {
	f := newGrainsFake(t)
	f.live = grainsFixture(t, `{
		"id": "web-01",
		"role": "web",
		"num_cpus": 8,
		"ipv4": ["10.0.0.5", "10.0.0.6"],
		"nested": {"a": {"b": [1, 2]}},
		"zone": "b"
	}`)
	v := newGrainsView(t, f, true)
	if f.liveRead != 1 || f.cacheRead != 1 {
		t.Fatalf("reads = %d live, %d cached; want one each", f.liveRead, f.cacheRead)
	}
	if len(v.diffs) != 2 {
		t.Fatalf("diffs = %+v, want num_cpus and zone", v.diffs)
	}
	stale := map[string]grainsRow{}
	for _, row := range v.rows {
		if row.Stale {
			stale[row.Key] = row
		}
	}
	if len(stale) != 2 {
		t.Fatalf("stale rows = %+v, want two", stale)
	}
	if stale["num_cpus"].Value != "8" || stale["num_cpus"].Was != "4" {
		t.Errorf("num_cpus row = %+v, want 8 with 4 cached", stale["num_cpus"])
	}
	if stale["zone"].Was != "(not cached)" {
		t.Errorf("zone row = %+v, want a missing-cache marker", stale["zone"])
	}
	if !strings.Contains(v.Render(80, 10), "cache stale") {
		t.Errorf("banner lacks the stale note:\n%s", v.Render(80, 10))
	}
	if !strings.Contains(strings.Join(v.CopyLines(), "\n"), "‹was 4›") {
		t.Errorf("copy lines lack the cached value: %v", v.CopyLines())
	}
	if !strings.Contains(v.CommandLine(), "--live") {
		t.Errorf("command line = %q, want the live flag", v.CommandLine())
	}
}

// TestGrainsViewLiveError: a live read that fails keeps the error and shows no
// rows, rather than a stale screen that looks live.
func TestGrainsViewLiveError(t *testing.T) {
	f := newGrainsFake(t)
	f.liveErr = errs.New(errs.Timeout, "salt.grains.live", "no return from web-01")
	v := newGrainsView(t, f, true)
	if v.loadErr == nil || len(v.rows) != 0 {
		t.Fatalf("rows = %d, err = %v; want the error and no rows", len(v.rows), v.loadErr)
	}
	if !strings.Contains(v.Render(60, 5), "no return from web-01") {
		t.Errorf("banner lacks the error:\n%s", v.Render(60, 5))
	}
}

// TestGrainsViewNoBackend: a view without a backend reports it instead of
// panicking.
func TestGrainsViewNoBackend(t *testing.T) {
	v := NewSaltGrains(grainsEnv(), nil, "web-01", false)
	if cmd := v.Init(); cmd != nil {
		t.Fatalf("Init returned %T, want nothing", cmd())
	}
	if v.loadErr == nil {
		t.Fatal("loadErr = nil, want a configuration error")
	}
}

// TestGrainsViewFleetError: a failed fleet read flashes and leaves the rows
// alone.
func TestGrainsViewFleetError(t *testing.T) {
	f := newGrainsFake(t)
	f.fleetErr = errs.New(errs.Timeout, "salt.grains.cache", "cache timeout")
	v := newGrainsView(t, f, false)
	selectKey(t, v, "role")
	var flash tui.FlashMsg
	for _, msg := range pressGrains(t, v, grainsDistKey) {
		if m, ok := msg.(tui.FlashMsg); ok {
			flash = m
		}
	}
	if flash.Level != tui.FlashError {
		t.Fatalf("flash = %+v, want an error", flash)
	}
	if len(v.rows) != 5 {
		t.Errorf("rows = %d, want the cache rows untouched", len(v.rows))
	}
}

// ---- mine ----

// fakeMine is a mine cache without a master.
type fakeMine struct {
	entries  []salt.MineEntry
	value    string
	valueErr error
	err      error

	minion   string
	function string
}

func (f *fakeMine) Mine(_ context.Context) ([]salt.MineEntry, error) {
	return f.entries, f.err
}

func (f *fakeMine) MineValue(_ context.Context, minion, function string) (string, error) {
	f.minion, f.function = minion, function
	return f.value, f.valueErr
}

// newMineFake is a two-minion mine cache.
func newMineFake() *fakeMine {
	return &fakeMine{
		entries: []salt.MineEntry{
			{Minion: "db-01", Function: "network.ip_addrs", Value: "10.0.0.9", Size: 12},
			{Minion: "web-01", Function: "network.ip_addrs", Value: "10.0.0.5,10.0.0.6", Size: 22},
		},
		value: "[\n  \"10.0.0.5\",\n  \"10.0.0.6\"\n]",
	}
}

// newMineView builds the mine browser and settles its first load.
func newMineView(t *testing.T, f *fakeMine) *SaltMine {
	t.Helper()
	v := NewSaltMine(grainsEnv(), f)
	settle(t, v, v.Init())
	return v
}

// TestMineViewRows: every mine value is a row with its minion, function, value
// and size, and the screen renders exactly w × h.
func TestMineViewRows(t *testing.T) {
	v := newMineView(t, newMineFake())
	if len(v.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(v.rows))
	}
	if !strings.Contains(mineRowText(v.rows[0]), "db-01") ||
		!strings.Contains(mineRowText(v.rows[0]), "network.ip_addrs") {
		t.Errorf("row text = %q, want the minion and the function", mineRowText(v.rows[0]))
	}
	out := v.Render(70, 6)
	lines := strings.Split(out, "\n")
	if len(lines) != 6 {
		t.Fatalf("Render gave %d lines, want 6", len(lines))
	}
	for i, ln := range lines {
		if got := len([]rune(ln)); got != 70 {
			t.Errorf("line %d is %d cells, want 70: %q", i, got, ln)
		}
	}
	if !strings.Contains(out, "MINION") || !strings.Contains(out, "network.ip_addrs") {
		t.Errorf("screen lacks the header or the rows:\n%s", out)
	}
	if v.Crumb() != "mine (2)" {
		t.Errorf("crumb = %q, want mine (2)", v.Crumb())
	}
}

// TestMineViewFullValue: enter reads one value in full and shows it in a
// dialog, never in the view itself.
func TestMineViewFullValue(t *testing.T) {
	f := newMineFake()
	v := newMineView(t, f)
	var open tui.OpenDialogMsg
	for _, msg := range pressGrains(t, v, "enter") {
		if m, ok := msg.(tui.OpenDialogMsg); ok {
			open = m
		}
	}
	if open.Dialog == nil {
		t.Fatal("enter opened no dialog")
	}
	if f.minion != "db-01" || f.function != "network.ip_addrs" {
		t.Errorf("read %s/%s, want db-01/network.ip_addrs", f.minion, f.function)
	}
	box, ok := open.Dialog.(*pillarValueDialog)
	if !ok {
		t.Fatalf("dialog = %T, want the text box", open.Dialog)
	}
	if !strings.Contains(box.View(40, 10), "10.0.0.5") {
		t.Errorf("dialog lacks the value:\n%s", box.View(40, 10))
	}
	if strings.Contains(strings.Join(v.CopyLines(), "\n"), "[\n") {
		t.Errorf("copy lines carry the pretty value: %v", v.CopyLines())
	}
}

// TestMineViewFilter: the filter matches minion, function and value.
func TestMineViewFilter(t *testing.T) {
	v := newMineView(t, newMineFake())
	v.setFilter("web-01")
	if len(v.rows) != 1 || v.rows[0].Minion != "web-01" {
		t.Fatalf("filter web-01 = %+v, want one row", v.rows)
	}
	v.setFilter("10.0.0.9")
	if len(v.rows) != 1 || v.rows[0].Minion != "db-01" {
		t.Errorf("filter by value = %+v, want the db-01 row", v.rows)
	}
	v.setFilter("")
	if len(v.rows) != 2 {
		t.Errorf("unfiltered rows = %d, want 2", len(v.rows))
	}
}

// TestMineViewError: a failed read keeps the error and the screen says so.
func TestMineViewError(t *testing.T) {
	f := newMineFake()
	f.err = errs.New(errs.Unreachable, "salt.mine", "master unreachable")
	v := newMineView(t, f)
	if v.loadErr == nil || len(v.rows) != 0 {
		t.Fatalf("rows = %d, err = %v; want the error", len(v.rows), v.loadErr)
	}
	if !strings.Contains(v.Render(70, 5), "unreachable") {
		t.Errorf("banner lacks the error:\n%s", v.Render(70, 5))
	}
}
