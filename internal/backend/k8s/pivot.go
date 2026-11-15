package k8s

import (
	"context"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"jumpr/internal/errs"
	"jumpr/internal/inventory"
	"jumpr/internal/resource"
)

// The kinds and keys the node ↔ host pivot works with (SPEC W3).
const (
	kindPod  = "pod"
	kindNode = "node"
	kindHost = "host"

	// Key of the node's host overview on a pod or node row.
	hostKey = "h"
	// Key of "pods on this node" on a host row that is a node.
	nodePodsKey = "P"
	// Key of the node itself on a host row that is a node.
	nodeKey = "K"
	// Key of the node's units and journal on a pod or node row.
	unitsKey   = "u"
	journalKey = "j"

	// Labels the inventory source sets, after the merge's source prefix.
	nodeLabel = "k8s.node"
)

// pivotState is the installed pivot configuration. It is guarded by Backend.mu
// because internal/app sets it after the backend is registered.
type pivotState struct {
	inv  inventory.Inventory
	byIP bool
}

// PivotOptions configures the node ↔ host pivot (SPEC W3).
type PivotOptions struct {
	// Inventory is the merged inventory: a node resolves to the host the SSH
	// and Salt records know by the same name or address. Nil leaves every node
	// unmatched.
	Inventory inventory.Inventory
	// SSHByIP is kubernetes.node_ssh_by_ip: a node with no inventory match
	// still opens its InternalIP as a host, which is what a cluster whose
	// nodes are not in ssh_config wants.
	SSHByIP bool
}

// SetPivot installs the pivot configuration. The zero value leaves every node
// unmatched, so a caller that never calls this gets no pivot instead of a
// wrong one.
func (b *Backend) SetPivot(o PivotOptions) {
	b.mu.Lock()
	b.pivot = pivotState{inv: o.Inventory, byIP: o.SSHByIP}
	b.mu.Unlock()
}

// pivotConfig returns the installed configuration.
func (b *Backend) pivotConfig() pivotState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pivot
}

// links returns the pivots of a pod or a node row: the host overview of the
// node it runs on, the units on that host and its journal.
func (b *Backend) links(r resource.Resource) []resource.Link {
	name, internalIP, ok := nodeOf(r)
	if !ok {
		return nil
	}
	host, found := b.nodeHost(name, internalIP)
	if !found {
		// The key must exist so the table can explain why it does not open
		// anything (RefuseLink); the query stays empty.
		return []resource.Link{{Label: "host of node " + name, Key: hostKey}}
	}
	return []resource.Link{
		{
			Label: "host overview of node " + name,
			Key:   hostKey,
			To:    resource.Query{Kind: kindHost, Hosts: []resource.HostID{host}},
		},
		{
			Label: "units on this node",
			Key:   unitsKey,
			To:    resource.Query{Kind: "unit", Hosts: []resource.HostID{host}},
		},
		{
			Label: "journal",
			Key:   journalKey,
			To:    resource.Query{Kind: "journal", Hosts: []resource.HostID{host}},
		},
	}
}

// RefuseLink explains a pivot that cannot be taken: a node the inventory does
// not know, on a cluster without kubernetes.node_ssh_by_ip, is flashed instead
// of opening a host view that would fail to dial.
func (b *Backend) RefuseLink(r resource.Resource, key string) string {
	if key != hostKey {
		return ""
	}
	name, internalIP, ok := nodeOf(r)
	if !ok {
		return ""
	}
	if _, found := b.nodeHost(name, internalIP); found {
		return ""
	}
	return "node " + name + " is not in the inventory"
}

// extraLinks returns the pivots a host row has when it is a Kubernetes node:
// the pods on that node and the node itself. It is pure — the node name comes
// from the row's k8s.node label, which the inventory source sets, so the Linux
// backend can append these without importing this package.
func (b *Backend) extraLinks(r resource.Resource) []resource.Link {
	if !isHostRow(r) {
		return nil
	}
	node := strings.TrimSpace(r.Labels[nodeLabel])
	if node == "" {
		node = strings.TrimSpace(r.Labels["node"])
	}
	if node == "" {
		return nil
	}
	return []resource.Link{
		{
			Label: "pods on this node",
			Key:   nodePodsKey,
			To:    resource.Query{Kind: kindPod, Args: map[string]string{"node": node}},
		},
		{
			Label: "node " + node,
			Key:   nodeKey,
			To:    resource.Query{Kind: kindNode, Args: map[string]string{"name": node}},
		},
	}
}

// isHostRow reports whether r is a host row of any backend.
func isHostRow(r resource.Resource) bool {
	return strings.EqualFold(string(r.Ref.Kind), kindHost)
}

// nodeOf returns the node name and, when the row has one, the node's InternalIP.
func nodeOf(r resource.Resource) (name, internalIP string, ok bool) {
	switch strings.ToLower(string(r.Ref.Kind)) {
	case kindPod:
		name, _ = r.Fields["node"].(string)
	case kindNode:
		name = string(r.Ref.Name)
		if name == "" {
			name, _ = r.Fields["name"].(string)
		}
		internalIP, _ = r.Fields["internal_ip"].(string)
	default:
		return "", "", false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false
	}
	return name, strings.TrimSpace(internalIP), true
}

// nodeHost resolves a node to the inventory host jumpr can reach over SSH: the
// host the node's name belongs to, then the host its InternalIP belongs to, and
// only when kubernetes.node_ssh_by_ip allows it the bare InternalIP itself.
func (b *Backend) nodeHost(name, internalIP string) (resource.HostID, bool) {
	p := b.pivotConfig()
	name = strings.TrimSpace(name)
	internalIP = strings.TrimSpace(internalIP)
	if p.inv != nil && name != "" {
		if h, ok := p.inv.Resolve(name); ok {
			return h.ID, true
		}
	}
	if !p.byIP || internalIP == "" {
		return "", false
	}
	if p.inv != nil {
		if h, ok := p.inv.Resolve(internalIP); ok {
			return h.ID, true
		}
	}
	return resource.NormalizeHostID(internalIP), true
}

// AlertPodsQuery is the pods view an alert opens: the pods of the namespace its
// labels name and, when they name one, that pod. namespace (and usually pod)
// are the labels Kubernetes alerting rules carry.
func AlertPodsQuery(labels map[string]string) (resource.Query, bool) {
	ns := strings.TrimSpace(labels["namespace"])
	if ns == "" {
		return resource.Query{}, false
	}
	args := map[string]string{"namespace": ns}
	if pod := strings.TrimSpace(labels["pod"]); pod != "" {
		args["name"] = pod
	}
	return resource.Query{Kind: kindPod, Args: args}, true
}

// AlertNodes lists the pods q selects and returns the inventory hosts of their
// nodes, sorted. reason says why the list is empty when it is: the alert's pods
// are gone, or a node is not in the inventory.
func (b *Backend) AlertNodes(ctx context.Context, q resource.Query) ([]resource.HostID, string, error) {
	pods, err := b.pods(ctx, q)
	if err != nil {
		return nil, "", err
	}
	if len(pods) == 0 {
		return nil, "the alert's pods are not running", nil
	}
	names := make([]string, 0, len(pods))
	seen := make(map[string]bool, len(pods))
	for _, pod := range pods {
		node := strings.TrimSpace(pod.Spec.NodeName)
		if node == "" || seen[node] {
			continue
		}
		seen[node] = true
		names = append(names, node)
	}
	sort.Strings(names)

	ips := b.nodeIPs(ctx, names)
	var hosts []resource.HostID
	var missing []string
	for _, name := range names {
		if id, ok := b.nodeHost(name, ips[name]); ok {
			hosts = append(hosts, id)
			continue
		}
		missing = append(missing, name)
	}
	if len(hosts) == 0 {
		if len(missing) > 0 {
			return nil, "node " + missing[0] + " is not in the inventory", nil
		}
		return nil, "the alert's pods are not assigned to a node", nil
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i] < hosts[j] })
	return hosts, "", nil
}

// pods lists the pods of q. It reads the API server directly instead of an
// informer: an alert must open before any cache has synced, and the list is one
// namespace wide.
func (b *Backend) pods(ctx context.Context, q resource.Query) ([]corev1.Pod, error) {
	ap, err := b.api()
	if err != nil {
		return nil, err
	}
	ns := strings.TrimSpace(q.Args["namespace"])
	if strings.EqualFold(ns, "all") {
		ns = ""
	}
	label, err := serverSelector(q.Selector)
	if err != nil {
		return nil, err
	}
	list, err := ap.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: label})
	if err != nil {
		return nil, &errs.Error{Kind: Classify(err), Op: "k8s.pods", Msg: oneLine(err.Error()), Err: err}
	}
	name := strings.TrimSpace(q.Args["name"])
	node := strings.TrimSpace(q.Args["node"])
	out := make([]corev1.Pod, 0, len(list.Items))
	for _, pod := range list.Items {
		if name != "" && pod.Name != name {
			continue
		}
		if node != "" && pod.Spec.NodeName != node {
			continue
		}
		out = append(out, pod)
	}
	return out, nil
}

// nodeIPs returns the InternalIP of the named nodes, for the node_ssh_by_ip
// fallback. A node that cannot be read is simply absent.
func (b *Backend) nodeIPs(ctx context.Context, names []string) map[string]string {
	out := make(map[string]string, len(names))
	if len(names) == 0 {
		return out
	}
	nodes, err := b.Nodes(ctx)
	if err != nil {
		return out
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	for i := range nodes {
		n := &nodes[i]
		if !want[n.Name] {
			continue
		}
		if ip := nodeInternalIP(n); ip != "" {
			out[n.Name] = ip
		}
	}
	return out
}

// nodeInternalIP is the first InternalIP address of a node.
func nodeInternalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP && strings.TrimSpace(a.Address) != "" {
			return strings.TrimSpace(a.Address)
		}
	}
	return ""
}

// NodeStatus is the KUBERNETES line of a host overview: the node's ready state
// and the pods it runs.
type NodeStatus struct {
	Name         string
	Status       string // Ready | NotReady | Unknown
	Ready        bool
	Pods         int
	NotReadyPods int
}

// NodeStatusOf returns the node behind an inventory host of this cluster. ok is
// false for a host that is not a node. It is what the host overview's
// KUBERNETES line reads.
func (b *Backend) NodeStatusOf(ctx context.Context, host resource.HostID) (NodeStatus, bool, error) {
	ap, err := b.api()
	if err != nil {
		return NodeStatus{}, false, err
	}
	nodes, err := b.Nodes(ctx)
	if err != nil {
		return NodeStatus{}, false, err
	}
	node := matchNode(nodes, b.hostNames(host))
	if node == nil {
		return NodeStatus{}, false, nil
	}
	status, colour := nodeStatus(node)
	out := NodeStatus{Name: node.Name, Status: status, Ready: colour == resource.StatusOK}

	list, err := ap.typed.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, true, &errs.Error{Kind: Classify(err), Op: "k8s.pods", Msg: oneLine(err.Error()), Err: err}
	}
	for i := range list.Items {
		pod := &list.Items[i]
		if pod.Spec.NodeName != node.Name {
			continue
		}
		out.Pods++
		if !podIsReady(pod) {
			out.NotReadyPods++
		}
	}
	return out, true, nil
}

// hostNames is every name an inventory host answers to: its ID, its aliases and
// the node its own labels name.
func (b *Backend) hostNames(host resource.HostID) map[resource.HostID]bool {
	out := map[resource.HostID]bool{host: true}
	p := b.pivotConfig()
	if p.inv == nil {
		return out
	}
	h, ok := p.inv.Resolve(string(host))
	if !ok {
		return out
	}
	out[h.ID] = true
	for _, a := range h.Aliases {
		out[resource.NormalizeHostID(a)] = true
	}
	if node := strings.TrimSpace(h.Labels[nodeLabel]); node != "" {
		out[resource.NormalizeHostID(node)] = true
	}
	return out
}

// matchNode finds the node a host is: by name, by hostname label or by any of
// its addresses.
func matchNode(nodes []corev1.Node, names map[resource.HostID]bool) *corev1.Node {
	want := func(name string) bool {
		return names[resource.NormalizeHostID(name)]
	}
	for i := range nodes {
		n := &nodes[i]
		if want(n.Name) || want(n.Labels[hostnameLabel]) {
			return n
		}
		for _, a := range n.Status.Addresses {
			if want(a.Address) {
				return n
			}
		}
	}
	return nil
}

// podIsReady reports whether every container of the pod is ready. A pod that
// finished successfully is not counted as failing.
func podIsReady(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return true
	case corev1.PodFailed:
		return false
	}
	if len(pod.Status.ContainerStatuses) == 0 {
		return pod.Status.Phase == corev1.PodRunning
	}
	for _, c := range pod.Status.ContainerStatuses {
		if !c.Ready {
			return false
		}
	}
	return true
}
