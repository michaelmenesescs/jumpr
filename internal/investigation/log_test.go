package investigation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/bus"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// fixedClock returns a now function stuck on one instant.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// openLog opens a log over a temp root with a fixed clock and an investigation
// already open.
func openLog(t *testing.T) (*FileLog, Meta, string) {
	t.Helper()
	root := t.TempDir()
	l := NewLog(root, newRed(t), fixedClock(at(14, 2, 11)))
	meta, err := l.Open("API 5xx in EWR", resource.TimeWindow{}, []resource.HostID{"web-01", "web-02"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l, meta, root
}

func TestLogOpenResumeClose(t *testing.T) {
	l, meta, root := openLog(t)

	if want := "2026-11-12-api-5xx-in-ewr"; meta.ID != want {
		t.Fatalf("ID = %q, want %q", meta.ID, want)
	}
	if meta.Status != "open" {
		t.Fatalf("Status = %q, want open", meta.Status)
	}
	if want := filepath.Join(root, "investigations", meta.ID+".md"); meta.Path != want {
		t.Fatalf("Path = %q, want %q", meta.Path, want)
	}

	file := readFile(t, meta.Path)
	if !strings.Contains(file, "**open**") {
		t.Fatalf("open entry missing:\n%s", file)
	}
	if !strings.Contains(file, "`API 5xx in EWR` on 2 hosts") {
		t.Fatalf("open text missing:\n%s", file)
	}
	if !strings.Contains(file, "## Timeline") {
		t.Fatalf("timeline heading missing:\n%s", file)
	}

	cur, ok := l.Current()
	if !ok || cur.ID != meta.ID {
		t.Fatalf("Current() = %+v, %v; want %s", cur, ok, meta.ID)
	}

	if err := l.Append(Entry{Kind: EntryNote, Text: "haproxy holds :443"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if file := readFile(t, meta.Path); !strings.Contains(file, "**note** haproxy holds :443") {
		t.Fatalf("note not appended:\n%s", file)
	}

	l.Close()
	if _, ok := l.Current(); ok {
		t.Fatalf("Current() is still open after Close")
	}
	// With nothing open, a note is dropped: the audit file only takes actions,
	// denials and input.
	if err := l.Append(Entry{Kind: EntryNote, Text: "after close"}); err != nil {
		t.Fatalf("Append after close: %v", err)
	}
	if _, err := os.Stat(l.AuditPath(at(14, 2, 11))); err == nil {
		t.Fatalf("a note wrote an audit file")
	}

	resumed, err := l.Resume(meta.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.ID != meta.ID || resumed.Title != meta.Title || resumed.Path != meta.Path {
		t.Fatalf("Resume = %+v, want %+v", resumed, meta)
	}
	if _, ok := l.Current(); !ok {
		t.Fatalf("Current() is closed after Resume")
	}
	if _, err := l.Resume("nope"); err == nil {
		t.Fatalf("Resume of an unknown id returned no error")
	}
}

// TestAppendWithoutInvestigationGoesToAuditOnlyForActions is the rule of
// BUILD-PLAN §4.10: no investigation means actions, denials and input go to the
// day's audit file and everything else is dropped silently.
func TestAppendWithoutInvestigationGoesToAuditOnlyForActions(t *testing.T) {
	root := t.TempDir()
	now := at(14, 5, 12)
	l := NewLog(root, newRed(t), fixedClock(now))

	kept := []EntryKind{EntryAction, EntryDenied, EntryInput}
	dropped := []EntryKind{EntryOpen, EntryView, EntryProbe, EntryJournal, EntryNote, EntryFinding, EntryResolve}

	for _, k := range dropped {
		if err := l.Append(Entry{Kind: k, Text: "dropped " + string(k)}); err != nil {
			t.Fatalf("Append(%s): %v", k, err)
		}
	}
	if _, err := os.Stat(l.AuditPath(now)); err == nil {
		t.Fatalf("a dropped kind created the audit file")
	}

	for _, k := range kept {
		if err := l.Append(Entry{Kind: k, Text: "kept " + string(k)}); err != nil {
			t.Fatalf("Append(%s): %v", k, err)
		}
	}

	path := filepath.Join(root, "audit", "2026-11-12.md")
	file := readFile(t, path)
	if !strings.Contains(file, "# Audit 2026-11-12") {
		t.Fatalf("audit header missing:\n%s", file)
	}
	for _, k := range kept {
		if !strings.Contains(file, "**"+string(k)+"** kept "+string(k)) {
			t.Fatalf("%s entry missing from the audit file:\n%s", k, file)
		}
	}
	for _, k := range dropped {
		if strings.Contains(file, "dropped "+string(k)) {
			t.Fatalf("%s entry reached the audit file:\n%s", k, file)
		}
	}
}

func TestLogRecordDenied(t *testing.T) {
	l, meta, _ := openLog(t)
	p := bus.Proposal{
		Plan: action.Plan{
			Spec:  action.Spec{ID: "linux.systemctl.stop", Title: "stop"},
			Title: "stop sshd on 1 host",
		},
		Denied: &bus.Denial{Rule: "never stop sshd"},
	}
	if err := l.RecordDenied(p); err != nil {
		t.Fatalf("RecordDenied: %v", err)
	}
	file := readFile(t, meta.Path)
	if !strings.Contains(file, "**denied** `stop sshd on 1 host` denied: never stop sshd") {
		t.Fatalf("denied entry missing:\n%s", file)
	}
}

func TestRecordActionGroupsOutputs(t *testing.T) {
	l, meta, _ := openLog(t)

	steps := make([]action.Step, 0, 500)
	results := make([]action.StepResult, 0, 500)
	for i := 0; i < 500; i++ {
		host := resource.HostID(fmt.Sprintf("web-ewr-%03d", i%500+1))
		steps = append(steps, action.Step{Target: action.Target{Host: host}, Display: "systemctl restart nginx.service"})
		if i < 498 {
			results = append(results, action.StepResult{
				Target: action.Target{Host: host}, RC: 0, Output: "restarting nginx\nactive (running)",
			})
			continue
		}
		results = append(results, action.StepResult{
			Target: action.Target{Host: host}, RC: 1, Output: "Job for nginx.service failed\nsee journalctl",
		})
	}
	p := bus.Proposal{
		Plan: action.Plan{
			Spec:  action.Spec{ID: "linux.systemctl.restart", Title: "restart"},
			Title: "restart nginx.service on 500 hosts",
			Steps: steps,
		},
	}
	summary := bus.Summary{OK: 498, Failed: 2}
	if err := l.RecordAction(p, summary, results); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}

	file := readFile(t, meta.Path)
	if !strings.Contains(file, "**action** `restart nginx.service on 500 hosts` → ok 498/500, 2 failed") {
		t.Fatalf("action line missing:\n%s", file)
	}
	// 500 results collapse into two blocks; the same command is named once.
	if n := strings.Count(file, "rc=0 on 498 hosts"); n != 1 {
		t.Fatalf("rc=0 block appears %d times, want 1", n)
	}
	if n := strings.Count(file, "rc=1 on 2 hosts"); n != 1 {
		t.Fatalf("rc=1 block appears %d times, want 1", n)
	}
	if n := strings.Count(file, "systemctl restart nginx.service"); n != 1 {
		t.Fatalf("plan display appears %d times, want 1", n)
	}
	if !strings.Contains(file, "Job for nginx.service failed") {
		t.Fatalf("failing output missing:\n%s", file)
	}
	if n := strings.Count(file, "rc="); n != 2 {
		t.Fatalf("attachment has %d rc blocks, want 2", n)
	}
}

func TestRecordActionWithoutInvestigationGoesToAudit(t *testing.T) {
	root := t.TempDir()
	now := at(14, 5, 12)
	l := NewLog(root, newRed(t), fixedClock(now))
	p := bus.Proposal{Plan: action.Plan{Title: "restart nginx.service on 2 hosts"}}
	if err := l.RecordAction(p, bus.Summary{OK: 2}, nil); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	file := readFile(t, filepath.Join(root, "audit", "2026-11-12.md"))
	if !strings.Contains(file, "**action** `restart nginx.service on 2 hosts` → ok 2/2") {
		t.Fatalf("audit action entry missing:\n%s", file)
	}
}

func TestLogOpenKeepsAllTargets(t *testing.T) {
	root := t.TempDir()
	l := NewLog(root, newRed(t), fixedClock(at(14, 2, 11)))

	hosts := make([]resource.HostID, 0, 40)
	for i := 0; i < 40; i++ {
		hosts = append(hosts, resource.HostID(fmt.Sprintf("web-ewr-%03d", i+1)))
	}
	meta, err := l.Open("wide", resource.TimeWindow{}, hosts, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The frontmatter folds past 30 hosts; the record the session works from
	// must still know every target, or `@investigation` would under-target.
	if len(meta.Targets) != len(hosts) {
		t.Fatalf("Meta.Targets has %d hosts, want %d", len(meta.Targets), len(hosts))
	}
	cur, ok := l.Current()
	if !ok || len(cur.Targets) != len(hosts) {
		t.Fatalf("Current().Targets has %d hosts, want %d", len(cur.Targets), len(hosts))
	}
	file := readFile(t, meta.Path)
	if !strings.Contains(file, "# and 10 more targets") {
		t.Fatalf("frontmatter did not fold the target list:\n%s", file)
	}
	if !strings.Contains(file, "**open** `wide` on 40 hosts") {
		t.Fatalf("open bullet does not count every host:\n%s", file)
	}
}

func TestResolve(t *testing.T) {
	l, meta, _ := openLog(t)

	if err := l.Resolve("stale haproxy bound :443; restarted haproxy"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	file := readFile(t, meta.Path)
	if !strings.Contains(file, "**resolve** stale haproxy bound :443; restarted haproxy") {
		t.Fatalf("resolve entry missing:\n%s", file)
	}
	if !strings.Contains(file, "status: resolved") {
		t.Fatalf("status not updated:\n%s", file)
	}
	if !strings.Contains(file, "## Resolution\nstale haproxy bound :443; restarted haproxy") {
		t.Fatalf("resolution section not written:\n%s", file)
	}
	cur, ok := l.Current()
	if !ok || cur.Status != "resolved" {
		t.Fatalf("Current() = %+v, %v; want resolved", cur, ok)
	}

	// Resolving with nothing open is a configuration mistake, not a write.
	l.Close()
	if err := l.Resolve("again"); err == nil {
		t.Fatalf("Resolve with no investigation returned no error")
	} else if k := errs.KindOf(err); k != errs.Config {
		t.Fatalf("kind = %s, want %s", k, errs.Config)
	}
}

func TestLogConcurrentAppends(t *testing.T) {
	l, meta, _ := openLog(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := l.Append(Entry{Kind: EntryNote, Text: fmt.Sprintf("note %02d", i)}); err != nil {
				t.Errorf("Append: %v", err)
			}
		}(i)
	}
	wg.Wait()

	file := readFile(t, meta.Path)
	if n := strings.Count(file, "**note**"); n != 20 {
		t.Fatalf("file has %d notes, want 20:\n%s", n, file)
	}
	if _, err := ReadMeta(meta.Path); err != nil {
		t.Fatalf("file is not valid after concurrent appends: %v", err)
	}
}
