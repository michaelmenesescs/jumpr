package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"jumpr/internal/errs"
)

// logAPI is a minimal API server for the log tests: it answers GET /log,
// GET /pods/{pod}, GET /pods and the apps/v1 workload GETs.
type logAPI struct {
	// lines is what every log stream writes before it ends (or holds).
	lines []string
	// hold keeps a log response open until the client cancels it.
	hold bool
	// pods are served by GET /pods/{pod}; podsList by GET /pods.
	pods     map[string]*corev1.Pod
	podsList []string
	// selector is the deployment/statefulset/daemonset selector.
	selector *metav1.LabelSelector

	mu      sync.Mutex
	queries []url.Values
	logs    int
	ended   chan struct{}
}

// logServer starts the API server the clientset talks to.
func logServer(t *testing.T, api *logAPI) *httptest.Server {
	t.Helper()
	api.ended = make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return srv
}

// logClientset builds a typed clientset over srv.
func logClientset(t *testing.T, srv *httptest.Server) kubernetes.Interface {
	t.Helper()
	cs, err := kubernetes.NewForConfig(&rest.Config{
		Host: srv.URL,
		// The default client-side rate limit is 5 QPS, which would make the
		// 20-pod workload test take seconds for no reason.
		QPS:   100,
		Burst: 200,
		ContentConfig: rest.ContentConfig{
			GroupVersion: &schema.GroupVersion{Version: "v1"},
		},
	})
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	return cs
}

// logBackend builds a backend over the test clientset.
func logBackend(t *testing.T, api *logAPI) *Backend {
	t.Helper()
	srv := logServer(t, api)
	b := NewWithClient(nil)
	b.typed = logClientset(t, srv)
	return b
}

// serve routes one request.
func (a *logAPI) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/log"):
		a.serveLog(w, r)
	case strings.Contains(path, "/pods/"):
		a.servePod(w, r)
	case strings.HasSuffix(path, "/pods"):
		a.servePods(w)
	case strings.Contains(path, "/deployments/"), strings.Contains(path, "/statefulsets/"),
		strings.Contains(path, "/daemonsets/"):
		a.serveWorkload(w, path)
	default:
		http.NotFound(w, r)
	}
}

// serveLog writes the scripted lines, then ends (or holds until cancel).
func (a *logAPI) serveLog(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.queries = append(a.queries, r.URL.Query())
	a.logs++
	a.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain")
	for _, line := range a.lines {
		fmt.Fprintln(w, line)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if a.hold {
		<-r.Context().Done()
	}
	a.ended <- struct{}{}
}

// servePod answers one pod GET.
func (a *logAPI) servePod(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	a.mu.Lock()
	pod := a.pods[name]
	a.mu.Unlock()
	if pod == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, pod)
}

// servePods answers the pod list of a workload.
func (a *logAPI) servePods(w http.ResponseWriter) {
	items := make([]corev1.Pod, 0, len(a.podsList))
	for _, name := range a.podsList {
		items = append(items, *workloadPod(name))
	}
	writeJSON(w, &corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		Items:    items,
	})
}

// serveWorkload answers a deployment/statefulset/daemonset GET with selector.
func (a *logAPI) serveWorkload(w http.ResponseWriter, path string) {
	sel := a.selector
	if sel == nil {
		sel = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	}
	switch {
	case strings.Contains(path, "/deployments/"):
		writeJSON(w, &appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			Spec:     appsv1.DeploymentSpec{Selector: sel},
		})
	case strings.Contains(path, "/statefulsets/"):
		writeJSON(w, &appsv1.StatefulSet{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
			Spec:     appsv1.StatefulSetSpec{Selector: sel},
		})
	default:
		writeJSON(w, &appsv1.DaemonSet{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
			Spec:     appsv1.DaemonSetSpec{Selector: sel},
		})
	}
}

// writeJSON writes v as application/json.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// workloadPod is one pod of a workload with one running container.
func workloadPod(name string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
}

// podWith builds a pod with the named containers.
func podWith(name string, containers ...string) *corev1.Pod {
	p := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod"},
	}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	return p
}

// logLines drains a log channel, failing when it does not close in time.
func logLines(t *testing.T, ch <-chan LogLine) []LogLine {
	t.Helper()
	var out []LogLine
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, line)
		case <-deadline:
			t.Fatalf("log channel did not close; got %d lines", len(out))
		}
	}
}

// logQueries returns the query of every log request, in order.
func (a *logAPI) logQueries() []url.Values {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]url.Values(nil), a.queries...)
}

// TestLogsStream reads a plain stream and parses the timestamp prefix.
func TestLogsStream(t *testing.T) {
	api := &logAPI{
		pods: map[string]*corev1.Pod{"web-1": podWith("web-1", "app")},
		lines: []string{
			"2026-11-12T14:03:05.123456789Z listening on :8080",
			"2026-11-12T14:03:06Z upstream timed out",
		},
	}
	b := logBackend(t, api)

	ch, err := b.Logs(context.Background(), "prod", "web-1", LogOptions{})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	lines := logLines(t, ch)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2: %+v", len(lines), lines)
	}
	if got := lines[0].Text; got != "listening on :8080" {
		t.Errorf("text = %q", got)
	}
	want := time.Date(2026, 11, 12, 14, 3, 5, 123456789, time.UTC)
	if !lines[0].Time.Equal(want) {
		t.Errorf("time = %v, want %v", lines[0].Time, want)
	}
	if lines[0].Pod != "web-1" || lines[0].Container != "app" {
		t.Errorf("labels = %s/%s, want web-1/app", lines[0].Pod, lines[0].Container)
	}
	want2 := time.Date(2026, 11, 12, 14, 3, 6, 0, time.UTC)
	if !lines[1].Time.Equal(want2) {
		t.Errorf("second line time = %v, want %v", lines[1].Time, want2)
	}

	q := api.logQueries()
	if len(q) != 1 {
		t.Fatalf("log requests = %d, want 1", len(q))
	}
	if got := q[0].Get("container"); got != "app" {
		t.Errorf("container = %q, want the pod's first container", got)
	}
	if got := q[0].Get("timestamps"); got != "true" {
		t.Errorf("timestamps = %q, want true", got)
	}
	if got := q[0].Get("tailLines"); got != "500" {
		t.Errorf("tailLines = %q, want the 500 default", got)
	}
	if q[0].Get("previous") != "" || q[0].Get("follow") != "" {
		t.Errorf("unexpected flags: %v", q[0])
	}
}

// TestLogsLongLineKeepsAtMost64KiB checks a runaway line is clipped, not
// carried whole.
func TestLogsLongLineKeepsAtMost64KiB(t *testing.T) {
	api := &logAPI{
		pods:  map[string]*corev1.Pod{"web-1": podWith("web-1", "app")},
		lines: []string{"2026-11-12T14:03:05Z " + strings.Repeat("x", MaxLogLine+4096)},
	}
	b := logBackend(t, api)
	ch, err := b.Logs(context.Background(), "prod", "web-1", LogOptions{Container: "app"})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	lines := logLines(t, ch)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	if got := len(lines[0].Text); got != MaxLogLine {
		t.Errorf("line length = %d, want %d", got, MaxLogLine)
	}
}

// TestLogsFollowCancel checks a follow stream stops when its context is
// canceled, which is what closing the view does.
func TestLogsFollowCancel(t *testing.T) {
	api := &logAPI{
		pods:  map[string]*corev1.Pod{"web-1": podWith("web-1", "app")},
		hold:  true,
		lines: []string{"2026-11-12T14:03:05Z first"},
	}
	b := logBackend(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Logs(ctx, "prod", "web-1", LogOptions{Follow: true, Container: "app"})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	select {
	case line := <-ch:
		if line.Text != "first" {
			t.Fatalf("first line = %q", line.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no first line")
	}
	q := api.logQueries()
	if len(q) != 1 || q[0].Get("follow") != "true" {
		t.Fatalf("follow flag not sent: %v", q)
	}

	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed, which is the requirement
			}
		case <-deadline:
			t.Fatal("the follow stream did not close after cancel")
		}
	}
}

// TestLogsPreviousFlag checks previous, the session window and an explicit
// container reach the request.
func TestLogsPreviousFlag(t *testing.T) {
	api := &logAPI{
		pods:  map[string]*corev1.Pod{"web-1": podWith("web-1", "app", "sidecar")},
		lines: []string{"2026-11-12T14:03:05Z before the crash"},
	}
	b := logBackend(t, api)
	since := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)

	ch, err := b.Logs(context.Background(), "prod", "web-1", LogOptions{
		Container: "sidecar",
		Previous:  true,
		SinceTime: since,
		TailLines: 50,
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	logLines(t, ch)

	q := api.logQueries()
	if len(q) != 1 {
		t.Fatalf("log requests = %d, want 1", len(q))
	}
	if got := q[0].Get("previous"); got != "true" {
		t.Errorf("previous = %q, want true", got)
	}
	if got := q[0].Get("container"); got != "sidecar" {
		t.Errorf("container = %q, want sidecar", got)
	}
	if got := q[0].Get("sinceTime"); got != "2026-11-12T13:00:00Z" {
		t.Errorf("sinceTime = %q", got)
	}
	if got := q[0].Get("tailLines"); got != "50" {
		t.Errorf("tailLines = %q, want the explicit 50", got)
	}
}

// TestLogsAllContainers checks the merged stream carries every container and
// labels each line with its source.
func TestLogsAllContainers(t *testing.T) {
	api := &logAPI{
		pods:  map[string]*corev1.Pod{"web-1": podWith("web-1", "app", "sidecar")},
		lines: []string{"2026-11-12T14:03:05Z both"},
	}
	b := logBackend(t, api)

	ch, err := b.Logs(context.Background(), "prod", "web-1", LogOptions{AllContainers: true})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	lines := logLines(t, ch)
	seen := map[string]int{}
	for _, l := range lines {
		seen[l.Container]++
	}
	if seen["app"] != 1 || seen["sidecar"] != 1 {
		t.Errorf("containers = %v, want one line each", seen)
	}
}

// TestLogsWorkloadPods checks a deployment row merges its pods, capped at 20,
// with the pod name on every line.
func TestLogsWorkloadPods(t *testing.T) {
	api := &logAPI{
		pods: map[string]*corev1.Pod{},
	}
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("web-%02d", i)
		api.pods[name] = workloadPod(name)
		api.podsList = append(api.podsList, name)
	}
	api.lines = []string{"2026-11-12T14:03:05Z ready"}
	b := logBackend(t, api)

	ch, err := b.WorkloadLogs(context.Background(), "deployment", "prod", "web", LogOptions{})
	if err != nil {
		t.Fatalf("WorkloadLogs: %v", err)
	}
	lines := logLines(t, ch)
	if len(lines) != MaxWorkloadPods {
		t.Fatalf("lines = %d, want %d (one per pod, capped)", len(lines), MaxWorkloadPods)
	}
	pods := map[string]bool{}
	for _, l := range lines {
		pods[l.Pod] = true
	}
	if len(pods) != MaxWorkloadPods {
		t.Errorf("distinct pods = %d, want %d", len(pods), MaxWorkloadPods)
	}
	if pods["web-24"] {
		t.Error("the 21st pod and later must not be read")
	}
	if n := api.logQueries(); len(n) != MaxWorkloadPods {
		t.Errorf("log requests = %d, want %d", len(n), MaxWorkloadPods)
	}
}

// TestLogsPodInfoCrashLoop checks the crash-loop state and the container order.
func TestLogsPodInfoCrashLoop(t *testing.T) {
	p := podWith("web-1", "app")
	p.Spec.InitContainers = []corev1.Container{{Name: "init"}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "app",
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		},
	}}
	b := logBackend(t, &logAPI{pods: map[string]*corev1.Pod{"web-1": p}})

	info, err := b.PodInfo(context.Background(), "prod", "web-1")
	if err != nil {
		t.Fatalf("PodInfo: %v", err)
	}
	if !info.CrashLoop {
		t.Error("CrashLoop = false, want true")
	}
	if len(info.Containers) != 2 || info.Containers[0] != "init" || info.Containers[1] != "app" {
		t.Errorf("containers = %v, want [init app]", info.Containers)
	}
}

// TestLogsMissingPod checks a 404 is not-available, not internal.
func TestLogsMissingPod(t *testing.T) {
	b := logBackend(t, &logAPI{pods: map[string]*corev1.Pod{}})
	_, err := b.Logs(context.Background(), "prod", "gone", LogOptions{})
	if err == nil {
		t.Fatal("Logs on a missing pod returned no error")
	}
	if kind := errs.KindOf(err); kind != errs.NotAvailable {
		t.Errorf("kind = %q, want not-available: %v", kind, err)
	}
}

// TestLogsUntimestampedLine checks output without a timestamp prefix survives
// whole.
func TestLogsUntimestampedLine(t *testing.T) {
	api := &logAPI{
		pods:  map[string]*corev1.Pod{"web-1": podWith("web-1", "app")},
		lines: []string{"not a timestamp at all"},
	}
	b := logBackend(t, api)
	ch, err := b.Logs(context.Background(), "prod", "web-1", LogOptions{Container: "app"})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	lines := logLines(t, ch)
	if len(lines) != 1 || lines[0].Text != "not a timestamp at all" || !lines[0].Time.IsZero() {
		t.Fatalf("lines = %+v", lines)
	}
}
