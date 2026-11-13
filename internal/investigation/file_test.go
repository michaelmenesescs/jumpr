package investigation

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"jumper/internal/action"
	"jumper/internal/errs"

	"jumper/internal/redact"
	"jumper/internal/resource"
)

// update rewrites testdata/golden-*.md instead of comparing, in the house style
// of `go test -update`.
var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// testLoc is the fixed zone every test clock lives in, so a bullet's wall clock
// never depends on where the test runs.
var testLoc = time.FixedZone("EST", -5*60*60)

// at is a fixed wall clock on 2026-11-12 in testLoc.
func at(h, m, s int) time.Time {
	return time.Date(2026, 11, 12, h, m, s, 0, testLoc)
}

func newRed(t *testing.T) *redact.Redactor {
	t.Helper()
	r, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}
	return r
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// resolvedLine is the one line in a golden file that cannot be fixed by the
// test itself: SetStatus stamps time.Now(). It is normalized on both sides.
var resolvedLine = regexp.MustCompile(`(?m)^resolved: .*$`)

func normalizeResolved(s string) string {
	return resolvedLine.ReplaceAllString(s, "resolved: 2026-11-12T14:08:00-05:00")
}

func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run go test -update)", path, err)
	}
	if got == string(want) {
		return
	}
	t.Errorf("golden mismatch: %s\n%s", path, firstDifference(string(want), got))
}

// firstDifference points at the first differing line so a failure is readable.
func firstDifference(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("line %d:\n  want: %q\n  got:  %q", i+1, w, g)
		}
	}
	return "no difference (trailing bytes?)"
}

func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"API 5xx in EWR", "api-5xx-in-ewr"},
		{"  --hello,  world!!  ", "hello-world"},
		{"C++ / Go", "c-go"},
		{"", ""},
		{"!!!", ""},
		{strings.Repeat("a", 60), strings.Repeat("a", 48)},
		{"2026 Q4 review", "2026-q4-review"},
	}
	for _, c := range cases {
		if got := Slug(c.in); got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A truncated slug never ends in a dash.
	if got := Slug(strings.Repeat("a", 47) + " bcd"); strings.HasSuffix(got, "-") {
		t.Errorf("Slug ends in a dash: %q", got)
	}
}

func TestNewID(t *testing.T) {
	if got, want := NewID(at(14, 2, 11), "API 5xx in EWR"), "2026-11-12-api-5xx-in-ewr"; got != want {
		t.Errorf("NewID = %q, want %q", got, want)
	}
	if got, want := NewID(at(14, 2, 11), "!!!"), "2026-11-12"; got != want {
		t.Errorf("NewID without a usable title = %q, want %q", got, want)
	}
}

func sampleMeta() Meta {
	targets := make([]resource.HostID, 0, 24)
	for i := 1; i <= 24; i++ {
		targets = append(targets, resource.HostID(fmt.Sprintf("web-ewr-%03d.example.internal", i)))
	}
	return Meta{
		ID:      "2026-11-12-api-5xx-ewr",
		Title:   "API 5xx in EWR",
		Status:  "open",
		Opened:  at(14, 2, 11),
		Window:  resource.TimeWindow{From: at(13, 30, 0), ToNow: true, Label: "-30m"},
		Targets: targets,
		Alerts:  []string{`HighErrorRate{service="api"}`},
		Tags:    []string{"api", "nginx"},
	}
}

func TestCreate(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantPath := filepath.Join(root, "investigations", "2026-11-12-api-5xx-ewr.md")
	if path != wantPath {
		t.Fatalf("Create path = %q, want %q", path, wantPath)
	}

	meta, err := ReadMeta(path)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if meta.ID != "2026-11-12-api-5xx-ewr" || meta.Title != "API 5xx in EWR" || meta.Status != "open" {
		t.Errorf("bad meta: %+v", meta)
	}
	if !meta.Opened.Equal(at(14, 2, 11)) {
		t.Errorf("Opened = %v, want %v", meta.Opened, at(14, 2, 11))
	}
	if !meta.Window.From.Equal(at(13, 30, 0)) || !meta.Window.ToNow {
		t.Errorf("Window = %+v, want from 13:30 open-ended", meta.Window)
	}
	if len(meta.Targets) != 24 || meta.Targets[23] != "web-ewr-024.example.internal" {
		t.Errorf("Targets = %v", meta.Targets)
	}
	if len(meta.Alerts) != 1 || meta.Alerts[0] != `HighErrorRate{service="api"}` {
		t.Errorf("Alerts = %v", meta.Alerts)
	}
	if len(meta.Tags) != 2 || meta.Tags[0] != "api" || meta.Tags[1] != "nginx" {
		t.Errorf("Tags = %v", meta.Tags)
	}

	text := readFile(t, path)
	for _, want := range []string{"\n# API 5xx in EWR\n", "\n## Timeline\n", "\n## Findings\n", "\n## Resolution\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("created file lacks %q:\n%s", want, text)
		}
	}

	// Never overwrite: the second and third ids get a suffix.
	p2, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if filepath.Base(p2) != "2026-11-12-api-5xx-ewr-2.md" {
		t.Errorf("second Create = %q", filepath.Base(p2))
	}
	m2, err := ReadMeta(p2)
	if err != nil {
		t.Fatalf("ReadMeta second: %v", err)
	}
	if m2.ID != "2026-11-12-api-5xx-ewr-2" {
		t.Errorf("second id = %q, want the suffixed id", m2.ID)
	}
	p3, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("third Create: %v", err)
	}
	if filepath.Base(p3) != "2026-11-12-api-5xx-ewr-3.md" {
		t.Errorf("third Create = %q", filepath.Base(p3))
	}
}

func TestCreateFoldsTargets(t *testing.T) {
	root := t.TempDir()
	m := sampleMeta()
	m.Targets = nil
	for i := 0; i < 35; i++ {
		m.Targets = append(m.Targets, resource.HostID(fmt.Sprintf("h%02d.example.internal", i)))
	}
	path, err := Create(root, m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	text := readFile(t, path)
	if !strings.Contains(text, "\n# and 5 more targets\n") {
		t.Errorf("missing fold comment:\n%s", text)
	}
	meta, err := ReadMeta(path)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(meta.Targets) != 30 {
		t.Errorf("folded file lists %d targets, want 30", len(meta.Targets))
	}
}

func TestFormatEntry(t *testing.T) {
	red := newRed(t)
	got := FormatEntry(Entry{
		Time:  at(14, 2, 40),
		Kind:  EntryProbe,
		Actor: action.Human,
		Text:  "`failed-units` on 24 hosts → 22 identical, 2 differ",
	}, red)
	want := "- 14:02:40 **probe** `failed-units` on 24 hosts → 22 identical, 2 differ"
	if got != want {
		t.Errorf("FormatEntry =\n%q\nwant\n%q", got, want)
	}

	// A non-human actor is credited at the end of the line.
	got = FormatEntry(Entry{Time: at(14, 5, 12), Kind: EntryAction, Actor: "plugin:rollout", Text: "restarted nginx"}, red)
	want = "- 14:05:12 **action** restarted nginx by **plugin:rollout**"
	if got != want {
		t.Errorf("FormatEntry with actor =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatEntryOutput(t *testing.T) {
	red := newRed(t)
	var out []string
	for i := 1; i <= 38; i++ {
		out = append(out, fmt.Sprintf("nginx[%d]: worker %d started", 1000+i, i))
	}
	got := FormatEntry(Entry{
		Time:   at(14, 3, 5),
		Kind:   EntryJournal,
		Actor:  action.Human,
		Text:   "nginx on web-ewr-007 → `bind() to 0.0.0.0:443 failed`",
		Output: strings.Join(out, "\n") + "\n",
	}, red)

	lines := strings.Split(got, "\n")
	if lines[0] != "- 14:03:05 **journal** nginx on web-ewr-007 → `bind() to 0.0.0.0:443 failed`" {
		t.Errorf("bullet = %q", lines[0])
	}
	if lines[1] != "  <details><summary>output (38 lines)</summary>" {
		t.Errorf("summary line = %q", lines[1])
	}
	if lines[2] != "" {
		t.Errorf("expected a blank line after the summary, got %q", lines[2])
	}
	if lines[3] != "  ```" {
		t.Errorf("fence = %q", lines[3])
	}
	if lines[len(lines)-2] != "  ```" || lines[len(lines)-1] != "  </details>" {
		t.Errorf("block does not end with the fence and </details>: %q", lines[len(lines)-3:])
	}
	if !strings.Contains(got, "\n  nginx[1001]: worker 1 started\n") {
		t.Errorf("output line not indented by two spaces:\n%s", got)
	}
	if n := strings.Count(got, "  ```"); n != 2 {
		t.Errorf("want exactly two fence lines, got %d", n)
	}
}

func TestOutputWithBackticks(t *testing.T) {
	red := newRed(t)
	output := strings.Join([]string{
		"here is a fence:",
		"```yaml",
		"key: value",
		"```",
		"and a longer one:",
		"````",
		"after",
	}, "\n")
	got := FormatEntry(Entry{Time: at(14, 3, 5), Kind: EntryJournal, Text: "cat file", Output: output}, red)

	lines := strings.Split(got, "\n")
	// A run of four backticks in the output forces a five-backtick fence, so
	// the embedded ``` / ```` lines stay inside the block.
	if lines[3] != "  `````" {
		t.Errorf("opening fence = %q, want five backticks:\n%s", lines[3], got)
	}
	if lines[len(lines)-2] != "  `````" {
		t.Errorf("closing fence = %q, want five backticks:\n%s", lines[len(lines)-2], got)
	}
	if n := strings.Count(got, "`````"); n != 2 {
		t.Errorf("want exactly two five-backtick fences, got %d:\n%s", n, got)
	}
	// The whole output survives verbatim, just indented.
	for _, ln := range strings.Split(output, "\n") {
		if ln != "" && !strings.Contains(got, "\n  "+ln+"\n") {
			t.Errorf("output line %q missing:\n%s", ln, got)
		}
	}
}

func TestFormatEntryCapsOutput(t *testing.T) {
	red := newRed(t)
	big := strings.Repeat("0123456789abcdef\n", 2000) // 34 kB
	got := FormatEntry(Entry{Time: at(14, 3, 5), Kind: EntryJournal, Text: "big", Output: big}, red)
	// The cap applies to the output itself; each line then gains two spaces of
	// indentation, so the rendered block is allowed that much more.
	if len(got) > maxOutputBytes+4096 {
		t.Errorf("capped entry is %d bytes, want roughly %d", len(got), maxOutputBytes)
	}
	if !strings.Contains(got, "output truncated") {
		t.Errorf("truncation is not announced:\n%s", got[len(got)-200:])
	}
	if !strings.Contains(got, "lines)</summary>") {
		t.Errorf("summary missing:\n%s", got[:200])
	}
}

// goldenInvestigation builds the file the acceptance describes: create, an open
// entry, a probe, a journal entry with 38 lines of output, an action, a note, a
// finding, then resolve.
func goldenInvestigation(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	red := newRed(t)

	path, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var output []string
	for i := 1; i <= 38; i++ {
		output = append(output, fmt.Sprintf("Nov 12 14:03:0%d web-ewr-007 nginx[%d]: worker %d started", i%10, 1000+i, i))
	}
	entries := []Entry{
		{Time: at(14, 2, 11), Kind: EntryOpen, Text: "from alert `HighErrorRate` (24 hosts)"},
		{Time: at(14, 2, 40), Kind: EntryProbe, Text: "`failed-units` on 24 hosts → 22 identical, 2 differ (`nginx.service failed`)"},
		{
			Time:   at(14, 3, 5),
			Kind:   EntryJournal,
			Text:   "nginx on web-ewr-007 → `bind() to 0.0.0.0:443 failed (98: Address already in use)`",
			Output: strings.Join(output, "\n"),
		},
		{Time: at(14, 5, 12), Kind: EntryAction, Actor: action.Human, Text: "`systemctl restart nginx` on 2 hosts → ok 2/2"},
		{Time: at(14, 6, 0), Kind: EntryNote, Text: "stale haproxy bound :443 after salt run 20261112135501"},
		{Time: at(14, 7, 0), Kind: EntryFinding, Text: "haproxy held :443, so nginx never bound"},
	}
	for _, e := range entries {
		if err := AppendEntry(path, e, red); err != nil {
			t.Fatalf("AppendEntry %s: %v", e.Kind, err)
		}
	}
	if err := SetStatus(path, "resolved", "stale haproxy process bound :443; restarted haproxy"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	return path
}

func TestCreateGoldenInvestigation(t *testing.T) {
	path := goldenInvestigation(t)
	compareGolden(t, "golden-investigation.md", normalizeResolved(readFile(t, path)))
}

func TestAppendEntry(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	red := newRed(t)
	for i, e := range []Entry{
		{Time: at(14, 2, 11), Kind: EntryOpen, Text: "opened"},
		{Time: at(14, 2, 40), Kind: EntryProbe, Text: "probed"},
	} {
		if err := AppendEntry(path, e, red); err != nil {
			t.Fatalf("AppendEntry %d: %v", i, err)
		}
	}
	text := readFile(t, path)
	open := strings.Index(text, "- 14:02:11 **open** opened")
	probe := strings.Index(text, "- 14:02:40 **probe** probed")
	findings := strings.Index(text, "## Findings")
	if open < 0 || probe < 0 {
		t.Fatalf("bullets missing:\n%s", text)
	}
	if !(open < probe && probe < findings) {
		t.Errorf("bullets are not in order inside ## Timeline:\n%s", text)
	}
	if !strings.Contains(text, "probed\n\n## Findings") {
		t.Errorf("no blank line between the timeline and the next heading:\n%s", text)
	}
	// The frontmatter and title are untouched by appends.
	meta, err := ReadMeta(path)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if meta.ID != "2026-11-12-api-5xx-ewr" || meta.Status != "open" {
		t.Errorf("frontmatter changed: %+v", meta)
	}
}

func TestAppendFindingMirrored(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	red := newRed(t)
	finding := Entry{Time: at(14, 7, 0), Kind: EntryFinding, Text: "haproxy held :443"}
	if err := AppendEntry(path, finding, red); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	text := readFile(t, path)
	if !strings.Contains(text, "- 14:07:00 **finding** haproxy held :443") {
		t.Errorf("timeline bullet missing:\n%s", text)
	}
	i := strings.Index(text, "## Findings\n")
	j := strings.Index(text, "## Resolution")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("sections out of order:\n%s", text)
	}
	if got := text[i:j]; !strings.Contains(got, "- haproxy held :443") {
		t.Errorf("finding not mirrored under ## Findings: %q", got)
	}
}

func TestAppendToHandEditedFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "investigations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hand.md")
	hand := "---\nid: hand\nstatus: open\nopened: 2026-11-12T14:00:00-05:00\nwindow: [2026-11-12T13:00:00-05:00, now]\n---\n" +
		"# Hand edited\n\n## Findings\n- something I typed\n"
	if err := os.WriteFile(path, []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}

	red := newRed(t)
	if err := AppendEntry(path, Entry{Time: at(14, 2, 40), Kind: EntryNote, Text: "added later"}, red); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	text := readFile(t, path)
	if !strings.HasPrefix(text, hand) {
		t.Errorf("hand-edited content was not preserved:\n%s", text)
	}
	if !strings.HasSuffix(text, "\n## Timeline\n- 14:02:40 **note** added later\n") {
		t.Errorf("missing appended section:\n%s", text)
	}
}

func TestReadMetaMissingFile(t *testing.T) {
	_, err := ReadMeta(filepath.Join(t.TempDir(), "nope.md"))
	if err == nil || errs.KindOf(err) != errs.Internal {
		t.Fatalf("ReadMeta on a missing file: %v", err)
	}
}

func TestReadMetaBadFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.md")
	if err := os.WriteFile(path, []byte("# no frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadMeta(path)
	if err == nil || errs.KindOf(err) != errs.Parse {
		t.Fatalf("ReadMeta on a malformed file: %v", err)
	}
}

func TestSetStatus(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	red := newRed(t)
	if err := AppendEntry(path, Entry{Time: at(14, 6, 0), Kind: EntryNote, Text: "a note"}, red); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}

	before := time.Now().Add(-time.Minute)
	if err := SetStatus(path, "resolved", "stale haproxy process bound :443"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	meta, err := ReadMeta(path)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if meta.Status != "resolved" {
		t.Errorf("status = %q, want resolved", meta.Status)
	}

	text := readFile(t, path)
	if !strings.Contains(text, "- 14:06:00 **note** a note") {
		t.Errorf("timeline lost by SetStatus:\n%s", text)
	}
	if !strings.Contains(text, "\n## Resolution\nstale haproxy process bound :443\n") {
		t.Errorf("resolution text missing:\n%s", text)
	}
	var fm frontmatter
	fmText, _, err := splitFrontmatter(text)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		t.Fatalf("frontmatter no longer parses: %v", err)
	}
	if fm.Resolved.Before(before) {
		t.Errorf("resolved timestamp = %v, want around now", fm.Resolved)
	}

	// An unknown status is refused and leaves the file alone.
	if err := SetStatus(path, "done", ""); err == nil || errs.KindOf(err) != errs.Config {
		t.Errorf("SetStatus with a bad status = %v, want an errs.Config error", err)
	}
	if got := readFile(t, path); got != text {
		t.Errorf("a rejected SetStatus rewrote the file")
	}
}

func TestSetStatusKeepsUnknownKeys(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	text := strings.Replace(readFile(t, path), "status: open\n", "status: open\nowner: mike\n", 1)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetStatus(path, "abandoned", ""); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if got := readFile(t, path); !strings.Contains(got, "owner: mike") {
		t.Errorf("hand-added frontmatter key was lost:\n%s", got)
	}
}

func TestList(t *testing.T) {
	root := t.TempDir()
	mk := func(id string, opened time.Time) {
		m := sampleMeta()
		m.ID = id
		m.Opened = opened
		if _, err := Create(root, m); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	mk("2026-11-10-older", time.Date(2026, 11, 10, 9, 0, 0, 0, testLoc))
	mk("2026-11-12-newest", time.Date(2026, 11, 12, 14, 0, 0, 0, testLoc))
	mk("2026-11-11-middle", time.Date(2026, 11, 11, 8, 0, 0, 0, testLoc))

	// A file without frontmatter is skipped, not fatal.
	junk := filepath.Join(root, "investigations", "junk.md")
	if err := os.WriteFile(junk, []byte("# junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	metas, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ids []string
	for _, m := range metas {
		ids = append(ids, m.ID)
	}
	want := []string{"2026-11-12-newest", "2026-11-11-middle", "2026-11-10-older"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("List = %v, want %v", ids, want)
	}

	empty, err := List(t.TempDir())
	if err != nil || len(empty) != 0 {
		t.Errorf("List on an empty root = %v, %v", empty, err)
	}
}

func TestAppendAudit(t *testing.T) {
	root := t.TempDir()
	red := newRed(t)
	now := at(14, 5, 12)
	entry := Entry{Time: now, Kind: EntryAction, Actor: action.Human, Text: "`systemctl restart nginx` on 2 hosts → ok 2/2"}
	if err := AppendAudit(root, entry, red, now); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	path := filepath.Join(root, "audit", "2026-11-12.md")
	text := readFile(t, path)
	if !strings.HasPrefix(text, "# Audit 2026-11-12\n\n## Timeline\n") {
		t.Errorf("audit header missing:\n%s", text)
	}
	if !strings.Contains(text, "- 14:05:12 **action** `systemctl restart nginx` on 2 hosts → ok 2/2") {
		t.Errorf("audit bullet missing:\n%s", text)
	}

	second := Entry{Time: at(14, 6, 0), Kind: EntryDenied, Text: "denied `systemctl stop sshd`"}
	if err := AppendAudit(root, second, red, at(14, 6, 0)); err != nil {
		t.Fatalf("second AppendAudit: %v", err)
	}
	text = readFile(t, path)
	if n := strings.Count(text, "\n- "); n != 2 {
		t.Errorf("want two audit bullets, got %d:\n%s", n, text)
	}
	if n := strings.Count(text, "# Audit "); n != 1 {
		t.Errorf("header written %d times:\n%s", n, text)
	}
}

func TestAppendAuditGolden(t *testing.T) {
	root := t.TempDir()
	red := newRed(t)
	now := at(14, 5, 12)
	entries := []Entry{
		{Time: at(14, 5, 12), Kind: EntryAction, Actor: action.Human, Text: "`systemctl restart nginx` on 2 hosts → ok 2/2"},
		{Time: at(14, 6, 0), Kind: EntryDenied, Actor: "plugin:policy", Text: "`systemctl stop sshd` on web-ewr-007"},
	}
	for _, e := range entries {
		if err := AppendAudit(root, e, red, now); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}
	compareGolden(t, "golden-audit.md", readFile(t, filepath.Join(root, "audit", "2026-11-12.md")))
}

func TestFormatEntryRedaction(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	red := newRed(t)
	const token = "Bearer abcdefghijklmnopqrstuvwxyz0123456789"
	entry := Entry{
		Time:   at(14, 3, 5),
		Kind:   EntryJournal,
		Text:   "curl with the service token",
		Output: "GET /health HTTP/1.1\nAuthorization: " + token + "\nhost: web-ewr-007",
	}
	if err := AppendEntry(path, entry, red); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	text := readFile(t, path)
	if strings.Contains(text, "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("bearer token leaked into the file:\n%s", text)
	}
	if !strings.Contains(text, "‹redacted:bearer›") {
		t.Errorf("redaction marker missing:\n%s", text)
	}
}

func TestAppendConcurrentAppends(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, sampleMeta())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	red := newRed(t)

	const writers = 20
	var wg sync.WaitGroup
	errsCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := Entry{Time: at(14, 2, 40), Kind: EntryNote, Text: fmt.Sprintf("note %02d", i)}
			errsCh <- AppendEntry(path, e, red)
		}(i)
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatalf("concurrent AppendEntry: %v", err)
		}
	}

	text := readFile(t, path)
	if n := strings.Count(text, "**note**"); n != writers {
		t.Errorf("file has %d note bullets, want %d:\n%s", n, writers, text)
	}
	for i := 0; i < writers; i++ {
		if !strings.Contains(text, fmt.Sprintf("note %02d", i)) {
			t.Errorf("bullet %02d lost", i)
		}
	}
	if !strings.HasSuffix(text, "## Resolution\n") {
		t.Errorf("file no longer ends under ## Resolution:\n%s", text)
	}
	meta, err := ReadMeta(path)
	if err != nil {
		t.Fatalf("file is not valid after concurrent appends: %v", err)
	}
	if meta.ID != "2026-11-12-api-5xx-ewr" {
		t.Errorf("frontmatter damaged: %+v", meta)
	}
	if strings.Contains(text, ".tmp") {
		t.Errorf("a temporary file name leaked into the document")
	}
}
