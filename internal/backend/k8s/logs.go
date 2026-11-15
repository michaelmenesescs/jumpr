package k8s

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

const (
	// DefaultTailLines is what a log request asks for when LogOptions.TailLines
	// is zero (SPEC §8.2).
	DefaultTailLines int64 = 500
	// MaxLogLine caps one line. A server may emit any length; the view cannot
	// show more than 64 KiB of one line, so the rest is dropped.
	MaxLogLine = 64 << 10
	// MaxWorkloadPods caps the pods merged for one deployment, statefulset or
	// daemonset row.
	MaxWorkloadPods = 20
)

// LogLine is one line of a pod's log, split from its RFC 3339 timestamp.
// Pod and Container name the stream the line came from, so a view merging
// several streams can prefix the line without parsing it.
type LogLine struct {
	Time      time.Time
	Pod       string
	Container string
	Text      string
	// Err is set alone on the last item of a stream that failed, so the view
	// can show the kind without an error field on every line.
	Err error
}

// LogOptions is what one log request asks for.
type LogOptions struct {
	// Container is the container to read. Empty means the pod's first
	// container.
	Container string
	// AllContainers merges every container of the pod into the one channel,
	// each line labelled with its container name.
	AllContainers bool
	// Previous reads the logs of the previous instance of the container.
	Previous bool
	// Follow keeps the stream open and delivers lines as they appear.
	Follow bool
	// SinceTime is the session window's lower bound. Zero means no bound.
	SinceTime time.Time
	// TailLines is how many lines from the end to ask for. Zero means
	// DefaultTailLines.
	TailLines int64
}

// PodInfo is what the log screen needs to know about a pod before it reads.
type PodInfo struct {
	// Containers lists the pod's containers in spec order, init containers
	// first, so "the first container" is stable.
	Containers []string
	// CrashLoop reports a container waiting in CrashLoopBackOff (or whose last
	// termination said so), which makes the log screen select previous logs.
	CrashLoop bool
}

// Logs streams the logs of one pod. It returns an error only when nothing can
// be started; a stream that fails later closes the channel after one LogLine
// carrying Err. The caller cancels ctx to stop a follow; the API connection is
// closed with it (source: CoreV1().Pods(ns).GetLogs(...).Stream).
func (b *Backend) Logs(ctx context.Context, ns, pod string, o LogOptions) (<-chan LogLine, error) {
	ns, pod = strings.TrimSpace(ns), strings.TrimSpace(pod)
	if ns == "" || pod == "" {
		return nil, errs.New(errs.Config, "k8s.Logs", "namespace and pod are required")
	}
	typed, err := b.typedClient()
	if err != nil {
		return nil, err
	}
	containers, err := logContainers(ctx, typed, ns, pod, o)
	if err != nil {
		return nil, err
	}

	opts := podLogOptions(o)
	streams := make([]logStream, 0, len(containers))
	for _, c := range containers {
		co := *opts
		co.Container = c
		rc, err := typed.CoreV1().Pods(ns).GetLogs(pod, &co).Stream(ctx)
		if err != nil {
			for _, s := range streams {
				_ = s.rc.Close()
			}
			return nil, logError("k8s.Logs", ns+"/"+pod, err)
		}
		streams = append(streams, logStream{container: c, rc: rc})
	}

	out := make(chan LogLine, 64)
	go func() {
		defer close(out)
		var wg sync.WaitGroup
		for _, s := range streams {
			wg.Add(1)
			go func(s logStream) {
				defer wg.Done()
				defer func() { _ = s.rc.Close() }()
				readLogs(ctx, out, ns+"/"+pod, pod, s.container, s.rc)
			}(s)
		}
		wg.Wait()
	}()
	return out, nil
}

// WorkloadLogs merges the logs of the pods of one deployment, statefulset or
// daemonset (at most MaxWorkloadPods, by name). Every line carries its pod
// name, so the view can prefix it. A pod whose stream cannot be opened is
// skipped; the error of the last one is returned only when no stream started.
func (b *Backend) WorkloadLogs(ctx context.Context, kind resource.KindName, ns, name string, o LogOptions) (<-chan LogLine, error) {
	ns, name = strings.TrimSpace(ns), strings.TrimSpace(name)
	if ns == "" || name == "" {
		return nil, errs.New(errs.Config, "k8s.WorkloadLogs", "namespace and workload name are required")
	}
	typed, err := b.typedClient()
	if err != nil {
		return nil, err
	}
	sel, err := workloadSelector(ctx, typed, kind, ns, name)
	if err != nil {
		return nil, err
	}
	list, err := typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, logError("k8s.WorkloadLogs", ns+"/"+name, err)
	}
	names := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	if len(names) > MaxWorkloadPods {
		names = names[:MaxWorkloadPods]
	}
	if len(names) == 0 {
		return nil, errs.New(errs.NotAvailable, "k8s.WorkloadLogs",
			fmt.Sprintf("%s/%s %s selects no pods", ns, name, kind))
	}

	streams := make([]<-chan LogLine, 0, len(names))
	var lastErr error
	for _, pod := range names {
		ch, err := b.Logs(ctx, ns, pod, o)
		if err != nil {
			lastErr = err
			continue
		}
		streams = append(streams, ch)
	}
	if len(streams) == 0 {
		if lastErr == nil {
			lastErr = errs.New(errs.NotAvailable, "k8s.WorkloadLogs", ns+"/"+name+": no logs")
		}
		return nil, lastErr
	}

	out := make(chan LogLine, 64)
	go func() {
		defer close(out)
		var wg sync.WaitGroup
		for _, ch := range streams {
			wg.Add(1)
			go func(ch <-chan LogLine) {
				defer wg.Done()
				for line := range ch {
					select {
					case out <- line:
					case <-ctx.Done():
						return
					}
				}
			}(ch)
		}
		wg.Wait()
	}()
	return out, nil
}

// PodInfo reads the pod and reports its containers and whether one of them is
// crash-looping. The log screen asks before it starts a stream.
func (b *Backend) PodInfo(ctx context.Context, ns, pod string) (PodInfo, error) {
	ns, pod = strings.TrimSpace(ns), strings.TrimSpace(pod)
	if ns == "" || pod == "" {
		return PodInfo{}, errs.New(errs.Config, "k8s.PodInfo", "namespace and pod are required")
	}
	typed, err := b.typedClient()
	if err != nil {
		return PodInfo{}, err
	}
	return podInfo(ctx, typed, ns, pod)
}

// podInfo is PodInfo over an already built client.
func podInfo(ctx context.Context, typed kubernetes.Interface, ns, pod string) (PodInfo, error) {
	p, err := typed.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return PodInfo{}, logError("k8s.PodInfo", ns+"/"+pod, err)
	}
	info := PodInfo{}
	for _, c := range p.Spec.InitContainers {
		info.Containers = append(info.Containers, c.Name)
	}
	for _, c := range p.Spec.Containers {
		info.Containers = append(info.Containers, c.Name)
	}
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, s := range statuses {
		if crashLooping(s) {
			info.CrashLoop = true
		}
	}
	return info, nil
}

// crashLooping reports the CrashLoopBackOff state, current or last terminated.
func crashLooping(s corev1.ContainerStatus) bool {
	if s.State.Waiting != nil && s.State.Waiting.Reason == "CrashLoopBackOff" {
		return true
	}
	if s.LastTerminationState.Terminated != nil && s.LastTerminationState.Terminated.Reason == "CrashLoopBackOff" {
		return true
	}
	return false
}

// logStream is one open container log stream.
type logStream struct {
	container string
	rc        io.ReadCloser
}

// logContainers decides which containers of the pod to read.
func logContainers(ctx context.Context, typed kubernetes.Interface, ns, pod string, o LogOptions) ([]string, error) {
	if o.Container != "" && !o.AllContainers {
		return []string{o.Container}, nil
	}
	info, err := podInfo(ctx, typed, ns, pod)
	if err != nil {
		return nil, err
	}
	if len(info.Containers) == 0 {
		return nil, errs.New(errs.NotAvailable, "k8s.Logs", ns+"/"+pod+": no containers")
	}
	if o.AllContainers {
		return info.Containers, nil
	}
	return info.Containers[:1], nil
}

// podLogOptions translates LogOptions into the API's request options.
// Timestamps is always on: the line time is parsed out of the prefix. The tail
// cap and the sinceTime bound are independent: one caps the count, the other
// bounds the window (BUILD-PLAN §8).
func podLogOptions(o LogOptions) *corev1.PodLogOptions {
	tail := o.TailLines
	if tail <= 0 {
		tail = DefaultTailLines
	}
	opts := &corev1.PodLogOptions{
		Previous:   o.Previous,
		Follow:     o.Follow,
		Timestamps: true,
		TailLines:  &tail,
	}
	if !o.SinceTime.IsZero() {
		t := metav1.NewTime(o.SinceTime)
		opts.SinceTime = &t
	}
	return opts
}

// readLogs copies one stream into out until it ends or ctx is canceled.
func readLogs(ctx context.Context, out chan<- LogLine, host, pod, container string, rc io.Reader) {
	r := bufio.NewReaderSize(rc, MaxLogLine)
	for {
		raw, err := r.ReadString('\n')
		if raw != "" {
			line := parseLogLine(strings.TrimRight(raw, "\r\n"))
			line.Pod, line.Container = pod, container
			select {
			case out <- line:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				select {
				case out <- LogLine{Pod: pod, Container: container, Err: logError("k8s.Logs", host, err)}:
				case <-ctx.Done():
				}
			}
			return
		}
	}
}

// parseLogLine splits the RFC 3339 timestamp off the front of a line. A line
// whose prefix is not a timestamp is kept whole, with a zero Time: remote
// output never makes the parser fail.
func parseLogLine(raw string) LogLine {
	ts, rest, ok := strings.Cut(raw, " ")
	if !ok {
		return LogLine{Text: clipLine(raw)}
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return LogLine{Text: clipLine(raw)}
	}
	return LogLine{Time: t, Text: clipLine(rest)}
}

// clipLine truncates a line to MaxLogLine bytes without splitting a rune.
func clipLine(s string) string {
	if len(s) <= MaxLogLine {
		return s
	}
	cut := MaxLogLine
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start reports whether b can start a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// workloadSelector returns the pod selector of one workload object.
func workloadSelector(ctx context.Context, typed kubernetes.Interface, kind resource.KindName, ns, name string) (*metav1.LabelSelector, error) {
	var sel *metav1.LabelSelector
	var err error
	switch kind {
	case "deployment":
		obj, gerr := typed.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		err = gerr
		if obj != nil {
			sel = obj.Spec.Selector
		}
	case "statefulset":
		obj, gerr := typed.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		err = gerr
		if obj != nil {
			sel = obj.Spec.Selector
		}
	case "daemonset":
		obj, gerr := typed.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		err = gerr
		if obj != nil {
			sel = obj.Spec.Selector
		}
	default:
		return nil, errs.New(errs.Config, "k8s.WorkloadLogs",
			fmt.Sprintf("%q has no pods to read logs from", kind))
	}
	if err != nil {
		return nil, logError("k8s.WorkloadLogs", ns+"/"+name, err)
	}
	if sel == nil || len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return nil, errs.New(errs.Config, "k8s.WorkloadLogs",
			fmt.Sprintf("%s/%s %s has no selector", ns, name, kind))
	}
	return sel, nil
}

// typedClient returns the typed clientset, from the injected test client when
// there is one.
func (b *Backend) typedClient() (kubernetes.Interface, error) {
	if b.typed != nil {
		return b.typed, nil
	}
	if b.client == nil {
		return nil, errs.New(errs.Config, "k8s.client", "no kubernetes client")
	}
	return b.client.Clientset()
}

// logError classifies a client-go log error: a missing pod or container is
// not-available, everything else keeps Classify's kind.
func logError(op, host string, err error) error {
	kind := Classify(err)
	switch {
	case apierrors.IsNotFound(err):
		kind = errs.NotAvailable
	case apierrors.IsBadRequest(err):
		kind = errs.Config
	case Unconfigured(err):
		kind = errs.Config
	}
	e := errs.Wrap(kind, op, err)
	e.Msg = host
	return e
}
