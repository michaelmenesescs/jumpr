package tui

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend/k8s"
	"jumper/internal/backend/prom"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/tui/keymap"
)

// Constants of the alert → investigation flow.
const (
	// alertLoadTimeout bounds the Get that reads the selected alert row.
	alertLoadTimeout = 10 * time.Second
	// AlertWindowLead is how far before startsAt the investigation window
	// begins: enough context to see the alert build up, not so much that the
	// first range query is expensive.
	AlertWindowLead = 30 * time.Minute
	// alertTitleLabels is the most main labels an investigation title carries.
	alertTitleLabels = 3
)

// alertResolvedMsg is one alert row read back, with the hosts and window its
// investigation will use. The resolution runs in the command; the root only
// applies it.
type alertResolvedMsg struct {
	Row    resource.Resource
	Title  string
	Alerts []string
	Hosts  []resource.HostID
	Reason string
	Window resource.TimeWindow
	Start  time.Time
}

// alertOpenKey takes `enter` on an alert row before the table's own open does,
// so an alert expands into an investigation instead of flashing "no drill-in".
// It returns nil for every other key, kind and situation (filter input, a
// non-table view), and the key then takes its normal route.
func (r *Root) alertOpenKey(k tea.KeyMsg) tea.Cmd {
	if r == nil || r.env == nil || r.mux == nil {
		return nil
	}
	keys := keymap.Default()
	if r.env.Keys != nil {
		keys = r.env.Keys
	}
	if !keys.Open.Matches(k.String()) {
		return nil
	}
	if _, c := r.mux.Focused(); c == nil {
		return nil
	}
	subj := r.focusedSubject()
	if subj.Ref.Kind != prom.KindAlert {
		return nil
	}
	return r.loadAlert(subj.Ref)
}

// loadAlert reads the alert row through its backend and resolves its labels to
// hosts, all inside the returned command.
func (r *Root) loadAlert(ref resource.Ref) tea.Cmd {
	if r.env == nil || r.env.Registry == nil {
		return nil
	}
	b, ok := r.env.Registry.Get(ref.Backend)
	if !ok {
		b, _, ok = r.env.Registry.Lookup(string(prom.KindAlert))
		if !ok {
			return Flash(FlashWarn, "no backend serves alerts")
		}
	}
	inv := r.env.Inventory
	now := time.Now
	if r.env.Now != nil {
		now = r.env.Now
	}
	at := now()

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), alertLoadTimeout)
		defer cancel()
		row, err := b.Get(ctx, ref)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "alert: " + err.Error()}
		}
		labels := alertLabels(row)
		hosts, reason := prom.ResolveAlertHosts(labels, inv, nil)
		start := alertStartsAt(row, at)
		window := resource.TimeWindow{
			From:  start.Add(-AlertWindowLead),
			To:    at,
			Label: "alert",
		}
		return alertResolvedMsg{
			Row:    row,
			Title:  alertTitle(labels),
			Alerts: alertNames(labels),
			Hosts:  hosts,
			Reason: reason,
			Window: window,
			Start:  start,
		}
	}
}

// openAlert applies a resolved alert: any registered opener gets the row first
// (the k8s pods view, once it exists), then an alert that names Kubernetes pods
// takes the pods path, then the built-in alert investigation runs.
func (r *Root) openAlert(m alertResolvedMsg) tea.Cmd {
	if r == nil || r.env == nil {
		return nil
	}
	for _, opener := range r.env.AlertOpeners {
		if opener == nil {
			continue
		}
		if cmd, ok := opener(m.Row); ok {
			return cmd
		}
	}
	// An alert whose labels name a namespace (and usually a pod) is a
	// Kubernetes alert: the pods view opens filtered to them and the
	// investigation targets the nodes those pods run on (task 111, SPEC W3).
	if q, ok := k8sAlertPods(m.Row); ok {
		if resolver, ok := r.alertNodeResolver(); ok {
			return r.openK8sAlert(m, q, resolver)
		}
	}
	return r.applyAlert(m)
}

// applyAlert is the built-in alert flow: the alert's window and hosts become the
// session's, the investigation opens and the host list is pushed.
func (r *Root) applyAlert(m alertResolvedMsg) tea.Cmd {
	if r == nil || r.env == nil {
		return nil
	}

	if r.env.Session != nil {
		r.env.Session.Window = m.Window
		r.env.Session.Targets = targets.Set{
			Label: alertTargetLabel(m),
			Hosts: append([]resource.HostID(nil), m.Hosts...),
		}
	}
	r.routeMux(WindowChangedMsg{Window: m.Window})
	r.routeMux(TargetsChangedMsg{Set: targets.Set{
		Label: alertTargetLabel(m),
		Hosts: append([]resource.HostID(nil), m.Hosts...),
	}})

	var cmds []tea.Cmd
	if inv, ok := invLog(r.env); ok {
		title, alerts, window, hosts := m.Title, m.Alerts, m.Window, m.Hosts
		cmds = append(cmds, func() tea.Msg {
			meta, err := inv.Open(title, window, hosts, alerts)
			if err != nil {
				return FlashMsg{Level: FlashError, Text: err.Error()}
			}
			return InvestigationChangedMsg{Meta: &meta}
		})
	}

	if len(m.Hosts) == 0 {
		reason := m.Reason
		if reason == "" {
			reason = "the alert resolves to no host"
		}
		cmds = append(cmds, Flash(FlashWarn, "%s", reason))
		return batchCmds(cmds)
	}

	hosts := append([]resource.HostID(nil), m.Hosts...)
	cmds = append(cmds, r.openQuery(OpenQueryMsg{Query: resource.Query{
		Kind:   "host",
		Hosts:  hosts,
		Window: m.Window,
	}}))
	cmds = append(cmds, Flash(FlashOK, "alert on %s", pluralHosts(len(hosts))))
	return batchCmds(cmds)
}

// k8sAlertPods is the pods view a Kubernetes alert opens: its namespace label
// and, when it has one, its pod label (task 111, SPEC W3).
func k8sAlertPods(row resource.Resource) (resource.Query, bool) {
	return k8s.AlertPodsQuery(alertLabels(row))
}

// alertNodeResolver is what the Kubernetes alert flow needs from the k8s
// backend: the inventory hosts of the nodes the alert's pods run on.
type alertNodeResolver interface {
	AlertNodes(ctx context.Context, q resource.Query) ([]resource.HostID, string, error)
}

// alertNodeResolver finds the registered Kubernetes backend.
func (r *Root) alertNodeResolver() (alertNodeResolver, bool) {
	if r == nil || r.env == nil || r.env.Registry == nil {
		return nil, false
	}
	b, ok := r.env.Registry.Get(k8sBackendName)
	if !ok || b == nil {
		return nil, false
	}
	resolver, ok := b.(alertNodeResolver)
	return resolver, ok
}

// openK8sAlert opens a Kubernetes alert: the pods view filtered to the alert's
// pods, the session targets and the investigation, both on the nodes those pods
// run on. The pods are read once for both (alertNodeSet).
func (r *Root) openK8sAlert(m alertResolvedMsg, q resource.Query, resolver alertNodeResolver) tea.Cmd {
	if r.env.Session != nil {
		r.env.Session.Window = m.Window
	}
	r.routeMux(WindowChangedMsg{Window: m.Window})

	nodes := &alertNodeSet{resolver: resolver, query: q}
	return batchCmds([]tea.Cmd{
		r.alertNodeTargetsCmd(m, nodes),
		r.alertNodeInvestigationCmd(m, nodes),
		r.openQuery(OpenQueryMsg{Query: q}),
	})
}

// alertNodeTargetsCmd resolves the alert's pods to their nodes and makes those
// hosts the session targets.
func (r *Root) alertNodeTargetsCmd(m alertResolvedMsg, nodes *alertNodeSet) tea.Cmd {
	label := alertTargetLabel(m)
	return func() tea.Msg {
		hosts, reason, err := nodes.resolve()
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "alert nodes: " + err.Error()}
		}
		if len(hosts) == 0 {
			if reason == "" {
				reason = "the alert resolves to no node"
			}
			return FlashMsg{Level: FlashWarn, Text: reason}
		}
		return TargetsChangedMsg{Set: targets.Set{
			Label: label,
			Hosts: append([]resource.HostID(nil), hosts...),
		}}
	}
}

// alertNodeInvestigationCmd opens the investigation on the nodes of the alert's
// pods. It opens even when no node resolves, as the built-in flow does: the
// record is the evidence that the alert was looked at.
func (r *Root) alertNodeInvestigationCmd(m alertResolvedMsg, nodes *alertNodeSet) tea.Cmd {
	inv, ok := invLog(r.env)
	if !ok {
		return nil
	}
	title, alerts, window := m.Title, m.Alerts, m.Window
	return func() tea.Msg {
		hosts, _, err := nodes.resolve()
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "alert nodes: " + err.Error()}
		}
		meta, err := inv.Open(title, window, hosts, alerts)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		return InvestigationChangedMsg{Meta: &meta}
	}
}

// alertNodeSet is the one node resolution an alert's two commands share: the
// session targets and the investigation must see the same hosts, and the pods
// and nodes are read once.
type alertNodeSet struct {
	once     sync.Once
	resolver alertNodeResolver
	query    resource.Query
	hosts    []resource.HostID
	reason   string
	err      error
}

// resolve reads the alert's pods and their nodes, once.
func (a *alertNodeSet) resolve() ([]resource.HostID, string, error) {
	a.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), alertLoadTimeout)
		defer cancel()
		a.hosts, a.reason, a.err = a.resolver.AlertNodes(ctx, a.query)
	})
	return a.hosts, a.reason, a.err
}

// alertTargetLabel is the label the session's target set carries.
func alertTargetLabel(m alertResolvedMsg) string {
	if name := alertNameOf(m); name != "" {
		return name
	}
	return "alert"
}

// alertNameOf is the alertname of a resolved alert.
func alertNameOf(m alertResolvedMsg) string {
	if len(m.Alerts) > 0 {
		return m.Alerts[0]
	}
	return strings.TrimSpace(m.Row.Str("alert"))
}

// batchCmds batches the commands that are not nil.
func batchCmds(cmds []tea.Cmd) tea.Cmd {
	out := cmds[:0]
	for _, c := range cmds {
		if c != nil {
			out = append(out, c)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return tea.Batch(out...)
}

// pluralHosts renders a host count for a flash.
func pluralHosts(n int) string {
	if n == 1 {
		return "1 host"
	}
	return strconv.Itoa(n) + " hosts"
}

// alertLabels is the label set of an alert row: the typed payload when the
// backend kept one, else the row's labels.
func alertLabels(row resource.Resource) map[string]string {
	if raw, ok := row.Raw.(prom.AMAlert); ok && len(raw.Labels) > 0 {
		return raw.Labels
	}
	return row.Labels
}

// alertStartsAt is when the alert started firing.
func alertStartsAt(row resource.Resource, now time.Time) time.Time {
	if raw, ok := row.Raw.(prom.AMAlert); ok && !raw.StartsAt.IsZero() {
		return raw.StartsAt
	}
	if t, ok := row.Fields["start"].(time.Time); ok && !t.IsZero() {
		return t
	}
	return now
}

// alertNames is Meta.Alerts: the alert name, plus the fingerprint when the row
// has one, so the record can be matched against the alert later.
func alertNames(labels map[string]string) []string {
	name := strings.TrimSpace(labels["alertname"])
	if name == "" {
		return nil
	}
	return []string{name}
}

// alertTitle is the investigation title: the alert name and its main labels,
// e.g. "HighErrorRate dc=ewr service=api". severity and the host label keys are
// left out: they describe presentation or are already the target set.
func alertTitle(labels map[string]string) string {
	name := strings.TrimSpace(labels["alertname"])
	if name == "" {
		name = "alert"
	}
	skip := map[string]bool{"alertname": true, "severity": true}
	for _, k := range prom.DefaultHostLabelKeys {
		skip[k] = true
	}
	keys := make([]string, 0, len(labels))
	for k, v := range labels {
		if skip[k] || strings.TrimSpace(v) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > alertTitleLabels {
		keys = keys[:alertTitleLabels]
	}
	parts := make([]string, 0, len(keys)+1)
	parts = append(parts, name)
	for _, k := range keys {
		parts = append(parts, k+"="+strings.TrimSpace(labels[k]))
	}
	return strings.Join(parts, " ")
}
