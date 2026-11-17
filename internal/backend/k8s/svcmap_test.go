package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"jumpr/internal/svcmap"
)

func mapPod(ns, name, rs string, lbl map[string]string, env ...corev1.EnvVar) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs}}},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: env}}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func TestBuildServiceMapDeclaredEdges(t *testing.T) {
	ing := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "shop.example.internal",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path:    "/",
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}},
			}}}},
		}}},
	}
	svc := func(name string) corev1.Service {
		return corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app": name},
				Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: corev1.ProtocolTCP}}},
		}
	}
	pods := []corev1.Pod{
		mapPod("shop", "web-6d5f9c7b8-aaaaa", "web-6d5f9c7b8", map[string]string{"app": "web"},
			corev1.EnvVar{Name: "CART_URL", Value: "http://cart.shop.svc.cluster.local:80/api"}),
		mapPod("shop", "web-6d5f9c7b8-bbbbb", "web-6d5f9c7b8", map[string]string{"app": "web"}),
		mapPod("shop", "cart-5c9d8f7b6-ccccc", "cart-5c9d8f7b6", map[string]string{"app": "cart"},
			corev1.EnvVar{Name: "MODE", Value: "web"},             // a bare word is not a reference
			corev1.EnvVar{Name: "SELF", Value: "http://cart:80"}), // nor is its own Service
	}
	g := buildServiceMap([]networkingv1.Ingress{ing}, []corev1.Service{svc("web"), svc("cart")}, pods)
	got := strings.Join(g.Lines(false), "\n")
	if strings.Contains(got, "env MODE") || strings.Contains(got, "env SELF") {
		t.Errorf("a bare word or a self reference became an edge:\n%s", got)
	}
	for _, want := range []string{
		"Ingress shop/shop  shop.example.internal",
		"Service shop/web  :80   [declared · ingress rule · shop.example.internal/]",
		"Deployment shop/web  2/2 pods ready  :8080/TCP   [declared · selector · app=web]",
		"Service shop/cart   [declared · env reference · env CART_URL of container app]",
		"Deployment shop/cart  1/1 pods ready",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestAddHubbleFlows(t *testing.T) {
	lines := strings.Join([]string{
		`{"flow":{"verdict":"FORWARDED","is_reply":false,"source":{"namespace":"shop","pod_name":"web-1","workloads":[{"name":"web","kind":"Deployment"}]},"destination":{"namespace":"shop","pod_name":"cart-1","workloads":[{"name":"cart","kind":"Deployment"}]},"l4":{"TCP":{"destination_port":8080}}}}`,
		`{"flow":{"verdict":"DROPPED","is_reply":false,"source":{"namespace":"shop","pod_name":"web-1","workloads":[{"name":"web","kind":"Deployment"}]},"destination":{"namespace":"shop","pod_name":"cart-1","workloads":[{"name":"cart","kind":"Deployment"}]},"l4":{"TCP":{"destination_port":8080}}}}`,
		`{"flow":{"verdict":"FORWARDED","is_reply":true,"source":{"namespace":"shop","pod_name":"cart-1"},"destination":{"namespace":"shop","pod_name":"web-1"},"l4":{"TCP":{"destination_port":41000}}}}`,
		`{"flow":{"verdict":"FORWARDED","source":{"namespace":"shop","pod_name":"cart-1","workloads":[{"name":"cart","kind":"Deployment"}]},"destination":{"labels":["reserved:world"]},"destination_names":["api.example.com"],"l4":{"TCP":{"destination_port":443}}}}`,
		`not json`,
		`{"flow":{"verdict":"FORWARDED","source":{"namespace":"a","pod_name":"x"},"destination":{"namespace":"a","pod_name":"y"},"l4":{"ICMPv4":{}}}}`,
	}, "\n")
	g := svcmap.New()
	if used := addHubbleFlows(g, []byte(lines)); used != 3 {
		t.Fatalf("used %d flows, want 3 (reply, junk and ICMP are skipped)", used)
	}
	got := strings.Join(g.Lines(false), "\n")
	for _, want := range []string{
		"Deployment shop/cart  :8080/TCP   [observed · hubble · 1 forwarded, 1 DROPPED]",
		"External api.example.com  :443/TCP   [observed · hubble · 1 forwarded]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
