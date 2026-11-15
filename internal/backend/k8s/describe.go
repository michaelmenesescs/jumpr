package k8s

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

const (
	// readTimeout bounds one describe/yaml read, the events of a node or a
	// service included.
	readTimeout = 30 * time.Second
	// redactedValue is what one secret value becomes in the yaml view.
	redactedValue = "‹redacted %d bytes›"
	// noValue is what describe prints for an empty field, as kubectl does.
	noValue = "<none>"
)

// ObjectEvent is one event of an object as the describe and events views show
// it. Age is computed against the reader's clock, so the text is stable in a
// golden file.
type ObjectEvent struct {
	Type    string
	Reason  string
	Count   int32
	Last    time.Time
	Age     time.Duration
	From    string
	Message string
}

// ObjectDetail is everything the object screen shows about one object.
type ObjectDetail struct {
	Ref  resource.Ref
	Kind string
	// Describe is the describe-equivalent text of the object and its events.
	Describe string
	// YAML is what the yaml tab shows. For a secret whose values were
	// revealed it holds them; otherwise every secret value is a size marker.
	YAML string
	// SafeYAML is the redacted rendering. It is set only when YAML holds
	// revealed secret values, so copy mode never carries them.
	SafeYAML string
	// Events are the object's events, newest last.
	Events []ObjectEvent
	// Redacted is how many secret values YAML replaced in this read.
	Redacted int
	// Revealed is true when YAML keeps a secret's values.
	Revealed bool
	// EventErr is why the events could not be read. The rest of the detail
	// is still valid: a missing events permission must not hide describe.
	EventErr error
}

// ObjectReader reads describe output, events and YAML for one object through
// the dynamic client (SPEC §8.2). Nothing here goes through the informer cache:
// describe needs fields a list row does not keep, and a reveal must be a
// deliberate read, not a cache lookup.
type ObjectReader struct {
	dyn dynamic.Interface
	now func() time.Time
}

// NewObjectReader builds a reader over a dynamic client. now is the clock the
// event ages are computed against.
func NewObjectReader(dyn dynamic.Interface, now func() time.Time) *ObjectReader {
	if now == nil {
		now = time.Now
	}
	return &ObjectReader{dyn: dyn, now: now}
}

// Read fetches ref and returns its describe text, its YAML and its events.
// reveal keeps a secret's values in the YAML; without it every data and
// stringData value is replaced by its size.
func (r *ObjectReader) Read(ctx context.Context, ref resource.Ref, reveal bool) (ObjectDetail, error) {
	def, ok := defFor(ref.Kind)
	if !ok {
		return ObjectDetail{}, errs.New(errs.Config, "k8s.object",
			fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return ObjectDetail{}, errs.New(errs.Config, "k8s.object", "the object needs a name")
	}
	ref.Name = name
	ref.Kind = def.spec.Name
	if def.namespaced {
		if ref.Namespace == "" {
			ref.Namespace = defaultNamespace
		}
	} else {
		ref.Namespace = ""
	}

	obj, err := r.get(ctx, def, ref)
	if err != nil {
		return ObjectDetail{}, err
	}

	detail := ObjectDetail{Ref: ref, Kind: objectKindName(def)}
	events, eventErr := r.events(ctx, def, ref, obj.GetUID())
	detail.Events, detail.EventErr = events, eventErr
	detail.Describe, err = r.describe(ctx, def, obj, events, eventErr)
	if err != nil {
		return ObjectDetail{}, err
	}
	detail.YAML, detail.Redacted, err = marshalYAML(def, obj, reveal)
	if err != nil {
		return ObjectDetail{}, err
	}
	if reveal && string(def.spec.Name) == "secret" {
		detail.Revealed = true
		if safe, _, serr := marshalYAML(def, obj, false); serr == nil {
			detail.SafeYAML = safe
		}
	}
	return detail, nil
}

// ReadObject is what the describe/yaml/events screen calls (SPEC §8.2). It
// bounds the whole read, the events included, and returns *errs.Error for any
// failure.
func (b *Backend) ReadObject(ctx context.Context, ref resource.Ref, reveal bool) (ObjectDetail, error) {
	r, err := b.objectReader()
	if err != nil {
		return ObjectDetail{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return r.Read(ctx, ref, reveal)
}

// objectReader builds the reader over the current context's dynamic client.
func (b *Backend) objectReader() (*ObjectReader, error) {
	if b.client == nil {
		return nil, errs.New(errs.Config, "k8s.object", "no Kubernetes client is configured")
	}
	dyn, err := b.client.Dynamic()
	if err != nil {
		return nil, err
	}
	return NewObjectReader(dyn, b.now), nil
}

// SecretRevealed reports whether a read of ref returned a secret's values.
// The view uses it to keep the yaml tab out of copy mode.
func (d ObjectDetail) SecretRevealed() bool { return d.Revealed && d.SafeYAML != "" }

// get reads one object with the dynamic client.
func (r *ObjectReader) get(ctx context.Context, def kindDef, ref resource.Ref) (*unstructured.Unstructured, error) {
	var ri dynamic.ResourceInterface = r.dyn.Resource(def.gvr)
	if def.namespaced {
		ri = r.dyn.Resource(def.gvr).Namespace(ref.Namespace)
	}
	obj, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		kind := Classify(err)
		msg := fmt.Sprintf("%s %s: %s", def.spec.Name, objectLabel(ref), oneLine(err.Error()))
		if apierrors.IsNotFound(err) {
			kind = errs.NotAvailable
			msg = fmt.Sprintf("%s %s not found", def.spec.Name, objectLabel(ref))
		}
		return nil, &errs.Error{Kind: kind, Op: "k8s.object", Msg: msg, Err: err}
	}
	if obj == nil {
		return nil, errs.New(errs.Remote, "k8s.object",
			fmt.Sprintf("%s %s came back empty", def.spec.Name, objectLabel(ref)))
	}
	return obj, nil
}

// events reads the events of one object by involvedObject.uid and sorts them
// newest last. The field selector is what a real API server filters on; the
// uid is checked again here, because not every server (nor the fake client)
// honours it.
func (r *ObjectReader) events(ctx context.Context, def kindDef, ref resource.Ref, uid types.UID) ([]ObjectEvent, error) {
	if uid == "" {
		return nil, nil
	}
	ns := ref.Namespace
	if !def.namespaced {
		ns = ""
	}
	opts := metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(uid)}
	var ri dynamic.ResourceInterface = r.dyn.Resource(eventsGVR())
	if ns != "" {
		ri = r.dyn.Resource(eventsGVR()).Namespace(ns)
	}
	list, err := ri.List(ctx, opts)
	if err != nil {
		return nil, &errs.Error{Kind: Classify(err), Op: "k8s.events",
			Msg: "events of " + objectLabel(ref) + ": " + oneLine(err.Error()), Err: err}
	}
	now := r.now()
	out := make([]ObjectEvent, 0, len(list.Items))
	for i := range list.Items {
		var ev corev1.Event
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &ev); err != nil {
			continue // a shape we do not understand never breaks describe
		}
		if ev.InvolvedObject.UID != uid {
			continue
		}
		last := ev.LastTimestamp.Time
		if last.IsZero() {
			last = ev.EventTime.Time
		}
		if last.IsZero() {
			last = ev.CreationTimestamp.Time
		}
		age := time.Duration(0)
		if !last.IsZero() {
			if d := now.Sub(last); d > 0 {
				age = d
			}
		}
		out = append(out, ObjectEvent{
			Type:    ev.Type,
			Reason:  ev.Reason,
			Count:   ev.Count,
			Last:    last,
			Age:     age,
			From:    eventSource(ev),
			Message: oneLine(ev.Message),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.Before(out[j].Last)
		}
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		return out[i].Message < out[j].Message
	})
	return out, nil
}

// eventSource is the FROM cell: the reporting component, or the controller
// that reported the event.
func eventSource(ev corev1.Event) string {
	src := ev.Source.Component
	if ev.Source.Host != "" {
		if src != "" {
			src += ", " + ev.Source.Host
		} else {
			src = ev.Source.Host
		}
	}
	if src == "" {
		src = ev.ReportingController
	}
	if src == "" {
		return noValue
	}
	return src
}

// describe dispatches on the kind and falls back to the generic summary when
// the object does not have the shape the typed client expects.
func (r *ObjectReader) describe(ctx context.Context, def kindDef, obj *unstructured.Unstructured, events []ObjectEvent, eventErr error) (string, error) {
	switch string(def.spec.Name) {
	case "pod":
		if pod, ok := typedObject[corev1.Pod](obj); ok {
			return describePod(pod, events, eventErr, r.now()), nil
		}
	case "deployment":
		if d, ok := typedObject[appsv1.Deployment](obj); ok {
			return describeDeployment(d, events, eventErr), nil
		}
	case "statefulset":
		if s, ok := typedObject[appsv1.StatefulSet](obj); ok {
			return describeStatefulSet(s, events, eventErr), nil
		}
	case "daemonset":
		if d, ok := typedObject[appsv1.DaemonSet](obj); ok {
			return describeDaemonSet(d, events, eventErr), nil
		}
	case "node":
		if n, ok := typedObject[corev1.Node](obj); ok {
			pods, err := r.podsOnNode(ctx, n.Name)
			return describeNode(n, pods, err, events, eventErr), nil
		}
	case "service":
		if s, ok := typedObject[corev1.Service](obj); ok {
			return describeService(s, r.serviceEndpoints(ctx, s), events, eventErr), nil
		}
	}
	return describeGeneric(def, obj, events, eventErr), nil
}

// typedObject converts an unstructured object into a typed one. It reports
// false when the object is not of that shape.
func typedObject[T any](u *unstructured.Unstructured) (*T, bool) {
	v := new(T)
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, v); err != nil {
		return nil, false
	}
	return v, true
}

// describePod is kubectl's pod summary: the containers with their state, last
// state and resources, the probes, the node, the QoS class, the conditions and
// then the events.
func describePod(pod *corev1.Pod, events []ObjectEvent, eventErr error, now time.Time) string {
	var w descWriter
	node := pod.Spec.NodeName
	if node != "" && pod.Status.HostIP != "" {
		node += "/" + pod.Status.HostIP
	}
	w.fields(0, [][2]string{
		{"Name", pod.Name},
		{"Namespace", pod.Namespace},
		{"Node", node},
		{"Start Time", timeText(pod.CreationTimestamp.Time)},
		{"Status", string(pod.Status.Phase)},
		{"IP", pod.Status.PodIP},
		{"Labels", labelText(pod.Labels)},
		{"QoS Class", string(pod.Status.QOSClass)},
	})

	if len(pod.Spec.InitContainers) > 0 {
		w.blank()
		w.add("Init Containers:")
		statuses := containerStatuses(pod.Status.InitContainerStatuses)
		for i := range pod.Spec.InitContainers {
			c := &pod.Spec.InitContainers[i]
			w.container(c, statuses[c.Name])
		}
	}

	w.blank()
	if len(pod.Spec.Containers) == 0 {
		w.add("Containers:  %s", noValue)
	} else {
		w.add("Containers:")
		statuses := containerStatuses(pod.Status.ContainerStatuses)
		for i := range pod.Spec.Containers {
			c := &pod.Spec.Containers[i]
			w.container(c, statuses[c.Name])
		}
	}

	w.blank()
	w.conditions(0, podConditions(pod.Status.Conditions))
	w.events(events, eventErr)
	return w.text()
}

// describeDeployment is the deployment summary: replicas, strategy,
// conditions, events.
func describeDeployment(d *appsv1.Deployment, events []ObjectEvent, eventErr error) string {
	var w descWriter
	pairs := [][2]string{
		{"Name", d.Name},
		{"Namespace", d.Namespace},
		{"Creation Timestamp", timeText(d.CreationTimestamp.Time)},
		{"Labels", labelText(d.Labels)},
		{"Selector", labelSelectorText(d.Spec.Selector)},
		{"Replicas", fmt.Sprintf("%d desired | %d updated | %d total | %d available | %d unavailable",
			int32Or(d.Spec.Replicas, 1), d.Status.UpdatedReplicas, d.Status.Replicas,
			d.Status.AvailableReplicas, d.Status.UnavailableReplicas)},
		{"Strategy Type", string(d.Spec.Strategy.Type)},
	}
	if ru := d.Spec.Strategy.RollingUpdate; ru != nil {
		pairs = append(pairs, [2]string{"Rolling Update Strategy",
			intOrPercent(ru.MaxUnavailable, "25%") + " max unavailable, " +
				intOrPercent(ru.MaxSurge, "25%") + " max surge"})
	}
	w.fields(0, pairs)
	w.blank()
	w.conditions(0, deploymentConditions(d.Status.Conditions))
	w.events(events, eventErr)
	return w.text()
}

// describeStatefulSet is the statefulset summary: replicas, update strategy,
// conditions, events.
func describeStatefulSet(s *appsv1.StatefulSet, events []ObjectEvent, eventErr error) string {
	var w descWriter
	pairs := [][2]string{
		{"Name", s.Name},
		{"Namespace", s.Namespace},
		{"Creation Timestamp", timeText(s.CreationTimestamp.Time)},
		{"Labels", labelText(s.Labels)},
		{"Selector", labelSelectorText(s.Spec.Selector)},
		{"Replicas", fmt.Sprintf("%d desired | %d ready | %d current",
			int32Or(s.Spec.Replicas, 1), s.Status.ReadyReplicas, s.Status.CurrentReplicas)},
		{"Service Name", s.Spec.ServiceName},
		{"Pod Management Policy", string(s.Spec.PodManagementPolicy)},
		{"Update Strategy", string(s.Spec.UpdateStrategy.Type)},
	}
	if ru := s.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
		pairs = append(pairs, [2]string{"Partition", strconv.Itoa(int(*ru.Partition))})
	}
	w.fields(0, pairs)
	w.blank()
	w.conditions(0, statefulSetConditions(s.Status.Conditions))
	w.events(events, eventErr)
	return w.text()
}

// describeDaemonSet is the daemonset summary: the node counts, the update
// strategy, the selectors, conditions, events.
func describeDaemonSet(d *appsv1.DaemonSet, events []ObjectEvent, eventErr error) string {
	var w descWriter
	w.fields(0, [][2]string{
		{"Name", d.Name},
		{"Namespace", d.Namespace},
		{"Creation Timestamp", timeText(d.CreationTimestamp.Time)},
		{"Labels", labelText(d.Labels)},
		{"Selector", labelSelectorText(d.Spec.Selector)},
		{"Node-Selector", labelText(d.Spec.Template.Spec.NodeSelector)},
		{"Desired Number of Nodes Scheduled", strconv.Itoa(int(d.Status.DesiredNumberScheduled))},
		{"Current Number of Nodes Scheduled", strconv.Itoa(int(d.Status.CurrentNumberScheduled))},
		{"Number of Nodes Scheduled with Up-to-date Pods", strconv.Itoa(int(d.Status.UpdatedNumberScheduled))},
		{"Number of Nodes Scheduled with Available Pods", strconv.Itoa(int(d.Status.NumberAvailable))},
		{"Number of Nodes Misscheduled", strconv.Itoa(int(d.Status.NumberMisscheduled))},
		{"Number Ready", strconv.Itoa(int(d.Status.NumberReady))},
		{"Update Strategy", string(d.Spec.UpdateStrategy.Type)},
	})
	w.blank()
	w.conditions(0, daemonSetConditions(d.Status.Conditions))
	w.events(events, eventErr)
	return w.text()
}

// describeNode is the node summary: roles, addresses, taints, conditions,
// capacity and allocatable, and how many pods run on it.
func describeNode(n *corev1.Node, pods int, podErr error, events []ObjectEvent, eventErr error) string {
	var w descWriter
	unschedulable := "false"
	if n.Spec.Unschedulable {
		unschedulable = "true"
	}
	podText := noValue
	switch {
	case podErr != nil:
		podText = "unavailable"
	case pods >= 0:
		podText = strconv.Itoa(pods)
	}
	w.fields(0, [][2]string{
		{"Name", n.Name},
		{"Roles", nodeRoles(n)},
		{"Labels", labelText(n.Labels)},
		{"Annotations", labelText(n.Annotations)},
		{"Creation Timestamp", timeText(n.CreationTimestamp.Time)},
		{"Unschedulable", unschedulable},
		{"Taints", taintsText(n.Spec.Taints)},
		{"Pods", podText},
	})
	if len(n.Status.Addresses) > 0 {
		w.blank()
		w.add("Addresses:")
		addrs := make([][2]string, 0, len(n.Status.Addresses))
		for _, a := range n.Status.Addresses {
			addrs = append(addrs, [2]string{string(a.Type), a.Address})
		}
		w.fields(2, addrs)
	}
	w.blank()
	w.conditions(0, nodeConditions(n.Status.Conditions))
	w.block(0, "Capacity", resourcesText(n.Status.Capacity))
	w.block(0, "Allocatable", resourcesText(n.Status.Allocatable))
	w.events(events, eventErr)
	return w.text()
}

// describeService is the service summary: type, ports, selector, endpoints and
// events.
func describeService(s *corev1.Service, endpoints string, events []ObjectEvent, eventErr error) string {
	var w descWriter
	w.fields(0, [][2]string{
		{"Name", s.Name},
		{"Namespace", s.Namespace},
		{"Labels", labelText(s.Labels)},
		{"Selector", labelText(s.Spec.Selector)},
		{"Type", string(s.Spec.Type)},
		{"IP", s.Spec.ClusterIP},
	})
	if len(s.Spec.Ports) > 0 {
		w.blank()
		for _, p := range s.Spec.Ports {
			name := p.Name
			if name == "" {
				name = noValue
			}
			w.add("Port:  %s  %d/%s", name, p.Port, p.Protocol)
			w.add("TargetPort:  %s", p.TargetPort.String())
			if p.NodePort != 0 {
				w.add("NodePort:  %d/%s", p.NodePort, p.Protocol)
			}
			w.blank()
		}
	}
	w.fields(0, [][2]string{
		{"Endpoints", endpoints},
		{"Session Affinity", string(s.Spec.SessionAffinity)},
	})
	w.events(events, eventErr)
	return w.text()
}

// describeGeneric is the summary every other kind gets: metadata, whatever
// conditions the object publishes and its events.
func describeGeneric(def kindDef, obj *unstructured.Unstructured, events []ObjectEvent, eventErr error) string {
	var w descWriter
	w.fields(0, [][2]string{
		{"Name", obj.GetName()},
		{"Namespace", obj.GetNamespace()},
		{"Kind", objectKindName(def)},
		{"Creation Timestamp", timeText(obj.GetCreationTimestamp().Time)},
		{"Labels", labelText(obj.GetLabels())},
		{"Annotations", labelText(obj.GetAnnotations())},
	})
	w.blank()
	w.conditions(0, unstructuredConditions(obj))
	w.events(events, eventErr)
	return w.text()
}

// EventLines renders events as a table whose first line is the header, newest
// last. It is what the events tab shows.
func EventLines(events []ObjectEvent) []string {
	if len(events) == 0 {
		return []string{noValue}
	}
	rows := make([][5]string, 0, len(events))
	typeW, reasonW, ageW, fromW := len("Type"), len("Reason"), len("Age"), len("From")
	for _, e := range events {
		reason := e.Reason
		if e.Count > 1 {
			reason = fmt.Sprintf("%s (x%d)", reason, e.Count)
		}
		row := [5]string{e.Type, reason, ageText(e.Age), e.From, e.Message}
		rows = append(rows, row)
		typeW = max(typeW, len(row[0]))
		reasonW = max(reasonW, len(row[1]))
		ageW = max(ageW, len(row[2]))
		fromW = max(fromW, len(row[3]))
	}
	pad := func(s string, w int) string { return s + strings.Repeat(" ", w-len(s)) }
	header := pad("Type", typeW) + "  " + pad("Reason", reasonW) + "  " +
		pad("Age", ageW) + "  " + pad("From", fromW) + "  Message"
	rule := strings.Repeat("-", typeW) + "  " + strings.Repeat("-", reasonW) + "  " +
		strings.Repeat("-", ageW) + "  " + strings.Repeat("-", fromW) + "  " + strings.Repeat("-", len("Message"))
	out := []string{header, rule}
	for _, row := range rows {
		out = append(out, pad(row[0], typeW)+"  "+pad(row[1], reasonW)+"  "+
			pad(row[2], ageW)+"  "+pad(row[3], fromW)+"  "+row[4])
	}
	return out
}

// marshalYAML renders one object as YAML for the yaml tab. managedFields is
// dropped; a secret's data and stringData values become ‹redacted N bytes›
// unless reveal is true. The count is how many values were replaced.
func marshalYAML(def kindDef, obj *unstructured.Unstructured, reveal bool) (string, int, error) {
	u := obj.DeepCopy()
	unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
	if u.GetAPIVersion() == "" {
		u.SetAPIVersion(def.gvr.GroupVersion().String())
	}
	if u.GetKind() == "" {
		u.SetKind(objectKindName(def))
	}
	redacted := 0
	if string(def.spec.Name) == "secret" && !reveal {
		redacted += redactSecretField(u, "data")
		redacted += redactSecretField(u, "stringData")
	}
	out, err := yaml.Marshal(u.Object)
	if err != nil {
		return "", 0, &errs.Error{Kind: errs.Parse, Op: "k8s.yaml",
			Msg: "the object did not marshal", Err: err}
	}
	return string(out), redacted, nil
}

// redactSecretField replaces every string value of one secret map by its size.
// data holds base64, so its size is the decoded length.
func redactSecretField(u *unstructured.Unstructured, field string) int {
	m, ok, err := unstructured.NestedMap(u.Object, field)
	if err != nil || !ok {
		return 0
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	count := 0
	for _, k := range keys {
		v, ok := m[k].(string)
		if !ok {
			continue
		}
		size := len(v)
		if field == "data" {
			if decoded, derr := base64.StdEncoding.DecodeString(v); derr == nil {
				size = len(decoded)
			}
		}
		m[k] = fmt.Sprintf(redactedValue, size)
		count++
	}
	if count > 0 {
		_ = unstructured.SetNestedMap(u.Object, m, field)
	}
	return count
}

// podsOnNode counts the pods scheduled on a node, the way kubectl describe node
// does: every pod whose spec.nodeName is the node and which is not finished.
func (r *ObjectReader) podsOnNode(ctx context.Context, node string) (int, error) {
	if node == "" {
		return 0, nil
	}
	opts := metav1.ListOptions{FieldSelector: "spec.nodeName=" + node}
	list, err := r.dyn.Resource(podsGVR()).List(ctx, opts)
	if err != nil {
		return 0, &errs.Error{Kind: Classify(err), Op: "k8s.pods",
			Msg: "pods on " + node + ": " + oneLine(err.Error()), Err: err}
	}
	count := 0
	for i := range list.Items {
		name, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "nodeName")
		if name != node {
			continue
		}
		phase, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "phase")
		if phase == string(corev1.PodSucceeded) || phase == string(corev1.PodFailed) {
			continue
		}
		count++
	}
	return count, nil
}

// serviceEndpoints renders the endpoints of a service as kubectl does:
// "10.0.0.21:8080,10.0.0.22:8080", or <none>.
func (r *ObjectReader) serviceEndpoints(ctx context.Context, s *corev1.Service) string {
	ep, err := r.dyn.Resource(endpointsGVR()).Namespace(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil || ep == nil {
		return noValue
	}
	subsets, _, _ := unstructured.NestedSlice(ep.Object, "subsets")
	var out []string
	for _, item := range subsets {
		subset, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		var ports []string
		if raw, ok := subset["ports"].([]interface{}); ok {
			for _, p := range raw {
				pm, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				if n, ok := integer(pm["port"]); ok {
					ports = append(ports, strconv.FormatInt(n, 10))
				}
			}
		}
		raw, _ := subset["addresses"].([]interface{})
		for _, a := range raw {
			am, ok := a.(map[string]interface{})
			if !ok {
				continue
			}
			ip, _ := am["ip"].(string)
			if ip == "" {
				continue
			}
			if len(ports) == 0 {
				out = append(out, ip)
				continue
			}
			for _, port := range ports {
				out = append(out, ip+":"+port)
			}
		}
	}
	if len(out) == 0 {
		return noValue
	}
	return strings.Join(out, ",")
}

// descWriter accumulates the lines of one describe summary.
type descWriter struct{ lines []string }

// add appends one formatted line.
func (w *descWriter) add(format string, args ...any) {
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

// text is the finished describe text, one trailing newline included.
func (w *descWriter) text() string {
	return strings.Join(w.lines, "\n") + "\n"
}

// blank appends an empty separator line.
func (w *descWriter) blank() { w.lines = append(w.lines, "") }

// fields writes "Key: value" lines with the keys of this block aligned. An
// empty value becomes <none>.
func (w *descWriter) fields(indent int, pairs [][2]string) {
	if len(pairs) == 0 {
		return
	}
	width := 0
	for _, p := range pairs {
		width = max(width, len(p[0]))
	}
	pad := strings.Repeat(" ", indent)
	for _, p := range pairs {
		key := p[0] + ":"
		w.lines = append(w.lines, pad+key+strings.Repeat(" ", width-len(p[0])+1)+orNone(p[1]))
	}
}

// optFields is fields without the empty pairs.
func (w *descWriter) optFields(indent int, pairs [][2]string) {
	kept := make([][2]string, 0, len(pairs))
	for _, p := range pairs {
		if strings.TrimSpace(p[1]) != "" {
			kept = append(kept, p)
		}
	}
	w.fields(indent, kept)
}

// block writes a titled group of key/value lines.
func (w *descWriter) block(indent int, title string, pairs [][2]string) {
	if len(pairs) == 0 {
		w.add("%s%s:  %s", strings.Repeat(" ", indent), title, noValue)
		return
	}
	w.add("%s%s:", strings.Repeat(" ", indent), title)
	w.fields(indent+2, pairs)
}

// events writes the events section of a describe summary.
func (w *descWriter) events(events []ObjectEvent, eventErr error) {
	w.blank()
	if eventErr != nil {
		w.add("Events:  unavailable: %s", oneLine(eventErr.Error()))
		return
	}
	if len(events) == 0 {
		w.add("Events:  %s", noValue)
		return
	}
	w.add("Events:")
	for _, line := range EventLines(events) {
		w.add("  %s", line)
	}
}

// conditions writes a condition table, or "<none>".
func (w *descWriter) conditions(indent int, rows []conditionRow) {
	pad := strings.Repeat(" ", indent)
	if len(rows) == 0 {
		w.lines = append(w.lines, pad+"Conditions:  "+noValue)
		return
	}
	w.lines = append(w.lines, pad+"Conditions:")
	typeW, statusW, reasonW := len("Type"), len("Status"), 0
	hasMessage := false
	for _, r := range rows {
		typeW = max(typeW, len(r.Type))
		statusW = max(statusW, len(r.Status))
		reasonW = max(reasonW, len(r.Reason))
		if r.Message != "" {
			hasMessage = true
		}
	}
	padCell := func(s string, width int) string { return s + strings.Repeat(" ", width-len(s)) }
	line := func(r conditionRow) string {
		s := pad + "  " + padCell(r.Type, typeW) + "  " + padCell(r.Status, statusW)
		if reasonW > 0 {
			s += "  " + padCell(r.Reason, reasonW)
		}
		if hasMessage {
			s += "  " + r.Message
		}
		return strings.TrimRight(s, " ")
	}
	header := pad + "  " + padCell("Type", typeW) + "  " + padCell("Status", statusW)
	if reasonW > 0 {
		header += "  " + padCell("Reason", reasonW)
	}
	if hasMessage {
		header += "  Message"
	}
	w.lines = append(w.lines, strings.TrimRight(header, " "))
	for _, r := range rows {
		w.lines = append(w.lines, line(r))
	}
}

// container writes one container's block: image, state, last state, whether it
// is ready, its restart count, limits, requests and probes.
func (w *descWriter) container(c *corev1.Container, status *corev1.ContainerStatus) {
	w.add("  %s:", c.Name)
	pairs := [][2]string{{"Image", c.Image}}
	if status != nil && status.ContainerID != "" {
		pairs = append(pairs, [2]string{"Container ID", status.ContainerID})
	}
	w.fields(4, pairs)
	if status != nil {
		w.containerState(4, "State", status.State)
		if !emptyState(status.LastTerminationState) {
			w.containerState(4, "Last State", status.LastTerminationState)
		}
		w.fields(4, [][2]string{
			{"Ready", boolText(status.Ready)},
			{"Restart Count", strconv.Itoa(int(status.RestartCount))},
		})
	}
	if len(c.Resources.Limits) > 0 {
		w.block(4, "Limits", resourcesText(c.Resources.Limits))
	}
	if len(c.Resources.Requests) > 0 {
		w.block(4, "Requests", resourcesText(c.Resources.Requests))
	}
	w.optFields(4, [][2]string{
		{"Liveness", probeText(c.LivenessProbe)},
		{"Readiness", probeText(c.ReadinessProbe)},
		{"Startup", probeText(c.StartupProbe)},
	})
	if len(c.Ports) > 0 {
		w.fields(4, [][2]string{{"Ports", containerPorts(c.Ports)}})
	}
}

// containerState writes one state block, with its reason and exit code.
func (w *descWriter) containerState(indent int, label string, st corev1.ContainerState) {
	switch {
	case st.Waiting != nil:
		w.fields(indent, [][2]string{{label, "Waiting"}})
		w.optFields(indent+2, [][2]string{
			{"Reason", st.Waiting.Reason},
			{"Message", st.Waiting.Message},
		})
	case st.Running != nil:
		w.fields(indent, [][2]string{{label, "Running"}})
		w.optFields(indent+2, [][2]string{{"Started", timeText(st.Running.StartedAt.Time)}})
	case st.Terminated != nil:
		w.fields(indent, [][2]string{{label, "Terminated"}})
		w.optFields(indent+2, [][2]string{
			{"Reason", st.Terminated.Reason},
			{"Exit Code", strconv.Itoa(int(st.Terminated.ExitCode))},
			{"Started", timeText(st.Terminated.StartedAt.Time)},
			{"Finished", timeText(st.Terminated.FinishedAt.Time)},
		})
	default:
		w.fields(indent, [][2]string{{label, noValue}})
	}
}

// conditionRow is one row of a condition table.
type conditionRow struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

// podConditions maps the pod's conditions.
func podConditions(conds []corev1.PodCondition) []conditionRow {
	out := make([]conditionRow, 0, len(conds))
	for _, c := range conds {
		out = append(out, conditionRow{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	return out
}

// deploymentConditions maps a deployment's conditions.
func deploymentConditions(conds []appsv1.DeploymentCondition) []conditionRow {
	out := make([]conditionRow, 0, len(conds))
	for _, c := range conds {
		out = append(out, conditionRow{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	return out
}

// statefulSetConditions maps a statefulset's conditions.
func statefulSetConditions(conds []appsv1.StatefulSetCondition) []conditionRow {
	out := make([]conditionRow, 0, len(conds))
	for _, c := range conds {
		out = append(out, conditionRow{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	return out
}

// daemonSetConditions maps a daemonset's conditions.
func daemonSetConditions(conds []appsv1.DaemonSetCondition) []conditionRow {
	out := make([]conditionRow, 0, len(conds))
	for _, c := range conds {
		out = append(out, conditionRow{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	return out
}

// nodeConditions maps a node's conditions.
func nodeConditions(conds []corev1.NodeCondition) []conditionRow {
	out := make([]conditionRow, 0, len(conds))
	for _, c := range conds {
		out = append(out, conditionRow{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	return out
}

// unstructuredConditions reads status.conditions of an object of any kind,
// skipping what does not parse.
func unstructuredConditions(obj *unstructured.Unstructured) []conditionRow {
	raw, ok, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !ok {
		return nil
	}
	out := make([]conditionRow, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, conditionRow{
			Type: stringField(m, "type"), Status: stringField(m, "status"),
			Reason: stringField(m, "reason"), Message: stringField(m, "message"),
		})
	}
	return out
}

// stringField reads one string member of an unstructured map.
func stringField(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

// integer reads one numeric member of an unstructured map.
func integer(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// containerStatuses indexes the container statuses by container name.
func containerStatuses(list []corev1.ContainerStatus) map[string]*corev1.ContainerStatus {
	out := make(map[string]*corev1.ContainerStatus, len(list))
	for i := range list {
		out[list[i].Name] = &list[i]
	}
	return out
}

// emptyState reports whether a container state carries nothing.
func emptyState(st corev1.ContainerState) bool {
	return st.Waiting == nil && st.Running == nil && st.Terminated == nil
}

// probeText renders one probe the way kubectl does, without the timing knobs
// kubectl's own describe adds after a count: type, the action and the timings.
func probeText(p *corev1.Probe) string {
	if p == nil {
		return ""
	}
	var head string
	switch {
	case p.Exec != nil:
		head = "exec [" + strings.Join(p.Exec.Command, " ") + "]"
	case p.HTTPGet != nil:
		scheme := strings.ToLower(string(p.HTTPGet.Scheme))
		if scheme == "" {
			scheme = "http"
		}
		head = scheme + "-get " + scheme + "://" + p.HTTPGet.Host + ":" +
			p.HTTPGet.Port.String() + p.HTTPGet.Path
	case p.TCPSocket != nil:
		head = "tcp-socket :" + p.TCPSocket.Port.String()
	case p.GRPC != nil:
		head = "grpc " + strconv.Itoa(int(p.GRPC.Port))
		if p.GRPC.Service != nil && *p.GRPC.Service != "" {
			head += "/" + *p.GRPC.Service
		}
	default:
		return ""
	}
	return fmt.Sprintf("%s delay=%s timeout=%s period=%s #success=%d #failure=%d",
		head, seconds(p.InitialDelaySeconds), seconds(p.TimeoutSeconds),
		seconds(p.PeriodSeconds), p.SuccessThreshold, p.FailureThreshold)
}

// seconds renders a probe timing, "0s" included, as kubectl does.
func seconds(v int32) string { return strconv.Itoa(int(v)) + "s" }

// containerPorts renders the declared ports of a container.
func containerPorts(ports []corev1.ContainerPort) string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		text := strconv.Itoa(int(p.ContainerPort)) + "/" + string(p.Protocol)
		if p.Name != "" {
			text = p.Name + ":" + text
		}
		out = append(out, text)
	}
	return strings.Join(out, ", ")
}

// resourcesText is the sorted key/value list of a resource list.
func resourcesText(list corev1.ResourceList) [][2]string {
	keys := make([]string, 0, len(list))
	for k := range list {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		q := list[corev1.ResourceName(k)]
		out = append(out, [2]string{k, q.String()})
	}
	return out
}

// taintsText renders the taints of a node.
func taintsText(taints []corev1.Taint) string {
	if len(taints) == 0 {
		return noValue
	}
	out := make([]string, 0, len(taints))
	for _, t := range taints {
		text := t.Key
		if t.Value != "" {
			text += "=" + t.Value
		}
		text += ":" + string(t.Effect)
		out = append(out, text)
	}
	return strings.Join(out, ", ")
}

// labelSelectorText renders a metav1 label selector; an empty one is <none>.
func labelSelectorText(sel *metav1.LabelSelector) string {
	if sel == nil {
		return noValue
	}
	text := metav1.FormatLabelSelector(sel)
	if strings.TrimSpace(text) == "" {
		return noValue
	}
	return text
}

// labelText renders a label or annotation map, sorted; an empty map is <none>.
func labelText(m map[string]string) string {
	if len(m) == 0 {
		return noValue
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return strings.Join(out, ",")
}

// objectLabel is "namespace/name", or the name for a cluster-scoped object.
func objectLabel(ref resource.Ref) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "/" + ref.Name
}

// orNone is a value, or <none> when it is empty.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return noValue
	}
	return s
}

// boolText is "True" or "False", as describe prints booleans.
func boolText(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// timeText is a stable timestamp for a describe line.
func timeText(t time.Time) string {
	if t.IsZero() {
		return noValue
	}
	return t.UTC().Format("Mon, 02 Jan 2006 15:04:05 -0700")
}

// ageText is a short age for the events table.
func ageText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

// int32Or is a pointer's value, or def when it is nil.
func int32Or(v *int32, def int32) int32 {
	if v == nil {
		return def
	}
	return *v
}

// intOrPercent is an int-or-percent's text, or def when it is nil.
func intOrPercent(v *intstr.IntOrString, def string) string {
	if v == nil {
		return def
	}
	return v.String()
}

// objectKindName is the API Kind of a kind definition, for the YAML's apiVersion
// and kind and for describe's generic header.
func objectKindName(def kindDef) string {
	switch def.gvr.Resource {
	case "pods":
		return "Pod"
	case "deployments":
		return "Deployment"
	case "statefulsets":
		return "StatefulSet"
	case "daemonsets":
		return "DaemonSet"
	case "jobs":
		return "Job"
	case "cronjobs":
		return "CronJob"
	case "nodes":
		return "Node"
	case "services":
		return "Service"
	case "endpoints":
		return "Endpoints"
	case "ingresses":
		return "Ingress"
	case "configmaps":
		return "ConfigMap"
	case "secrets":
		return "Secret"
	case "events":
		return "Event"
	case "namespaces":
		return "Namespace"
	case "persistentvolumeclaims":
		return "PersistentVolumeClaim"
	case "persistentvolumes":
		return "PersistentVolume"
	}
	return def.gvr.Resource
}

// eventsGVR is the core/v1 events resource.
func eventsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}
}

// podsGVR is the core/v1 pods resource.
func podsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
}

// endpointsGVR is the core/v1 endpoints resource.
func endpointsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "endpoints"}
}
