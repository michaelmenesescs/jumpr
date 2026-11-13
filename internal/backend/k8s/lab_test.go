//go:build lab

// M9 lab tests (task 112 requirement 3): the real cluster of SPEC §22 — a
// two-node k3s cluster whose kubeconfig the lab controller writes to
// lab/.keys/kubeconfig and whose context is `@@`.
//
// They skip with "lab k3s cluster not running" unless that kubeconfig exists,
// so `make lab-test` is green on a laptop with the lab shut down. Every test
// drives jumper's packages: the backend's List/Get, the log reader, the pod
// shell and the action bus, not the TUI.
//
// The fault `oom-pod` (labtest.Fault) is the lab's own; the node of the pod it
// breaks is resolved to a lab host, and the same machine is read over SSH for
// the kernel's OOM line. Nothing is installed on any node.
package k8s

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/config"
	"jumper/internal/labtest"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

const (
	// labKubeconfig is where the lab controller writes the k3s kubeconfig.
	labKubeconfig = "lab/.keys/kubeconfig"
	// labContext is the context of that kubeconfig (lab/README.md).
	labContext = "@@"
	// labSkip is the message every test in this file uses when the cluster is
	// not up.
	labSkip = "lab k3s cluster not running"
	// labNamespace is the namespace the write tests create and delete.
	labNamespace = "jumper-lab"
	// labCordonNode is the node the cordon test takes out of service.
	labCordonNode = "k3s-2"
	// labNodeCount is how many nodes the lab's k3s cluster has, all Ready.
	labNodeCount = 2
	// labWait bounds a poll for a cluster state.
	labWait = 3 * time.Minute
	// labStep is the poll interval.
	labStep = 2 * time.Second
	// labDomain is the suffix of every lab host (SPEC §22).
	labDomain = ".lab.example.internal"
	// labPauseImage keeps a test deployment running without a registry pull of
	// an application image.
	labPauseImage = "registry.k8s.io/pause:3.9"
)

// TestLabK8sNodesReady is requirement 3: the lab's two k3s nodes are Ready in
// the nodes kind.
func TestLabK8sNodesReady(t *testing.T) {
	b := labBackend(t)
	rows := labRows(t, b, resource.Query{Kind: "node"})
	if len(rows) != labNodeCount {
		t.Fatalf("nodes = %d, want %d", len(rows), labNodeCount)
	}
	ready := 0
	for _, r := range rows {
		if labField(r, "status") == "Ready" {
			ready++
		}
	}
	if ready != labNodeCount {
		t.Fatalf("%d/%d nodes Ready, want all %d", ready, len(rows), labNodeCount)
	}
}

// TestLabK8sOOMPod is requirement 3's W3 walk: the fault makes a pod reach
// CrashLoopBackOff/OOMKilled, its previous logs are not empty, its node is an
// inventory host, and that host's kernel journal carries the OOM line.
func TestLabK8sOOMPod(t *testing.T) {
	b := labBackend(t)
	labtest.Fault(t, "oom-pod")

	var pod resource.Resource
	deadline := time.Now().Add(labWait)
	for time.Now().Before(deadline) && pod.Ref.Name == "" {
		for _, r := range labRows(t, b, resource.Query{Kind: "pod", Args: map[string]string{"namespace": "all"}}) {
			status := labField(r, "status")
			if strings.Contains(status, "CrashLoopBackOff") || strings.Contains(status, "OOMKilled") {
				pod = r
				break
			}
		}
		if pod.Ref.Name == "" {
			time.Sleep(labStep)
		}
	}
	if pod.Ref.Name == "" {
		t.Fatalf("no pod reached CrashLoopBackOff/OOMKilled within %s", labWait)
	}
	if pod.Status != resource.StatusCrit {
		t.Errorf("pod status colour = %v, want crit", pod.Status)
	}

	// Previous logs: the instance the kubelet OOM-killed.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ch, err := b.Logs(ctx, pod.Ref.Namespace, pod.Ref.Name, LogOptions{Previous: true, TailLines: 50})
	if err != nil {
		t.Fatalf("lab: previous logs: %v", err)
	}
	var lines int
	for line := range ch {
		if strings.TrimSpace(line.Text) != "" {
			lines++
		}
	}
	if lines == 0 {
		t.Errorf("previous logs of %s/%s are empty", pod.Ref.Namespace, pod.Ref.Name)
	}

	// The node of that pod is an inventory host: the k8s inventory source names
	// it after the node, and the lab's SSH config has the same machine.
	node := labField(pod, "node")
	if node == "" {
		t.Fatalf("pod %s/%s has no node field", pod.Ref.Namespace, pod.Ref.Name)
	}
	host := resource.HostID(node + labDomain)
	if !labHost(host) {
		t.Fatalf("node %q does not resolve to a lab host", node)
	}
	cmd := transport.Command{
		Script:  `journalctl -k --no-pager -q | grep -iE 'killed process|oom-kill|out of memory'`,
		Sudo:    true,
		Timeout: time.Minute,
	}
	res, err := labtest.SSH(t).Exec(ctx, host, cmd)
	if err != nil {
		t.Fatalf("lab: kernel journal of %s: %v", host, err)
	}
	if res.RC != 0 || !strings.Contains(strings.ToLower(string(res.Stdout)), "oom") &&
		!strings.Contains(strings.ToLower(string(res.Stdout)), "killed process") {
		t.Fatalf("kernel journal of %s has no OOM line (rc %d):\n%s%s", host, res.RC,
			res.Stdout, res.Stderr)
	}
}

// TestLabK8sRolloutRestart is requirement 3: a rollout restart of a test
// deployment through the bus.
func TestLabK8sRolloutRestart(t *testing.T) {
	b := labBackend(t)
	cs := labDeployment(t, b, "web")
	bs := labBus(t, b)

	results := labRun(t, bs, action.Request{
		SpecID: SpecRolloutRestart,
		Targets: []action.Target{{
			Ref:    resource.Ref{Backend: backendName, Kind: "deployment", Namespace: labNamespace, Name: "web"},
			Labels: map[string]string{"k8s.namespace": labNamespace},
		}},
	})
	if len(results) != 1 || results[0].RC != 0 {
		t.Fatalf("rollout restart results = %+v, want one successful step", results)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dep, err := cs.AppsV1().Deployments(labNamespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("lab: get deployment: %v", err)
	}
	if dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] == "" {
		t.Error("the deployment has no restartedAt annotation after the rollout restart")
	}
}

// TestLabK8sExecEcho is requirement 3: `echo ok` in a pod shell.
func TestLabK8sExecEcho(t *testing.T) {
	b := labBackend(t)
	labDeployment(t, b, "shell")

	pod := labReadyPod(t, b, labNamespace)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := transport.PTYRequest{Term: "xterm-256color", Cols: 80, Rows: 24, Command: "sh"}
	shell, err := b.OpenShell(ctx, pod.Ref.Namespace, pod.Ref.Name, "", req)
	if err != nil {
		t.Fatalf("lab: OpenShell: %v", err)
	}
	defer shell.Close()
	if _, err := io.WriteString(shell, "echo ok\n"); err != nil {
		t.Fatalf("lab: write: %v", err)
	}
	text := labRead(t, shell, "ok", 90*time.Second)
	if !strings.Contains(text, "ok") {
		t.Fatalf("shell output = %q, want ok", text)
	}
}

// TestLabK8sCordonUncordon is requirement 3: cordon k3s-2 and uncordon it, both
// through the bus.
func TestLabK8sCordonUncordon(t *testing.T) {
	b := labBackend(t)
	bs := labBus(t, b)
	cs, err := b.client.Clientset()
	if err != nil {
		t.Fatalf("lab: clientset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	uncordon := func() {
		node, err := cs.CoreV1().Nodes().Get(context.Background(), labCordonNode, metav1.GetOptions{})
		if err != nil || !node.Spec.Unschedulable {
			return
		}
		node.Spec.Unschedulable = false
		if _, err := cs.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
			t.Errorf("lab: cleanup uncordon: %v", err)
		}
	}
	t.Cleanup(uncordon)

	target := action.Target{
		Ref:    resource.Ref{Backend: backendName, Kind: "node", Name: labCordonNode},
		Labels: map[string]string{"k8s.context": labContext},
	}
	labRun(t, bs, action.Request{SpecID: SpecNodeCordon, Targets: []action.Target{target}})

	node, err := cs.CoreV1().Nodes().Get(ctx, labCordonNode, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("lab: get node: %v", err)
	}
	if !node.Spec.Unschedulable {
		t.Fatalf("%s is schedulable after k8s.node.cordon", labCordonNode)
	}

	labRun(t, bs, action.Request{SpecID: SpecNodeUncordon, Targets: []action.Target{target}})
	node, err = cs.CoreV1().Nodes().Get(ctx, labCordonNode, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("lab: get node after uncordon: %v", err)
	}
	if node.Spec.Unschedulable {
		t.Fatalf("%s is still unschedulable after k8s.node.uncordon", labCordonNode)
	}
}

// labBackend loads lab/jumper/config.yaml, moves to the repository root (the
// kubeconfig path is relative to it) and skips unless the k3s cluster is up.
func labBackend(t *testing.T) *Backend {
	t.Helper()
	root := labtest.Root(t)
	if _, err := os.Stat(filepath.Join(root, labKubeconfig)); err != nil {
		t.Skipf("%s: %s is missing; run: go run ./cmd/sitrep-lab up", labSkip, labKubeconfig)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("lab: getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("lab: chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("lab: home: %v", err)
	}
	cfg, err := config.Load(filepath.Join("lab", "jumper", "config.yaml"), home)
	if err != nil {
		t.Fatalf("lab: config: %v", err)
	}
	if strings.TrimSpace(cfg.Kubernetes.Kubeconfig) == "" {
		t.Skipf("%s: lab/jumper/config.yaml sets no kubernetes.kubeconfig", labSkip)
	}
	b, err := New(Options{Kubeconfig: cfg.Kubernetes.Kubeconfig, Home: home})
	if err != nil {
		t.Fatalf("lab: k8s backend: %v", err)
	}
	t.Cleanup(b.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := b.SwitchContext(ctx, labContext); err != nil {
		t.Skipf("%s: context %q: %v", labSkip, labContext, err)
	}
	return b
}

// labRows lists one kind and fails on a stream error.
func labRows(t *testing.T, b *Backend, q resource.Query) []resource.Resource {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ch, err := b.List(ctx, q)
	if err != nil {
		t.Fatalf("lab: List %s: %v", q.Kind, err)
	}
	var rows []resource.Resource
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("lab: List %s: %v", q.Kind, res.Err)
		}
		rows = append(rows, res.Items...)
	}
	return rows
}

// labField is a string field of a row, or "".
func labField(r resource.Resource, id string) string {
	if v, ok := r.Fields[id].(string); ok {
		return v
	}
	return ""
}

// labHost reports whether host is one of the lab's machines.
func labHost(host resource.HostID) bool {
	for _, h := range labtest.Hosts() {
		if h == host {
			return true
		}
	}
	return false
}

// labDeployment creates a one-replica pause deployment in labNamespace, with
// the namespace deleted in cleanup, and returns the cluster's clientset.
func labDeployment(t *testing.T, b *Backend, name string) kubernetes.Interface {
	t.Helper()
	cs, err := b.client.Clientset()
	if err != nil {
		t.Fatalf("lab: clientset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: labNamespace}}
	if _, err := cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("lab: create namespace: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := cs.CoreV1().Namespaces().Delete(ctx, labNamespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("lab: delete namespace: %v", err)
		}
	})
	one := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: labNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "pause", Image: labPauseImage}},
				},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(labNamespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("lab: create deployment: %v", err)
	}
	return cs
}

// labReadyPod waits for one ready pod in namespace.
func labReadyPod(t *testing.T, b *Backend, namespace string) resource.Resource {
	t.Helper()
	deadline := time.Now().Add(labWait)
	for time.Now().Before(deadline) {
		for _, r := range labRows(t, b, resource.Query{Kind: "pod", Args: map[string]string{"namespace": namespace}}) {
			ready := labField(r, "ready")
			if ready != "" && !strings.HasPrefix(ready, "0/") {
				return r
			}
		}
		time.Sleep(labStep)
	}
	t.Fatalf("no ready pod in %s within %s", namespace, labWait)
	return resource.Resource{}
}

// labBus is a bus with this backend registered as the planner of the k8s
// specs, and the default policy.
func labBus(t *testing.T, b *Backend) bus.Bus {
	t.Helper()
	bs := bus.New(bus.Options{Policy: bus.DefaultPolicy()})
	if err := bs.Register("k8s", b); err != nil {
		t.Fatalf("lab: bus.Register: %v", err)
	}
	return bs
}

// labRun proposes req, executes it and returns the results once the rollout has
// finished. Every step must succeed.
func labRun(t *testing.T, bs bus.Bus, req action.Request) []action.StepResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), labWait)
	defer cancel()
	req.Actor = action.Human
	p, err := bs.Propose(ctx, req)
	if err != nil {
		t.Fatalf("lab: Propose %s: %v", req.SpecID, err)
	}
	if p.Denied != nil {
		t.Fatalf("lab: proposal denied: %s", p.Denied.Reason)
	}
	run, err := bs.Execute(ctx, p, bus.Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("lab: Execute %s: %v", req.SpecID, err)
	}
	var results []action.StepResult
	deadline := time.After(labWait)
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				return results
			}
			if ev.Result != nil {
				if ev.Result.Err != nil {
					t.Fatalf("lab: step failed: %v", ev.Result.Err)
				}
				results = append(results, *ev.Result)
			}
			if ev.Phase == bus.PhaseAborted || ev.Phase == bus.PhaseStopped {
				t.Fatalf("lab: rollout ended in phase %v", ev.Phase)
			}
		case <-deadline:
			t.Fatalf("lab: %s did not finish within %s", req.SpecID, labWait)
		}
	}
}

// labRead reads from r until want appears or timeout passes, and returns what
// was read.
func labRead(t *testing.T, r io.Reader, want string, timeout time.Duration) string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
				if strings.Contains(b.String(), want) {
					out <- b.String()
					return
				}
			}
			if err != nil {
				out <- b.String()
				return
			}
		}
	}()
	select {
	case text := <-out:
		return text
	case <-time.After(timeout):
		t.Fatalf("no output within %s", timeout)
		return ""
	}
}
