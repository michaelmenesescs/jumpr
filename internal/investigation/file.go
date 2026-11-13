// Package investigation reads and writes the plain-Markdown investigation and
// audit records (SPEC §10, BUILD-PLAN §4.10).
//
// A record is one Markdown file: YAML frontmatter, an H1 title and a fixed set
// of H2 sections. Everything after the closing frontmatter line is opaque text
// and is preserved byte for byte except for the parts a call inserts or
// replaces.
package investigation

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"jumper/internal/errs"
	"jumper/internal/redact"
	"jumper/internal/resource"
)

// Meta describes an investigation without its timeline (BUILD-PLAN §4.10).
type Meta struct {
	ID      string // "2026-11-12-api-5xx-ewr"
	Title   string
	Status  string // open | resolved | abandoned
	Opened  time.Time
	Window  resource.TimeWindow
	Targets []resource.HostID
	Alerts  []string
	Tags    []string
	Path    string
}

const (
	// Section headings, in file order.
	headingTimeline   = "## Timeline"
	headingFindings   = "## Findings"
	headingResolution = "## Resolution"

	// metaMaxTargets is how many hosts the frontmatter lists before folding.
	metaMaxTargets = 30
	// slugMax is the longest slug NewID keeps.
	slugMax = 48

	dirInvestigations = "investigations"
	dirAudit          = "audit"
)

// Statuses accepted by SetStatus. Anything else is a configuration mistake.
var validStatus = map[string]bool{
	"open":      true,
	"resolved":  true,
	"abandoned": true,
}

// Slug lower-cases title, turns every run of other characters into a single
// dash, trims the dashes and keeps at most 48 characters.
func Slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > slugMax {
		s = strings.TrimRight(s[:slugMax], "-")
	}
	return s
}

// NewID builds the file stem for an investigation opened at now:
// "2026-11-12-api-5xx-ewr". A title with nothing usable leaves just the date.
func NewID(now time.Time, title string) string {
	date := now.Format("2006-01-02")
	if slug := Slug(title); slug != "" {
		return date + "-" + slug
	}
	return date
}

// Create writes a new investigation under root/investigations and returns its
// path. It never overwrites: when the id is taken the file gets a -2, -3, …
// suffix and its frontmatter id follows.
func Create(root string, m Meta) (string, error) {
	const op = "investigation.create"
	opened := m.Opened
	if opened.IsZero() {
		opened = time.Now()
	}
	base := m.ID
	if base == "" {
		base = NewID(opened, m.Title)
	}
	dir := filepath.Join(root, dirInvestigations)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errs.Wrap(errs.Internal, op, err)
	}

	for n := 1; n < 10000; n++ {
		id := base
		if n > 1 {
			id = base + "-" + strconv.Itoa(n)
		}
		path := filepath.Join(dir, id+".md")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", errs.Wrap(errs.Internal, op, err)
		}
		_, werr := f.WriteString(renderInvestigation(m, id, opened))
		cerr := f.Close()
		if werr == nil {
			werr = cerr
		}
		if werr != nil {
			return "", errs.Wrap(errs.Internal, op, werr)
		}
		return path, nil
	}
	return "", errs.New(errs.Internal, op, "too many files for id "+base)
}

// renderInvestigation is the whole initial file: frontmatter, title and the
// three empty sections in order.
func renderInvestigation(m Meta, id string, opened time.Time) string {
	status := m.Status
	if status == "" {
		status = "open"
	}
	title := m.Title
	if title == "" {
		title = id
	}
	targets := make([]string, len(m.Targets))
	for i, h := range m.Targets {
		targets[i] = string(h)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("id: " + yamlScalar(id) + "\n")
	b.WriteString("status: " + yamlScalar(status) + "\n")
	b.WriteString("opened: " + opened.Format(time.RFC3339) + "\n")
	b.WriteString("window: " + windowList(m.Window, opened) + "\n")
	if len(targets) > metaMaxTargets {
		b.WriteString("targets: " + flowList(targets[:metaMaxTargets]) + "\n")
		b.WriteString(fmt.Sprintf("# and %d more targets\n", len(targets)-metaMaxTargets))
	} else {
		b.WriteString("targets: " + flowList(targets) + "\n")
	}
	b.WriteString("alerts: " + flowList(m.Alerts) + "\n")
	b.WriteString("tags: " + flowList(m.Tags) + "\n")
	b.WriteString("---\n")
	b.WriteString("# " + title + "\n")
	b.WriteString("\n")
	b.WriteString(headingTimeline + "\n")
	b.WriteString("\n")
	b.WriteString(headingFindings + "\n")
	b.WriteString("\n")
	b.WriteString(headingResolution + "\n")
	return b.String()
}

// windowList renders the window as the two-element list of SPEC §10, with
// "now" standing in for an open-ended window.
func windowList(w resource.TimeWindow, opened time.Time) string {
	from := w.From
	if from.IsZero() {
		from = opened
	}
	to := "now"
	if !w.ToNow && !w.To.IsZero() {
		to = w.To.Format(time.RFC3339)
	}
	return "[" + from.Format(time.RFC3339) + ", " + to + "]"
}

// plainScalar matches strings that need no quoting in YAML flow context.
// Dates and numbers are excluded because the parser would resolve them to
// another type.
var (
	plainScalar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./@+-]*$`)
	scalarLike  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$|^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// yamlScalar renders one string as a flow-context YAML scalar, quoting it when
// the plain form could be misread or is not allowed inside [ ].
func yamlScalar(s string) string {
	switch strings.ToLower(s) {
	case "y", "n", "yes", "no", "true", "false", "on", "off", "null", "~", "now":
		return strconv.Quote(s)
	}
	if plainScalar.MatchString(s) && !scalarLike.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// flowList renders a list in the flow style SPEC §10 uses, e.g. [api, nginx].
func flowList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = yamlScalar(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// frontmatter is the typed view of the block between the --- lines.
type frontmatter struct {
	ID       string     `yaml:"id"`
	Status   string     `yaml:"status"`
	Opened   time.Time  `yaml:"opened"`
	Window   windowSpec `yaml:"window"`
	Targets  []string   `yaml:"targets"`
	Alerts   []string   `yaml:"alerts"`
	Tags     []string   `yaml:"tags"`
	Resolved time.Time  `yaml:"resolved"`
}

// windowSpec decodes the window list keeping each element's literal text, so a
// timestamp does not have to be re-formatted on the way back in.
type windowSpec []string

func (w *windowSpec) UnmarshalYAML(node *yaml.Node) error {
	var nodes []yaml.Node
	if err := node.Decode(&nodes); err != nil {
		return err
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, strings.TrimSpace(n.Value))
	}
	*w = out
	return nil
}

// ReadMeta parses the frontmatter and the H1 title of an investigation file.
func ReadMeta(path string) (Meta, error) {
	const op = "investigation.read_meta"
	data, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, errs.Wrap(errs.Internal, op, err)
	}
	fmText, body, err := splitFrontmatter(string(data))
	if err != nil {
		return Meta{}, errs.New(errs.Parse, op, path+": "+err.Error())
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return Meta{}, errs.New(errs.Parse, op, path+": "+err.Error())
	}
	meta := Meta{
		ID:     fm.ID,
		Title:  titleOf(body),
		Status: fm.Status,
		Opened: fm.Opened,
		Alerts: fm.Alerts,
		Tags:   fm.Tags,
		Path:   path,
	}
	if meta.ID == "" {
		meta.ID = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if meta.Status == "" {
		meta.Status = "open"
	}
	for _, h := range fm.Targets {
		meta.Targets = append(meta.Targets, resource.HostID(h))
	}
	if w, ok := parseWindow(fm.Window); ok {
		meta.Window = w
	}
	return meta, nil
}

// parseWindow turns the two-element frontmatter list back into a TimeWindow.
func parseWindow(spec windowSpec) (resource.TimeWindow, bool) {
	if len(spec) != 2 || spec[0] == "" {
		return resource.TimeWindow{}, false
	}
	from, err := parseTimestamp(spec[0])
	if err != nil {
		return resource.TimeWindow{}, false
	}
	w := resource.TimeWindow{From: from, Label: strings.Join(spec, "..")}
	if strings.EqualFold(spec[1], "now") || spec[1] == "" {
		w.ToNow = true
		return w, true
	}
	to, err := parseTimestamp(spec[1])
	if err != nil {
		return resource.TimeWindow{}, false
	}
	w.To = to
	return w, true
}

func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// splitFrontmatter returns the text between the --- lines and everything after
// the closing line, unchanged.
func splitFrontmatter(text string) (fm, body string, err error) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", errors.New("missing frontmatter")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), nil
		}
	}
	return "", "", errors.New("unterminated frontmatter")
}

// titleOf returns the text of the first H1 in body.
func titleOf(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "# "))
		}
	}
	return ""
}

// SetStatus rewrites the status, stamps resolved: and writes resolution under
// ## Resolution. Empty resolution text leaves the section as it is. Only the
// two frontmatter lines change; every other byte is preserved.
func SetStatus(path, status, resolution string) error {
	const op = "investigation.set_status"
	if !validStatus[status] {
		return errs.New(errs.Config, op, "unknown status "+strconv.Quote(status))
	}
	resolution = strings.TrimSpace(resolution)
	return updateFile(path, op, func(data []byte) ([]byte, error) {
		fmText, body, err := splitFrontmatter(string(data))
		if err != nil {
			return nil, errs.New(errs.Parse, op, path+": "+err.Error())
		}
		lines := strings.Split(fmText, "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
		lines = setFrontmatterField(lines, "status", status)
		lines = setFrontmatterField(lines, "resolved", time.Now().Format(time.RFC3339))
		if resolution != "" {
			body = replaceSection(body, headingResolution, resolution)
		}
		return []byte("---\n" + strings.Join(lines, "\n") + "\n---\n" + body), nil
	})
}

// setFrontmatterField replaces a top-level scalar line, appending the key when
// it is absent. Comments and unknown keys are left untouched.
func setFrontmatterField(lines []string, key, value string) []string {
	line := key + ": " + value
	for i, ln := range lines {
		if strings.HasPrefix(ln, key+":") {
			lines[i] = line
			return lines
		}
	}
	return append(lines, line)
}

// List returns every investigation under root, newest first.
func List(root string) ([]Meta, error) {
	const op = "investigation.list"
	dir := filepath.Join(root, dirInvestigations)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.Wrap(errs.Internal, op, err)
	}
	var out []Meta
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".md") {
			continue
		}
		m, err := ReadMeta(filepath.Join(dir, de.Name()))
		if err != nil {
			// A file someone else owns or half-wrote is skipped, not fatal.
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Opened.Equal(out[j].Opened) {
			return out[i].Opened.After(out[j].Opened)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// maxWriteAttempts bounds how often a writer restarts when another writer
// renamed a new file over the one it had locked.
const maxWriteAttempts = 200

// updateFile applies fn to the file's content under an exclusive flock and a
// read-modify-write that lands as one rename, so readers never see a partial
// file and writers never lose each other's bullets.
func updateFile(path, op string, fn func([]byte) ([]byte, error)) error {
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		done, err := updateFileOnce(path, op, fn)
		if done || err != nil {
			return err
		}
	}
	return errs.New(errs.Internal, op, "file changed underneath the writer: "+path)
}

// updateFileOnce makes one attempt. It reports done=false, nil when the file
// was replaced between open and lock, in which case the caller retries.
func updateFileOnce(path, op string, fn func([]byte) ([]byte, error)) (done bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return true, errs.Wrap(errs.Internal, op, err)
	}
	defer f.Close()

	if err := lockFile(f); err != nil {
		return true, errs.Wrap(errs.Internal, op, err)
	}
	defer unlockFile(f)

	locked, err := f.Stat()
	if err != nil {
		return true, errs.Wrap(errs.Internal, op, err)
	}
	current, err := os.Stat(path)
	if err != nil {
		return true, errs.Wrap(errs.Internal, op, err)
	}
	if !os.SameFile(locked, current) {
		// Another writer renamed a new file over this one; its lock is on the
		// file we now have to read. Start again on the current inode.
		return false, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return true, errs.Wrap(errs.Internal, op, err)
	}
	out, err := fn(data)
	if err != nil {
		return true, err
	}
	if err := writeAtomic(path, out); err != nil {
		return true, errs.Wrap(errs.Internal, op, err)
	}
	return true, nil
}

// lockFile takes an exclusive advisory lock. It is released by unlockFile and
// automatically when the descriptor closes.
func lockFile(f *os.File) error {
	return syscallFlock(f, true)
}

func unlockFile(f *os.File) {
	_ = syscallFlock(f, false)
}

// syscallFlock takes or releases an exclusive advisory lock. flock(2) is
// available on macOS and Linux without cgo.
func syscallFlock(f *os.File, lock bool) error {
	how := syscall.LOCK_UN
	if lock {
		how = syscall.LOCK_EX
	}
	return syscall.Flock(int(f.Fd()), how)
}

// writeAtomic writes data to a temporary file beside path and renames it over
// path, so the destination is always either the old or the new content.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// replaceSection replaces the body under heading (up to the next H2) with text,
// leaving the rest of the document alone.
func replaceSection(text, heading, body string) string {
	lines := strings.Split(text, "\n")
	start, end, ok := sectionRange(lines, heading)
	if !ok {
		out := strings.TrimRight(text, "\n")
		if out != "" {
			out += "\n\n"
		}
		return out + heading + "\n" + body + "\n"
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	tail := lines[end:]
	out := make([]string, 0, len(lines)+4)
	out = append(out, lines[:start]...)
	out = append(out, strings.Split(body, "\n")...)
	out = append(out, tail...)
	return strings.Join(out, "\n")
}

// sectionRange returns the line range [start,end) of the body under heading.
func sectionRange(lines []string, heading string) (start, end int, ok bool) {
	hi := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == heading {
			hi = i
			break
		}
	}
	if hi < 0 {
		return 0, 0, false
	}
	end = len(lines)
	for i := hi + 1; i < len(lines); i++ {
		if isH2(lines[i]) {
			end = i
			break
		}
	}
	return hi + 1, end, true
}

// isH2 reports whether ln opens a level-two section.
func isH2(ln string) bool {
	if !strings.HasPrefix(ln, "## ") {
		return false
	}
	return strings.TrimSpace(ln) != ""
}

// insertUnder adds block as new lines at the end of heading's body. A missing
// heading is appended at the end of the document rather than failing, because
// the file may have been edited by hand.
func insertUnder(text, heading, block string) string {
	lines := strings.Split(text, "\n")
	start, end, ok := sectionRange(lines, heading)
	if !ok {
		out := strings.TrimRight(text, "\n")
		if out != "" {
			out += "\n\n"
		}
		return out + heading + "\n" + block + "\n"
	}

	// Insert after the last non-blank line of the body, keeping the blank
	// lines that separate the section from the next heading.
	at := start
	for i := end - 1; i >= start; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			at = i + 1
			break
		}
	}
	insert := strings.Split(block, "\n")
	out := make([]string, 0, len(lines)+len(insert))
	out = append(out, lines[:at]...)
	out = append(out, insert...)
	out = append(out, lines[at:]...)
	return strings.Join(out, "\n")
}

// AppendEntry inserts one bullet at the end of ## Timeline, under an exclusive
// lock and an atomic rename. Findings are mirrored under ## Findings.
func AppendEntry(path string, e Entry, red *redact.Redactor) error {
	const op = "investigation.append"
	bullet := FormatEntry(e, red)
	if strings.TrimSpace(bullet) == "" {
		return errs.New(errs.Internal, op, "empty entry")
	}
	return updateFile(path, op, func(data []byte) ([]byte, error) {
		out := insertUnder(string(data), headingTimeline, bullet)
		if e.Kind == EntryFinding {
			if text := strings.TrimSpace(redText(red, e.Text)); text != "" {
				out = insertUnder(out, headingFindings, "- "+text)
			}
		}
		return []byte(out), nil
	})
}
