package k8s

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"jumper/internal/inventory"
	"jumper/internal/resource"
)

// pivotInventory is a merged inventory holding one host that is also a node.
func pivotInventory(t *testing.T) inventory.Inventory {
	t.Helper()
	src := testSource{name: "static", hosts: []inventory.Host{{
		ID:      "k3s-1.lab.example.internal",
		Addr:    "k3s-1.lab.example.internal",
		Aliases: []string{"k3s-1", "10.0.0.11"},
		Labels:  map[string]string{"host": "k3s-1.lab.example.internal", "k8s.node": "k3s-1"},
		SeenAt:  sourceNow,
	}}}
	store := inventory.New([]inventory.Source{src}, nil, "", func() time.Time { return sourceNow })
	if failed := store.Refresh(context.Background()); len(failed) != 0 {
		t.Fatalf("Refresh failed: %v", failed)
	}
	return store
}

// testPodRow is a pod row on node.
func testPodRow(namespace, name, node string) resource.Resource {
	return resource.Resource{
		Ref: resource.Ref{Backend: backendName, Kind: kindPod, Namespace: namespace, Name: name},
		Fields: map[string]any{
			"namespace": namespace,
			"name":      name,
			"node":      node,
		},
		Labels: map[string]string{"app": "api"},
	}
}

// testNodeRow is a node row with an InternalIP.
func testNodeRow(name, internalIP string) resource.Resource {
	return resource.Resource{
		Ref:    resource.Ref{Backend: backendName, Kind: kindNode, Name: name},
		Fields: map[string]any{"name": name, "internal_ip": internalIP},
	}
}

// linkByKey indexes links by their key binding.
func linkByKey(links []resource.Link) map[string]resource.Link {
	out := make(map[string]resource.Link, len(links))
	for _, l := range links {
		if l.Key != "" {
			out[l.Key] = l
		}
	}
	return out
}

// TestPivotPodOpensNodeHost is requirement 2: a pod row opens the host overview
// of the node it runs on, the units on that host and its journal.
func TestPivotPodOpensNodeHost(t *testing.T) {
	b, _ := testBackend(t, "prod")
	b.SetPivot(PivotOptions{Inventory: pivotInventory(t)})

	links := linkByKey(b.Links(testPodRow("prod", "api-7d9f", "k3s-1")))
	host, ok := links[hostKey]
	if !ok {
		t.Fatalf("pod row has no %q link: %+v", hostKey, links)
	}
	if host.To.Kind != kindHost || len(host.To.Hosts) != 1 || host.To.Hosts[0] != "k3s-1.lab.example.internal" {
		t.Fatalf("host link = %+v, want the inventory host of node k3s-1", host)
	}
	units, ok := links[unitsKey]
	if !ok || units.To.Kind != "unit" || units.To.Hosts[0] != "k3s-1.lab.example.internal" {
		t.Fatalf("units link = %+v, want the units of the node's host", units)
	}
	journal, ok := links[journalKey]
	if !ok || journal.To.Kind != "journal" || journal.To.Hosts[0] != "k3s-1.lab.example.internal" {
		t.Fatalf("journal link = %+v, want the journal of the node's host", journal)
	}
	if got := b.RefuseLink(testPodRow("prod", "api-7d9f", "k3s-1"), hostKey); got != "" {
		t.Fatalf("RefuseLink = %q, want no refusal for a node in the inventory", got)
	}
}

// TestPivotNodeNotInInventory: a node the inventory does not know has the `h`
// key but no query, and pressing it flashes instead of opening a dead host.
func TestPivotNodeNotInInventory(t *testing.T) {
	b, _ := testBackend(t, "prod")
	b.SetPivot(PivotOptions{Inventory: pivotInventory(t)})

	row := testNodeRow("k3s-9", "10.0.0.99")
	links := linkByKey(b.Links(row))
	host, ok := links[hostKey]
	if !ok {
		t.Fatalf("node row has no %q link: %+v", hostKey, links)
	}
	if host.To.Kind != "" || len(host.To.Hosts) != 0 {
		t.Fatalf("host link = %+v, want an empty query for an unmatched node", host)
	}
	if got, want := b.RefuseLink(row, hostKey), "node k3s-9 is not in the inventory"; got != want {
		t.Fatalf("RefuseLink = %q, want %q", got, want)
	}

	// A key that is not the host pivot is never refused: the units and journal
	// of an unmatched node simply are not offered.
	if got := b.RefuseLink(row, unitsKey); got != "" {
		t.Fatalf("RefuseLink(units) = %q, want none", got)
	}
}

// TestPivotNodeSSHByIP: with kubernetes.node_ssh_by_ip the unmatched node opens
// its InternalIP, which is what a cluster outside ssh_config needs.
func TestPivotNodeSSHByIP(t *testing.T) {
	b, _ := testBackend(t, "prod")
	b.SetPivot(PivotOptions{Inventory: pivotInventory(t), SSHByIP: true})

	row := testNodeRow("k3s-9", "10.0.0.99")
	host, ok := linkByKey(b.Links(row))[hostKey]
	if !ok {
		t.Fatalf("node row has no %q link", hostKey)
	}
	if len(host.To.Hosts) != 1 || host.To.Hosts[0] != "10.0.0.99" {
		t.Fatalf("host link = %+v, want the InternalIP as the host", host)
	}
	if got := b.RefuseLink(row, hostKey); got != "" {
		t.Fatalf("RefuseLink = %q, want none when node_ssh_by_ip is on", got)
	}

	// A node whose InternalIP the inventory knows opens that host instead.
	known, ok := linkByKey(b.Links(testNodeRow("k3s-1", "10.0.0.11")))[hostKey]
	if !ok || known.To.Hosts[0] != "k3s-1.lab.example.internal" {
		t.Fatalf("host link = %+v, want the inventory host, not the bare IP", known)
	}
}

// TestPivotHostRowIsNode is the other half of requirement 2: a host row that is
// a node offers the pods on that node (P) and the node itself (K).
func TestPivotHostRowIsNode(t *testing.T) {
	b, _ := testBackend(t, "prod")
	b.SetPivot(PivotOptions{Inventory: pivotInventory(t)})

	row := resource.Resource{
		Ref:    resource.Ref{Backend: "linux", Kind: kindHost, Host: "k3s-1.lab.example.internal"},
		Labels: map[string]string{"host": "k3s-1.lab.example.internal", "k8s.node": "k3s-1"},
	}
	links := linkByKey(b.ExtraLinks(row))
	pods, ok := links[nodePodsKey]
	if !ok {
		t.Fatalf("host row of a node has no %q link: %+v", nodePodsKey, links)
	}
	if pods.To.Kind != kindPod || pods.To.Args["node"] != "k3s-1" {
		t.Fatalf("pods link = %+v, want the pods on node k3s-1", pods)
	}
	node, ok := links[nodeKey]
	if !ok || node.To.Kind != kindNode || node.To.Args["name"] != "k3s-1" {
		t.Fatalf("node link = %+v, want the node k3s-1", node)
	}

	// A host that is not a node gets nothing extra.
	plain := resource.Resource{
		Ref:    resource.Ref{Backend: "linux", Kind: kindHost, Host: "web-01"},
		Labels: map[string]string{"host": "web-01"},
	}
	if got := b.ExtraLinks(plain); len(got) != 0 {
		t.Fatalf("non-node host row got pivots: %+v", got)
	}
	// Neither does a row of another kind.
	if got := b.ExtraLinks(testPodRow("prod", "api-7d9f", "k3s-1")); len(got) != 0 {
		t.Fatalf("pod row got host pivots: %+v", got)
	}
}

// TestPivotAlertPodsQuery: an alert with a namespace label opens the pods of
// that namespace, narrowed to the pod when it names one (requirement 4).
func TestPivotAlertPodsQuery(t *testing.T) {
	q, ok := AlertPodsQuery(map[string]string{"alertname": "PodCrashLooping", "namespace": "prod", "pod": "api-7d9f"})
	if !ok {
		t.Fatal("AlertPodsQuery found no pods for an alert with a namespace")
	}
	if q.Kind != kindPod || q.Args["namespace"] != "prod" || q.Args["name"] != "api-7d9f" {
		t.Fatalf("query = %+v, want the prod pods named api-7d9f", q)
	}
	only := map[string]string{"namespace": "prod"}
	q, ok = AlertPodsQuery(only)
	if !ok || q.Args["name"] != "" {
		t.Fatalf("query without a pod = %+v, want the whole namespace", q)
	}
	if _, ok := AlertPodsQuery(map[string]string{"alertname": "HighErrorRate"}); ok {
		t.Fatal("AlertPodsQuery found pods for an alert without a namespace")
	}
}

// TestPivotAlertNodes: the investigation targets of a k8s alert are the
// inventory hosts of its pods' nodes.
func TestPivotAlertNodes(t *testing.T) {
	ready := testPod("prod", "api-7d9f", "k3s-1", nil)
	other := testPod("prod", "api-7d9g", "k3s-1", nil)
	gone := testPod("prod", "web-1", "k3s-2", nil)
	objs := []runtime.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k3s-1"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{addr(corev1.NodeInternalIP, "10.0.0.11")}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k3s-2"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{addr(corev1.NodeInternalIP, "10.0.0.12")}}},
		ready, other, gone,
	}
	b, _ := testBackend(t, "prod", objs...)
	b.SetPivot(PivotOptions{Inventory: pivotInventory(t)})

	q, ok := AlertPodsQuery(map[string]string{"namespace": "prod"})
	if !ok {
		t.Fatal("no query")
	}
	hosts, reason, err := b.AlertNodes(context.Background(), q)
	if err != nil {
		t.Fatalf("AlertNodes: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "k3s-1.lab.example.internal" {
		t.Fatalf("hosts = %v, want the one inventory host of both nodes' pods", hosts)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want none", reason)
	}

	// The pod label narrows the list to the pod that is actually failing.
	one, _ := AlertPodsQuery(map[string]string{"namespace": "prod", "pod": "web-1"})
	hosts, reason, err = b.AlertNodes(context.Background(), one)
	if err != nil {
		t.Fatalf("AlertNodes(one pod): %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("hosts = %v, want none: node k3s-2 is not in the inventory", hosts)
	}
	if reason != "node k3s-2 is not in the inventory" {
		t.Fatalf("reason = %q, want the missing node named", reason)
	}

	// An alert whose pods are gone says so.
	none, _ := AlertPodsQuery(map[string]string{"namespace": "prod", "pod": "gone"})
	if _, reason, err = b.AlertNodes(context.Background(), none); err != nil || reason == "" {
		t.Fatalf("AlertNodes(missing pod) = %q, %v; want a reason", reason, err)
	}
}

// TestPivotPodsViewFilter: the pods view of an alert is filtered by the
// namespace and, when the alert names one, by the pod (Args["name"]).
func TestPivotPodsViewFilter(t *testing.T) {
	b, _ := testBackend(t, "prod",
		testPod("prod", "api-7d9f", "k3s-1", nil),
		testPod("prod", "api-7d9fg", "k3s-1", nil),
		testPod("dev", "api-7d9f", "k3s-1", nil),
	)
	q, ok := AlertPodsQuery(map[string]string{"namespace": "prod", "pod": "api-7d9f"})
	if !ok {
		t.Fatal("AlertPodsQuery found no pods")
	}
	rows, errsSeen := collect(t, b, q)
	if len(errsSeen) != 0 {
		t.Fatalf("List errors: %v", errsSeen)
	}
	if got := names(rows); len(got) != 1 || got[0] != "prod/api-7d9f" {
		t.Fatalf("rows = %v, want only prod/api-7d9f", got)
	}

	// Without the pod label the whole namespace opens.
	all, _ := AlertPodsQuery(map[string]string{"namespace": "prod"})
	rows, _ = collect(t, b, all)
	if got := names(rows); len(got) != 2 {
		t.Fatalf("rows = %v, want the two prod pods", got)
	}
}

// TestPivotNodeStatus is requirement 3: the host overview's KUBERNETES line
// reads the node's ready state, its pod count and how many pods are not ready.
func TestPivotNodeStatus(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "k3s-1"},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{addr(corev1.NodeInternalIP, "10.0.0.11")},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	running := testPod("prod", "api-7d9f", "k3s-1", nil)
	broken := testPod("prod", "api-broken", "k3s-1", nil)
	broken.Status.ContainerStatuses = []corev1.ContainerStatus{containerWaiting("app", "CrashLoopBackOff", 4)}
	elsewhere := testPod("prod", "web-1", "k3s-2", nil)

	b, _ := testBackend(t, "prod", node, running, broken, elsewhere)
	b.SetPivot(PivotOptions{Inventory: pivotInventory(t)})

	status, ok, err := b.NodeStatusOf(context.Background(), "k3s-1.lab.example.internal")
	if err != nil {
		t.Fatalf("NodeStatusOf: %v", err)
	}
	if !ok {
		t.Fatal("NodeStatusOf found no node for an inventory host that is one")
	}
	if status.Name != "k3s-1" || !status.Ready || status.Status != "Ready" {
		t.Fatalf("status = %+v, want a ready node", status)
	}
	if status.Pods != 2 || status.NotReadyPods != 1 {
		t.Fatalf("status = %+v, want 2 pods and 1 not ready", status)
	}

	// A host that is not a node of this cluster says so instead of guessing.
	if _, ok, err := b.NodeStatusOf(context.Background(), "web-01"); ok || err != nil {
		t.Fatalf("NodeStatusOf(web-01) = %v, %v; want not a node", ok, err)
	}
}
