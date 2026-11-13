package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend/k8s"
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

// RegisterK8sCommands registers `:ctx` and `:ns` on c. It is called by
// internal/app when it builds the command registry.
func RegisterK8sCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	if err := c.Register(ctxCommand()); err != nil {
		return err
	}
	return c.Register(nsCommand())
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
