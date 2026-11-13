// Kubernetes actions through the action bus (SPEC §8.2): delete, scale,
// rollout restart and the node cordon/uncordon/drain set, each with the plan
// and preview the operator confirms before anything runs.
//
// A backend only plans. Plan may read the API server (the objects and the
// server-side dry-run evictions of a drain preview); every write is a Step
// whose Run performs it through client-go, and nothing outside internal/bus
// calls Run. A drain never force-deletes: pods that cannot be evicted are
// reported as blocking instead.

package k8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/shq"
)

// Spec IDs offered by this backend. Every one starts with "k8s.".
const (
	// SpecDelete deletes one object. Destructive.
	SpecDelete = "k8s.delete"
	// SpecScale changes a workload's replica count.
	SpecScale = "k8s.scale"
	// SpecRolloutRestart rolls a workload's pods by updating the restartedAt
	// annotation of its pod template.
	SpecRolloutRestart = "k8s.rollout.restart"
	// SpecNodeCordon marks a node unschedulable.
	SpecNodeCordon = "k8s.node.cordon"
	// SpecNodeUncordon marks a node schedulable again.
	SpecNodeUncordon = "k8s.node.uncordon"
	// SpecNodeDrain cords a node and evicts its pods. Destructive.
	SpecNodeDrain = "k8s.node.drain"
)

// Parameter names of the specs.
const (
	// ParamReplicas is the replica count `k8s.scale` sets.
	ParamReplicas = "replicas"
	// ParamGrace is the drain's grace period in seconds.
	ParamGrace = "grace"
	// ParamDeleteEmptyDirData allows draining pods with emptyDir volumes.
	ParamDeleteEmptyDirData = "delete-emptydir-data"
	// ParamIgnoreDaemonsets allows draining a node that runs daemonset pods.
	ParamIgnoreDaemonsets = "ignore-daemonsets"
)

const (
	// yesParam and noParam are the only values the two yes/no parameters take.
	yesParam = "yes"
	noParam  = "no"

	// opPlan is the error Op of Plan and its previews.
	opPlan = "k8s.Plan"
	// opDelete, opScale, opRollout, opCordon, opUncordon and opDrain are the
	// error Ops of the steps.
	opDelete   = "k8s.delete"
	opScale    = "k8s.scale"
	opRollout  = "k8s.rollout"
	opCordon   = "k8s.cordon"
	opUncordon = "k8s.uncordon"
	opDrain    = "k8s.drain"

	// writeTimeout bounds a delete, scale, restart or cordon step.
	writeTimeout = 60 * time.Second
	// drainTimeout bounds one whole drain step: PDB-blocked evictions retry for
	// this long before the step fails (task 109).
	drainTimeout = 10 * time.Minute
	// defaultGrace is kubectl's default drain grace period.
	defaultGrace = 30
	// evictDelay is the first retry delay of a 429 eviction; it doubles up to
	// evictMaxDelay.
	evictDelay    = 100 * time.Millisecond
	evictMaxDelay = 2 * time.Second

	// restartAnnotation is the annotation kubectl rollout restart writes.
	restartAnnotation = "kubectl.kubernetes.io/restartedAt"
	// mirrorAnnotation marks a static (mirror) pod, which has no API-server
	// controller and is skipped by a drain.
	mirrorAnnotation = "kubernetes.io/config.mirror"

	// contextLabel and namespaceLabel are the target labels policy selectors
	// see (task 109 requirement 2).
	contextLabel   = "k8s.context"
	namespaceLabel = "k8s.namespace"
)

// addSpecs are the write specs of this backend, in menu order.
func addSpecs() []action.Spec {
	return []action.Spec{
		deleteSpec(),
		scaleSpec(),
		rolloutRestartSpec(),
		nodeCordonSpec(),
		nodeUncordonSpec(),
		nodeDrainSpec(),
	}
}

func deleteSpec() action.Spec {
	return action.Spec{
		ID:        SpecDelete,
		Title:     "delete",
		Risk:      action.RiskDestructive,
		HasDryRun: true,
	}
}

func scaleSpec() action.Spec {
	return action.Spec{
		ID:        SpecScale,
		Title:     "scale",
		Risk:      action.RiskNormal,
		HasDryRun: true,
		Params: []action.Param{
			{Name: ParamReplicas, Title: "replicas", Required: true},
		},
	}
}

func rolloutRestartSpec() action.Spec {
	return action.Spec{
		ID:        SpecRolloutRestart,
		Title:     "rollout restart",
		Risk:      action.RiskNormal,
		HasDryRun: true,
	}
}

func nodeCordonSpec() action.Spec {
	return action.Spec{ID: SpecNodeCordon, Title: "cordon", Risk: action.RiskNormal}
}

func nodeUncordonSpec() action.Spec {
	return action.Spec{ID: SpecNodeUncordon, Title: "uncordon", Risk: action.RiskNormal}
}

func nodeDrainSpec() action.Spec {
	return action.Spec{
		ID:        SpecNodeDrain,
		Title:     "drain",
		Risk:      action.RiskDestructive,
		HasDryRun: true,
		Params: []action.Param{
			{Name: ParamGrace, Title: "grace period (seconds)", Default: "30"},
			{Name: ParamDeleteEmptyDirData, Title: "delete emptyDir data", Default: noParam,
				Choices: []string{yesParam, noParam}},
			{Name: ParamIgnoreDaemonsets, Title: "ignore daemonsets", Default: noParam,
				Choices: []string{yesParam, noParam}},
		},
	}
}

// actionSpecByID looks a spec up by its exact ID.
func actionSpecByID(id string) (action.Spec, bool) {
	for _, s := range addSpecs() {
		if s.ID == id {
			return s, true
		}
	}
	return action.Spec{}, false
}

// specsForKind is what can be done to one kind of object.
func specsForKind(kind string) []action.Spec {
	switch kind {
	case "pod":
		return []action.Spec{deleteSpec()}
	case "deployment", "statefulset":
		return []action.Spec{deleteSpec(), scaleSpec(), rolloutRestartSpec()}
	case "daemonset":
		return []action.Spec{deleteSpec(), rolloutRestartSpec()}
	case "node":
		return []action.Spec{nodeCordonSpec(), nodeUncordonSpec(), nodeDrainSpec()}
	}
	// Every other kind the backend serves can be deleted.
	return []action.Spec{deleteSpec()}
}

var _ action.Planner = (*Backend)(nil)

// Actions lists what can be done to selection. Pure: it only reads the rows it
// is given. A selection of mixed kinds offers nothing, because no single spec
// covers both.
func (b *Backend) Actions(selection []resource.Resource) []action.Spec {
	if len(selection) == 0 {
		return nil
	}
	first, ok := defFor(selection[0].Ref.Kind)
	if !ok {
		return nil
	}
	for _, r := range selection[1:] {
		def, ok := defFor(r.Ref.Kind)
		if !ok || def.spec.Name != first.spec.Name {
			return nil
		}
	}
	return specsForKind(string(first.spec.Name))
}

// planTarget is one object of a plan, with the request target it came from so
// policy still sees the labels.
type planTarget struct {
	target action.Target
	ref    resource.Ref
	def    kindDef
}

// Plan resolves req into one step per object. It reads the objects a preview
// needs and, for a drain, runs one server-side dry-run eviction per pod.
// Nothing is written.
func (b *Backend) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	spec, ok := actionSpecByID(req.SpecID)
	if !ok {
		return action.Plan{}, errs.New(errs.Config, opPlan, fmt.Sprintf("unknown spec %q", req.SpecID))
	}
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, opPlan, "no targets")
	}
	cs, err := b.liveClientset()
	if err != nil {
		return action.Plan{}, err
	}
	planned, err := b.planTargets(req.Targets)
	if err != nil {
		return action.Plan{}, err
	}
	for _, pt := range planned {
		if !specAllows(spec.ID, string(pt.def.spec.Name)) {
			return action.Plan{}, errs.New(errs.Config, opPlan,
				fmt.Sprintf("%s cannot act on %s", spec.ID, pt.def.spec.Name))
		}
	}

	plan := action.Plan{
		Spec:    spec,
		Request: req,
		Title:   planTitle(spec.ID, req.Params, planned),
		Via:     b.via(),
	}
	switch spec.ID {
	case SpecDelete:
		preview, err := b.deletePreview(ctx, cs, planned)
		if err != nil {
			return action.Plan{}, err
		}
		plan.Preview = preview
	case SpecScale:
		replicas, err := parseReplicas(req.Params)
		if err != nil {
			return action.Plan{}, err
		}
		preview, err := b.scalePreview(ctx, cs, planned, replicas)
		if err != nil {
			return action.Plan{}, err
		}
		plan.Preview = preview
	case SpecRolloutRestart:
		preview, err := b.rolloutPreview(ctx, cs, planned)
		if err != nil {
			return action.Plan{}, err
		}
		plan.Preview = preview
	case SpecNodeDrain:
		params, err := drainParams(req.Params)
		if err != nil {
			return action.Plan{}, err
		}
		if len(planned) != 1 {
			return action.Plan{}, errs.New(errs.Config, opPlan, "drain acts on one node")
		}
		preview, warnings, err := b.drainPreview(ctx, cs, planned[0], params)
		if err != nil {
			return action.Plan{}, err
		}
		plan.Preview = preview
		plan.Warnings = warnings
	}

	for _, pt := range planned {
		pt := pt
		var display string
		var run func(context.Context) action.StepResult
		switch spec.ID {
		case SpecDelete:
			display = b.kubectlLine(pt.ref.Namespace, "delete", string(pt.def.spec.Name), pt.ref.Name)
			run = func(ctx context.Context) action.StepResult {
				return b.runDelete(ctx, pt)
			}
		case SpecScale:
			replicas, _ := parseReplicas(req.Params)
			display = b.kubectlLine(pt.ref.Namespace, "scale", string(pt.def.spec.Name), pt.ref.Name,
				"--replicas="+strconv.Itoa(replicas))
			run = func(ctx context.Context) action.StepResult {
				return b.runScale(ctx, pt, replicas)
			}
		case SpecRolloutRestart:
			display = b.kubectlLine(pt.ref.Namespace, "rollout", "restart",
				string(pt.def.spec.Name), pt.ref.Name)
			run = func(ctx context.Context) action.StepResult {
				return b.runRolloutRestart(ctx, pt)
			}
		case SpecNodeCordon:
			display = b.kubectlLine("", "cordon", "node", pt.ref.Name)
			run = func(ctx context.Context) action.StepResult {
				return b.runNodeUnschedulable(ctx, pt, true)
			}
		case SpecNodeUncordon:
			display = b.kubectlLine("", "uncordon", "node", pt.ref.Name)
			run = func(ctx context.Context) action.StepResult {
				return b.runNodeUnschedulable(ctx, pt, false)
			}
		case SpecNodeDrain:
			params, _ := drainParams(req.Params)
			display = b.drainLine(pt.ref.Name, params)
			run = func(ctx context.Context) action.StepResult {
				return b.runDrain(ctx, pt, params)
			}
		default:
			return action.Plan{}, errs.New(errs.Config, opPlan, fmt.Sprintf("spec %q is not plannable", spec.ID))
		}
		plan.Steps = append(plan.Steps, action.Step{Target: pt.target, Display: display, Run: run})
	}
	return plan, nil
}

// specAllows reports whether specID can act on kind.
func specAllows(specID, kind string) bool {
	switch specID {
	case SpecDelete:
		return kind != "node"
	case SpecScale, SpecRolloutRestart:
		switch kind {
		case "deployment", "statefulset":
			return true
		case "daemonset":
			return specID == SpecRolloutRestart
		}
		return false
	case SpecNodeCordon, SpecNodeUncordon, SpecNodeDrain:
		return kind == "node"
	}
	return false
}

// planTargets resolves the request's targets onto objects, with the namespace
// filled in and the policy labels added, de-duplicated, in request order.
func (b *Backend) planTargets(targets []action.Target) ([]planTarget, error) {
	out := make([]planTarget, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		def, ok := defFor(t.Ref.Kind)
		if !ok {
			return nil, errs.New(errs.Config, opPlan, fmt.Sprintf("unknown kind %q", t.Ref.Kind))
		}
		name := strings.TrimSpace(t.Ref.Name)
		if name == "" {
			return nil, errs.New(errs.Config, opPlan, "target has no name")
		}
		ns := ""
		if def.namespaced {
			ns = strings.TrimSpace(t.Ref.Namespace)
			if ns == "" {
				ns = b.sessionNamespace()
			}
			if ns == "" {
				return nil, errs.New(errs.Config, opPlan,
					fmt.Sprintf("%s %s has no namespace", def.spec.Name, name))
			}
		}
		ref := t.Ref
		ref.Backend = backendName
		ref.Kind = def.spec.Name
		ref.Namespace = ns
		ref.Name = name

		key := string(ref.Kind) + "/" + ref.Namespace + "/" + ref.Name
		if seen[key] {
			continue
		}
		seen[key] = true

		labels := make(map[string]string, len(t.Labels)+2)
		for k, v := range t.Labels {
			labels[k] = v
		}
		labels[contextLabel] = b.contextName()
		nsLabel := ns
		if nsLabel == "" && string(def.spec.Name) == "namespace" {
			nsLabel = name
		}
		labels[namespaceLabel] = nsLabel
		target := t
		target.Ref = ref
		target.Labels = labels
		out = append(out, planTarget{target: target, ref: ref, def: def})
	}
	return out, nil
}

// liveClientset is the typed client a step uses: the one internal/app injected
// (or the tests), else the client of the current context.
func (b *Backend) liveClientset() (kubernetes.Interface, error) {
	if b.typed != nil {
		return b.typed, nil
	}
	if b.client == nil {
		return nil, errs.New(errs.Config, opPlan, "k8s backend not configured: no client")
	}
	return b.client.Clientset()
}

// contextName is the context a plan reports and its displays name.
func (b *Backend) contextName() string {
	if b.client == nil {
		return ""
	}
	return b.client.CurrentContext()
}

// sessionNamespace is the client's session namespace, or the injected one in a
// test.
func (b *Backend) sessionNamespace() string {
	if b.client != nil {
		return b.client.Namespace()
	}
	return b.namespace
}

// via is the transport name a k8s plan reports: "k8s:<context>".
func (b *Backend) via() string {
	if name := b.contextName(); name != "" {
		return backendName + ":" + name
	}
	return backendName
}

// kubectlLine is the kubectl command equivalent to one step, built with shq so
// the display is the same well-formed line the CLI would send.
func (b *Backend) kubectlLine(namespace string, words ...string) string {
	args := []string{"kubectl"}
	if name := b.contextName(); name != "" {
		args = append(args, "--context", name)
	}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return shq.Join(append(args, words...)...)
}

// drainLine is the kubectl drain command of a step.
func (b *Backend) drainLine(node string, params drainParameters) string {
	words := []string{"drain", "node/" + node, "--grace-period=" + strconv.Itoa(params.grace)}
	if params.ignoreDaemonsets {
		words = append(words, "--ignore-daemonsets")
	}
	if params.deleteEmptyDirData {
		words = append(words, "--delete-emptydir-data")
	}
	return b.kubectlLine("", words...)
}

// planTitle is the one-line title of the plan dialog.
func planTitle(specID string, params map[string]string, planned []planTarget) string {
	if len(planned) == 1 {
		pt := planned[0]
		name := string(pt.def.spec.Name) + " " + pt.ref.Name
		switch specID {
		case SpecDelete:
			return "delete " + name
		case SpecScale:
			replicas, _ := parseReplicas(params)
			return fmt.Sprintf("scale %s to %d", name, replicas)
		case SpecRolloutRestart:
			return "rollout restart " + name
		case SpecNodeCordon:
			return "cordon node " + pt.ref.Name
		case SpecNodeUncordon:
			return "uncordon node " + pt.ref.Name
		case SpecNodeDrain:
			return "drain node " + pt.ref.Name
		}
	}
	switch specID {
	case SpecDelete:
		return fmt.Sprintf("delete %d objects", len(planned))
	case SpecScale:
		replicas, _ := parseReplicas(params)
		return fmt.Sprintf("scale %d objects to %d replicas", len(planned), replicas)
	case SpecRolloutRestart:
		return fmt.Sprintf("rollout restart %d objects", len(planned))
	case SpecNodeCordon:
		return fmt.Sprintf("cordon %d nodes", len(planned))
	case SpecNodeUncordon:
		return fmt.Sprintf("uncordon %d nodes", len(planned))
	case SpecNodeDrain:
		return fmt.Sprintf("drain %d nodes", len(planned))
	}
	return fmt.Sprintf("%s on %d objects", specID, len(planned))
}

// parseReplicas reads the scale parameter.
func parseReplicas(params map[string]string) (int, error) {
	text := strings.TrimSpace(params[ParamReplicas])
	if text == "" {
		return 0, errs.New(errs.Config, opPlan, "scale needs a replicas parameter")
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 {
		return 0, errs.New(errs.Config, opPlan, fmt.Sprintf("invalid replicas %q", text))
	}
	return n, nil
}

// drainParameters is the validated form of the drain parameters.
type drainParameters struct {
	grace              int
	deleteEmptyDirData bool
	ignoreDaemonsets   bool
}

// drainParams validates the drain parameters.
func drainParams(params map[string]string) (drainParameters, error) {
	out := drainParameters{grace: defaultGrace}
	if text := strings.TrimSpace(params[ParamGrace]); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 0 {
			return out, errs.New(errs.Config, opPlan, fmt.Sprintf("invalid grace %q", text))
		}
		out.grace = n
	}
	for _, p := range []struct {
		name string
		set  *bool
	}{
		{ParamDeleteEmptyDirData, &out.deleteEmptyDirData},
		{ParamIgnoreDaemonsets, &out.ignoreDaemonsets},
	} {
		switch value := strings.TrimSpace(params[p.name]); value {
		case "", noParam:
		case yesParam:
			*p.set = true
		default:
			return out, errs.New(errs.Config, opPlan,
				fmt.Sprintf("%s takes yes or no, not %q", p.name, value))
		}
	}
	return out, nil
}

// deletePreview says what deleting each object does: for a pod, who owns it
// and whether it comes back; for anything else, that it and its pods go.
func (b *Backend) deletePreview(ctx context.Context, cs kubernetes.Interface, planned []planTarget) (string, error) {
	lines := make([]string, 0, len(planned))
	for _, pt := range planned {
		subject := string(pt.def.spec.Name) + " " + pt.ref.Name
		if pt.def.namespaced && pt.ref.Namespace != "" {
			subject = pt.ref.Namespace + "/" + pt.ref.Name
		}
		if string(pt.def.spec.Name) != "pod" {
			lines = append(lines, fmt.Sprintf("%s %s: it and its pods will be gone", pt.def.spec.Name, subject))
			continue
		}
		pod, err := cs.CoreV1().Pods(pt.ref.Namespace).Get(ctx, pt.ref.Name, metav1.GetOptions{})
		if err != nil {
			return "", planError(err)
		}
		lines = append(lines, "pod "+subject+": "+ownerText(pod))
	}
	return strings.Join(lines, "\n"), nil
}

// ownerText is what a delete preview says about a pod's controller.
func ownerText(pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return fmt.Sprintf("controlled by %s %s — it will be recreated", ref.Kind, ref.Name)
		}
	}
	return "bare pod: it will be gone"
}

// scalePreview compares the workload's current replicas with the new count.
func (b *Backend) scalePreview(ctx context.Context, cs kubernetes.Interface, planned []planTarget, replicas int) (string, error) {
	lines := make([]string, 0, len(planned))
	for _, pt := range planned {
		current, err := workloadReplicas(ctx, cs, pt.ref)
		if err != nil {
			return "", planError(err)
		}
		lines = append(lines, fmt.Sprintf("%s %s: current %d → new %d replicas",
			pt.def.spec.Name, pt.ref.Name, current, replicas))
	}
	return strings.Join(lines, "\n"), nil
}

// workloadReplicas reads spec.replicas of a deployment or statefulset.
func workloadReplicas(ctx context.Context, cs kubernetes.Interface, ref resource.Ref) (int32, error) {
	switch string(ref.Kind) {
	case "deployment":
		dep, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		if dep.Spec.Replicas == nil {
			return 1, nil
		}
		return *dep.Spec.Replicas, nil
	case "statefulset":
		sts, err := cs.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		if sts.Spec.Replicas == nil {
			return 1, nil
		}
		return *sts.Spec.Replicas, nil
	}
	return 0, errs.New(errs.Config, opPlan, fmt.Sprintf("%s has no replica count", ref.Kind))
}

// rolloutPreview says how many pods the restart touches and which strategy the
// workload uses.
func (b *Backend) rolloutPreview(ctx context.Context, cs kubernetes.Interface, planned []planTarget) (string, error) {
	lines := make([]string, 0, len(planned))
	for _, pt := range planned {
		selector, strategy, err := workloadStrategy(ctx, cs, pt.ref)
		if err != nil {
			return "", planError(err)
		}
		affected, err := countPods(ctx, cs, pt.ref.Namespace, selector)
		if err != nil {
			return "", planError(err)
		}
		lines = append(lines, fmt.Sprintf("%s %s: %d pods affected; strategy %s",
			pt.def.spec.Name, pt.ref.Name, affected, strategy))
	}
	return strings.Join(lines, "\n"), nil
}

// workloadStrategy reads the pod selector and update strategy of a workload.
func workloadStrategy(ctx context.Context, cs kubernetes.Interface, ref resource.Ref) (*metav1.LabelSelector, string, error) {
	switch string(ref.Kind) {
	case "deployment":
		dep, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, "", err
		}
		strategy := string(dep.Spec.Strategy.Type)
		if strategy == "" {
			strategy = "RollingUpdate"
		}
		return dep.Spec.Selector, strategy, nil
	case "statefulset":
		sts, err := cs.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, "", err
		}
		strategy := string(sts.Spec.UpdateStrategy.Type)
		if strategy == "" {
			strategy = "RollingUpdate"
		}
		return sts.Spec.Selector, strategy, nil
	case "daemonset":
		ds, err := cs.AppsV1().DaemonSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, "", err
		}
		strategy := string(ds.Spec.UpdateStrategy.Type)
		if strategy == "" {
			strategy = "RollingUpdate"
		}
		return ds.Spec.Selector, strategy, nil
	}
	return nil, "", errs.New(errs.Config, opPlan, fmt.Sprintf("%s cannot be restarted", ref.Kind))
}

// countPods counts the pods in namespace that match selector.
func countPods(ctx context.Context, cs kubernetes.Interface, namespace string, selector *metav1.LabelSelector) (int, error) {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return 0, errs.New(errs.Parse, opPlan, "workload selector does not parse")
	}
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, pod := range pods.Items {
		if sel.Matches(labels.Set(pod.Labels)) {
			n++
		}
	}
	return n, nil
}

// drainClass is what a drain would do with one pod.
type drainClass int

const (
	// drainEvict: the pod would be evicted.
	drainEvict drainClass = iota
	// drainSkip: a daemonset or mirror pod the drain leaves alone.
	drainSkip
	// drainBlock: a pod the drain cannot evict.
	drainBlock
)

// drainCandidate is one pod of a drain and what happens to it.
type drainCandidate struct {
	pod    *corev1.Pod
	class  drainClass
	reason string
}

// drainPreview lists the pods on the node and runs one server-side dry-run
// eviction per evictable pod. The evictions the API server refuses are the
// blocking ones, with the reason it gave.
func (b *Backend) drainPreview(ctx context.Context, cs kubernetes.Interface, pt planTarget, params drainParameters) (string, []string, error) {
	pods, err := podsOnNode(ctx, cs, pt.ref.Name)
	if err != nil {
		return "", nil, planError(err)
	}
	candidates := classifyDrainPods(pods, params)
	if !params.ignoreDaemonsets && countClass(candidates, drainSkip) > 0 {
		for _, c := range candidates {
			if c.class == drainSkip && c.reason == "daemonset pod" {
				return "", nil, errs.New(errs.Config, opPlan,
					"node runs daemonset pods; pass ignore-daemonsets=yes to leave them running")
			}
		}
	}
	for i := range candidates {
		c := &candidates[i]
		if c.class != drainEvict {
			continue
		}
		if err := evictPod(ctx, cs, c.pod, params.grace, true); err != nil {
			c.class = drainBlock
			if apierrors.IsTooManyRequests(err) {
				c.reason = "PodDisruptionBudget"
			} else {
				c.reason = oneLine(err.Error())
			}
		} else {
			c.reason = "would be evicted"
		}
	}

	var warnings []string
	if blocked := countClass(candidates, drainBlock); blocked > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d %s block the drain; evictions retry for up to %s",
			blocked, plural(blocked, "pod", "pods"), drainTimeout))
	}
	if skipped := countClass(candidates, drainSkip); skipped > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d %s skipped (daemonsets and mirror pods)", skipped, plural(skipped, "pod", "pods")))
	}
	return drainPreviewText(candidates), warnings, nil
}

// podsOnNode lists every pod scheduled on node. The list is filtered here
// because a field selector is not honoured by every API-server front end.
func podsOnNode(ctx context.Context, cs kubernetes.Interface, node string) ([]corev1.Pod, error) {
	pods, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]corev1.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == node {
			out = append(out, pod)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// classifyDrainPods decides what a drain does with each pod before any
// eviction: mirror pods and daemonset pods are skipped, bare pods and pods with
// emptyDir volumes block unless the parameters allow otherwise.
func classifyDrainPods(pods []corev1.Pod, params drainParameters) []drainCandidate {
	out := make([]drainCandidate, 0, len(pods))
	for i := range pods {
		pod := pods[i]
		c := drainCandidate{pod: &pod, class: drainEvict}
		switch {
		case pod.Annotations[mirrorAnnotation] != "":
			c.class, c.reason = drainSkip, "mirror pod"
		case ownedBy(pod, "DaemonSet"):
			c.class, c.reason = drainSkip, "daemonset pod"
		case !hasController(pod):
			c.class, c.reason = drainBlock, "bare pod: no controller, it will not be evicted"
		case hasEmptyDir(pod) && !params.deleteEmptyDirData:
			c.class, c.reason = drainBlock, "local storage (emptyDir); pass delete-emptydir-data=yes"
		}
		out = append(out, c)
	}
	return out
}

// hasController reports whether a controller owns the pod.
func hasController(pod corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// ownedBy reports whether a controller of kind owns the pod.
func ownedBy(pod corev1.Pod, kind string) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == kind && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// hasEmptyDir reports whether the pod mounts an emptyDir volume.
func hasEmptyDir(pod corev1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.EmptyDir != nil {
			return true
		}
	}
	return false
}

// countClass counts the candidates of one class.
func countClass(candidates []drainCandidate, class drainClass) int {
	n := 0
	for _, c := range candidates {
		if c.class == class {
			n++
		}
	}
	return n
}

// drainPreviewText renders the grouped preview of a drain.
func drainPreviewText(candidates []drainCandidate) string {
	var text strings.Builder
	writeGroup := func(title string, class drainClass, withReason bool) {
		var lines []string
		for _, c := range candidates {
			if c.class != class {
				continue
			}
			line := "  " + c.pod.Namespace + "/" + c.pod.Name
			if withReason && c.reason != "" {
				line += " (" + c.reason + ")"
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			return
		}
		if text.Len() > 0 {
			text.WriteString("\n")
		}
		text.WriteString(fmt.Sprintf("%s (%d):\n%s", title, len(lines), strings.Join(lines, "\n")))
	}
	writeGroup("would evict", drainEvict, false)
	writeGroup("skipped", drainSkip, false)
	writeGroup("blocked", drainBlock, true)
	if text.Len() == 0 {
		return "no pods on the node"
	}
	return text.String()
}

// planError wraps a client-go failure of Plan.
func planError(err error) error {
	return &errs.Error{Kind: Classify(err), Op: opPlan, Msg: oneLine(err.Error()), Err: err}
}

// plural picks the singular or plural word.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// evictPod asks the API server to evict one pod. With dryRun the request is a
// server-side dry run: the eviction admission (PDBs included) runs, nothing is
// deleted.
func evictPod(ctx context.Context, cs kubernetes.Interface, pod *corev1.Pod, grace int, dryRun bool) error {
	seconds := int64(grace)
	opts := &metav1.DeleteOptions{GracePeriodSeconds: &seconds}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	return cs.PolicyV1().Evictions(pod.Namespace).Evict(ctx, &policyv1.Eviction{
		ObjectMeta:    metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		DeleteOptions: opts,
	})
}

// evictWithRetry evicts one pod, retrying a 429 (a PDB refuses right now) with
// a growing delay until ctx ends. Any other failure is returned at once.
func evictWithRetry(ctx context.Context, cs kubernetes.Interface, pod *corev1.Pod, grace int) error {
	delay := evictDelay
	for {
		err := evictPod(ctx, cs, pod, grace, false)
		if err == nil {
			return nil
		}
		if !apierrors.IsTooManyRequests(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return &errs.Error{Kind: errs.Timeout, Op: opDrain,
				Msg: "pods stayed blocked by a PodDisruptionBudget", Err: err}
		case <-time.After(delay):
		}
		if delay < evictMaxDelay {
			delay *= 2
			if delay > evictMaxDelay {
				delay = evictMaxDelay
			}
		}
	}
}

// stepResult is the shell every Run starts from.
func stepResult(target action.Target) action.StepResult {
	return action.StepResult{Target: target}
}

// fail fills a result with an error.
func fail(res action.StepResult, op string, start time.Time, err error) action.StepResult {
	res.RC = 1
	res.Duration = time.Since(start)
	res.Err = (&errs.Error{Kind: Classify(err), Op: op, Msg: oneLine(err.Error()), Err: err}).
		WithHost(string(res.Target.Host))
	res.Output = oneLine(err.Error())
	return res
}

// runDelete deletes one object.
func (b *Backend) runDelete(ctx context.Context, pt planTarget) action.StepResult {
	res := stepResult(pt.target)
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	cs, err := b.liveClientset()
	if err != nil {
		return fail(res, opDelete, start, err)
	}
	if err := deleteObject(cctx, cs, pt.ref); err != nil {
		return fail(res, opDelete, start, err)
	}
	res.Duration = time.Since(start)
	res.Output = fmt.Sprintf("deleted %s/%s", pt.ref.Kind, pt.ref.Name)
	return res
}

// deleteObject deletes one object of a known kind.
func deleteObject(ctx context.Context, cs kubernetes.Interface, ref resource.Ref) error {
	ns := ref.Namespace
	switch string(ref.Kind) {
	case "pod":
		return cs.CoreV1().Pods(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "service":
		return cs.CoreV1().Services(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "endpoints":
		return cs.CoreV1().Endpoints(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "configmap":
		return cs.CoreV1().ConfigMaps(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "secret":
		return cs.CoreV1().Secrets(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "event":
		return cs.CoreV1().Events(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "persistentvolumeclaim":
		return cs.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "persistentvolume":
		return cs.CoreV1().PersistentVolumes().Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "namespace":
		return cs.CoreV1().Namespaces().Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "deployment":
		return cs.AppsV1().Deployments(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "statefulset":
		return cs.AppsV1().StatefulSets(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "daemonset":
		return cs.AppsV1().DaemonSets(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "job":
		return cs.BatchV1().Jobs(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "cronjob":
		return cs.BatchV1().CronJobs(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	case "ingress":
		return cs.NetworkingV1().Ingresses(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	}
	return errs.New(errs.NotAvailable, opDelete, fmt.Sprintf("cannot delete %s", ref.Kind))
}

// runScale patches a workload's replica count.
func (b *Backend) runScale(ctx context.Context, pt planTarget, replicas int) action.StepResult {
	res := stepResult(pt.target)
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	cs, err := b.liveClientset()
	if err != nil {
		return fail(res, opScale, start, err)
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	var patchErr error
	switch string(pt.ref.Kind) {
	case "deployment":
		_, patchErr = cs.AppsV1().Deployments(pt.ref.Namespace).Patch(
			cctx, pt.ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "statefulset":
		_, patchErr = cs.AppsV1().StatefulSets(pt.ref.Namespace).Patch(
			cctx, pt.ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	default:
		patchErr = errs.New(errs.Config, opScale, fmt.Sprintf("%s cannot scale", pt.ref.Kind))
	}
	if patchErr != nil {
		return fail(res, opScale, start, patchErr)
	}
	res.Duration = time.Since(start)
	res.Output = fmt.Sprintf("scaled %s/%s to %d replicas", pt.ref.Kind, pt.ref.Name, replicas)
	return res
}

// runRolloutRestart patches the restartedAt annotation of the pod template.
func (b *Backend) runRolloutRestart(ctx context.Context, pt planTarget) action.StepResult {
	res := stepResult(pt.target)
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	cs, err := b.liveClientset()
	if err != nil {
		return fail(res, opRollout, start, err)
	}
	when := time.Now().UTC()
	if b.now != nil {
		when = b.now().UTC()
	}
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		restartAnnotation, when.Format(time.RFC3339)))
	var patchErr error
	switch string(pt.ref.Kind) {
	case "deployment":
		_, patchErr = cs.AppsV1().Deployments(pt.ref.Namespace).Patch(
			cctx, pt.ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "statefulset":
		_, patchErr = cs.AppsV1().StatefulSets(pt.ref.Namespace).Patch(
			cctx, pt.ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "daemonset":
		_, patchErr = cs.AppsV1().DaemonSets(pt.ref.Namespace).Patch(
			cctx, pt.ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	default:
		patchErr = errs.New(errs.Config, opRollout, fmt.Sprintf("%s cannot be restarted", pt.ref.Kind))
	}
	if patchErr != nil {
		return fail(res, opRollout, start, patchErr)
	}
	res.Duration = time.Since(start)
	res.Output = fmt.Sprintf("restarted %s/%s", pt.ref.Kind, pt.ref.Name)
	return res
}

// runNodeUnschedulable patches spec.unschedulable of a node.
func (b *Backend) runNodeUnschedulable(ctx context.Context, pt planTarget, unschedulable bool) action.StepResult {
	op := opCordon
	past := "cordoned"
	if !unschedulable {
		op, past = opUncordon, "uncordoned"
	}
	res := stepResult(pt.target)
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	cs, err := b.liveClientset()
	if err != nil {
		return fail(res, op, start, err)
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, unschedulable))
	if _, err := cs.CoreV1().Nodes().Patch(cctx, pt.ref.Name, types.MergePatchType, patch,
		metav1.PatchOptions{}); err != nil {
		return fail(res, op, start, err)
	}
	res.Duration = time.Since(start)
	res.Output = fmt.Sprintf("%s node/%s", past, pt.ref.Name)
	return res
}

// runDrain cords the node and evicts its pods: daemonset and mirror pods are
// skipped, bare pods and pods with emptyDir volumes (unless allowed) are
// reported as blocking, and every other pod is evicted with a 429 retry until
// the step timeout. Nothing is force-deleted.
func (b *Backend) runDrain(ctx context.Context, pt planTarget, params drainParameters) action.StepResult {
	res := stepResult(pt.target)
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()

	cs, err := b.liveClientset()
	if err != nil {
		return fail(res, opDrain, start, err)
	}
	if err := cordonNode(cctx, cs, pt.ref.Name); err != nil {
		return fail(res, opDrain, start, err)
	}
	pods, err := podsOnNode(cctx, cs, pt.ref.Name)
	if err != nil {
		return fail(res, opDrain, start, err)
	}
	candidates := classifyDrainPods(pods, params)

	var out []string
	out = append(out, "cordoned node/"+pt.ref.Name)
	blocked := 0
	var firstErr error
	for _, c := range candidates {
		name := c.pod.Namespace + "/" + c.pod.Name
		switch c.class {
		case drainSkip:
			out = append(out, fmt.Sprintf("skipped %s (%s)", name, c.reason))
		case drainBlock:
			blocked++
			out = append(out, fmt.Sprintf("blocked %s (%s)", name, c.reason))
		default:
			if err := evictWithRetry(cctx, cs, c.pod, params.grace); err != nil {
				blocked++
				if firstErr == nil {
					firstErr = err
				}
				out = append(out, fmt.Sprintf("failed to evict %s: %s", name, oneLine(err.Error())))
				continue
			}
			out = append(out, "evicted "+name)
		}
	}
	res.Output = strings.Join(out, "\n")
	res.Duration = time.Since(start)
	if blocked > 0 {
		res.RC = 1
		if firstErr == nil {
			firstErr = errs.New(errs.Remote, opDrain,
				fmt.Sprintf("%d %s could not be evicted", blocked, plural(blocked, "pod", "pods")))
		}
		res.Err = (&errs.Error{Kind: errs.KindOf(firstErr), Op: opDrain,
			Msg: oneLine(firstErr.Error()), Err: firstErr}).WithHost(string(res.Target.Host))
	}
	return res
}

// cordonNode marks a node unschedulable, before any eviction.
func cordonNode(ctx context.Context, cs kubernetes.Interface, node string) error {
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	_, err := cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
