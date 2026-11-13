package k8s

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/resource"
)

const (
	// SourceName is the inventory source name and, because the merge prefixes
	// every label with its source, the label namespace: "node" becomes
	// "k8s.node" in the merged inventory (BUILD-PLAN §4.7).
	SourceName = "k8s"
	// sourceTimeout bounds one node list. The inventory bounds a Load itself;
	// this keeps a single API call from eating that budget.
	sourceTimeout = 30 * time.Second
	// hostnameLabel is the node label kubectl also shows as the hostname.
	hostnameLabel = "kubernetes.io/hostname"
	// The allowlisted node labels, as the API server names them.
	zoneLabel         = "topology.kubernetes.io/zone"
	regionLabel       = "topology.kubernetes.io/region"
	instanceTypeLabel = "node.kubernetes.io/instance-type"
	archLabel         = "kubernetes.io/arch"
	nodeRolePrefix    = "node-role.kubernetes.io/"
)

// DefaultNodeLabelKeys is the allowlist of node labels the source copies into
// the inventory when SourceOptions.LabelKeys is empty
// (kubernetes.node_label_keys): zone, region, instance-type, the node-role
// keys and arch. An entry ending in "/" is a prefix — every label below it
// contributes "k8s.node-role.<role>". Any other entry is an exact label key and
// contributes "k8s.<last path segment>".
var DefaultNodeLabelKeys = []string{
	zoneLabel,
	regionLabel,
	instanceTypeLabel,
	nodeRolePrefix,
	archLabel,
}

// SourceOptions configures the k8s inventory source.
type SourceOptions struct {
	// Context is the kube context the nodes were listed from; it becomes the
	// k8s.context label, which is what tells two clusters' nodes apart.
	Context string
	// LabelKeys is the allowlist; nil uses DefaultNodeLabelKeys.
	LabelKeys []string
	// Now stamps SeenAt; nil uses the wall clock.
	Now func() time.Time
}

// NodeLister lists the cluster's nodes. *Backend implements it.
type NodeLister interface {
	Nodes(ctx context.Context) ([]corev1.Node, error)
}

// Source is the inventory source of a Kubernetes cluster: one host per node.
// Merging by alias joins a node with the SSH and Salt records of the same
// machine, so a node name that is a short name or a cloud instance ID still
// lines up (SPEC W3).
type Source struct {
	lister NodeLister
	opts   SourceOptions
}

var _ inventory.Source = (*Source)(nil)

// NewSource builds the source over a node lister, which is the backend itself
// when the cluster is the one the backend talks to.
func NewSource(l NodeLister, o SourceOptions) *Source {
	return &Source{lister: l, opts: o}
}

// Name is both the source name and its label namespace.
func (s *Source) Name() string { return SourceName }

// Load lists the nodes and turns them into hosts.
func (s *Source) Load(ctx context.Context) ([]inventory.Host, error) {
	if s == nil || s.lister == nil {
		return nil, errs.New(errs.Config, "k8s.inventory", "no cluster to list nodes from")
	}
	c, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()
	nodes, err := s.lister.Nodes(c)
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) {
			return nil, e
		}
		return nil, errs.Wrap(errs.Remote, "k8s.inventory", err)
	}
	return HostsFromNodes(nodes, s.opts, s.now()), nil
}

// now is the clock of the source.
func (s *Source) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

// NodeSource is the inventory source of this backend's cluster. The caller
// passes kubernetes.node_label_keys and the context name; internal/app builds
// it once the inventory exists.
func (b *Backend) NodeSource(o SourceOptions) *Source {
	return NewSource(b, o)
}

// Nodes lists the cluster's nodes. It is the NodeLister the inventory source
// reads, and the alert pivot and the host overview use it for one node.
func (b *Backend) Nodes(ctx context.Context) ([]corev1.Node, error) {
	ap, err := b.api()
	if err != nil {
		return nil, err
	}
	list, err := ap.typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, &errs.Error{Kind: Classify(err), Op: "k8s.nodes", Msg: oneLine(err.Error()), Err: err}
	}
	return list.Items, nil
}

// HostsFromNodes builds one host per node. The node name, every
// InternalIP/ExternalIP/Hostname address and the kubernetes.io/hostname label
// are aliases; the labels are k8s.node, k8s.context and the allowlist.
func HostsFromNodes(nodes []corev1.Node, o SourceOptions, now time.Time) []inventory.Host {
	keys := o.LabelKeys
	if len(keys) == 0 {
		keys = DefaultNodeLabelKeys
	}
	context := strings.TrimSpace(o.Context)
	hosts := make([]inventory.Host, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		name := strings.TrimSpace(node.Name)
		id := resource.NormalizeHostID(name)
		if id == "" {
			continue
		}
		h := inventory.Host{
			ID:      id,
			Aliases: nodeAliases(node),
			Labels:  nodeLabels(node, keys),
			SeenAt:  now,
		}
		h.Labels["node"] = name
		if context != "" {
			h.Labels["context"] = context
		}
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	return hosts
}

// nodeAliases is every name the node answers to: its own name, the
// kubernetes.io/hostname label and its InternalIP, ExternalIP and Hostname
// addresses.
func nodeAliases(node *corev1.Node) []string {
	out := make([]string, 0, 6)
	if name := strings.TrimSpace(node.Name); name != "" {
		out = append(out, name)
	}
	if hn := strings.TrimSpace(node.Labels[hostnameLabel]); hn != "" {
		out = append(out, hn)
	}
	for _, a := range node.Status.Addresses {
		switch a.Type {
		case corev1.NodeInternalIP, corev1.NodeExternalIP, corev1.NodeHostName:
			if addr := strings.TrimSpace(a.Address); addr != "" {
				out = append(out, addr)
			}
		}
	}
	return sortedUniqueStrings(out)
}

// nodeLabels copies the allowlisted labels of one node under their short names:
// the merge prefixes them with the source name, so "zone" reaches the inventory
// as "k8s.zone".
func nodeLabels(node *corev1.Node, allow []string) map[string]string {
	out := make(map[string]string, len(allow)+1)
	for _, entry := range allow {
		entry = strings.TrimSpace(entry)
		switch {
		case entry == "":
			continue
		case strings.HasSuffix(entry, "/"):
			base := prefixBase(entry)
			for k, v := range node.Labels {
				suffix, ok := strings.CutPrefix(k, entry)
				if !ok || suffix == "" {
					continue
				}
				// A prefixed label is a presence marker: it may carry no
				// value, which is exactly how the node-role keys work.
				out[base+"."+suffix] = v
			}
		default:
			if v := strings.TrimSpace(node.Labels[entry]); v != "" {
				out[shortLabelKey(entry)] = v
			}
		}
	}
	return out
}

// shortLabelKey is the last path segment of a label key: "topology.kubernetes.io/zone"
// becomes "zone".
func shortLabelKey(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[i+1:]
	}
	return key
}

// prefixBase is a label prefix without its trailing slash, shortened to its
// first dot-segment: "node-role.kubernetes.io/" becomes "node-role", so the
// allowlisted role labels reach the inventory as "k8s.node-role.<role>".
func prefixBase(entry string) string {
	base := strings.TrimSuffix(entry, "/")
	if i := strings.IndexByte(base, '.'); i > 0 {
		return base[:i]
	}
	return base
}

// sortedUniqueStrings returns the de-duplicated, sorted copy of in.
func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
