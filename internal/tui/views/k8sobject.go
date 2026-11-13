package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend/k8s"
	"jumper/internal/errs"
	"jumper/internal/investigation"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// KindK8sObject is the factory key of the describe/yaml/events screen. It is a
// view kind, not a backend kind: the k8s backend serves pod, deployment, …
const KindK8sObject resource.KindName = "k8s-object"

const (
	// k8sObjectTimeout bounds one describe/yaml/events read.
	k8sObjectTimeout = 30 * time.Second
	// k8sRevealKey reveals a secret's values, like the pillar screen's key.
	// It is upper case because `r` refreshes.
	k8sRevealKey = "R"
	// Tab indexes of the object screen.
	k8sTabDescribe = 0
	k8sTabYAML     = 1
	k8sTabEvents   = 2
)

// K8sObjectBackend is what the object screen needs from the Kubernetes
// backend. The k8s package implements it; tests use a fake.
type K8sObjectBackend interface {
	// ReadObject reads one object's describe text, YAML and events.
	// reveal keeps a secret's values in the YAML.
	ReadObject(ctx context.Context, ref resource.Ref, reveal bool) (k8s.ObjectDetail, error)
}

// k8sObjectMsg is one read, tagged with Seq so a superseded read is dropped.
type k8sObjectMsg struct {
	Seq    int
	Detail k8s.ObjectDetail
	Err    error
}

// K8sObject is `d`/`y` on one Kubernetes object (SPEC §8.2): the describe
// equivalent, the YAML and the events as three tabs, refreshed with `r`. A
// secret's values are hidden until `R` confirms a reveal, and even then copy
// mode stays on the redacted YAML: a revealed value is shown, never recorded
// and never copied (BUILD-PLAN §7).
type K8sObject struct {
	env   *tui.Env
	b     K8sObjectBackend
	ref   resource.Ref
	tabs  *Tabs
	keys  *keymap.Map
	theme *theme.Theme

	// base is the pane's context; every read derives from it.
	base context.Context

	seq     int
	cancel  context.CancelFunc
	detail  k8s.ObjectDetail
	loading bool
	loadErr error

	w, h int
}

var _ tui.View = (*K8sObject)(nil)

// NewK8sObject builds the object screen of one object.
func NewK8sObject(env *tui.Env, b K8sObjectBackend, ref resource.Ref) *K8sObject {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	th := theme.Dark()
	if env != nil && env.Theme != nil {
		th = env.Theme
	}
	v := &K8sObject{env: env, b: b, ref: ref, keys: keys, theme: th}
	crumb := k8sObjectCrumb(ref)
	loading := []string{"reading " + crumb + "..."}
	v.tabs = NewTabs(env, []Tab{
		{Key: "1", Title: "describe", View: NewText(env, crumb+" · describe", loading)},
		{Key: "2", Title: "yaml", View: NewText(env, crumb+" · yaml", loading)},
		{Key: "3", Title: "events", View: NewText(env, crumb+" · events", loading)},
	})
	return v
}

// Init starts the first read.
func (v *K8sObject) Init() tea.Cmd { return v.load(false) }

// SetContext makes every read derive from ctx, so closing the pane stops it.
func (v *K8sObject) SetContext(ctx context.Context) {
	if ctx != nil {
		v.base = ctx
	}
}

// Close cancels a read in flight.
func (v *K8sObject) Close() error {
	v.stop()
	return nil
}

// now is the clock the environment handed in.
func (v *K8sObject) now() time.Time {
	if v.env != nil && v.env.Now != nil {
		return v.env.Now()
	}
	return time.Now()
}

// baseCtx is the context every read derives from.
func (v *K8sObject) baseCtx() context.Context {
	if v.base != nil {
		return v.base
	}
	return context.Background()
}

// readOnly reports the session-level read-only switch.
func (v *K8sObject) readOnly() bool {
	return v.env != nil && v.env.Session != nil && v.env.Session.ReadOnly
}

// stop cancels a read in flight.
func (v *K8sObject) stop() {
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
}

// load abandons a read in flight and asks the backend for the object.
func (v *K8sObject) load(reveal bool) tea.Cmd {
	v.stop()
	v.seq++
	seq := v.seq
	v.loading = true
	v.loadErr = nil
	if v.b == nil {
		v.loading = false
		v.loadErr = errs.New(errs.Config, "views.k8sobject", "no Kubernetes backend configured")
		v.paintError()
		return nil
	}
	if strings.TrimSpace(v.ref.Name) == "" {
		v.loading = false
		v.loadErr = errs.New(errs.Config, "views.k8sobject", "the object needs a name")
		v.paintError()
		return nil
	}
	b, ref := v.b, v.ref
	ctx, cancel := context.WithCancel(v.baseCtx())
	v.cancel = cancel
	return func() tea.Msg {
		cctx, ccancel := context.WithTimeout(ctx, k8sObjectTimeout)
		defer ccancel()
		detail, err := b.ReadObject(cctx, ref, reveal)
		return k8sObjectMsg{Seq: seq, Detail: detail, Err: err}
	}
}

// Update folds reads and routes keys. `r` refreshes and `R` reveals a secret,
// before the tab strip sees the key.
func (v *K8sObject) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case k8sObjectMsg:
		if m.Seq != v.seq {
			return v, nil
		}
		return v, v.apply(m)
	case tui.RefreshRequestMsg:
		return v, v.load(v.detail.Revealed)
	case tea.KeyMsg:
		// While a tab owns the keyboard (the text search) every key is the
		// tab's; only outside it do `r` and `R` mean refresh and reveal.
		if !v.tabs.Capturing() {
			key := m.String()
			switch {
			case v.keys.Refresh.Matches(key):
				return v, v.load(v.detail.Revealed)
			case key == k8sRevealKey:
				return v, v.reveal()
			}
		}
		view, cmd := v.tabs.Update(msg)
		if next, ok := view.(*Tabs); ok {
			v.tabs = next
		}
		return v, cmd
	}
	return v, nil
}

// apply keeps a read, or the reason there is none, and repaints the tabs.
func (v *K8sObject) apply(m k8sObjectMsg) tea.Cmd {
	v.loading = false
	if m.Err != nil {
		v.loadErr = m.Err
		v.detail = k8s.ObjectDetail{Ref: v.ref}
		v.paintError()
		return tui.Flash(tui.FlashError, "%s: %v", k8sObjectCrumb(v.ref), m.Err)
	}
	wasRevealed := v.detail.Revealed
	v.detail = m.Detail
	v.loadErr = nil
	v.paint()
	if v.detail.Revealed && !wasRevealed {
		v.recordReveal()
		return tui.Flash(tui.FlashWarn, "secret values revealed: shown on screen only, never recorded or copied")
	}
	return nil
}

// paint fills the three tabs from the read.
func (v *K8sObject) paint() {
	crumb := k8sObjectCrumb(v.ref)
	describe := v.redact(splitLines(v.detail.Describe))
	yamlLines := splitLines(v.detail.YAML)
	if !v.detail.Revealed {
		yamlLines = v.redact(yamlLines)
	}
	events := k8s.EventLines(v.detail.Events)
	if v.detail.EventErr != nil {
		events = []string{"events unavailable: " + v.detail.EventErr.Error()}
	}
	events = v.redact(events)
	v.tabs.SetView(k8sTabDescribe, NewText(v.env, crumb+" · describe", describe))
	v.tabs.SetView(k8sTabYAML, NewText(v.env, crumb+" · yaml", yamlLines))
	v.tabs.SetView(k8sTabEvents, NewText(v.env, crumb+" · events", events))
}

// paintError shows the reason there is no detail on every tab.
func (v *K8sObject) paintError() {
	crumb := k8sObjectCrumb(v.ref)
	line := "unavailable: " + v.loadErr.Error()
	lines := v.redact([]string{line})
	v.tabs.SetView(k8sTabDescribe, NewText(v.env, crumb+" · describe", lines))
	v.tabs.SetView(k8sTabYAML, NewText(v.env, crumb+" · yaml", lines))
	v.tabs.SetView(k8sTabEvents, NewText(v.env, crumb+" · events", lines))
}

// redact passes lines through the session redactor (BUILD-PLAN §7).
func (v *K8sObject) redact(lines []string) []string {
	if v.env == nil || v.env.Redact == nil {
		return lines
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = v.env.Redact.String(line)
	}
	return out
}

// reveal asks for confirmation and then reads the secret with its values.
func (v *K8sObject) reveal() tea.Cmd {
	if !strings.EqualFold(string(v.ref.Kind), "secret") {
		return tui.Flash(tui.FlashInfo, "%s has no values to reveal", k8sObjectCrumb(v.ref))
	}
	if v.detail.Revealed {
		return tui.Flash(tui.FlashInfo, "the values of %s are already revealed", k8sObjectCrumb(v.ref))
	}
	if v.readOnly() {
		return tui.Flash(tui.FlashWarn, "read-only mode: secret values cannot be revealed")
	}
	if v.loading {
		return tui.Flash(tui.FlashInfo, "%s is still loading", k8sObjectCrumb(v.ref))
	}
	yes := v.load(true)
	text := fmt.Sprintf("reveal the values of %s? they will be shown on screen", k8sObjectCrumb(v.ref)) +
		"\njumper never records or copies them; copy mode keeps the size markers"
	th := v.theme
	return func() tea.Msg {
		return tui.OpenDialogMsg{Dialog: tui.NewConfirmDialog(th, "reveal secret values", text, yes)}
	}
}

// recordReveal appends the timeline entry of a reveal: the object, never a
// value.
func (v *K8sObject) recordReveal() {
	if v.env == nil || v.env.Recorder == nil {
		return
	}
	rec := v.env.Recorder
	if _, open := rec.Current(); !open {
		return
	}
	text := "secret values revealed: " + k8sObjectName(v.ref)
	if v.env.Redact != nil {
		text = v.env.Redact.String(text)
	}
	_ = rec.Append(investigation.Entry{
		Time:  v.now(),
		Kind:  investigation.EntryView,
		Actor: action.Human,
		Text:  text,
	})
}

// Render is the tab strip and the active tab.
func (v *K8sObject) Render(w, h int) string {
	v.w, v.h = w, h
	return v.tabs.Render(w, h)
}

// Crumb is the active tab's breadcrumb.
func (v *K8sObject) Crumb() string { return v.tabs.Crumb() }

// Hints are the keys the screen answers.
func (v *K8sObject) Hints() []tui.KeyHint {
	hints := v.tabs.Hints()
	hints = append(hints,
		tui.KeyHint{Key: firstKey(v.keys.Refresh), Text: "refresh"},
		tui.KeyHint{Key: k8sRevealKey, Text: "reveal"},
	)
	return hints
}

// Subject is the object.
func (v *K8sObject) Subject() tui.Subject {
	return tui.Subject{Kind: v.ref.Kind, Ref: v.ref}
}

// Capturing is true while a tab owns the keyboard (the text search).
func (v *K8sObject) Capturing() bool { return v.tabs.Capturing() }

// CommandLine is the `:` command line that reopens this screen.
func (v *K8sObject) CommandLine() string {
	return fmt.Sprintf("k8s-object --arg kind=%s --arg namespace=%s --arg name=%s",
		v.ref.Kind, v.ref.Namespace, v.ref.Name)
}

// CopyLines is the plain text of the active tab. While a secret's values are
// revealed the yaml tab reports the redacted YAML instead: copy mode never
// carries a secret.
func (v *K8sObject) CopyLines() []string {
	if v.tabs.Active() == k8sTabYAML && v.detail.SecretRevealed() {
		return v.redact(splitLines(v.detail.SafeYAML))
	}
	return v.tabs.CopyLines()
}

// k8sObjectCrumb is "pod prod/web-1" for a namespaced object, "node node-a"
// otherwise.
func k8sObjectCrumb(ref resource.Ref) string {
	return fmt.Sprintf("%s %s", ref.Kind, k8sObjectName(ref))
}

// k8sObjectName is "namespace/name", or the name for a cluster-scoped object.
func k8sObjectName(ref resource.Ref) string {
	if ref.Namespace != "" {
		return ref.Namespace + "/" + ref.Name
	}
	return ref.Name
}

// splitLines is the line list of a block of text, without the trailing empty
// line.
func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
