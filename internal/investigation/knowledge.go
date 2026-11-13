package investigation

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"jumper/internal/errs"
)

// The knowledge base is the half of the record that outlives one incident: the
// notes under <root>/knowledge and the closed investigations under
// <root>/investigations. BuildIndex reads both into one searchable set and
// Match answers "have I seen this before?" for the host, unit or alert a view
// is about (SPEC §10).
//
// Nothing here writes except CreateNote and RecordRootCause, which only ever
// add to the knowledge tree. Matching is read-only and safe for concurrent use:
// the panel matches on the UI goroutine's commands while appends land on
// another.

const (
	// The note kinds `:kb new <kind> <name>` accepts.
	NoteKindHost    = "host"
	NoteKindService = "service"
	NoteKindAlert   = "alert"
	NoteKindRunbook = "runbook"

	// dirKnowledge is the tree of curated notes under the root.
	dirKnowledge = "knowledge"

	// Score weights, strongest first (BUILD-PLAN §4.10, task 060).
	scoreAlert   = 100
	scoreUnit    = 80
	scoreHost    = 60
	scoreTag     = 30
	scoreText    = 10
	scoreTextMax = 30

	// maxHits caps what Match returns: a panel, not a search engine.
	maxHits = 20
	// snippetMax is the longest snippet, in runes.
	snippetMax = 160
	// tokenMax caps how many body tokens score, so a note quoting a whole log
	// cannot outrank one about the thing itself.
	tokenMax = 3
)

// Subject is what a match is asked about. The TUI converts its own Subject
// into this one so the index never imports the UI.
type Subject struct {
	Hosts []string
	Unit  string
	Alert string
	Tags  []string
}

// Hit is one note that matched, with the strongest reason it did.
type Hit struct {
	Path          string
	Title         string
	Why           string
	Snippet       string
	Score         int
	Investigation bool
	Status        string // investigation status; empty for a knowledge note
}

// Index is the searchable set of notes. Build it with BuildIndex; every method
// is safe for concurrent use and a nil *Index matches nothing.
type Index struct {
	root string

	mu   sync.RWMutex
	docs map[string]*noteDoc
}

// noteFront is the frontmatter of a note. Every key is optional; a note with
// none is still indexed.
type noteFront struct {
	Hosts    []string `yaml:"hosts"`
	Services []string `yaml:"services"`
	Alerts   []string `yaml:"alerts"`
	Tags     []string `yaml:"tags"`
	Status   string   `yaml:"status"`
}

// noteDoc is one indexed file.
type noteDoc struct {
	path          string
	rel           string
	title         string
	snippet       string
	status        string
	investigation bool
	hosts         []string
	services      []string
	alerts        []string
	tags          []string
	tokens        map[string]bool
	mod           time.Time
}

// BuildIndex walks <root>/knowledge/**.md and <root>/investigations/*.md. A
// missing root is an empty index, not an error: the knowledge base is
// optional and jumper has to start without one. Files that cannot be read or
// parsed are skipped; one bad note never breaks the index.
func BuildIndex(root string) (*Index, error) {
	const op = "investigation.build_index"
	if strings.TrimSpace(root) == "" {
		return nil, errs.New(errs.Config, op, "no knowledge root configured")
	}
	ix := &Index{root: root, docs: make(map[string]*noteDoc)}

	kroot := filepath.Join(root, dirKnowledge)
	err := filepath.WalkDir(kroot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			// An unreadable directory or file is skipped, like a bad note.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != kroot && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isNoteFile(d.Name()) {
			return nil
		}
		if doc, derr := readNote(root, path, false); derr == nil {
			ix.docs[doc.path] = doc
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errs.Wrap(errs.Internal, op, err)
	}

	idir := filepath.Join(root, dirInvestigations)
	entries, err := os.ReadDir(idir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ix, nil
		}
		return nil, errs.Wrap(errs.Internal, op, err)
	}
	for _, de := range entries {
		if de.IsDir() || !isNoteFile(de.Name()) {
			continue
		}
		path := filepath.Join(idir, de.Name())
		if doc, derr := readNote(root, path, true); derr == nil {
			ix.docs[doc.path] = doc
		}
	}
	return ix, nil
}

// isNoteFile reports whether name is a Markdown file jumper indexes.
func isNoteFile(name string) bool {
	return strings.HasSuffix(name, ".md") && !strings.HasPrefix(name, ".")
}

// Len is how many documents the index holds.
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.docs)
}

// readNote parses one file. Only an I/O failure is an error: a broken
// frontmatter block costs the keys, never the body.
func readNote(root, path string, isInvestigation bool) (*noteDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fm, body := splitNote(string(data))
	d := &noteDoc{
		path:          filepath.Clean(path),
		rel:           relTo(root, path),
		title:         titleOf(body),
		snippet:       snippetOf(body),
		status:        strings.ToLower(strings.TrimSpace(fm.Status)),
		investigation: isInvestigation,
		hosts:         lowerAll(fm.Hosts),
		services:      lowerAll(fm.Services),
		alerts:        lowerAll(fm.Alerts),
		tags:          lowerAll(fm.Tags),
		tokens:        tokenSet(body),
	}
	if d.title == "" {
		d.title = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if isInvestigation && d.status == "" {
		d.status = "open"
	}
	if info, serr := os.Stat(path); serr == nil {
		d.mod = info.ModTime()
	}
	d.addImplicitKeys(root)
	return d, nil
}

// splitNote returns the frontmatter and the body. A file without frontmatter,
// or with one that does not parse, is body only.
func splitNote(text string) (noteFront, string) {
	fmText, body, err := splitFrontmatter(text)
	if err != nil {
		return noteFront{}, text
	}
	var fm noteFront
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return noteFront{}, body
	}
	return fm, body
}

// addImplicitKeys applies the path conventions: a note filed under
// knowledge/hosts, knowledge/services or knowledge/alerts is about that host,
// service or alert whatever its frontmatter says.
func (d *noteDoc) addImplicitKeys(root string) {
	rel, err := filepath.Rel(root, d.path)
	if err != nil {
		return
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] != dirKnowledge {
		return
	}
	name := strings.ToLower(strings.TrimSuffix(parts[2], ".md"))
	if name == "" {
		return
	}
	switch parts[1] {
	case "hosts":
		d.hosts = appendOnce(d.hosts, name)
	case "services":
		d.services = appendOnce(d.services, name)
	case "alerts":
		d.alerts = appendOnce(d.alerts, name)
	}
}

// appendOnce adds v to list unless it is already there.
func appendOnce(list []string, v string) []string {
	for _, have := range list {
		if have == v {
			return list
		}
	}
	return append(list, v)
}

// Update re-reads one file, replacing what the index knew about it; a file that
// is gone is dropped. A path outside the root is a configuration mistake.
func (ix *Index) Update(path string) error {
	const op = "investigation.update_index"
	if ix == nil {
		return nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errs.New(errs.Config, op, "update needs a path")
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(ix.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errs.New(errs.Config, op, "path outside the knowledge root: "+path)
	}
	isInv := strings.HasPrefix(filepath.ToSlash(rel), dirInvestigations+"/")

	doc, err := readNote(ix.root, path, isInv)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			delete(ix.docs, path)
			return nil
		}
		return errs.Wrap(errs.Internal, op, err)
	}
	ix.docs[doc.path] = doc
	return nil
}

// Match returns the notes about s, strongest first, at most 20.
func (ix *Index) Match(s Subject) []Hit {
	if ix == nil {
		return nil
	}
	subj := newSubject(s)
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	hits := make([]Hit, 0, len(ix.docs))
	for _, d := range ix.docs {
		score, why := d.score(subj)
		if score <= 0 {
			continue
		}
		hits = append(hits, Hit{
			Path:          d.path,
			Title:         d.title,
			Why:           why,
			Snippet:       d.snippet,
			Score:         score,
			Investigation: d.investigation,
			Status:        d.status,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		mi, mj := ix.modOf(hits[i].Path), ix.modOf(hits[j].Path)
		if !mi.Equal(mj) {
			return mi.After(mj)
		}
		return hits[i].Path < hits[j].Path
	})
	if len(hits) > maxHits {
		hits = hits[:maxHits]
	}
	return hits
}

// modOf is the modification time of an indexed file; Match holds the read lock.
func (ix *Index) modOf(path string) time.Time {
	if d := ix.docs[path]; d != nil {
		return d.mod
	}
	return time.Time{}
}

// subject is a Subject with its fields lower-cased and de-duplicated once, so
// matching does not repeat the work per document.
type subject struct {
	hosts  []string
	unit   string
	alert  string
	tags   []string
	tokens []string
}

// newSubject normalizes s.
func newSubject(s Subject) subject {
	out := subject{
		unit:  strings.ToLower(strings.TrimSpace(s.Unit)),
		alert: strings.ToLower(strings.TrimSpace(s.Alert)),
	}
	for _, h := range s.Hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			out.hosts = appendOnce(out.hosts, h)
		}
	}
	for _, t := range s.Tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			out.tags = appendOnce(out.tags, t)
		}
	}
	out.tokens = subjectTokens(out)
	return out
}

// subjectTokens is the set of whole words a body may contain to count as a
// text match. Hosts contribute their full and short form; a unit or alert also
// contributes its bare name, so `nginx.service` still finds a note that says
// nginx.
func subjectTokens(s subject) []string {
	var out []string
	for _, h := range s.hosts {
		out = appendOnce(out, h)
		out = appendOnce(out, shortHost(h))
	}
	if s.unit != "" {
		out = appendOnce(out, s.unit)
		out = appendOnce(out, strings.TrimSuffix(s.unit, ".service"))
	}
	if s.alert != "" {
		out = appendOnce(out, s.alert)
		out = appendOnce(out, strings.TrimSuffix(s.alert, ".service"))
	}
	for _, t := range s.tags {
		out = appendOnce(out, t)
	}
	return out
}

// score is the total weight of d for s and the strongest single reason.
func (d *noteDoc) score(s subject) (int, string) {
	score, best, why := 0, 0, ""
	stronger := func(weight int, reason string) {
		if weight > best {
			best, why = weight, reason
		}
	}

	if s.alert != "" && hasFold(d.alerts, s.alert) {
		score += scoreAlert
		stronger(scoreAlert, "alert name")
	}
	if s.unit != "" && serviceMatches(d.services, s.unit) {
		score += scoreUnit
		stronger(scoreUnit, "unit "+s.unit)
	}
	if hostMatches(d.hosts, s.hosts) {
		score += scoreHost
		stronger(scoreHost, "host")
	}
	if tag, ok := tagMatch(d.tags, s.tags); ok {
		score += scoreTag
		stronger(scoreTag, "tag "+tag)
	}
	if n := d.textMatches(s.tokens); n > 0 {
		add := min(n, tokenMax) * scoreText
		score += min(add, scoreTextMax)
		stronger(scoreText, "text")
	}
	return score, why
}

// textMatches counts the subject tokens the body contains.
func (d *noteDoc) textMatches(tokens []string) int {
	n := 0
	for _, t := range tokens {
		if d.tokens[t] {
			n++
		}
	}
	return n
}

// hasFold reports whether list contains v, both already lower-cased.
func hasFold(list []string, v string) bool {
	for _, have := range list {
		if have == v {
			return true
		}
	}
	return false
}

// serviceMatches reports whether any service of d is the unit s, with or
// without the `.service` suffix: unit nginx.service matches service nginx.
func serviceMatches(services []string, unit string) bool {
	base := strings.TrimSuffix(unit, ".service")
	for _, svc := range services {
		if svc == unit || svc == base || strings.TrimSuffix(svc, ".service") == base {
			return true
		}
	}
	return false
}

// hostMatches reports whether any subject host is a host of d, by FQDN or by
// short name.
func hostMatches(hosts, subjects []string) bool {
	for _, sh := range subjects {
		for _, dh := range hosts {
			if sh == dh || shortHost(sh) == shortHost(dh) {
				return true
			}
		}
	}
	return false
}

// shortHost is the first label of a host name.
func shortHost(h string) string {
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[:i]
	}
	return h
}

// tagMatch returns the first subject tag d carries.
func tagMatch(docTags, subjectTags []string) (string, bool) {
	for _, st := range subjectTags {
		if hasFold(docTags, st) {
			return st, true
		}
	}
	return "", false
}

// tokenSplit splits text on everything that is not part of a word. Dots, @,
// slashes, dashes and underscores stay inside a token so `nginx.service` and
// `web-ewr-007` survive whole.
var tokenSplit = regexp.MustCompile(`[^a-z0-9._@/-]+`)

// tokenSet is the lower-cased token set of text. A token carrying a dot also
// contributes its base name: `nginx.service` adds `nginx`.
func tokenSet(text string) map[string]bool {
	out := make(map[string]bool)
	for _, raw := range tokenSplit.Split(strings.ToLower(text), -1) {
		raw = strings.Trim(raw, "._-/@")
		if raw == "" {
			continue
		}
		out[raw] = true
		if i := strings.IndexAny(raw, "._@"); i > 1 {
			out[raw[:i]] = true
		}
	}
	return out
}

// snippetOf is the first non-empty paragraph of body, flattened to one line
// and cut at snippetMax runes.
func snippetOf(body string) string {
	var b strings.Builder
	started := false
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			if started {
				break
			}
			continue
		}
		if !started && strings.HasPrefix(t, "#") {
			continue
		}
		started = true
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(t)
	}
	return truncRunes(strings.TrimSpace(b.String()), snippetMax)
}

// truncRunes cuts s at n runes without splitting one.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n]))
}

// lowerAll lower-cases and trims every entry, dropping the empty ones.
func lowerAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "" {
			out = appendOnce(out, v)
		}
	}
	return out
}

// relTo renders path relative to root, or path itself when they share nothing.
func relTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

// noteDir is the knowledge subdirectory and frontmatter key of a note kind.
func noteDir(kind string) (dir, field string, ok bool) {
	switch kind {
	case NoteKindHost:
		return "hosts", "hosts", true
	case NoteKindService:
		return "services", "services", true
	case NoteKindAlert:
		return "alerts", "alerts", true
	case NoteKindRunbook:
		return "runbooks", "tags", true
	}
	return "", "", false
}

// noteName matches the names a note may have: anything that is one path
// element, so nothing can escape the knowledge tree.
var noteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)

// NotePath is where the note of kind and name lives under root.
func NotePath(root, kind, name string) (string, error) {
	const op = "investigation.note_path"
	dir, _, ok := noteDir(kind)
	if !ok {
		return "", errs.New(errs.Config, op, "unknown note type "+kind)
	}
	name = strings.TrimSpace(name)
	if !noteName.MatchString(name) {
		return "", errs.New(errs.Config, op, "invalid note name "+name)
	}
	return filepath.Join(root, dirKnowledge, dir, name+".md"), nil
}

// CreateNote writes the template for a note of kind and name and returns its
// path. An existing note is left untouched and reported as created=false: `:kb
// new` never overwrites work.
func CreateNote(root, kind, name string) (path string, created bool, err error) {
	const op = "investigation.create_note"
	path, err = NotePath(root, kind, name)
	if err != nil {
		return "", false, err
	}
	if _, serr := os.Stat(path); serr == nil {
		return path, false, nil
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return "", false, errs.Wrap(errs.Internal, op, serr)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, errs.Wrap(errs.Internal, op, err)
	}
	_, field, _ := noteDir(kind)
	if err := writeAtomic(path, []byte(noteTemplate(kind, name, field))); err != nil {
		return "", false, errs.Wrap(errs.Internal, op, err)
	}
	return path, true, nil
}

// noteTemplate is the starter file of a note: frontmatter naming what it is
// about, the title, and the two sections `:inv resolve` and the user's editor
// write into.
func noteTemplate(kind, name, field string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	value := flowList([]string{key})
	if field == "tags" {
		value = flowList([]string{"runbook"})
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(field + ": " + value + "\n")
	b.WriteString("---\n")
	b.WriteString("# " + strings.TrimSpace(name) + "\n")
	b.WriteString("\n")
	b.WriteString("## Notes\n")
	b.WriteString("\n")
	b.WriteString("## Root causes\n")
	return b.String()
}

// headingRootCauses is where RecordRootCause files a line.
const headingRootCauses = "## Root causes"

// RecordRootCause appends text as a bullet under ## Root causes in the note of
// kind and name, creating the note first when it does not exist. It returns the
// note's path. Callers redact text before storing it (AGENTS.md rule 12).
func RecordRootCause(root, kind, name, text string) (string, error) {
	const op = "investigation.record_root_cause"
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errs.New(errs.Config, op, "an empty root cause has nothing to record")
	}
	path, _, err := CreateNote(root, kind, name)
	if err != nil {
		return "", err
	}
	bullet := "- " + strings.Join(strings.Fields(text), " ")
	err = updateFile(path, op, func(data []byte) ([]byte, error) {
		return []byte(insertUnder(string(data), headingRootCauses, bullet)), nil
	})
	if err != nil {
		return "", err
	}
	return path, nil
}
