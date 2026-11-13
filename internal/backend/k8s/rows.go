package k8s

import (
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"jumper/internal/resource"
)

// Prototypes for the typed informers. One per kind, so the informer knows the
// shape of what it stores.

func podPrototype() runtime.Object         { return &corev1.Pod{} }
func deploymentPrototype() runtime.Object  { return &appsv1.Deployment{} }
func statefulSetPrototype() runtime.Object { return &appsv1.StatefulSet{} }
func daemonSetPrototype() runtime.Object   { return &appsv1.DaemonSet{} }
func jobPrototype() runtime.Object         { return &batchv1.Job{} }
func cronJobPrototype() runtime.Object     { return &batchv1.CronJob{} }
func nodePrototype() runtime.Object        { return &corev1.Node{} }
func servicePrototype() runtime.Object     { return &corev1.Service{} }
func endpointsPrototype() runtime.Object   { return &corev1.Endpoints{} }
func ingressPrototype() runtime.Object     { return &networkingv1.Ingress{} }
func configMapPrototype() runtime.Object   { return &corev1.ConfigMap{} }
func eventPrototype() runtime.Object       { return &corev1.Event{} }
func namespacePrototype() runtime.Object   { return &corev1.Namespace{} }
func pvcPrototype() runtime.Object         { return &corev1.PersistentVolumeClaim{} }
func pvPrototype() runtime.Object          { return &corev1.PersistentVolume{} }

// newRow is the shell every row builder starts from: the Ref the whole UI
// keys on, the labels the selector filters and the raw object detail views
// use.
func newRow(def kindDef, obj metav1.Object, labels map[string]string, now time.Time) resource.Resource {
	ref := resource.Ref{
		Backend: backendName,
		Kind:    def.spec.Name,
		Name:    obj.GetName(),
	}
	if def.namespaced {
		ref.Namespace = obj.GetNamespace()
	}
	return resource.Resource{
		Ref:       ref,
		Fields:    make(map[string]any, len(def.spec.Columns)),
		Labels:    labels,
		Raw:       obj,
		FetchedAt: now,
	}
}

// put sets a text field, leaving it absent when the value is empty so the
// table renders its placeholder rather than a blank cell.
func put(r *resource.Resource, id, value string) {
	if value != "" {
		r.Fields[id] = value
	}
}

// ageSince is the AGE field: how long ago t was.
func ageSince(t, now time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	if d := now.Sub(t); d > 0 {
		return d
	}
	return 0
}

// count is kubelet's "ready/desired" cell.
func count(ready, total int) string { return fmt.Sprintf("%d/%d", ready, total) }

// workloadColour colours a workload by how many replicas are ready.
func workloadColour(ready, desired int32) resource.Status {
	switch {
	case desired <= 0 && ready <= 0:
		return resource.StatusOK
	case ready >= desired:
		return resource.StatusOK
	case ready == 0:
		return resource.StatusCrit
	}
	return resource.StatusWarn
}

// podRow builds the pod row from field-for-field kubectl's `get pods -o wide`
// essentials.
func podRow(o any, now time.Time) resource.Resource {
	pod, ok := o.(*corev1.Pod)
	if !ok || pod == nil {
		return resource.Resource{}
	}
	reason, ready, total, restarts := podStatus(pod)
	r := newRow(mustDef("pod"), pod, pod.Labels, now)
	put(&r, "namespace", pod.Namespace)
	put(&r, "name", pod.Name)
	put(&r, "ready", count(ready, total))
	put(&r, "status", reason)
	r.Fields["restarts"] = int64(restarts)
	r.Fields["age"] = ageSince(pod.CreationTimestamp.Time, now)
	put(&r, "ip", pod.Status.PodIP)
	put(&r, "node", pod.Spec.NodeName)
	r.Status = podColour(reason, ready, total)
	return r
}

// podStatus is kubectl's pod STATUS computation (printer/printers.go
// printPod): init containers, waiting and terminated reasons, the
// "Completed but a sidecar still runs" fix-up and the deletion timestamp.
// It returns the status text, the running containers, the container total and
// the restart count.
func podStatus(pod *corev1.Pod) (string, int, int, int) {
	restarts := 0
	total := len(pod.Spec.Containers)
	ready := 0

	reason := string(pod.Status.Phase)
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}

	initializing := false
	for i := range pod.Status.InitContainerStatuses {
		c := pod.Status.InitContainerStatuses[i]
		restarts += int(c.RestartCount)
		switch {
		case c.State.Terminated != nil && c.State.Terminated.ExitCode == 0:
			// This init container completed; look at the next one.
			continue
		case c.State.Terminated != nil:
			switch {
			case c.State.Terminated.Reason != "":
				reason = "Init:" + c.State.Terminated.Reason
			case c.State.Terminated.Signal != 0:
				reason = fmt.Sprintf("Init:Signal:%d", c.State.Terminated.Signal)
			default:
				reason = fmt.Sprintf("Init:ExitCode:%d", c.State.Terminated.ExitCode)
			}
			initializing = true
		case c.State.Waiting != nil && c.State.Waiting.Reason != "" &&
			c.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + c.State.Waiting.Reason
			initializing = true
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(pod.Spec.InitContainers))
			initializing = true
		}
		break
	}

	if !initializing || podInitialized(pod) {
		restarts = 0
		hasRunning := false
		for i := len(pod.Status.ContainerStatuses) - 1; i >= 0; i-- {
			c := pod.Status.ContainerStatuses[i]
			restarts += int(c.RestartCount)
			switch {
			case c.State.Waiting != nil && c.State.Waiting.Reason != "":
				reason = c.State.Waiting.Reason
			case c.State.Terminated != nil && c.State.Terminated.Reason != "":
				reason = c.State.Terminated.Reason
			case c.State.Terminated != nil:
				if c.State.Terminated.Signal != 0 {
					reason = fmt.Sprintf("Signal:%d", c.State.Terminated.Signal)
				} else {
					reason = fmt.Sprintf("ExitCode:%d", c.State.Terminated.ExitCode)
				}
			case c.State.Running != nil:
				hasRunning = true
				ready++
			}
		}
		// A Completed pod with a running sidecar is Running.
		if reason == "Completed" && hasRunning {
			reason = "Running"
		}
	}

	if !pod.ObjectMeta.DeletionTimestamp.IsZero() {
		switch {
		case pod.Status.Reason == "NodeLost", pod.Status.Phase == corev1.PodUnknown:
			reason = "Unknown"
		default:
			reason = "Terminating"
		}
	}

	if total == 0 {
		total = len(pod.Status.ContainerStatuses)
	}
	if total < ready {
		total = ready
	}
	return reason, ready, total, restarts
}

// podInitialized is kubectl's isPodInitializedConditionTrue.
func podInitialized(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded {
		return true
	}
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodInitialized {
			continue
		}
		return c.Status == corev1.ConditionTrue
	}
	return false
}

// criticalReasons are the pod states that need a human now.
var criticalReasons = []string{
	"CrashLoopBackOff",
	"OOMKilled",
	"ImagePullBackOff",
	"ErrImagePull",
	"RunContainerError",
	"CreateContainerError",
	"CreateContainerConfigError",
	"InvalidImageName",
	"Error",
	"DeadlineExceeded",
}

// podColour maps the computed status onto the row colour: anything not
// Ready/Running is Warn, the fatal ones are Crit (SPEC §8.2, task 106).
func podColour(reason string, ready, total int) resource.Status {
	for _, bad := range criticalReasons {
		if strings.Contains(reason, bad) {
			return resource.StatusCrit
		}
	}
	switch {
	case reason == "Running" && ready >= total:
		return resource.StatusOK
	case reason == "Completed", reason == "Succeeded":
		return resource.StatusOK
	}
	return resource.StatusWarn
}

// deploymentRow builds the deployment row.
func deploymentRow(o any, now time.Time) resource.Resource {
	d, ok := o.(*appsv1.Deployment)
	if !ok || d == nil {
		return resource.Resource{}
	}
	var desired int32
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	r := newRow(mustDef("deployment"), d, d.Labels, now)
	put(&r, "namespace", d.Namespace)
	put(&r, "name", d.Name)
	put(&r, "ready", count(int(d.Status.ReadyReplicas), int(desired)))
	r.Fields["up_to_date"] = int64(d.Status.UpdatedReplicas)
	r.Fields["available"] = int64(d.Status.AvailableReplicas)
	r.Fields["age"] = ageSince(d.CreationTimestamp.Time, now)
	r.Status = workloadColour(d.Status.ReadyReplicas, desired)
	return r
}

// statefulSetRow builds the statefulset row.
func statefulSetRow(o any, now time.Time) resource.Resource {
	s, ok := o.(*appsv1.StatefulSet)
	if !ok || s == nil {
		return resource.Resource{}
	}
	var desired int32
	if s.Spec.Replicas != nil {
		desired = *s.Spec.Replicas
	}
	r := newRow(mustDef("statefulset"), s, s.Labels, now)
	put(&r, "namespace", s.Namespace)
	put(&r, "name", s.Name)
	put(&r, "ready", count(int(s.Status.ReadyReplicas), int(desired)))
	r.Fields["age"] = ageSince(s.CreationTimestamp.Time, now)
	r.Status = workloadColour(s.Status.ReadyReplicas, desired)
	return r
}

// daemonSetRow builds the daemonset row.
func daemonSetRow(o any, now time.Time) resource.Resource {
	ds, ok := o.(*appsv1.DaemonSet)
	if !ok || ds == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("daemonset"), ds, ds.Labels, now)
	put(&r, "namespace", ds.Namespace)
	put(&r, "name", ds.Name)
	r.Fields["desired"] = int64(ds.Status.DesiredNumberScheduled)
	r.Fields["current"] = int64(ds.Status.CurrentNumberScheduled)
	r.Fields["ready"] = int64(ds.Status.NumberReady)
	r.Fields["up_to_date"] = int64(ds.Status.UpdatedNumberScheduled)
	r.Fields["available"] = int64(ds.Status.NumberAvailable)
	put(&r, "node_selector", selectorText(ds.Spec.Template.Spec.NodeSelector))
	r.Fields["age"] = ageSince(ds.CreationTimestamp.Time, now)
	r.Status = workloadColour(ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
	return r
}

// jobRow builds the job row.
func jobRow(o any, now time.Time) resource.Resource {
	j, ok := o.(*batchv1.Job)
	if !ok || j == nil {
		return resource.Resource{}
	}
	completions := int32(1)
	if j.Spec.Completions != nil {
		completions = *j.Spec.Completions
	}
	r := newRow(mustDef("job"), j, j.Labels, now)
	put(&r, "namespace", j.Namespace)
	put(&r, "name", j.Name)
	put(&r, "completions", count(int(j.Status.Succeeded), int(completions)))
	switch {
	case j.Status.CompletionTime != nil && j.Status.StartTime != nil:
		r.Fields["duration"] = j.Status.CompletionTime.Sub(j.Status.StartTime.Time)
	case j.Status.StartTime != nil:
		r.Fields["duration"] = ageSince(j.Status.StartTime.Time, now)
	}
	r.Fields["age"] = ageSince(j.CreationTimestamp.Time, now)
	switch {
	case j.Status.Failed > 0:
		r.Status = resource.StatusCrit
	case j.Status.Succeeded >= completions:
		r.Status = resource.StatusOK
	default:
		r.Status = resource.StatusWarn
	}
	return r
}

// cronJobRow builds the cronjob row.
func cronJobRow(o any, now time.Time) resource.Resource {
	c, ok := o.(*batchv1.CronJob)
	if !ok || c == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("cronjob"), c, c.Labels, now)
	put(&r, "namespace", c.Namespace)
	put(&r, "name", c.Name)
	put(&r, "schedule", c.Spec.Schedule)
	suspend := "False"
	if c.Spec.Suspend != nil && *c.Spec.Suspend {
		suspend = "True"
	}
	put(&r, "suspend", suspend)
	r.Fields["active"] = int64(len(c.Status.Active))
	if t := lastSchedule(c.Status.LastScheduleTime); !t.IsZero() {
		r.Fields["last_schedule"] = ageSince(t, now)
	}
	r.Fields["age"] = ageSince(c.CreationTimestamp.Time, now)
	if suspend == "True" {
		r.Status = resource.StatusNA
	} else {
		r.Status = resource.StatusOK
	}
	return r
}

// lastSchedule is the most recent time the controller scheduled a run.
func lastSchedule(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

// nodeRow builds the node row.
func nodeRow(o any, now time.Time) resource.Resource {
	n, ok := o.(*corev1.Node)
	if !ok || n == nil {
		return resource.Resource{}
	}
	status, colour := nodeStatus(n)
	r := newRow(mustDef("node"), n, n.Labels, now)
	put(&r, "name", n.Name)
	put(&r, "status", status)
	put(&r, "roles", nodeRoles(n))
	r.Fields["age"] = ageSince(n.CreationTimestamp.Time, now)
	put(&r, "version", n.Status.NodeInfo.KubeletVersion)
	put(&r, "internal_ip", nodeAddress(n, corev1.NodeInternalIP))
	r.Status = colour
	return r
}

// nodeStatus is the Ready condition, as kubectl prints it.
func nodeStatus(n *corev1.Node) (string, resource.Status) {
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		switch c.Status {
		case corev1.ConditionTrue:
			return "Ready", resource.StatusOK
		case corev1.ConditionFalse:
			return "NotReady", resource.StatusCrit
		}
		return "Unknown", resource.StatusWarn
	}
	return "Unknown", resource.StatusWarn
}

// nodeAddress returns the first address of one type.
func nodeAddress(n *corev1.Node, t corev1.NodeAddressType) string {
	for _, a := range n.Status.Addresses {
		if a.Type == t {
			return a.Address
		}
	}
	return ""
}

// nodeRoles renders the node-role.kubernetes.io/* labels, "<none>" when there
// are none, as kubectl does.
func nodeRoles(n *corev1.Node) string {
	const prefix = "node-role.kubernetes.io/"
	var roles []string
	for k := range n.Labels {
		if role, ok := strings.CutPrefix(k, prefix); ok && role != "" {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return "<none>"
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

// serviceRow builds the service row.
func serviceRow(o any, now time.Time) resource.Resource {
	s, ok := o.(*corev1.Service)
	if !ok || s == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("service"), s, s.Labels, now)
	put(&r, "namespace", s.Namespace)
	put(&r, "name", s.Name)
	put(&r, "type", string(s.Spec.Type))
	put(&r, "cluster_ip", s.Spec.ClusterIP)
	put(&r, "external_ip", serviceExternalIP(s))
	put(&r, "ports", servicePorts(s))
	r.Fields["age"] = ageSince(s.CreationTimestamp.Time, now)
	r.Status = resource.StatusOK
	return r
}

// serviceExternalIP is the load balancer address, else the spec's external
// IPs.
func serviceExternalIP(s *corev1.Service) string {
	var out []string
	for _, in := range s.Status.LoadBalancer.Ingress {
		switch {
		case in.IP != "":
			out = append(out, in.IP)
		case in.Hostname != "":
			out = append(out, in.Hostname)
		}
	}
	out = append(out, s.Spec.ExternalIPs...)
	return strings.Join(out, ",")
}

// servicePorts renders "80/TCP,443/TCP" and the node ports.
func servicePorts(s *corev1.Service) string {
	parts := make([]string, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		proto := string(p.Protocol)
		if proto == "" {
			proto = string(corev1.ProtocolTCP)
		}
		part := fmt.Sprintf("%d/%s", p.Port, proto)
		if p.NodePort != 0 {
			part = fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, proto)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

// endpointsRow builds the endpoints row: every address, port included.
func endpointsRow(o any, now time.Time) resource.Resource {
	e, ok := o.(*corev1.Endpoints)
	if !ok || e == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("endpoints"), e, e.Labels, now)
	put(&r, "namespace", e.Namespace)
	put(&r, "name", e.Name)
	var eps []string
	for _, subset := range e.Subsets {
		for _, a := range subset.Addresses {
			if len(subset.Ports) == 0 {
				eps = append(eps, a.IP)
				continue
			}
			for _, p := range subset.Ports {
				eps = append(eps, fmt.Sprintf("%s:%d", a.IP, p.Port))
			}
		}
	}
	put(&r, "endpoints", strings.Join(eps, ","))
	r.Fields["age"] = ageSince(e.CreationTimestamp.Time, now)
	if len(eps) == 0 {
		r.Status = resource.StatusWarn
	} else {
		r.Status = resource.StatusOK
	}
	return r
}

// ingressRow builds the ingress row.
func ingressRow(o any, now time.Time) resource.Resource {
	ing, ok := o.(*networkingv1.Ingress)
	if !ok || ing == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("ingress"), ing, ing.Labels, now)
	put(&r, "namespace", ing.Namespace)
	put(&r, "name", ing.Name)
	class := ""
	if ing.Spec.IngressClassName != nil {
		class = *ing.Spec.IngressClassName
	}
	if class == "" {
		class = ing.Annotations["kubernetes.io/ingress.class"]
	}
	put(&r, "class", class)
	var hosts []string
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			hosts = append(hosts, rule.Host)
		}
	}
	put(&r, "hosts", strings.Join(hosts, ","))
	put(&r, "address", ingressAddress(ing))
	put(&r, "ports", ingressPorts(ing))
	r.Fields["age"] = ageSince(ing.CreationTimestamp.Time, now)
	r.Status = resource.StatusOK
	return r
}

// ingressAddress is the load balancer address kubectl prints.
func ingressAddress(ing *networkingv1.Ingress) string {
	var out []string
	for _, in := range ing.Status.LoadBalancer.Ingress {
		switch {
		case in.IP != "":
			out = append(out, in.IP)
		case in.Hostname != "":
			out = append(out, in.Hostname)
		}
	}
	return strings.Join(out, ",")
}

// ingressPorts is the HTTP and HTTPS ports the rules route to.
func ingressPorts(ing *networkingv1.Ingress) string {
	seen := map[int32]bool{}
	var ports []int32
	add := func(p int32) {
		if p != 0 && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service != nil {
				add(path.Backend.Service.Port.Number)
			}
		}
	}
	if len(ports) == 0 {
		return ""
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = fmt.Sprintf("%d", p)
	}
	return strings.Join(out, ",")
}

// configMapRow builds the configmap row. The keys are counted; no value ever
// enters Fields.
func configMapRow(o any, now time.Time) resource.Resource {
	cm, ok := o.(*corev1.ConfigMap)
	if !ok || cm == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("configmap"), cm, cm.Labels, now)
	put(&r, "namespace", cm.Namespace)
	put(&r, "name", cm.Name)
	r.Fields["data"] = int64(len(cm.Data) + len(cm.BinaryData))
	r.Fields["age"] = ageSince(cm.CreationTimestamp.Time, now)
	r.Status = resource.StatusOK
	return r
}

// secretRow builds the secret row from metadata only: the metadata informer
// never receives the data, so it cannot leak into Fields, a log or a
// recording.
func secretRow(o any, now time.Time) resource.Resource {
	m, ok := o.(*metav1.PartialObjectMetadata)
	if !ok || m == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("secret"), m, m.Labels, now)
	put(&r, "namespace", m.Namespace)
	put(&r, "name", m.Name)
	r.Fields["age"] = ageSince(m.CreationTimestamp.Time, now)
	r.Status = resource.StatusOK
	return r
}

// eventRow builds the event row.
func eventRow(o any, now time.Time) resource.Resource {
	e, ok := o.(*corev1.Event)
	if !ok || e == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("event"), e, e.Labels, now)
	put(&r, "namespace", e.Namespace)
	put(&r, "type", e.Type)
	put(&r, "reason", e.Reason)
	if name := involvedObject(e.InvolvedObject); name != "" {
		put(&r, "object", name)
	}
	r.Fields["count"] = int64(e.Count)
	r.Fields["age"] = ageSince(eventTime(e), now)
	put(&r, "message", strings.Join(strings.Fields(e.Message), " "))
	if e.Type == corev1.EventTypeWarning {
		r.Status = resource.StatusWarn
	} else {
		r.Status = resource.StatusOK
	}
	return r
}

// involvedObject is "Kind/name", or the name when the kind is unknown.
func involvedObject(ref corev1.ObjectReference) string {
	switch {
	case ref.Kind != "" && ref.Name != "":
		return ref.Kind + "/" + ref.Name
	case ref.Name != "":
		return ref.Name
	}
	return ""
}

// eventTime is when the event last happened, following kubectl: the series'
// last observation, else the deprecated LastTimestamp, else creation.
func eventTime(e *corev1.Event) time.Time {
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		return e.Series.LastObservedTime.Time
	}
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// namespaceRow builds the namespace row.
func namespaceRow(o any, now time.Time) resource.Resource {
	n, ok := o.(*corev1.Namespace)
	if !ok || n == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("namespace"), n, n.Labels, now)
	put(&r, "name", n.Name)
	put(&r, "status", string(n.Status.Phase))
	r.Fields["age"] = ageSince(n.CreationTimestamp.Time, now)
	if n.Status.Phase == corev1.NamespaceTerminating {
		r.Status = resource.StatusWarn
	} else {
		r.Status = resource.StatusOK
	}
	return r
}

// accessModes abbreviates the access modes the way kubectl does.
func accessModes(modes []corev1.PersistentVolumeAccessMode) string {
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		switch m {
		case corev1.ReadWriteOnce:
			out = append(out, "RWO")
		case corev1.ReadOnlyMany:
			out = append(out, "ROX")
		case corev1.ReadWriteMany:
			out = append(out, "RWX")
		case corev1.ReadWriteOncePod:
			out = append(out, "RWOP")
		default:
			out = append(out, string(m))
		}
	}
	return strings.Join(out, ",")
}

// pvcColour maps a claim phase onto a row colour.
func pvcColour(phase corev1.PersistentVolumeClaimPhase) resource.Status {
	switch phase {
	case corev1.ClaimBound:
		return resource.StatusOK
	case corev1.ClaimPending:
		return resource.StatusWarn
	case corev1.ClaimLost:
		return resource.StatusCrit
	}
	return resource.StatusUnknown
}

// pvcRow builds the persistentvolumeclaim row.
func pvcRow(o any, now time.Time) resource.Resource {
	p, ok := o.(*corev1.PersistentVolumeClaim)
	if !ok || p == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("persistentvolumeclaim"), p, p.Labels, now)
	put(&r, "namespace", p.Namespace)
	put(&r, "name", p.Name)
	put(&r, "status", string(p.Status.Phase))
	put(&r, "volume", p.Spec.VolumeName)
	if q, ok := p.Status.Capacity[corev1.ResourceStorage]; ok {
		put(&r, "capacity", q.String())
	}
	put(&r, "access_modes", accessModes(p.Spec.AccessModes))
	if p.Spec.StorageClassName != nil {
		put(&r, "storage_class", *p.Spec.StorageClassName)
	}
	r.Fields["age"] = ageSince(p.CreationTimestamp.Time, now)
	r.Status = pvcColour(p.Status.Phase)
	return r
}

// pvRow builds the persistentvolume row.
func pvRow(o any, now time.Time) resource.Resource {
	p, ok := o.(*corev1.PersistentVolume)
	if !ok || p == nil {
		return resource.Resource{}
	}
	r := newRow(mustDef("persistentvolume"), p, p.Labels, now)
	put(&r, "name", p.Name)
	if q, ok := p.Spec.Capacity[corev1.ResourceStorage]; ok {
		put(&r, "capacity", q.String())
	}
	put(&r, "access_modes", accessModes(p.Spec.AccessModes))
	put(&r, "reclaim_policy", string(p.Spec.PersistentVolumeReclaimPolicy))
	put(&r, "status", string(p.Status.Phase))
	put(&r, "storage_class", p.Spec.StorageClassName)
	r.Fields["age"] = ageSince(p.CreationTimestamp.Time, now)
	switch p.Status.Phase {
	case corev1.VolumeBound:
		r.Status = resource.StatusOK
	case corev1.VolumeAvailable, corev1.VolumeReleased, corev1.VolumePending:
		r.Status = resource.StatusWarn
	case corev1.VolumeFailed:
		r.Status = resource.StatusCrit
	default:
		r.Status = resource.StatusUnknown
	}
	return r
}

// selectorText renders a node selector as kubectl's SELECTOR column does.
func selectorText(sel map[string]string) string {
	if len(sel) == 0 {
		return "<none>"
	}
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + sel[k]
	}
	return strings.Join(parts, ",")
}

// mustDef returns the definition of a kind that is in the table; a missing one
// is a programming error, and a zero spec keeps the row harmless instead of
// panicking on remote input.
func mustDef(name resource.KindName) kindDef {
	def, ok := defFor(name)
	if !ok {
		return kindDef{spec: resource.KindSpec{Name: name}}
	}
	return def
}
