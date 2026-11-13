package k8s

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"

	"jumper/internal/errs"
)

// echoOpener is a stream opener backed by an in-memory pair that echoes what it
// receives, the way a healthy pod port answers.
func echoOpener() (connOpener, *atomic.Int32) {
	var opened atomic.Int32
	open := func(context.Context) (io.ReadWriteCloser, error) {
		opened.Add(1)
		client, server := net.Pipe()
		go func() {
			defer func() { _ = server.Close() }()
			_, _ = io.Copy(server, server)
		}()
		return client, nil
	}
	return open, &opened
}

// dialForward opens a local connection to f.
func dialForward(t *testing.T, f *Forward) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", f.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// exchange writes and reads one round trip through conn.
func exchange(t *testing.T, conn net.Conn, msg string) string {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf)
}

// TestPortForwardLifecycle: a forward listens on loopback, copies every local
// connection to the pod port, counts the connections and bytes, and stop closes
// the listener.
func TestPortForwardLifecycle(t *testing.T) {
	b, _ := testBackend(t, "prod", testPod("prod", "web-1", "node-a", nil))
	open, opened := echoOpener()
	f, err := b.portForward(context.Background(), "prod", "web-1", ForwardPod, 0, 8080, open)
	if err != nil {
		t.Fatalf("portForward: %v", err)
	}
	if !strings.HasPrefix(f.Addr(), "127.0.0.1:") {
		t.Fatalf("addr = %q, want a loopback listener only", f.Addr())
	}
	if f.Local() == 0 {
		t.Fatal("Local = 0, want the ephemeral port the kernel assigned")
	}
	if f.Remote() != 8080 || f.Target() != "web-1" || f.Namespace() != "prod" {
		t.Fatalf("forward = %s -> %d in %s", f.Target(), f.Remote(), f.Namespace())
	}
	if f.Since().IsZero() {
		t.Fatal("Since is zero")
	}

	conn := dialForward(t, f)
	if got := exchange(t, conn, "ping"); got != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
	waitFor(t, 2*time.Second, func() bool { return f.Conns() == 1 })
	if f.Bytes() < 8 {
		t.Fatalf("Bytes = %d, want both directions counted", f.Bytes())
	}
	if got := opened.Load(); got != 1 {
		t.Fatalf("streams opened = %d, want 1", got)
	}

	if err := f.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-f.Done():
	default:
		t.Fatal("Done is not closed after Stop")
	}
	if _, err := net.DialTimeout("tcp", f.Addr(), 200*time.Millisecond); err == nil {
		t.Fatal("the listener still accepts connections after Stop")
	}
	if err := f.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestPortForwardServicePod: a service forward runs through one of the
// service's ready endpoint pods.
func TestPortForwardServicePod(t *testing.T) {
	eps := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{
				{IP: "10.0.0.9", TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "api-7f9c"}},
			},
		}},
	}
	b, _ := testBackend(t, "prod", eps)
	open, _ := echoOpener()
	f, err := b.portForward(context.Background(), "prod", "api", ForwardService, 0, 5432, open)
	if err != nil {
		t.Fatalf("portForward: %v", err)
	}
	defer func() { _ = f.Stop() }()
	if f.Target() != "api-7f9c" || f.Name() != "api" || f.Kind() != ForwardService {
		t.Fatalf("target = %s, name = %s, kind = %s", f.Target(), f.Name(), f.Kind())
	}
	if got := exchange(t, dialForward(t, f), "x"); got != "x" {
		t.Fatalf("echo = %q, want x", got)
	}
}

// TestPortForwardServiceByIP: endpoints without a pod TargetRef are matched
// against the pods' IPs.
func TestPortForwardServiceByIP(t *testing.T) {
	eps := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "10.0.0.5"}},
		}},
	}
	pod := testPod("prod", "api-1", "node-a", nil)
	pod.Status.PodIP = "10.0.0.5"
	objs := []runtime.Object{eps, pod}
	b, _ := testBackend(t, "prod", objs...)
	podName, err := b.forwardPod(context.Background(), api{typed: b.typed, namespace: "prod"}, "prod", "api", ForwardService)
	if err != nil {
		t.Fatalf("forwardPod: %v", err)
	}
	if podName != "api-1" {
		t.Fatalf("pod = %q, want api-1", podName)
	}
}

// TestPortForwardNoReadyEndpoint: a service without a ready endpoint is a
// not-available error, not an empty forward.
func TestPortForwardNoReadyEndpoint(t *testing.T) {
	eps := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"},
		Subsets: []corev1.EndpointSubset{{
			NotReadyAddresses: []corev1.EndpointAddress{{IP: "10.0.0.9"}},
		}},
	}
	b, _ := testBackend(t, "prod", eps)
	open, _ := echoOpener()
	_, err := b.portForward(context.Background(), "prod", "api", ForwardService, 0, 80, open)
	var e *errs.Error
	if !errors.As(err, &e) || e.Kind != errs.NotAvailable {
		t.Fatalf("err = %v, want a not-available error", err)
	}
}

// TestPortForwardInferKind: with no kind the pod wins, then the service.
func TestPortForwardInferKind(t *testing.T) {
	eps := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "10.0.0.9", TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "api-7f9c"}}},
		}},
	}
	b, _ := testBackend(t, "prod", testPod("prod", "web-1", "node-a", nil), eps)
	for _, tc := range []struct{ name, want string }{
		{"web-1", "web-1"},
		{"api", "api-7f9c"},
	} {
		if got, err := b.forwardPod(context.Background(), api{typed: b.typed, namespace: "prod"}, "prod", tc.name, ""); err != nil || got != tc.want {
			t.Fatalf("forwardPod(%s) = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// TestPortForwardRejectsPorts: a bad port never reaches a listener.
func TestPortForwardRejectsPorts(t *testing.T) {
	b, _ := testBackend(t, "prod", testPod("prod", "web-1", "node-a", nil))
	open, _ := echoOpener()
	cases := []struct{ local, remote int }{
		{0, 0},
		{0, 70000},
		{-1, 80},
		{70000, 80},
	}
	for _, c := range cases {
		if _, err := b.portForward(context.Background(), "prod", "web-1", ForwardPod, c.local, c.remote, open); err == nil {
			t.Fatalf("portForward(%d, %d) succeeded, want an error", c.local, c.remote)
		}
	}
	if _, err := b.portForward(context.Background(), "prod", "", ForwardPod, 0, 80, open); err == nil {
		t.Fatal("portForward with no name succeeded, want an error")
	}
}

// TestPortForwardSet: the session's set lists forwards by local port, refuses a
// duplicate and stops on request.
func TestPortForwardSet(t *testing.T) {
	b, _ := testBackend(t, "prod", testPod("prod", "web-1", "node-a", nil))
	open, _ := echoOpener()
	high, err := b.portForward(context.Background(), "prod", "web-1", ForwardPod, 0, 443, open)
	if err != nil {
		t.Fatalf("portForward: %v", err)
	}
	low, err := b.portForward(context.Background(), "prod", "web-1", ForwardPod, 0, 80, open)
	if err != nil {
		t.Fatalf("portForward: %v", err)
	}
	set := NewForwardSet()
	if err := set.Add(high); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := set.Add(low); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := set.Add(high); err == nil {
		t.Fatal("Add of the same local port succeeded, want an error")
	}
	list := set.List()
	if len(list) != 2 {
		t.Fatalf("List = %d forwards, want 2", len(list))
	}
	if list[0].Local() > list[1].Local() {
		t.Fatalf("List is not sorted by local port: %d, %d", list[0].Local(), list[1].Local())
	}
	if _, ok := set.Get(high.Local()); !ok {
		t.Fatalf("Get(%d) missed the forward", high.Local())
	}
	if err := set.Stop(low.Local()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := set.Get(low.Local()); ok {
		t.Fatal("a stopped forward stayed in the set")
	}
	if err := set.Stop(low.Local()); err == nil {
		t.Fatal("stopping a missing forward succeeded, want an error")
	}
	if err := set.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if got := len(set.List()); got != 0 {
		t.Fatalf("List after StopAll = %d, want 0", got)
	}
}

// TestPortForwardURL: the forward request targets the pod's portforward
// subresource of the session namespace.
func TestPortForwardURL(t *testing.T) {
	u, err := portForwardURL(&rest.Config{Host: "https://api.example.internal:6443"}, "prod", "web-1")
	if err != nil {
		t.Fatalf("portForwardURL: %v", err)
	}
	if u.Path != "/api/v1/namespaces/prod/pods/web-1/portforward" {
		t.Fatalf("path = %q", u.Path)
	}
	if u.Host != "api.example.internal:6443" {
		t.Fatalf("host = %q", u.Host)
	}
}

// TestForwardKindNormalize: kubectl's aliases are accepted.
func TestForwardKindNormalize(t *testing.T) {
	cases := map[string]ForwardKind{
		"pod": ForwardPod, "po": ForwardPod, "Pods": ForwardPod,
		"service": ForwardService, "svc": ForwardService, "SERVICES": ForwardService,
		"": "", "deployment": "",
	}
	for in, want := range cases {
		if got := ForwardKind(in).Normalize(); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
