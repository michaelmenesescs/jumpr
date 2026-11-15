package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/k8s"
	"jumpr/internal/investigation"
	"jumpr/internal/render"
)

// The `:ctx` and `:ns` command family (SPEC §8.2): the context switcher and the
// session namespace. Neither command blocks the UI: the context list and the
// switch run in a tea.Cmd with a deadline, and `:ns` only validates a name.
const (
	// k8sBackendName is the registration name of the Kubernetes backend.
	k8sBackendName = "k8s"
	// k8sContextTimeout bounds one `:ctx` listing or switch.
	k8sContextTimeout = 10 * time.Second
	// k8sHint is what the commands say when no k8s backend is registered.
	k8sHint = "kubernetes is not available: no k8s backend is registered"
	// k8sPortForwardTimeout bounds resolving a `:pf` target and dialing the API
	// server. The forward itself lives until `:pf stop`.
	k8sPortForwardTimeout = 15 * time.Second
	// pfKindLabel is the kind a `:pf` row shows, like a table's descendant.
	pfKindLabel = "portforward"
)

// K8sOps is what the `:ctx` and `:ns` commands need from the Kubernetes
// backend. The k8s package implements it; tests use a fake.
type K8sOps interface {
	// Contexts lists the kubeconfig's contexts, the current one marked.
	Contexts() ([]k8s.Context, error)
	// CurrentContext is the selected context.
	CurrentContext() string
	// SwitchContext selects a context and rebuilds the client. It reports
	// whether the context is protected, which makes the session read-only.
	SwitchContext(ctx context.Context, name string) (bool, error)
	// Namespace is the session namespace; "" means every namespace.
	Namespace() string
	// SetNamespace sets it: a name, or "all" for every namespace.
	SetNamespace(namespace string) error
}

// K8sForwards is what `:pf` needs from the Kubernetes backend: a port-forward
// that the session owns (SPEC §8.2). The k8s package implements it.
type K8sForwards interface {
	// PortForward starts a forward to ns/name; "" means the session
	// namespace. It returns once the listener is up.
	PortForward(ctx context.Context, ns, name string, kind k8s.ForwardKind, local, remote int) (*k8s.Forward, error)
}

// RegisterK8sCommands registers `:ctx`, `:ns` and `:pf` on c. It is called by
// internal/app when it builds the command registry. The port-forward set lives
// with the registry, which lives with the session server, so a forward survives
// a detach.
func RegisterK8sCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	if err := c.Register(ctxCommand()); err != nil {
		return err
	}
	if err := c.Register(nsCommand()); err != nil {
		return err
	}
	return c.Register(pfCommand(k8s.NewForwardSet()))
}

// k8sOpsOf returns the Kubernetes backend as the interface the commands use.
func k8sOpsOf(env *Env) (K8sOps, bool) {
	if env == nil || env.Registry == nil {
		return nil, false
	}
	b, ok := env.Registry.Get(k8sBackendName)
	if !ok || b == nil {
		return nil, false
	}
	ops, ok := b.(K8sOps)
	return ops, ok
}

// ctxCommand is `:ctx`: with no argument it lists the kubeconfig's contexts
// with the current one marked; with a name it switches.
func ctxCommand() Command {
	return Command{
		Name:    "ctx",
		Aliases: []string{"context"},
		Usage:   ":ctx [name]",
		Help:    "list the Kubernetes contexts or switch to one",
		Run: func(env *Env, args string) tea.Cmd {
			ops, ok := k8sOpsOf(env)
			if !ok {
				return Flash(FlashError, "%s", k8sHint)
			}
			name := strings.TrimSpace(args)
			if name == "" {
				return ctxListCmd(env, ops)
			}
			return ctxSwitchCmd(ops, name)
		},
	}
}

// nsCommand is `:ns <name>|all`: the namespace every k8s list is scoped to.
func nsCommand() Command {
	return Command{
		Name:    "ns",
		Aliases: []string{"namespace"},
		Usage:   ":ns <name>|all",
		Help:    "set the session Kubernetes namespace",
		Run: func(env *Env, args string) tea.Cmd {
			ops, ok := k8sOpsOf(env)
			if !ok {
				return Flash(FlashError, "%s", k8sHint)
			}
			name := strings.TrimSpace(args)
			if name == "" {
				return Flash(FlashInfo, "namespace: %s", namespaceLabel(ops.Namespace()))
			}
			if err := ops.SetNamespace(name); err != nil {
				return Flash(FlashError, "namespace %s: %v", name, err)
			}
			return Flash(FlashOK, "namespace: %s", namespaceLabel(ops.Namespace()))
		},
	}
}

// ctxListCmd reads the contexts and opens them as a text page.
func ctxListCmd(env *Env, ops K8sOps) tea.Cmd {
	return func() tea.Msg {
		ctxs, err := ops.Contexts()
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "contexts: " + err.Error()}
		}
		return PushViewMsg{View: newTextPage(env, "contexts", contextLines(ops.Namespace(), ctxs))}
	}
}

// ctxSwitchCmd switches context in the background. A protected context makes
// the session read-only, which is a message for the root (only the root mutates
// SessionState); the root's own flash then says so.
func ctxSwitchCmd(ops K8sOps, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), k8sContextTimeout)
		defer cancel()
		protected, err := ops.SwitchContext(ctx, name)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "context " + name + ": " + err.Error()}
		}
		if protected {
			return readOnlyChangedMsg{On: true}
		}
		text := "context " + name
		if ns := ops.Namespace(); ns != "" {
			text += ", namespace " + ns
		}
		return FlashMsg{Level: FlashOK, Text: text}
	}
}

// contextLines renders `:ctx`: the session namespace and one line per context,
// the current one starred and a protected one marked (SPEC §16).
func contextLines(namespace string, ctxs []k8s.Context) []string {
	lines := []string{
		kvLine("namespace", namespaceLabel(namespace)),
		"",
	}
	if len(ctxs) == 0 {
		return append(lines, "the kubeconfig has no contexts")
	}
	nameW, clusterW, userW := len("CONTEXT"), len("CLUSTER"), len("USER")
	for _, c := range ctxs {
		nameW = max(nameW, len(c.Name))
		clusterW = max(clusterW, len(c.Cluster))
		userW = max(userW, len(c.User))
	}
	lines = append(lines, "  "+padRight("CONTEXT", nameW)+"  "+
		padRight("CLUSTER", clusterW)+"  "+padRight("USER", userW)+"  NAMESPACE")
	for _, c := range ctxs {
		mark := " "
		if c.Current {
			mark = "*"
		}
		line := mark + " " + padRight(c.Name, nameW) + "  " +
			padRight(c.Cluster, clusterW) + "  " + padRight(c.User, userW) + "  " +
			namespaceLabel(c.Namespace)
		if c.Protected {
			line += "  (protected)"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "",
		"switch with :ctx <name>; a protected context makes the session read-only")
	return lines
}

// namespaceLabel renders a namespace for a message: "all" when every namespace
// is listed.
func namespaceLabel(namespace string) string {
	if strings.TrimSpace(namespace) == "" {
		return "all"
	}
	return namespace
}

// k8sForwardsOf returns the Kubernetes backend as the interface `:pf` uses.
func k8sForwardsOf(env *Env) (K8sForwards, bool) {
	if env == nil || env.Registry == nil {
		return nil, false
	}
	b, ok := env.Registry.Get(k8sBackendName)
	if !ok || b == nil {
		return nil, false
	}
	fw, ok := b.(K8sForwards)
	return fw, ok
}

// pfCommand is `:pf`: with no argument it lists the session's port-forwards,
// `:pf <local>:<remote> <target>` starts one and `:pf stop <local>` stops one.
// A forward belongs to the session: it keeps running when the client detaches.
func pfCommand(forwards *k8s.ForwardSet) Command {
	return Command{
		Name:    "pf",
		Aliases: []string{"portforward", "forward"},
		Usage:   ":pf [<local>:<remote> [pod|svc/]<name>] | :pf stop <local>",
		Help:    "list, start or stop a port-forward to a pod or service",
		Run: func(env *Env, args string) tea.Cmd {
			fw, ok := k8sForwardsOf(env)
			if !ok {
				return Flash(FlashError, "%s", k8sHint)
			}
			fields := strings.Fields(args)
			if len(fields) == 0 {
				return pfListCmd(env, forwards)
			}
			if strings.EqualFold(fields[0], "stop") {
				if len(fields) != 2 {
					return Flash(FlashWarn, "usage: :pf stop <local>")
				}
				local, err := strconv.Atoi(fields[1])
				if err != nil || local <= 0 {
					return Flash(FlashWarn, "%q is not a local port", fields[1])
				}
				return pfStopCmd(forwards, local)
			}
			local, remote, err := parsePortPair(fields[0])
			if err != nil {
				return Flash(FlashWarn, "%v", err)
			}
			if len(fields) < 2 {
				return Flash(FlashWarn, ":pf needs a pod or service: :pf %d:%d pod/<name>", local, remote)
			}
			kind, name := parseForwardTarget(fields[1])
			return pfStartCmd(env, fw, forwards, local, remote, kind, name)
		},
		Complete: func(_ *Env, args string) []string {
			if strings.HasPrefix(strings.TrimSpace(args), "stop ") {
				var out []string
				for _, f := range forwards.List() {
					out = append(out, "stop "+strconv.Itoa(f.Local()))
				}
				return out
			}
			return nil
		},
	}
}

// parsePortPair splits "<local>:<remote>". A local port of 0 asks the kernel for
// an ephemeral one.
func parsePortPair(s string) (int, int, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%q is not <local>:<remote>", s)
	}
	local, err := strconv.Atoi(parts[0])
	if err != nil || local < 0 || local > 65535 {
		return 0, 0, fmt.Errorf("%q is not a local port", parts[0])
	}
	remote, err := strconv.Atoi(parts[1])
	if err != nil || remote <= 0 || remote > 65535 {
		return 0, 0, fmt.Errorf("%q is not a remote port", parts[1])
	}
	return local, remote, nil
}

// parseForwardTarget splits "pod/web-1", "svc/api" or a bare "web-1". A bare
// name is resolved by the backend: a pod first, then a service.
func parseForwardTarget(s string) (k8s.ForwardKind, string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i > 0 {
		return k8s.ForwardKind(s[:i]).Normalize(), s[i+1:]
	}
	return "", s
}

// pfStartCmd starts the forward in the background: resolving a service and
// dialing the API server must never block the UI. The timeout bounds only that
// start; the forward itself lives until `:pf stop`.
func pfStartCmd(env *Env, fw K8sForwards, forwards *k8s.ForwardSet, local, remote int, kind k8s.ForwardKind, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), k8sPortForwardTimeout)
		defer cancel()
		f, err := fw.PortForward(ctx, "", name, kind, local, remote)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "port-forward: " + err.Error()}
		}
		if err := forwards.Add(f); err != nil {
			_ = f.Stop()
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		// A forward is an escape hatch worth recording, exactly like a shell:
		// an `input` entry in the open investigation, or the audit file.
		if env != nil {
			if cmd := env.AppendInvestigation(investigation.EntryInput, pfEntryText(f), ""); cmd != nil {
				_ = cmd()
			}
		}
		return FlashMsg{Level: FlashOK, Text: fmt.Sprintf("port-forward %s → %s:%d (local only)",
			f.Addr(), f.Target(), f.Remote())}
	}
}

// pfStopCmd stops one forward.
func pfStopCmd(forwards *k8s.ForwardSet, local int) tea.Cmd {
	return func() tea.Msg {
		if err := forwards.Stop(local); err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		return FlashMsg{Level: FlashOK, Text: fmt.Sprintf("port-forward on %d stopped", local)}
	}
}

// pfListCmd opens the session's port-forwards as a text page.
func pfListCmd(env *Env, forwards *k8s.ForwardSet) tea.Cmd {
	return func() tea.Msg {
		return PushViewMsg{View: newTextPage(env, "port forwards", pfLines(env, forwards))}
	}
}

// pfEntryText is what the timeline records when a forward starts.
func pfEntryText(f *k8s.Forward) string {
	target := f.Name()
	if f.Kind() == k8s.ForwardService && f.Target() != f.Name() {
		target += " (pod " + f.Target() + ")"
	}
	return fmt.Sprintf("port-forward %s on %s:%d", target, f.Addr(), f.Remote())
}

// pfLines renders `:pf`: one row per forward with its local address, target,
// connection and byte counts, and age.
func pfLines(env *Env, forwards *k8s.ForwardSet) []string {
	now := time.Now
	if env != nil && env.Now != nil {
		now = env.Now
	}
	list := forwards.List()
	if len(list) == 0 {
		return []string{
			"no port-forwards",
			"",
			"start one with :pf <local>:<remote> <pod|svc/name>",
		}
	}
	addrW, targetW := len("LOCAL"), len("TARGET")
	for _, f := range list {
		addrW = max(addrW, len(f.Addr()))
		targetW = max(targetW, len(pfTargetLabel(f)))
	}
	lines := []string{
		"  " + padRight("LOCAL", addrW) + "  " + padRight("TARGET", targetW) +
			"  " + padRight("KIND", len(pfKindLabel)) + "  CONNS  BYTES  AGE",
	}
	for _, f := range list {
		lines = append(lines, "  "+padRight(f.Addr(), addrW)+"  "+
			padRight(pfTargetLabel(f), targetW)+"  "+
			padRight(pfKindLabel, len(pfKindLabel))+"  "+
			padRight(render.Int(f.Conns()), len("CONNS"))+"  "+
			padRight(render.Bytes(f.Bytes()), len("BYTES"))+"  "+
			render.Age(f.Since(), now()))
	}
	lines = append(lines, "",
		"stop one with :pf stop <local>; a forward belongs to the session and survives detach")
	return lines
}

// pfTargetLabel is the ns/name:port a forward points at.
func pfTargetLabel(f *k8s.Forward) string {
	return f.Namespace() + "/" + f.Name() + ":" + strconv.Itoa(f.Remote())
}

// K8sContextBadge is the Kubernetes context as the status line shows it:
// "ctx:<name>", or "" when no k8s backend is registered or it has no context.
// The root's statusData puts it in StatusData.Context, next to the rollout
// badge.
func K8sContextBadge(env *Env) string {
	ops, ok := k8sOpsOf(env)
	if !ok {
		return ""
	}
	name := ops.CurrentContext()
	if name == "" {
		return ""
	}
	return fmt.Sprintf("ctx:%s", name)
}
