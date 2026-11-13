package investigation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// kbRoot is the checked-in knowledge base the parsing tests read.
func kbRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "kb")
}

// buildKB indexes the checked-in knowledge base.
func buildKB(t *testing.T) *Index {
	t.Helper()
	ix, err := BuildIndex(kbRoot(t))
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	return ix
}

// hitPaths renders the paths of hits relative to root, so failures are readable.
func hitPaths(root string, hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = relTo(root, h.Path)
	}
	return out
}

func TestBuildIndexWalksBothTrees(t *testing.T) {
	ix := buildKB(t)
	// hosts, services, alerts, two notes, one nested note, one investigation.
	if got, want := ix.Len(), 7; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
	if hits := ix.Match(Subject{Hosts: []string{"web-ewr-007"}}); len(hits) == 0 {
		t.Fatal("no hit for the host note")
	}
}

func TestMatchAlert(t *testing.T) {
	root := kbRoot(t)
	ix := buildKB(t)

	hits := ix.Match(Subject{Alert: "api-5xx"})
	if len(hits) == 0 {
		t.Fatal("no hit for the alert")
	}
	top := hits[0]
	if got, want := relTo(root, top.Path), "knowledge/alerts/api-5xx.md"; got != want {
		t.Fatalf("top hit = %q, want %q", got, want)
	}
	if top.Why != "alert name" {
		t.Fatalf("Why = %q, want \"alert name\"", top.Why)
	}
	if top.Score < scoreAlert {
		t.Fatalf("Score = %d, want at least %d", top.Score, scoreAlert)
	}
	if top.Investigation {
		t.Fatal("the alert note is not an investigation")
	}
	if top.Title != "api-5xx alert" {
		t.Fatalf("Title = %q, want the first H1", top.Title)
	}
}

func TestMatchUnitToService(t *testing.T) {
	root := kbRoot(t)
	ix := buildKB(t)

	hits := ix.Match(Subject{Unit: "nginx.service"})
	if len(hits) == 0 {
		t.Fatal("no hit for the unit")
	}
	top := hits[0]
	if got, want := relTo(root, top.Path), "knowledge/services/nginx.md"; got != want {
		t.Fatalf("top hit = %q, want %q", got, want)
	}
	if top.Why != "unit nginx.service" {
		t.Fatalf("Why = %q, want \"unit nginx.service\"", top.Why)
	}
	if top.Score < scoreUnit {
		t.Fatalf("Score = %d, want at least %d", top.Score, scoreUnit)
	}
}

func TestMatchHostShortName(t *testing.T) {
	root := kbRoot(t)
	ix := buildKB(t)

	// The note has no frontmatter hosts: the path convention supplies
	// "web-ewr-007", and both the short name and the FQDN find it.
	for _, host := range []string{"web-ewr-007", "web-ewr-007.example.internal", "WEB-EWR-007"} {
		hits := ix.Match(Subject{Hosts: []string{host}})
		if len(hits) == 0 {
			t.Fatalf("no hit for host %q", host)
		}
		if got, want := relTo(root, hits[0].Path), "knowledge/hosts/web-ewr-007.md"; got != want {
			t.Fatalf("host %q: top hit = %q, want %q", host, got, want)
		}
		if hits[0].Why != "host" {
			t.Fatalf("host %q: Why = %q, want \"host\"", host, hits[0].Why)
		}
	}

	// A frontmatter host matches by its short name too.
	hits := ix.Match(Subject{Hosts: []string{"edge-ewr-001"}})
	if len(hits) == 0 {
		t.Fatal("no hit for the frontmatter host by short name")
	}
	if got, want := relTo(root, hits[0].Path), "knowledge/notes/edge-cache.md"; got != want {
		t.Fatalf("top hit = %q, want %q", got, want)
	}
}

func TestIndexPathConventions(t *testing.T) {
	root := kbRoot(t)
	ix := buildKB(t)

	// knowledge/hosts/<h>.md → host, without any frontmatter.
	if hits := ix.Match(Subject{Hosts: []string{"web-ewr-007"}}); len(hits) == 0 {
		t.Fatal("knowledge/hosts convention did not yield a host key")
	}
	// knowledge/services/<s>.md → service.
	if hits := ix.Match(Subject{Unit: "nginx"}); len(hits) == 0 {
		t.Fatal("knowledge/services convention did not yield a service key")
	}
	// knowledge/alerts/<a>.md → alert.
	if hits := ix.Match(Subject{Alert: "api-5xx"}); len(hits) == 0 {
		t.Fatal("knowledge/alerts convention did not yield an alert key")
	}
	// knowledge/**/*.md → nested notes are walked.
	if hits := ix.Match(Subject{Tags: []string{"proxy"}}); len(hits) == 0 {
		t.Fatal("nested knowledge notes were not walked")
	}
	// investigations/*.md → the investigation, with its status.
	hits := ix.Match(Subject{Tags: []string{"varnish"}})
	found := false
	for _, h := range hits {
		if strings.HasSuffix(h.Path, filepath.Join("investigations", "2026-11-10-edge-cache-5xx.md")) {
			found = true
			if !h.Investigation {
				t.Fatal("the investigation hit is not flagged as one")
			}
			if h.Status != "resolved" {
				t.Fatalf("Status = %q, want resolved", h.Status)
			}
		}
	}
	if !found {
		t.Fatalf("investigation not indexed; hits = %v", hitPaths(root, hits))
	}

	// A broken frontmatter block costs the keys, never the body.
	hits = ix.Match(Subject{Tags: []string{"haproxy"}})
	if len(hits) == 0 {
		t.Fatal("a note with broken frontmatter was dropped from the index")
	}
}

func TestMatchSnapshot(t *testing.T) {
	root := kbRoot(t)
	ix := buildKB(t)

	hits := ix.Match(Subject{
		Hosts: []string{"web-ewr-007"},
		Unit:  "nginx.service",
		Alert: "api-5xx",
		Tags:  []string{"cache"},
	})
	if len(hits) == 0 {
		t.Fatal("no hits for a rich subject")
	}
	// The alert note wins: 100 beats the service's 80 and the host's 60.
	if got, want := relTo(root, hits[0].Path), "knowledge/alerts/api-5xx.md"; got != want {
		t.Fatalf("top hit = %q, want %q", got, want)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Fatalf("hits are not sorted by score: %v", hitPaths(root, hits))
		}
	}
	// The snippet is the first paragraph, flattened and bounded.
	for _, h := range hits {
		if len([]rune(h.Snippet)) > snippetMax {
			t.Fatalf("snippet is %d runes, want at most %d", len([]rune(h.Snippet)), snippetMax)
		}
		if strings.Contains(h.Snippet, "\n") {
			t.Fatalf("snippet has a newline: %q", h.Snippet)
		}
	}
	for _, h := range hits {
		if strings.Contains(relTo(root, h.Path), "edge-cache.md") {
			if !strings.HasPrefix(h.Snippet, "A cold varnish instance") {
				t.Fatalf("snippet = %q, want the first paragraph", h.Snippet)
			}
		}
	}
}

// writeNote writes one knowledge note and stamps its modification time.
func writeNote(t *testing.T, root, rel, body string, mod time.Time) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	return path
}

func TestMatchRanking(t *testing.T) {
	root := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeNote(t, root, "knowledge/alerts/disk.md", "---\nalerts: [disk]\n---\n# disk\n", old.Add(time.Hour))
	writeNote(t, root, "knowledge/services/nginx.md", "---\nservices: [nginx]\n---\n# nginx\n", old.Add(2*time.Hour))
	writeNote(t, root, "knowledge/hosts/h.md", "---\nhosts: [h]\n---\n# h\n", old.Add(3*time.Hour))
	writeNote(t, root, "knowledge/notes/tagged.md", "---\ntags: [api]\n---\n# tagged\n", old.Add(4*time.Hour))
	writeNote(t, root, "knowledge/notes/text.md", "---\n---\n# text\n\nonly the word kumquat appears here.\n", old.Add(5*time.Hour))

	ix, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	hits := ix.Match(Subject{
		Alert: "disk",
		Unit:  "nginx.service",
		Hosts: []string{"h"},
		Tags:  []string{"api", "kumquat"},
	})
	got := hitPaths(root, hits)
	want := []string{
		"knowledge/alerts/disk.md",
		"knowledge/services/nginx.md",
		"knowledge/hosts/h.md",
		"knowledge/notes/tagged.md",
		"knowledge/notes/text.md",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ranking = %v, want %v", got, want)
	}
	if hits[0].Score <= hits[1].Score || hits[1].Score <= hits[2].Score {
		t.Fatalf("scores are not strictly ordered: %d %d %d", hits[0].Score, hits[1].Score, hits[2].Score)
	}

	// Equal scores: the newest file wins.
	writeNote(t, root, "knowledge/notes/a-dup.md", "---\ntags: [dup]\n---\n# a\n", old)
	writeNote(t, root, "knowledge/notes/b-dup.md", "---\ntags: [dup]\n---\n# b\n", old.Add(time.Hour))
	ix, err = BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	hits = ix.Match(Subject{Tags: []string{"dup"}})
	if len(hits) != 2 {
		t.Fatalf("hits = %v, want both duplicates", hitPaths(root, hits))
	}
	if got, want := relTo(root, hits[0].Path), "knowledge/notes/b-dup.md"; got != want {
		t.Fatalf("newest-first tie break = %q, want %q", got, want)
	}

	// At most 20 hits, even when 25 notes match.
	for i := 0; i < 25; i++ {
		name := filepath.Join("knowledge", "notes", "many", "n"+string(rune('a'+i))+".md")
		writeNote(t, root, name, "---\ntags: [many]\n---\n# n\n", old.Add(time.Duration(i)*time.Minute))
	}
	ix, err = BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if hits := ix.Match(Subject{Tags: []string{"many"}}); len(hits) != maxHits {
		t.Fatalf("hits = %d, want the %d cap", len(hits), maxHits)
	}
}

func TestMatchUpdate(t *testing.T) {
	root := t.TempDir()
	path := writeNote(t, root, "knowledge/services/redis.md", "---\nservices: [redis]\n---\n# redis\n\nnothing yet.\n", time.Time{})
	ix, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if hits := ix.Match(Subject{Tags: []string{"oom"}}); len(hits) != 0 {
		t.Fatalf("hits before the append = %v, want none", hitPaths(root, hits))
	}
	writeNote(t, root, "knowledge/services/redis.md", "---\nservices: [redis]\n---\n# redis\n\nan oom kill after a failover.\n", time.Time{})
	if err := ix.Update(path); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if hits := ix.Match(Subject{Tags: []string{"oom"}}); len(hits) != 1 {
		t.Fatalf("hits after the append = %v, want one", hitPaths(root, hits))
	}

	// A brand-new note is picked up by Update too.
	fresh := writeNote(t, root, "knowledge/alerts/latency.md", "---\nalerts: [latency]\n---\n# latency\n", time.Time{})
	if err := ix.Update(fresh); err != nil {
		t.Fatalf("Update new note: %v", err)
	}
	if hits := ix.Match(Subject{Alert: "latency"}); len(hits) != 1 {
		t.Fatalf("hits for the new note = %v, want one", hitPaths(root, hits))
	}

	// A note that is gone is dropped.
	if err := os.Remove(fresh); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := ix.Update(fresh); err != nil {
		t.Fatalf("Update of a removed note: %v", err)
	}
	if hits := ix.Match(Subject{Alert: "latency"}); len(hits) != 0 {
		t.Fatalf("hits after removal = %v, want none", hitPaths(root, hits))
	}

	// A path outside the root is refused.
	if err := ix.Update(filepath.Join(t.TempDir(), "elsewhere.md")); err == nil {
		t.Fatal("Update accepted a path outside the root")
	}
}

func TestBuildIndexEmptyRoot(t *testing.T) {
	ix, err := BuildIndex(t.TempDir())
	if err != nil {
		t.Fatalf("BuildIndex of an empty root: %v", err)
	}
	if got := ix.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
	if hits := ix.Match(Subject{Hosts: []string{"web-01"}}); len(hits) != 0 {
		t.Fatalf("hits = %v, want none", hits)
	}
	if _, err := BuildIndex(""); err == nil {
		t.Fatal("BuildIndex accepted an empty root")
	}
}

func TestKnowledgeCreateNote(t *testing.T) {
	root := t.TempDir()

	path, created, err := CreateNote(root, NoteKindHost, "web-ewr-007.example.internal")
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if !created {
		t.Fatal("created = false for a new note")
	}
	if got, want := path, filepath.Join(root, "knowledge", "hosts", "web-ewr-007.example.internal.md"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	text := string(data)
	for _, want := range []string{"---\nhosts: [web-ewr-007.example.internal]\n---", "# web-ewr-007.example.internal", headingRootCauses} {
		if !strings.Contains(text, want) {
			t.Fatalf("note is missing %q:\n%s", want, text)
		}
	}
	// It indexes straight away, through the path convention.
	ix, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if hits := ix.Match(Subject{Hosts: []string{"web-ewr-007"}}); len(hits) != 1 {
		t.Fatalf("hits = %v, want the new note", hitPaths(root, hits))
	}

	// Creating it again keeps the file and reports created=false.
	before := text
	again, created, err := CreateNote(root, NoteKindHost, "web-ewr-007.example.internal")
	if err != nil {
		t.Fatalf("CreateNote again: %v", err)
	}
	if created {
		t.Fatal("created = true for an existing note")
	}
	if again != path {
		t.Fatalf("path = %q, want %q", again, path)
	}
	if data, _ := os.ReadFile(path); string(data) != before {
		t.Fatal("CreateNote overwrote an existing note")
	}

	// The other kinds land in their own directory with their own key.
	for kind, want := range map[string]string{
		NoteKindService: filepath.Join("knowledge", "services", "nginx.md"),
		NoteKindAlert:   filepath.Join("knowledge", "alerts", "api-5xx.md"),
		NoteKindRunbook: filepath.Join("knowledge", "runbooks", "failover.md"),
	} {
		name := strings.TrimSuffix(filepath.Base(want), ".md")
		got, created, err := CreateNote(root, kind, name)
		if err != nil || !created {
			t.Fatalf("CreateNote(%s): %v, created=%v", kind, err, created)
		}
		if relTo(root, got) != filepath.ToSlash(want) {
			t.Fatalf("CreateNote(%s) = %q, want %q", kind, relTo(root, got), want)
		}
	}

	// Bad input is a configuration error, never a panic or a path escape.
	for _, tc := range []struct{ kind, name string }{
		{"nope", "x"},
		{NoteKindHost, ""},
		{NoteKindHost, "../escape"},
		{NoteKindHost, "a/b"},
		{NoteKindAlert, ".hidden"},
	} {
		if _, _, err := CreateNote(root, tc.kind, tc.name); err == nil {
			t.Fatalf("CreateNote(%q, %q) accepted the input", tc.kind, tc.name)
		}
	}
}

func TestKnowledgeRecordRootCause(t *testing.T) {
	root := t.TempDir()

	path, err := RecordRootCause(root, NoteKindAlert, "api-5xx", "varnish warm-up missed the new build")
	if err != nil {
		t.Fatalf("RecordRootCause: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "alerts: [api-5xx]") {
		t.Fatalf("note has no frontmatter key:\n%s", text)
	}
	if !strings.Contains(text, headingRootCauses+"\n- varnish warm-up missed the new build") {
		t.Fatalf("root cause not filed under the heading:\n%s", text)
	}

	// A second call appends another bullet and keeps the first.
	if _, err := RecordRootCause(root, NoteKindAlert, "api-5xx", "and the health check lied"); err != nil {
		t.Fatalf("RecordRootCause again: %v", err)
	}
	data, _ = os.ReadFile(path)
	text = string(data)
	if !strings.Contains(text, "- varnish warm-up missed the new build") || !strings.Contains(text, "- and the health check lied") {
		t.Fatalf("both bullets must survive:\n%s", text)
	}

	// The new line is matchable after Update.
	ix, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if err := ix.Update(path); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if hits := ix.Match(Subject{Tags: []string{"warm-up"}}); len(hits) == 0 {
		t.Fatal("the appended root cause is not in the index")
	}

	if _, err := RecordRootCause(root, NoteKindAlert, "api-5xx", "  "); err == nil {
		t.Fatal("RecordRootCause accepted empty text")
	}
}
