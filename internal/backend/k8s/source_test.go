package k8s

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"jumper/internal/errs"
	"jumper/internal/inventory"
)

// sourceNow is the clock the source tests run against.
var sourceNow = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// testSource is one inventory source in the merge test.
type testSource struct {
	name  string
	hosts []inventory.Host
	err   error
}

func (s testSource) Name() string { return s.name }

func (s testSource) Load(context.Context) ([]inventory.Host, error) { return s.hosts, s.err }

// nodeOfTest is a node with a name, labels and addresses.
func nodeOfTest(name string, labels map[string]string, addrs ...corev1.NodeAddress) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     corev1.NodeStatus{Addresses: addrs},
	}
}

// addr is one node address.
func addr(t corev1.NodeAddressType, a string) corev1.NodeAddress {
	return corev1.NodeAddress{Type: t, Address: a}
}

// TestSourceHostsFromNodes is requirement 1: one host per node whose aliases are
// the node name, every InternalIP/ExternalIP/Hostname address and the
// kubernetes.io/hostname label, and whose labels are the allowlist plus
// k8s.node and k8s.context.
func TestSourceHostsFromNodes(t *testing.T) {
	nodes := []corev1.Node{
		nodeOfTest("k3s-1",
			map[string]string{
				"kubernetes.io/hostname":                "k3s-1",
				"topology.kubernetes.io/zone":           "lab-a",
				"topology.kubernetes.io/region":         "lab",
				"node.kubernetes.io/instance-type":      "cax11",
				"kubernetes.io/arch":                    "arm64",
				"node-role.kubernetes.io/control-plane": "",
				"node-role.kubernetes.io/etcd":          "",
				"unrelated.example.com/owner":           "platform",
			},
			addr(corev1.NodeInternalIP, "10.0.0.11"),
			addr(corev1.NodeExternalIP, "203.0.113.11"),
			addr(corev1.NodeHostName, "k3s-1.lab.example.internal"),
		),
		nodeOfTest("k3s-2",
			map[string]string{"kubernetes.io/hostname": "k3s-2"},
			addr(corev1.NodeInternalIP, "10.0.0.12"),
		),
		nodeOfTest("", nil), // a node without a name is skipped
	}

	hosts := HostsFromNodes(nodes, SourceOptions{Context: "lab", Now: func() time.Time { return sourceNow }}, sourceNow)
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, want 2 (%+v)", len(hosts), hosts)
	}
	if hosts[0].ID != "k3s-1" || hosts[1].ID != "k3s-2" {
		t.Fatalf("host IDs = %q, %q; want k3s-1, k3s-2", hosts[0].ID, hosts[1].ID)
	}
	h := hosts[0]
	wantAliases := []string{"10.0.0.11", "203.0.113.11", "k3s-1", "k3s-1.lab.example.internal"}
	if !sameStrings(h.Aliases, wantAliases) {
		t.Fatalf("aliases = %v, want %v", h.Aliases, wantAliases)
	}
	wantLabels := map[string]string{
		"node":                    "k3s-1",
		"context":                 "lab",
		"zone":                    "lab-a",
		"region":                  "lab",
		"instance-type":           "cax11",
		"arch":                    "arm64",
		"node-role.control-plane": "",
		"node-role.etcd":          "",
	}
	for k, want := range wantLabels {
		if got, ok := h.Labels[k]; !ok || got != want {
			t.Errorf("label %q = %q (present %v), want %q", k, got, ok, want)
		}
	}
	if _, ok := h.Labels["owner"]; ok {
		t.Errorf("label owner = %q, want the allowlist to keep it out", h.Labels["owner"])
	}
	if !h.SeenAt.Equal(sourceNow) {
		t.Errorf("SeenAt = %v, want %v", h.SeenAt, sourceNow)
	}
}

// TestSourceLabelAllowlist: kubernetes.node_label_keys replaces the default
// allowlist, and an entry ending in "/" is a prefix.
func TestSourceLabelAllowlist(t *testing.T) {
	node := nodeOfTest("web-1", map[string]string{
		"example.com/rack":               "r12",
		"topology.kubernetes.io/zone":    "lab-a",
		"node-role.kubernetes.io/worker": "",
	})
	hosts := HostsFromNodes([]corev1.Node{node}, SourceOptions{
		LabelKeys: []string{"example.com/rack", "node-role.kubernetes.io/"},
		Now:       func() time.Time { return sourceNow },
	}, sourceNow)
	if len(hosts) != 1 {
		t.Fatalf("hosts = %d, want 1", len(hosts))
	}
	labels := hosts[0].Labels
	if labels["rack"] != "r12" {
		t.Errorf("label rack = %q, want r12", labels["rack"])
	}
	if _, ok := labels["node-role.worker"]; !ok {
		t.Errorf("labels = %v, want the node-role prefix to contribute node-role.worker", labels)
	}
	if _, ok := labels["zone"]; ok {
		t.Errorf("labels = %v, want zone out: the allowlist was replaced", labels)
	}
}

// TestSourceLoad reads the nodes of a cluster through the backend and turns a
// list failure into the error kind Classify gave it.
func TestSourceLoad(t *testing.T) {
	b, _ := testBackend(t, "default", &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "k3s-1"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{addr(corev1.NodeInternalIP, "10.0.0.11")}},
	})
	src := b.NodeSource(SourceOptions{Context: "lab", Now: func() time.Time { return sourceNow }})
	if src.Name() != "k8s" {
		t.Fatalf("Name() = %q, want k8s", src.Name())
	}
	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(hosts) != 1 || hosts[0].ID != "k3s-1" || hosts[0].Labels["context"] != "lab" {
		t.Fatalf("hosts = %+v, want the one node with its context label", hosts)
	}

	failing := NewSource(errLister{err: errs.New(errs.Auth, "k8s.nodes", "forbidden")}, SourceOptions{})
	if _, err := failing.Load(context.Background()); errs.KindOf(err) != errs.Auth {
		t.Fatalf("Load error = %v, want kind auth", err)
	}
	if _, err := NewSource(nil, SourceOptions{}).Load(context.Background()); errs.KindOf(err) != errs.Config {
		t.Fatalf("Load without a lister = %v, want kind config", err)
	}
}

// errLister is a NodeLister that always fails.
type errLister struct{ err error }

func (l errLister) Nodes(context.Context) ([]corev1.Node, error) { return nil, l.err }

// TestSourceInventoryMerge is the acceptance case: an ssh host
// k3s-1.lab.example.internal, a salt minion k3s-1 and the k8s node k3s-1 with
// its InternalIP merge into one host carrying the labels of all three.
func TestSourceInventoryMerge(t *testing.T) {
	ssh := testSource{name: "ssh", hosts: []inventory.Host{{
		ID:      "k3s-1.lab.example.internal",
		Addr:    "k3s-1.lab.example.internal",
		Aliases: []string{"k3s-1"},
		Labels:  map[string]string{"user": "ubuntu"},
		SeenAt:  sourceNow,
	}}}
	salt := testSource{name: "salt", hosts: []inventory.Host{{
		ID:     "k3s-1",
		Labels: map[string]string{"role": "k3s-server", "env": "lab"},
		SeenAt: sourceNow,
	}}}
	nodes := []corev1.Node{nodeOfTest("k3s-1",
		map[string]string{
			"kubernetes.io/hostname":       "k3s-1",
			"topology.kubernetes.io/zone":  "lab-a",
			"node-role.kubernetes.io/etcd": "",
		},
		addr(corev1.NodeInternalIP, "10.0.0.11"),
	)}
	kube := NewSource(nodeListerOf(nodes), SourceOptions{Context: "lab", Now: func() time.Time { return sourceNow }})

	store := inventory.New([]inventory.Source{ssh, salt, kube}, nil, "", func() time.Time { return sourceNow })
	if failed := store.Refresh(context.Background()); len(failed) != 0 {
		t.Fatalf("Refresh failed: %v", failed)
	}
	hosts := store.Hosts()
	if len(hosts) != 1 {
		t.Fatalf("merged hosts = %d, want 1 (%+v)", len(hosts), hosts)
	}
	h := hosts[0]
	if h.ID != "k3s-1.lab.example.internal" {
		t.Fatalf("ID = %q, want the FQDN of the ssh record", h.ID)
	}
	if h.Addr != "k3s-1.lab.example.internal" {
		t.Fatalf("Addr = %q, want the ssh record's address", h.Addr)
	}
	want := map[string]string{
		"ssh.user":           "ubuntu",
		"salt.role":          "k3s-server",
		"salt.env":           "lab",
		"k8s.node":           "k3s-1",
		"k8s.context":        "lab",
		"k8s.zone":           "lab-a",
		"k8s.node-role.etcd": "",
	}
	for k, want := range want {
		if got, ok := h.Labels[k]; !ok || got != want {
			t.Errorf("label %q = %q (present %v), want %q", k, got, ok, want)
		}
	}
	if !sameStrings(h.Sources, []string{"k8s", "salt", "ssh"}) {
		t.Errorf("sources = %v, want k8s, salt and ssh", h.Sources)
	}
	// The node resolves to the SSH-reachable host, which is what the pod
	// pivot and the host overview dial.
	if got, ok := store.Resolve("10.0.0.11"); !ok || got.ID != h.ID {
		t.Errorf("Resolve(10.0.0.11) = %q, %v; want %q", got.ID, ok, h.ID)
	}
}

// nodeListerOf is a NodeLister over a fixed node list.
func nodeListerOf(nodes []corev1.Node) NodeLister { return fixedNodes(nodes) }

type fixedNodes []corev1.Node

func (f fixedNodes) Nodes(context.Context) ([]corev1.Node, error) { return []corev1.Node(f), nil }

// sameStrings compares two string slices.
func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// compile-time guard that the source is an inventory.Source.
var _ inventory.Source = (*Source)(nil)
