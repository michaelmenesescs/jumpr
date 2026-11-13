package salt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/transport/transporttest"
)

// pillarJobReply renders a jobs.list_job answer whose function is a pillar
// function, which jobReply (built for cmd.run_all) cannot express.
func pillarJobReply(t *testing.T, jid, fun, minionID, ret string) transporttest.Reply {
	t.Helper()
	w := wireJob{
		JID:        jid,
		Function:   fun,
		Target:     []string{"web-01"},
		TargetType: "list",
		User:       "sudo_sre",
		StartTime:  "2026, Nov 12 13:55:01.123456",
		Minions:    []string{minionID},
		Result: map[string]wireResult{
			minionID: {Return: json.RawMessage(ret), Retcode: 0, Success: true},
		},
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	return transporttest.Reply{Stdout: string(b) + "\n"}
}

// pillarTree is the scripted pillar of web-01: one secret, one list, one of
// each scalar type.
const pillarTree = `{
  "nginx": {"tls": {"key": "s3cr3t-value", "cert_file": "/etc/nginx/tls/server.crt"}},
  "tags": ["web", "edge", "prod"],
  "enabled": true,
  "workers": 8,
  "ratio": 1.5,
  "nothing": null
}`

// findPillar returns the node at path in a tree.
func findPillar(nodes []PillarNode, path string) (PillarNode, bool) {
	for _, n := range nodes {
		if n.Key == path {
			return n, true
		}
		if found, ok := findPillar(n.Children, path); ok {
			return found, true
		}
	}
	return PillarNode{}, false
}

// TestPillarKeysDropsValues is requirement 1: the tree carries types and
// lengths, and not one byte of a value survives.
func TestPillarKeysDropsValues(t *testing.T) {
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return pillarJobReply(t, jid, "pillar.items", "web-01", pillarTree)
	})
	r := NewRunner(m.fake, master, RunnerOptions{})
	b := New(Options{Runner: r, Master: master})

	ctx, cancel := context.WithTimeout(context.Background(), scaledWait(5*time.Second))
	defer cancel()
	tree, err := b.PillarKeys(ctx, "web-01")
	if err != nil {
		t.Fatalf("PillarKeys: %v", err)
	}
	if tree.Minion != "web-01" {
		t.Errorf("Minion = %q, want web-01", tree.Minion)
	}

	want := []struct {
		path string
		typ  string
		n    int
	}{
		{path: "nginx", typ: "dict"},
		{path: "nginx:tls", typ: "dict"},
		{path: "nginx:tls:key", typ: "str", n: len("s3cr3t-value")},
		{path: "nginx:tls:cert_file", typ: "str", n: len("/etc/nginx/tls/server.crt")},
		{path: "tags", typ: "list", n: 3},
		{path: "enabled", typ: "bool"},
		{path: "workers", typ: "int"},
		{path: "ratio", typ: "float"},
		{path: "nothing", typ: "null"},
	}
	for _, w := range want {
		node, ok := findPillar(tree.Nodes, w.path)
		if !ok {
			t.Fatalf("no node %q in %+v", w.path, tree.Nodes)
		}
		if node.Type != w.typ {
			t.Errorf("%s type = %q, want %q", w.path, node.Type, w.typ)
		}
		if node.Len != w.n {
			t.Errorf("%s len = %d, want %d", w.path, node.Len, w.n)
		}
	}

	for _, secret := range []string{"s3cr3t-value", "server.crt", "edge", "prod"} {
		if got := fmt.Sprintf("%#v", tree); strings.Contains(got, secret) {
			t.Errorf("the tree still carries %q: %s", secret, got)
		}
	}

	// The submission is the pillar function, run as a job.
	var submitted string
	for _, c := range m.fake.Calls() {
		if strings.Contains(c.Command.Script, "--async") {
			submitted = c.Command.Script
		}
	}
	if !strings.Contains(submitted, "pillar.items") {
		t.Errorf("submission %q does not call pillar.items", submitted)
	}
}

// TestPillarKeysRejectsBadMinion: a minion id is never passed to a command
// line unchecked.
func TestPillarKeysRejectsBadMinion(t *testing.T) {
	b := New(Options{Master: master})
	for _, bad := range []string{"", " ", "web-01; rm -rf /", "-L", "web 01", "/etc/passwd"} {
		if _, err := b.PillarKeys(context.Background(), bad); errs.KindOf(err) != errs.Config {
			t.Errorf("PillarKeys(%q) error = %v, want config", bad, err)
		}
	}
}

// TestPillarKeysNoReturn: a minion that never answered is a timeout, and the
// tree is empty.
func TestPillarKeysNoReturn(t *testing.T) {
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return pillarJobReply(t, jid, "pillar.items", "web-01", "false")
	})
	r := NewRunner(m.fake, master, RunnerOptions{})
	b := New(Options{Runner: r, Master: master})

	ctx, cancel := context.WithTimeout(context.Background(), scaledWait(5*time.Second))
	defer cancel()
	tree, err := b.PillarKeys(ctx, "web-01")
	if errs.KindOf(err) != errs.Timeout {
		t.Fatalf("PillarKeys error = %v, want timeout", err)
	}
	if len(tree.Nodes) != 0 {
		t.Errorf("tree = %+v, want no nodes", tree.Nodes)
	}
}

// TestPillarValueRevealsOneKey: pillar.get is asked for exactly one path and
// its answer comes back verbatim.
func TestPillarValueRevealsOneKey(t *testing.T) {
	fake := transporttest.New().On("pillar.get", transporttest.Reply{Stdout: `{"web-01": "s3cr3t-value"}` + "\n"})
	r := NewRunner(fake, master, RunnerOptions{})
	b := New(Options{Runner: r, Master: master})

	got, err := b.PillarValue(context.Background(), "web-01", "nginx:tls:key")
	if err != nil {
		t.Fatalf("PillarValue: %v", err)
	}
	if got != "s3cr3t-value" {
		t.Errorf("value = %q, want the one key asked for", got)
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("made %d calls, want 1", len(calls))
	}
	line := calls[0].Command.Script
	for _, want := range []string{"pillar.get", "nginx:tls:key", "-L web-01"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q is missing %q", line, want)
		}
	}
}

// TestPillarValueRendersStructuredValues: a dict value is shown as JSON, not
// as a Go map.
func TestPillarValueRendersStructuredValues(t *testing.T) {
	fake := transporttest.New().On("pillar.get", transporttest.Reply{
		Stdout: `{"web-01": {"user": "app", "port": 5432}}` + "\n",
	})
	b := New(Options{Runner: NewRunner(fake, master, RunnerOptions{}), Master: master})

	got, err := b.PillarValue(context.Background(), "web-01", "postgres")
	if err != nil {
		t.Fatalf("PillarValue: %v", err)
	}
	for _, want := range []string{"\"user\": \"app\"", "\"port\": 5432"} {
		if !strings.Contains(got, want) {
			t.Errorf("value %q is missing %q", got, want)
		}
	}
}

// TestPillarValueRejectsBadPath: the key path is validated before it can reach
// a command line.
func TestPillarValueRejectsBadPath(t *testing.T) {
	fake := transporttest.New()
	b := New(Options{Runner: NewRunner(fake, master, RunnerOptions{}), Master: master})
	for _, bad := range []string{"", ":", "a b", "nginx;id", "nginx:tls:key=$(id)", "..", "-x"} {
		if _, err := b.PillarValue(context.Background(), "web-01", bad); errs.KindOf(err) != errs.Config {
			t.Errorf("PillarValue(%q) error = %v, want config", bad, err)
		}
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("%d calls reached the master, want 0", len(calls))
	}
}

// TestPillarDiffReportsTypeChanges: the comparison names the paths and the
// types, and never a value.
func TestPillarDiffReportsTypeChanges(t *testing.T) {
	left := PillarTree{Minion: "web-01", Nodes: []PillarNode{
		{Key: "nginx", Type: "dict", Children: []PillarNode{
			{Key: "nginx:tls", Type: "dict", Children: []PillarNode{
				{Key: "nginx:tls:key", Type: "str", Len: 12},
			}},
			{Key: "nginx:port", Type: "int"},
		}},
		{Key: "only_left", Type: "bool"},
	}}
	right := PillarTree{Minion: "web-02", Nodes: []PillarNode{
		{Key: "nginx", Type: "dict", Children: []PillarNode{
			{Key: "nginx:tls", Type: "list", Len: 1},
			{Key: "nginx:port", Type: "int"},
		}},
		{Key: "only_right", Type: "str", Len: 3},
	}}

	got := DiffPillars(left, right)
	want := []PillarDiff{
		{Path: "nginx:tls", Left: "dict", Right: "list"},
		{Path: "nginx:tls:key", Left: "str"},
		{Path: "only_left", Left: "bool"},
		{Path: "only_right", Right: "str"},
	}
	if len(got) != len(want) {
		t.Fatalf("DiffPillars = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if same := DiffPillars(left, left); len(same) != 0 {
		t.Errorf("a tree differs from itself: %+v", same)
	}
}

// TestJobDetailHidesPillar is requirement 5: a pillar.* job's arguments and
// returns are replaced by the marker the job detail shows.
func TestJobDetailHidesPillar(t *testing.T) {
	raw := json.RawMessage(`{"key": "s3cr3t-value"}`)
	info := JobInfo{
		JID:       jidA,
		Function:  "pillar.items",
		Arguments: []json.RawMessage{json.RawMessage(`"s3cr3t-value"`)},
		Result: map[string]MinionReturn{
			"web-01": {Return: raw, Retcode: 0, Success: true},
		},
	}
	hidden := HideJobInfo(info)
	if len(hidden.Arguments) != 0 {
		t.Errorf("Arguments = %v, want none", hidden.Arguments)
	}
	mr, ok := hidden.Result["web-01"]
	if !ok {
		t.Fatalf("Result lost the minion: %+v", hidden.Result)
	}
	if string(mr.Return) != PillarHidden {
		t.Errorf("return = %q, want %q", mr.Return, PillarHidden)
	}
	if !mr.Success {
		t.Error("Success was lost")
	}
	if got := fmt.Sprintf("%#v", hidden); strings.Contains(got, "s3cr3t-value") {
		t.Errorf("the hidden job still carries the value: %s", got)
	}

	// A job of any other function is untouched.
	other := JobInfo{JID: jidA, Function: "cmd.run", Result: map[string]MinionReturn{
		"web-01": {Return: json.RawMessage(`"up"`)},
	}}
	if got := HideJobInfo(other); string(got.Result["web-01"].Return) != `"up"` || len(got.Result) != 1 {
		t.Errorf("cmd.run job was rewritten: %+v", got)
	}
	if HiddenFunction("pillar.get") != true || HiddenFunction("pillar.items") != true {
		t.Error("a pillar function is not hidden")
	}
	for _, fun := range []string{"", "cmd.run", "state.apply", "pillaritems"} {
		if HiddenFunction(fun) {
			t.Errorf("%q is hidden, want visible", fun)
		}
	}
}

// logCapture is a slog handler that keeps everything it is given.
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (h *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *logCapture) Handle(_ context.Context, r slog.Record) error {
	var line strings.Builder
	line.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		line.WriteString(" " + a.Key + "=" + fmt.Sprint(a.Value.Any()))
		return true
	})
	h.mu.Lock()
	h.buf.WriteString(line.String())
	h.buf.WriteString("\n")
	h.mu.Unlock()
	return nil
}

func (h *logCapture) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *logCapture) WithGroup(string) slog.Handler { return h }

func (h *logCapture) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.String()
}

// TestPillarSensitiveCallNotLogged (the task calls it TestSensitiveCallNotLogged;
// the Pillar keeps it inside the acceptance command's -run filter) is requirement 5: a sensitive call's function,
// arguments and output never reach the log, while an ordinary call's metadata
// does.
func TestPillarSensitiveCallNotLogged(t *testing.T) {
	const secret = "s3cr3t-value"
	fake := transporttest.New().
		On("pillar.get", transporttest.Reply{Stdout: `{"web-01": "` + secret + `"}` + "\n"}).
		On("test.ping", transporttest.Reply{Stdout: `{"web-01": true}` + "\n"})
	log := &logCapture{}
	r := NewRunner(fake, master, RunnerOptions{Log: slog.New(log)})
	b := New(Options{Runner: r, Master: master})

	if _, err := b.PillarValue(context.Background(), "web-01", "nginx:tls:key"); err != nil {
		t.Fatalf("PillarValue: %v", err)
	}
	got := log.String()
	if !strings.Contains(got, "sensitive=true") {
		t.Errorf("the log does not mark the call sensitive:\n%s", got)
	}
	for _, leak := range []string{secret, "pillar.get", "nginx:tls:key", "stdout_bytes"} {
		if strings.Contains(got, leak) {
			t.Errorf("the log carries %q:\n%s", leak, got)
		}
	}

	// The same runner, an ordinary call: metadata is logged.
	log2 := &logCapture{}
	r2 := NewRunner(fake, master, RunnerOptions{Log: slog.New(log2)})
	if _, err := r2.Run(context.Background(), Call{
		Tool:   toolSalt,
		Target: &Target{Type: "list", Expr: "web-01"},
		Fun:    "test.ping",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ordinary := log2.String()
	for _, want := range []string{"fun=test.ping", "stdout_bytes="} {
		if !strings.Contains(ordinary, want) {
			t.Errorf("the log is missing %q:\n%s", want, ordinary)
		}
	}
}

// TestSensitiveFlagOnAnyCall: Sensitive works for a function that is not a
// pillar function.
func TestSensitiveFlagOnAnyCall(t *testing.T) {
	log := &logCapture{}
	fake := transporttest.New().On("cmd.run", transporttest.Reply{Stdout: `{"web-01": "s3cr3t-value"}` + "\n"})
	r := NewRunner(fake, master, RunnerOptions{Log: slog.New(log)})
	if _, err := r.Run(context.Background(), Call{
		Tool:      toolSalt,
		Target:    &Target{Type: "list", Expr: "web-01"},
		Fun:       "cmd.run",
		Args:      []string{"cat /etc/shadow"},
		Sensitive: true,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := log.String(); strings.Contains(got, "cmd.run") || strings.Contains(got, "shadow") ||
		strings.Contains(got, "s3cr3t-value") {
		t.Errorf("a sensitive cmd.run reached the log:\n%s", got)
	}
}

// TestPillarCallIsSensitiveByFunction: no caller has to remember the flag for
// a pillar function.
func TestPillarCallIsSensitiveByFunction(t *testing.T) {
	for _, c := range []Call{
		{Tool: toolSalt, Fun: "pillar.items"},
		{Tool: toolSalt, Fun: "pillar.get", Args: []string{"a:b"}},
		{Tool: toolSalt, Fun: "cmd.run", Sensitive: true},
	} {
		if !c.hidden() {
			t.Errorf("%s %v is not hidden", c.Fun, c.Args)
		}
	}
	if (Call{Tool: toolSalt, Fun: "test.ping"}).hidden() {
		t.Error("test.ping is hidden")
	}
}
