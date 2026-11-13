package k8s

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// actionContext is the kubeconfig context every test plan reports.
const actionContext = "lab"

// actionPod is one pod of an action test, scheduled on node n1 and owned by
// owner (nil = bare).
func actionPod(namespace, name string, owner *metav1.OwnerReference) *corev1.Pod {
	pod := testPod(namespace, name, "n1", map[string]string{"app": "web"})
	if owner != nil {
		pod.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return pod
}

// controllerRef is a controller OwnerReference.
func controllerRef(kind, name string) *metav1.OwnerReference {
	yes := true
	return &metav1.OwnerReference{Kind: kind, Name: name, Controller: &yes}
}

// actionDeployment is one deployment of an action test.
func actionDeployment(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
		},
	}
}

// actionNode is one node of an action test.
func actionNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// actionBackend builds a backend over a fake clientset with the context the
// displays name.
func actionBackend(t *testing.T, ns string, objs ...runtime.Object) (*Backend, *fake.Clientset) {
	t.Helper()
	b, cli := testBackend(t, ns, objs...)
	b.client = &Client{contextName: actionContext}
	fixed := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return fixed }
	return b, cli
}

// podTarget is a target of a pod row.
func podTarget(namespace, name string) action.Target {
	return action.Target{Ref: resource.Ref{Backend: backendName, Kind: "pod", Namespace: namespace, Name: name}}
}

// evictionCalls records the evictions a fake clientset saw, split by dry run.
type evictionCalls struct {
	mu      sync.Mutex
	dry     []string
	real    []string
	reactor func(name string, dry bool, n int) error
}

// install adds the eviction reactor to cli.
func (e *evictionCalls) install(cli *fake.Clientset) {
	cli.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		create, ok := a.(ktesting.CreateAction)
		if !ok || create.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev, ok := create.GetObject().(*policyv1.Eviction)
		if !ok {
			return false, nil, nil
		}
		dry := ev.DeleteOptions != nil && len(ev.DeleteOptions.DryRun) > 0 &&
			ev.DeleteOptions.DryRun[0] == metav1.DryRunAll
		e.mu.Lock()
		n := 0
		if dry {
			e.dry = append(e.dry, ev.Name)
			n = len(e.dry)
		} else {
			e.real = append(e.real, ev.Name)
			n = len(e.real)
		}
		reactor := e.reactor
		e.mu.Unlock()
		if reactor != nil {
			if err := reactor(ev.Name, dry, n); err != nil {
				return true, nil, err
			}
		}
		return true, nil, nil
	})
}

// counts is the number of dry and real evictions seen.
func (e *evictionCalls) counts() (dry, real int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.dry), len(e.real)
}

// tooManyRequests is the 429 a PodDisruptionBudget produces.
func tooManyRequests() error {
	return apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 5)
}

// TestActionsSpecs: the menu is per kind and a mixed selection offers nothing.
func TestActionsSpecs(t *testing.T) {
	b := NewWithClient(nil)
	ids := func(sel ...resource.Resource) []string {
		out := []string{}
		for _, s := range b.Actions(sel) {
			out = append(out, s.ID)
		}
		return out
	}
	pod := resource.Resource{Ref: resource.Ref{Kind: "pod"}}
	deploy := resource.Resource{Ref: resource.Ref{Kind: "deployment"}}
	ds := resource.Resource{Ref: resource.Ref{Kind: "daemonset"}}
	node := resource.Resource{Ref: resource.Ref{Kind: "node"}}
	cases := []struct {
		name string
		sel  []resource.Resource
		want []string
	}{
		{"pod", []resource.Resource{pod}, []string{SpecDelete}},
		{"deployment", []resource.Resource{deploy}, []string{SpecDelete, SpecScale, SpecRolloutRestart}},
		{"daemonset", []resource.Resource{ds}, []string{SpecDelete, SpecRolloutRestart}},
		{"node", []resource.Resource{node}, []string{SpecNodeCordon, SpecNodeUncordon, SpecNodeDrain}},
		{"mixed", []resource.Resource{pod, deploy}, nil},
		{"unknown", []resource.Resource{{Ref: resource.Ref{Kind: "widget"}}}, nil},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(tc.sel...)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Actions = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlanDeleteOwnerAndLabels: the delete preview names the controller and the
// step targets carry the policy labels, and the delete happens in Run only.
func TestPlanDeleteOwnerAndLabels(t *testing.T) {
	owned := actionPod("prod", "web-1", controllerRef("ReplicaSet", "web-rs"))
	bare := actionPod("prod", "bare-1", nil)
	b, cli := actionBackend(t, "prod", owned, bare)

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  SpecDelete,
		Targets: []action.Target{podTarget("prod", "web-1"), podTarget("prod", "bare-1")},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Via != "k8s:"+actionContext {
		t.Errorf("Via = %q, want k8s:%s", plan.Via, actionContext)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(plan.Steps))
	}
	if want := "kubectl --context lab -n prod delete pod web-1"; plan.Steps[0].Display != want {
		t.Errorf("display = %q, want %q", plan.Steps[0].Display, want)
	}
	if got := plan.Steps[0].Target.Labels[contextLabel]; got != actionContext {
		t.Errorf("target %s = %q, want %q", contextLabel, got, actionContext)
	}
	if got := plan.Steps[0].Target.Labels[namespaceLabel]; got != "prod" {
		t.Errorf("target %s = %q, want prod", namespaceLabel, got)
	}
	for _, want := range []string{
		"controlled by ReplicaSet web-rs — it will be recreated",
		"bare pod: it will be gone",
	} {
		if !strings.Contains(plan.Preview, want) {
			t.Errorf("preview = %q, want it to contain %q", plan.Preview, want)
		}
	}
	// Plan reads, it does not delete.
	if _, err := cli.CoreV1().Pods("prod").Get(context.Background(), "web-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("Plan deleted the pod: %v", err)
	}

	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if _, err := cli.CoreV1().Pods("prod").Get(context.Background(), "web-1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod still exists after Run: %v", err)
	}
}

// TestPlanScale: the preview compares current and new replicas and Run patches
// the replica count.
func TestPlanScale(t *testing.T) {
	b, cli := actionBackend(t, "prod", actionDeployment("prod", "web", 2))
	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  SpecScale,
		Targets: []action.Target{{Ref: resource.Ref{Kind: "deployment", Namespace: "prod", Name: "web"}}},
		Params:  map[string]string{ParamReplicas: "5"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if want := "current 2 → new 5 replicas"; !strings.Contains(plan.Preview, want) {
		t.Errorf("preview = %q, want %q", plan.Preview, want)
	}
	if want := "kubectl --context lab -n prod scale deployment web --replicas=5"; plan.Steps[0].Display != want {
		t.Errorf("display = %q, want %q", plan.Steps[0].Display, want)
	}
	dep, err := cli.AppsV1().Deployments("prod").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if *dep.Spec.Replicas != 2 {
		t.Fatalf("Plan changed replicas to %d", *dep.Spec.Replicas)
	}

	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	dep, err = cli.AppsV1().Deployments("prod").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if *dep.Spec.Replicas != 5 {
		t.Errorf("replicas = %d, want 5", *dep.Spec.Replicas)
	}
}

// TestPlanScaleBadParam: a replica count that is not a number is a config error.
func TestPlanScaleBadParam(t *testing.T) {
	b, _ := actionBackend(t, "prod", actionDeployment("prod", "web", 2))
	_, err := b.Plan(context.Background(), action.Request{
		SpecID:  SpecScale,
		Targets: []action.Target{{Ref: resource.Ref{Kind: "deployment", Namespace: "prod", Name: "web"}}},
		Params:  map[string]string{ParamReplicas: "many"},
	})
	if err == nil || errs.KindOf(err) != errs.Config {
		t.Fatalf("err = %v, want a config error", err)
	}
}

// TestPlanRolloutRestart: the preview counts the affected pods and names the
// strategy, and Run patches the restartedAt annotation.
func TestPlanRolloutRestart(t *testing.T) {
	matching := []runtime.Object{
		actionDeployment("prod", "web", 2),
		actionPod("prod", "web-1", controllerRef("ReplicaSet", "web-rs")),
		actionPod("prod", "web-2", controllerRef("ReplicaSet", "web-rs")),
	}
	other := testPod("prod", "worker-1", "node-a", map[string]string{"app": "worker"})
	b, cli := actionBackend(t, "prod", append(matching, other)...)

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  SpecRolloutRestart,
		Targets: []action.Target{{Ref: resource.Ref{Kind: "deployment", Namespace: "prod", Name: "web"}}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, want := range []string{"2 pods affected", "strategy RollingUpdate"} {
		if !strings.Contains(plan.Preview, want) {
			t.Errorf("preview = %q, want it to contain %q", plan.Preview, want)
		}
	}
	if want := "kubectl --context lab -n prod rollout restart deployment web"; plan.Steps[0].Display != want {
		t.Errorf("display = %q, want %q", plan.Steps[0].Display, want)
	}

	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	dep, err := cli.AppsV1().Deployments("prod").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, want := dep.Spec.Template.Annotations[restartAnnotation], "2026-11-13T12:00:00Z"; got != want {
		t.Errorf("%s = %q, want %q", restartAnnotation, got, want)
	}
}

// TestPlanCordonUncordon: both node patches are planned and run.
func TestPlanCordonUncordon(t *testing.T) {
	b, cli := actionBackend(t, "", actionNode("n1"))
	for _, tc := range []struct {
		spec        string
		wantDisplay string
		want        bool
	}{
		{SpecNodeCordon, "kubectl --context lab cordon node n1", true},
		{SpecNodeUncordon, "kubectl --context lab uncordon node n1", false},
	} {
		plan, err := b.Plan(context.Background(), action.Request{
			SpecID:  tc.spec,
			Targets: []action.Target{{Ref: resource.Ref{Kind: "node", Name: "n1"}}},
		})
		if err != nil {
			t.Fatalf("%s Plan: %v", tc.spec, err)
		}
		if plan.Steps[0].Display != tc.wantDisplay {
			t.Errorf("%s display = %q, want %q", tc.spec, plan.Steps[0].Display, tc.wantDisplay)
		}
		if got := plan.Steps[0].Target.Labels[namespaceLabel]; got != "" {
			t.Errorf("%s %s = %q, want empty", tc.spec, namespaceLabel, got)
		}
		res := plan.Steps[0].Run(context.Background())
		if res.Err != nil {
			t.Fatalf("%s Run: %v", tc.spec, res.Err)
		}
		node, err := cli.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get node: %v", err)
		}
		if node.Spec.Unschedulable != tc.want {
			t.Errorf("%s unschedulable = %t, want %t", tc.spec, node.Spec.Unschedulable, tc.want)
		}
	}
}

// TestPlanDrainPreviewBlocksPDB: the preview separates evictable, skipped and
// blocking pods, and the PDB refusal comes from a server-side dry-run eviction.
func TestPlanDrainPreviewBlocksPDB(t *testing.T) {
	dsPod := actionPod("prod", "ds-1", controllerRef("DaemonSet", "node-agent"))
	mirror := actionPod("prod", "mirror-1", nil)
	mirror.Annotations = map[string]string{mirrorAnnotation: "abc"}
	local := actionPod("prod", "local-1", controllerRef("ReplicaSet", "local-rs"))
	local.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	b, cli := actionBackend(t, "prod",
		actionNode("n1"),
		actionPod("prod", "ok-1", controllerRef("ReplicaSet", "ok-rs")),
		actionPod("prod", "pdb-1", controllerRef("ReplicaSet", "pdb-rs")),
		dsPod, mirror, local,
		actionPod("prod", "bare-1", nil),
	)
	calls := &evictionCalls{reactor: func(name string, dry bool, _ int) error {
		if name == "pdb-1" && dry {
			return tooManyRequests()
		}
		return nil
	}}
	calls.install(cli)

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  SpecNodeDrain,
		Targets: []action.Target{{Ref: resource.Ref{Kind: "node", Name: "n1"}}},
		Params:  map[string]string{ParamIgnoreDaemonsets: yesParam},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, want := range []string{
		"would evict (1)", "prod/ok-1",
		"blocked (3)", "prod/pdb-1 (PodDisruptionBudget)",
		"prod/local-1 (local storage (emptyDir); pass delete-emptydir-data=yes)",
		"prod/bare-1 (bare pod: no controller, it will not be evicted)",
		"skipped (2)", "prod/ds-1", "prod/mirror-1",
	} {
		if !strings.Contains(plan.Preview, want) {
			t.Errorf("preview = %q, want it to contain %q", plan.Preview, want)
		}
	}
	if len(plan.Warnings) == 0 {
		t.Error("a drain with blocking pods must warn")
	}
	if want := "kubectl --context lab drain node/n1 --grace-period=30 --ignore-daemonsets"; plan.Steps[0].Display != want {
		t.Errorf("display = %q, want %q", plan.Steps[0].Display, want)
	}
	dry, real := calls.counts()
	if dry != 2 {
		t.Errorf("dry-run evictions = %d, want 2 (ok-1 and pdb-1)", dry)
	}
	if real != 0 {
		t.Errorf("real evictions during Plan = %d, want 0", real)
	}
}

// TestPlanDrainNeedsIgnoreDaemonsets: without the flag a node that runs
// daemonset pods is refused, as kubectl drain does.
func TestPlanDrainNeedsIgnoreDaemonsets(t *testing.T) {
	b, _ := actionBackend(t, "prod",
		actionNode("n1"),
		actionPod("prod", "ds-1", controllerRef("DaemonSet", "node-agent")),
	)
	_, err := b.Plan(context.Background(), action.Request{
		SpecID:  SpecNodeDrain,
		Targets: []action.Target{{Ref: resource.Ref{Kind: "node", Name: "n1"}}},
	})
	if err == nil || errs.KindOf(err) != errs.Config {
		t.Fatalf("err = %v, want a config error about ignore-daemonsets", err)
	}
	if !strings.Contains(err.Error(), "ignore-daemonsets") {
		t.Errorf("err = %v, want it to name ignore-daemonsets", err)
	}
}

// TestDrainEvictionRetriesOn429: a PDB 429 is retried and the drain finishes
// once the API server accepts, cordoning the node first.
func TestDrainEvictionRetriesOn429(t *testing.T) {
	b, cli := actionBackend(t, "prod",
		actionNode("n1"),
		actionPod("prod", "web-1", controllerRef("ReplicaSet", "web-rs")),
	)
	calls := &evictionCalls{reactor: func(name string, dry bool, n int) error {
		if !dry && n == 1 {
			return tooManyRequests()
		}
		return nil
	}}
	calls.install(cli)

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  SpecNodeDrain,
		Targets: []action.Target{{Ref: resource.Ref{Kind: "node", Name: "n1"}}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if !strings.Contains(res.Output, "evicted prod/web-1") {
		t.Errorf("output = %q, want an evicted line", res.Output)
	}
	_, real := calls.counts()
	if real != 2 {
		t.Errorf("real evictions = %d, want 2 (429 then success)", real)
	}
	node, err := cli.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if !node.Spec.Unschedulable {
		t.Error("drain did not cordon the node")
	}
}

// TestDrainCancelled: a pod that stays PDB-blocked ends the drain when the
// context is cancelled, instead of waiting for the step timeout.
func TestDrainCancelled(t *testing.T) {
	b, cli := actionBackend(t, "prod",
		actionNode("n1"),
		actionPod("prod", "web-1", controllerRef("ReplicaSet", "web-rs")),
	)
	calls := &evictionCalls{reactor: func(name string, dry bool, _ int) error {
		if !dry {
			return tooManyRequests()
		}
		return nil
	}}
	calls.install(cli)

	plan, err := b.Plan(context.Background(), action.Request{
		SpecID:  SpecNodeDrain,
		Targets: []action.Target{{Ref: resource.Ref{Kind: "node", Name: "n1"}}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	step := plan.Steps[0]
	done := make(chan action.StepResult, 1)
	go func() { done <- step.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		if res.Err == nil {
			t.Fatal("a cancelled drain must fail")
		}
		if kind := errs.KindOf(res.Err); kind != errs.Timeout && kind != errs.Canceled {
			t.Errorf("kind = %q, want timeout or canceled", kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not honour the context cancel")
	}
}
