package k8s

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"jumpr/internal/errs"
	"jumpr/internal/svcmap"
)

// The cluster side of the service map (SPEC v0.3 "service map").
//
// Declared edges come from objects every cluster has: an Ingress rule names a
// Service, a Service selects pods, and a container's environment or arguments
// name another Service. Observed edges come from Hubble, which only a Cilium
// cluster has: jumpr asks each Cilium agent pod for its recent flows through
// the pod exec API — nothing is installed and nothing is written.
const (
	opServiceMap = "k8s.ServiceMap"
	// hubbleLast is how many recent flows each agent is asked for.
	hubbleLast = "3000"
	// hubbleAgents caps how many agent pods are asked, so a large cluster
	// costs a bounded number of exec calls.
	hubbleAgents = 6
	// hubbleTimeout bounds one agent's answer.
	hubbleTimeout = 12 * time.Second
	// ciliumSelector finds the Cilium agent pods.
	ciliumSelector = "k8s-app=cilium"
	// ciliumContainer is the agent container, which carries the hubble CLI.
	ciliumContainer = "cilium-agent"
)

// hubbleFlow is the part of one `hubble observe -o jsonpb` line the map uses.
type hubbleFlow struct {
	Flow struct {
		Verdict     string         `json:"verdict"`
		IsReply     *bool          `json:"is_reply"`
		Source      hubbleEndpoint `json:"source"`
		Destination hubbleEndpoint `json:"destination"`
		L4          struct {
			TCP *struct {
				DestinationPort int `json:"destination_port"`
			} `json:"TCP"`
			UDP *struct {
				DestinationPort int `json:"destination_port"`
			} `json:"UDP"`
		} `json:"l4"`
		DestinationNames []string `json:"destination_names"`
	} `json:"flow"`
}

type hubbleEndpoint struct {
	Namespace string   `json:"namespace"`
	PodName   string   `json:"pod_name"`
	Labels    []string `json:"labels"`
	Workloads []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"workloads"`
}

// workloadKind maps an owner kind onto the map's vocabulary. An owner the map
// has no word for stays a Pod, which is what it certainly is.
func workloadKind(kind string) svcmap.Kind {
	switch kind {
	case "Deployment":
		return svcmap.Deployment
	case "StatefulSet":
		return svcmap.StatefulSet
	case "DaemonSet":
		return svcmap.DaemonSet
	case "Job":
		return svcmap.Job
	default:
		return svcmap.Pod
	}
}

// podWorkload names the workload that owns pod: the Deployment behind its
// ReplicaSet, its StatefulSet, DaemonSet or Job, or the pod itself.
func podWorkload(pod *corev1.Pod) svcmap.Vertex {
	for _, o := range pod.OwnerReferences {
		switch o.Kind {
		case "ReplicaSet":
			name := o.Name
			if i := strings.LastIndex(name, "-"); i > 0 {
				name = name[:i]
			}
			return svcmap.Vertex{Kind: svcmap.Deployment, Scope: pod.Namespace, Name: name}
		case "StatefulSet", "DaemonSet", "Job":
			return svcmap.Vertex{Kind: workloadKind(o.Kind), Scope: pod.Namespace, Name: o.Name}
		}
	}
	return svcmap.Vertex{Kind: svcmap.Pod, Scope: pod.Namespace, Name: pod.Name}
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// hubbleVertex names one end of a flow.
func hubbleVertex(e hubbleEndpoint, names []string) svcmap.Vertex {
	if len(e.Workloads) > 0 && e.Workloads[0].Name != "" {
		return svcmap.Vertex{Kind: workloadKind(e.Workloads[0].Kind), Scope: e.Namespace, Name: e.Workloads[0].Name}
	}
	if e.PodName != "" {
		return svcmap.Vertex{Kind: svcmap.Pod, Scope: e.Namespace, Name: e.PodName}
	}
	for _, l := range e.Labels {
		switch l {
		case "reserved:world":
			name := "world"
			if len(names) > 0 && names[0] != "" {
				name = names[0]
			}
			return svcmap.Vertex{Kind: svcmap.External, Name: name}
		case "reserved:host", "reserved:remote-node":
			return svcmap.Vertex{Kind: svcmap.Node, Name: "cluster nodes"}
		case "reserved:kube-apiserver":
			return svcmap.Vertex{Kind: svcmap.Service, Scope: "default", Name: "kubernetes"}
		case "reserved:ingress":
			return svcmap.Vertex{Kind: svcmap.Ingress, Name: "cilium ingress"}
		}
	}
	return svcmap.Vertex{}
}

// addHubbleFlows turns flow lines into observed edges. Replies are skipped so
// an edge always points from the side that opened the connection; lines that
// are not flows are skipped without complaint. It returns how many it used.
func addHubbleFlows(g *svcmap.Graph, jsonLines []byte) int {
	used := 0
	sc := bufio.NewScanner(bytes.NewReader(jsonLines))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var f hubbleFlow
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		if f.Flow.IsReply != nil && *f.Flow.IsReply {
			continue
		}
		port := ""
		switch {
		case f.Flow.L4.TCP != nil:
			port = strconv.Itoa(f.Flow.L4.TCP.DestinationPort) + "/TCP"
		case f.Flow.L4.UDP != nil:
			port = strconv.Itoa(f.Flow.L4.UDP.DestinationPort) + "/UDP"
		default:
			continue
		}
		src := hubbleVertex(f.Flow.Source, nil)
		dst := hubbleVertex(f.Flow.Destination, f.Flow.DestinationNames)
		if src.Name == "" || dst.Name == "" {
			continue
		}
		e := svcmap.Edge{From: g.Add(src), To: g.Add(dst), Port: port, Source: svcmap.Hubble}
		switch f.Flow.Verdict {
		case "DROPPED", "ERROR":
			e.Dropped = 1
		default:
			e.Forwarded = 1
		}
		g.Connect(e)
		used++
	}
	return used
}

// refTokens splits a configuration value into the host-like tokens in it.
func refTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.')
	})
}

// serviceRefs finds the Services a pod's containers name in their
// environment, command and arguments: "cart" in the pod's own namespace, or
// "cart.shop", "cart.shop.svc" and the cluster.local forms anywhere.
func serviceRefs(pod *corev1.Pod, byName map[string]*corev1.Service) map[string]string {
	found := map[string]string{}
	check := func(value, where string) {
		lower := strings.ToLower(value)
		for _, tok := range refTokens(value) {
			tok = strings.TrimSuffix(tok, ".")
			tok = strings.TrimSuffix(tok, ".cluster.local")
			tok = strings.TrimSuffix(tok, ".svc")
			key := tok
			if !strings.Contains(tok, ".") {
				// A bare word is only a reference when the value uses it as
				// an address ("http://cart/", "cart:8080"); NAME=traefik or
				// USER=web is not a connection.
				if !strings.Contains(lower, "//"+tok) && !strings.Contains(lower, tok+":") {
					continue
				}
				key = tok + "." + pod.Namespace
			}
			if svc, ok := byName[key]; ok {
				id := svc.Namespace + "/" + svc.Name
				if _, seen := found[id]; !seen {
					found[id] = where
				}
			}
		}
	}
	for _, list := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for _, c := range list {
			for _, e := range c.Env {
				// Kubernetes injects <SVC>_SERVICE_HOST style variables for
				// every Service; only what the manifest sets is a reference.
				if e.Value != "" {
					check(e.Value, "env "+e.Name+" of container "+c.Name)
				}
			}
			for _, a := range append(append([]string(nil), c.Command...), c.Args...) {
				check(a, "arguments of container "+c.Name)
			}
		}
	}
	return found
}

// buildServiceMap builds the declared part of the map from listed objects.
func buildServiceMap(ingresses []networkingv1.Ingress, services []corev1.Service, pods []corev1.Pod) *svcmap.Graph {
	g := svcmap.New()
	byName := make(map[string]*corev1.Service, len(services))
	for i := range services {
		byName[services[i].Name+"."+services[i].Namespace] = &services[i]
	}
	svcVertex := func(ns, name string) string {
		return g.Add(svcmap.Vertex{Kind: svcmap.Service, Scope: ns, Name: name})
	}

	// Workloads, with how many of their pods are ready.
	type tally struct{ ready, total int }
	counts := map[string]*tally{}
	owners := make([]svcmap.Vertex, len(pods))
	for i := range pods {
		w := podWorkload(&pods[i])
		owners[i] = w
		t := counts[w.ID()]
		if t == nil {
			t = &tally{}
			counts[w.ID()] = t
		}
		t.total++
		if podReady(&pods[i]) {
			t.ready++
		}
	}
	for i := range owners {
		t := counts[owners[i].ID()]
		owners[i].Detail = fmt.Sprintf("%d/%d pods ready", t.ready, t.total)
	}

	for i := range ingresses {
		ing := &ingresses[i]
		var hosts []string
		for _, r := range ing.Spec.Rules {
			if r.Host != "" {
				hosts = append(hosts, r.Host)
			}
		}
		from := g.Add(svcmap.Vertex{Kind: svcmap.Ingress, Scope: ing.Namespace, Name: ing.Name, Detail: strings.Join(hosts, ", ")})
		link := func(b *networkingv1.IngressBackend, note string) {
			if b == nil || b.Service == nil {
				return
			}
			port := b.Service.Port.Name
			if b.Service.Port.Number != 0 {
				port = strconv.Itoa(int(b.Service.Port.Number))
			}
			g.Connect(svcmap.Edge{From: from, To: svcVertex(ing.Namespace, b.Service.Name), Port: port, Source: svcmap.IngressRule, Note: note})
		}
		link(ing.Spec.DefaultBackend, "default backend")
		for _, r := range ing.Spec.Rules {
			if r.HTTP == nil {
				continue
			}
			for _, p := range r.HTTP.Paths {
				b := p.Backend
				link(&b, r.Host+p.Path)
			}
		}
	}

	for i := range services {
		svc := &services[i]
		from := svcVertex(svc.Namespace, svc.Name)
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		var ports []string
		for _, p := range svc.Spec.Ports {
			ports = append(ports, p.TargetPort.String()+"/"+string(p.Protocol))
		}
		for j := range pods {
			if pods[j].Namespace != svc.Namespace || !sel.Matches(labels.Set(pods[j].Labels)) {
				continue
			}
			g.Connect(svcmap.Edge{From: from, To: g.Add(owners[j]), Port: strings.Join(ports, ","), Source: svcmap.Selector, Note: sel.String()})
		}
	}

	for j := range pods {
		refs := serviceRefs(&pods[j], byName)
		ids := make([]string, 0, len(refs))
		for id := range refs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			ns, name, _ := strings.Cut(id, "/")
			if svc := byName[name+"."+ns]; svc != nil && len(svc.Spec.Selector) > 0 && ns == pods[j].Namespace &&
				labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(pods[j].Labels)) {
				continue // its own Service
			}
			g.Connect(svcmap.Edge{From: g.Add(owners[j]), To: svcVertex(ns, name), Source: svcmap.EnvRef, Note: refs[id]})
		}
	}
	return g
}

// captureURL is the pod exec URL of a command whose output is collected: no
// stdin and no terminal, unlike the interactive shell's.
func captureURL(cfg *rest.Config, ns, pod, container string, argv []string) *url.URL {
	u, err := url.Parse(cfg.Host)
	if err != nil || u.Host == "" {
		u = &url.URL{}
	}
	u.Path = path.Join("/api/v1/namespaces/"+ns+"/pods/"+pod, "exec")
	q := url.Values{}
	if container != "" {
		q.Set("container", container)
	}
	for _, a := range argv {
		q.Add("command", a)
	}
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	u.RawQuery = q.Encode()
	return u
}

// hubbleFlows asks one Cilium agent pod for its recent flows.
func hubbleFlows(ctx context.Context, cfg *rest.Config, ns, pod string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, hubbleTimeout)
	defer cancel()
	u := captureURL(cfg, ns, pod, ciliumContainer, []string{"hubble", "observe", "--last", hubbleLast, "-o", "jsonpb"})
	ex, err := pickExecutor(cfg, u, spdyExecutor, webSocketExecutor)
	if err != nil {
		return nil, err
	}
	var out, stderr bytes.Buffer
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &stderr}); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, firstLine(msg))
		}
		return nil, err
	}
	return out.Bytes(), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ServiceMap builds the cluster's service map over every namespace. On a
// cluster with Cilium and Hubble the map carries observed edges as well; on
// any other cluster it carries the declared ones and says so in its notes.
func (b *Backend) ServiceMap(ctx context.Context) (*svcmap.Graph, error) {
	cs, err := b.client.Clientset()
	if err != nil {
		return nil, errs.Wrap(errs.KindOf(err), opServiceMap, err)
	}
	ings, err := cs.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errs.Wrap(errs.Unreachable, opServiceMap, err)
	}
	svcs, err := cs.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errs.Wrap(errs.Unreachable, opServiceMap, err)
	}
	pods, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errs.Wrap(errs.Unreachable, opServiceMap, err)
	}
	g := buildServiceMap(ings.Items, svcs.Items, pods.Items)

	var agents []corev1.Pod
	for _, p := range pods.Items {
		if p.Labels["k8s-app"] == "cilium" && p.Status.Phase == corev1.PodRunning {
			agents = append(agents, p)
		}
	}
	if len(agents) == 0 {
		g.Notes = append(g.Notes, "no Cilium agent pods ("+ciliumSelector+") in context "+b.client.CurrentContext()+
			": the edges are declared configuration (Ingress rules, Service selectors, env references), not observed traffic")
		return g, nil
	}
	cfg, err := b.client.RESTConfig()
	if err != nil {
		g.Notes = append(g.Notes, "Cilium found, but the exec API is not usable: "+err.Error())
		return g, nil
	}
	asked, flows := 0, 0
	for _, a := range agents {
		if asked == hubbleAgents {
			g.Notes = append(g.Notes, fmt.Sprintf("asked %d of %d Cilium agents; flows on the other nodes are missing", asked, len(agents)))
			break
		}
		asked++
		raw, herr := hubbleFlows(ctx, cfg, a.Namespace, a.Name)
		if herr != nil {
			g.Notes = append(g.Notes, "hubble on "+a.Spec.NodeName+": "+herr.Error())
			continue
		}
		flows += addHubbleFlows(g, raw)
	}
	g.Notes = append(g.Notes, fmt.Sprintf("observed edges are the last %s flows of %d Cilium agent(s): %d flows used; quiet paths may be missing", hubbleLast, asked, flows))
	return g, nil
}
