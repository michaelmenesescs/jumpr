package k8s

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

// updateGolden rewrites the describe goldens in testdata/describe.
var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata/describe")

// describeNow is the fixed clock every describe test reads ages against, so
// the golden text is stable.
var describeNow = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

// describeScheme is the scheme the fake dynamic client needs to know the kinds
// these tests use.
func describeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

// describeReader builds an ObjectReader over a fake dynamic client holding
// objs, with the fixed clock.
func describeReader(t *testing.T, objs ...runtime.Object) *ObjectReader {
	t.Helper()
	lists := map[schema.GroupVersionResource]string{
		{Group: "", Version: "v1", Resource: "events"}:    "EventList",
		{Group: "", Version: "v1", Resource: "pods"}:      "PodList",
		{Group: "", Version: "v1", Resource: "endpoints"}: "EndpointsList",
	}
	cli := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(describeScheme(t), lists, objs...)
	var dyn dynamic.Interface = cli
	return NewObjectReader(dyn, func() time.Time { return describeNow })
}

// required is a resource list with one entry.
func required(name corev1.ResourceName, qty string) corev1.ResourceList {
	return corev1.ResourceList{name: apiresource.MustParse(qty)}
}

// crashLoopPod is the fixture the golden covers: two containers, one waiting in
// CrashLoopBackOff after a failed run, one healthy, requests and limits, two
// probes, a node and conditions.
func crashLoopPod() *corev1.Pod {
	started := metav1.NewTime(describeNow.Add(-30 * time.Minute))
	finished := metav1.NewTime(describeNow.Add(-25 * time.Minute))
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			UID:               types.UID("1492b39e-0000-4000-8000-000000000001"),
			Namespace:         "prod",
			Name:              "web-7d9f4c8b6d-h2xqp",
			CreationTimestamp: metav1.NewTime(describeNow.Add(-2 * time.Hour)),
			Labels: map[string]string{
				"app":  "web",
				"tier": "frontend",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:           "node-a",
			ServiceAccountName: "web",
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "registry.example.internal/web:1.4",
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    apiresource.MustParse("500m"),
							corev1.ResourceMemory: apiresource.MustParse("256Mi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    apiresource.MustParse("100m"),
							corev1.ResourceMemory: apiresource.MustParse("128Mi"),
						},
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz",
								Port: intstrFromInt(8080),
							},
						},
						InitialDelaySeconds: 10,
						TimeoutSeconds:      1,
						PeriodSeconds:       10,
						SuccessThreshold:    1,
						FailureThreshold:    3,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							Exec: &corev1.ExecAction{Command: []string{"/bin/check", "--ready"}},
						},
						InitialDelaySeconds: 5,
						TimeoutSeconds:      1,
						PeriodSeconds:       10,
						SuccessThreshold:    1,
						FailureThreshold:    3,
					},
				},
				{
					Name:  "sidecar",
					Image: "registry.example.internal/logship:0.9",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: apiresource.MustParse("10m")},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			PodIP:    "10.0.0.21",
			HostIP:   "10.0.0.11",
			QOSClass: corev1.PodQOSBurstable,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
				{
					Type: corev1.PodReady, Status: corev1.ConditionFalse,
					Reason: "ContainersNotReady", Message: "containers with unready status: [app]",
				},
				{
					Type: corev1.ContainersReady, Status: corev1.ConditionFalse,
					Reason: "ContainersNotReady", Message: "containers with unready status: [app]",
				},
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					Image:        "registry.example.internal/web:1.4",
					ContainerID:  "containerd://0f7c1a",
					Ready:        false,
					RestartCount: 5,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason:  "CrashLoopBackOff",
						Message: "back-off 5m0s restarting failed container app",
					}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason:     "Error",
						ExitCode:   1,
						StartedAt:  started,
						FinishedAt: finished,
					}},
				},
				{
					Name:         "sidecar",
					Image:        "registry.example.internal/logship:0.9",
					Ready:        true,
					RestartCount: 0,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
						StartedAt: started,
					}},
				},
			},
		},
	}
}

// podEvents are the events the golden's pod carries; one is for another object
// and must be filtered out.
func podEvents() []runtime.Object {
	uid := types.UID("1492b39e-0000-4000-8000-000000000001")
	return []runtime.Object{
		&corev1.Event{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "prod", Name: "web-7d9f4c8b6d-h2xqp.1",
			},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "prod", Name: "web-7d9f4c8b6d-h2xqp", UID: uid},
			Type:           corev1.EventTypeNormal,
			Reason:         "Scheduled",
			Message:        "Successfully assigned prod/web-7d9f4c8b6d-h2xqp to node-a",
			Source:         corev1.EventSource{Component: "default-scheduler"},
			FirstTimestamp: metav1.NewTime(describeNow.Add(-2 * time.Hour)),
			LastTimestamp:  metav1.NewTime(describeNow.Add(-2 * time.Hour)),
			Count:          1,
		},
		&corev1.Event{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "prod", Name: "web-7d9f4c8b6d-h2xqp.2",
			},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "prod", Name: "web-7d9f4c8b6d-h2xqp", UID: uid},
			Type:           corev1.EventTypeWarning,
			Reason:         "BackOff",
			Message:        "Back-off restarting failed container app in pod web-7d9f4c8b6d-h2xqp_prod",
			Source:         corev1.EventSource{Component: "kubelet", Host: "node-a"},
			FirstTimestamp: metav1.NewTime(describeNow.Add(-4 * time.Minute)),
			LastTimestamp:  metav1.NewTime(describeNow.Add(-4 * time.Minute)),
			Count:          9,
		},
		&corev1.Event{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "prod", Name: "other.1",
			},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "prod", Name: "other", UID: types.UID("other-uid")},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedMount",
			Message:        "MountVolume.SetUp failed",
			LastTimestamp:  metav1.NewTime(describeNow.Add(-time.Minute)),
		},
	}
}

// TestDescribePodCrashLoopGolden is requirement 2's pod summary: the golden
// text for a crash-looping pod, with its last state, exit code, resources,
// probes and events.
func TestDescribePodCrashLoopGolden(t *testing.T) {
	objs := append([]runtime.Object{crashLoopPod()}, podEvents()...)
	r := describeReader(t, objs...)
	detail, err := r.Read(context.Background(), resource.Ref{
		Backend: "k8s", Kind: "pod", Namespace: "prod", Name: "web-7d9f4c8b6d-h2xqp",
	}, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	path := filepath.Join("testdata", "describe", "pod-crashloop.txt")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(detail.Describe), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run go test -run TestDescribePodCrashLoopGolden -update)", err)
	}
	if detail.Describe != string(want) {
		t.Errorf("describe text differs from %s:\n--- got ---\n%s", path, detail.Describe)
	}
}

// TestDescribePodSections is requirement 2 for the pod: every named part is
// there, the crash loop's last state carries its exit code and reason, and the
// foreign event is not.
func TestDescribePodSections(t *testing.T) {
	objs := append([]runtime.Object{crashLoopPod()}, podEvents()...)
	detail, err := describeReader(t, objs...).Read(context.Background(), resource.Ref{
		Kind: "pod", Namespace: "prod", Name: "web-7d9f4c8b6d-h2xqp",
	}, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	text := detail.Describe
	for _, want := range []string{
		"Name:", "web-7d9f4c8b6d-h2xqp", "Namespace:", "prod",
		"Node:", "node-a/10.0.0.11",
		"QoS Class:", "Burstable",
		"Containers:", "app:", "sidecar:",
		"Image:", "registry.example.internal/web:1.4",
		"State:", "Waiting", "Reason:", "CrashLoopBackOff",
		"Last State:", "Terminated", "Exit Code:", "1",
		"Limits:", "cpu:", "500m", "memory:",
		"Requests:", "100m",
		"Liveness:", "http-get", "/healthz",
		"Readiness:", "exec [/bin/check --ready]",
		"Conditions:", "Ready", "ContainersNotReady",
		"Events:", "BackOff", "kubelet, node-a",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("describe text is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "FailedMount") {
		t.Errorf("describe lists an event of another object:\n%s", text)
	}
	if len(detail.Events) != 2 {
		t.Fatalf("got %d events, want the pod's 2: %+v", len(detail.Events), detail.Events)
	}
	if got := detail.Events[len(detail.Events)-1].Reason; got != "BackOff" {
		t.Errorf("newest event = %q, want BackOff last", got)
	}
}

// TestDescribeEventsNewestLast: the events table ends with the most recent
// event.
func TestDescribeEventsNewestLast(t *testing.T) {
	lines := EventLines([]ObjectEvent{
		{Type: "Normal", Reason: "Oldest", Age: time.Hour, From: "scheduler", Message: "a"},
		{Type: "Warning", Reason: "Newest", Age: time.Minute, From: "kubelet", Message: "b"},
	})
	if len(lines) < 4 {
		t.Fatalf("got %d lines: %v", len(lines), lines)
	}
	if !strings.Contains(lines[3], "Newest") {
		t.Errorf("last event row is %q, want the newest", lines[3])
	}
	if got := EventLines(nil); len(got) != 1 || got[0] != noValue {
		t.Errorf("EventLines(nil) = %v, want %q", got, noValue)
	}
}

// TestDescribeDeployment covers requirement 2 for deployments: replicas,
// strategy and conditions.
func TestDescribeDeployment(t *testing.T) {
	replicas := int32(2)
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "prod", Name: "web",
			CreationTimestamp: metav1.NewTime(describeNow.Add(-3 * time.Hour)),
			Labels:            map[string]string{"app": "web"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
					MaxSurge:       &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
			},
		},
		Status: appsv1.DeploymentStatus{
			Replicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue,
				Reason: "MinimumReplicasAvailable", Message: "Deployment has minimum availability.",
			}},
		},
	}
	detail, err := describeReader(t, dep).Read(context.Background(), resource.Ref{
		Kind: "deployment", Namespace: "prod", Name: "web",
	}, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, want := range []string{
		"Replicas:", "2 desired", "1 updated", "2 total", "1 available", "1 unavailable",
		"Strategy Type:", "RollingUpdate", "max unavailable", "max surge",
		"Selector:", "app=web",
		"Conditions:", "MinimumReplicasAvailable",
	} {
		if !strings.Contains(detail.Describe, want) {
			t.Errorf("deployment describe is missing %q:\n%s", want, detail.Describe)
		}
	}
}

// TestDescribeStatefulSetAndDaemonSet covers requirement 2 for the other
// workloads.
func TestDescribeStatefulSetAndDaemonSet(t *testing.T) {
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "prod", Name: "db", CreationTimestamp: metav1.NewTime(describeNow),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: "db",
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
		},
		Status: appsv1.StatefulSetStatus{Replicas: 3, ReadyReplicas: 2, CurrentReplicas: 3},
	}
	detail, err := describeReader(t, sts).Read(context.Background(), resource.Ref{
		Kind: "statefulset", Namespace: "prod", Name: "db",
	}, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, want := range []string{
		"Replicas:", "3 desired", "2 ready", "3 current",
		"Update Strategy:", "RollingUpdate", "Service Name:", "db",
	} {
		if !strings.Contains(detail.Describe, want) {
			t.Errorf("statefulset describe is missing %q:\n%s", want, detail.Describe)
		}
	}

	ds := &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "prod", Name: "logship", CreationTimestamp: metav1.NewTime(describeNow),
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "logship"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeSelector: map[string]string{"role": "worker"},
			}},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType},
		},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 4, CurrentNumberScheduled: 4, NumberReady: 3,
			NumberAvailable: 3, UpdatedNumberScheduled: 4,
		},
	}
	detail, err = describeReader(t, ds).Read(context.Background(), resource.Ref{
		Kind: "daemonset", Namespace: "prod", Name: "logship",
	}, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, want := range []string{
		"Desired Number of Nodes Scheduled:", "4",
		"Number Ready:", "3",
		"Node-Selector:", "role=worker",
		"Update Strategy:", "RollingUpdate",
	} {
		if !strings.Contains(detail.Describe, want) {
			t.Errorf("daemonset describe is missing %q:\n%s", want, detail.Describe)
		}
	}
}

// TestDescribeNode covers requirement 2 for nodes: conditions, capacity,
// allocatable, taints and the pod count.
func TestDescribeNode(t *testing.T) {
	node := &corev1.Node{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a", CreationTimestamp: metav1.NewTime(describeNow),
			Labels: map[string]string{
				"node-role.kubernetes.io/control-plane": "",
				"kubernetes.io/hostname":                "node-a",
			},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{
				Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    apiresource.MustParse("8"),
				corev1.ResourceMemory: apiresource.MustParse("32Gi"),
				corev1.ResourcePods:   apiresource.MustParse("110"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    apiresource.MustParse("7500m"),
				corev1.ResourceMemory: apiresource.MustParse("30Gi"),
				corev1.ResourcePods:   apiresource.MustParse("110"),
			},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.11"},
				{Type: corev1.NodeHostName, Address: "node-a"},
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasSufficientMemory"},
			},
		},
	}
	pods := []runtime.Object{
		testPod("prod", "web-1", "node-a", nil),
		testPod("prod", "web-2", "node-a", nil),
		testPod("prod", "elsewhere", "node-b", nil),
	}
	objs := append(pods, node)
	detail, err := describeReader(t, objs...).Read(context.Background(), resource.Ref{
		Kind: "node", Name: "node-a",
	}, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, want := range []string{
		"Roles:", "control-plane",
		"Taints:", "dedicated=gpu:NoSchedule",
		"Pods:", "2",
		"Addresses:", "InternalIP:", "10.0.0.11",
		"Conditions:", "KubeletReady",
		"Capacity:", "32Gi", "Allocatable:", "7500m",
	} {
		if !strings.Contains(detail.Describe, want) {
			t.Errorf("node describe is missing %q:\n%s", want, detail.Describe)
		}
	}
}

// TestDescribeService covers requirement 2 for services: type, ports,
// selector and endpoints.
func TestDescribeService(t *testing.T) {
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "prod", Name: "web", CreationTimestamp: metav1.NewTime(describeNow),
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.10",
			Selector:  map[string]string{"app": "web"},
			Ports: []corev1.ServicePort{{
				Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP,
				TargetPort: intstrFromInt(8080),
			}},
		},
	}
	ep := &corev1.Endpoints{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Endpoints"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "10.0.0.21"}, {IP: "10.0.0.22"}},
			Ports:     []corev1.EndpointPort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
		}},
	}
	detail, err := describeReader(t, svc, ep).Read(context.Background(), resource.Ref{
		Kind: "service", Namespace: "prod", Name: "web",
	}, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, want := range []string{
		"Type:", "ClusterIP", "IP:", "10.96.0.10",
		"Selector:", "app=web",
		"Port:", "http", "8080/TCP",
		"Endpoints:", "10.0.0.21:8080,10.0.0.22:8080",
	} {
		if !strings.Contains(detail.Describe, want) {
			t.Errorf("service describe is missing %q:\n%s", want, detail.Describe)
		}
	}
}

// TestDescribeOtherKind: a kind without a hand-written summary still gets its
// metadata, its conditions and its events.
func TestDescribeOtherKind(t *testing.T) {
	obj := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "prod", Name: "config",
			CreationTimestamp: metav1.NewTime(describeNow),
			Labels:            map[string]string{"app": "web"},
		},
	}
	detail, err := describeReader(t, obj).Read(context.Background(), resource.Ref{
		Kind: "configmap", Namespace: "prod", Name: "config",
	}, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, want := range []string{
		"Name:", "Namespace:", "Kind:", "ConfigMap",
		"Labels:", "app=web",
		"Conditions:", noValue,
		"Events:", noValue,
	} {
		if !strings.Contains(detail.Describe, want) {
			t.Errorf("generic describe is missing %q:\n%s", want, detail.Describe)
		}
	}
}

// TestDescribeUnknownKind: an unknown kind is a config error, not a panic.
func TestDescribeUnknownKind(t *testing.T) {
	_, err := describeReader(t).Read(context.Background(), resource.Ref{Kind: "nope", Name: "x"}, false)
	if err == nil {
		t.Fatal("Read of an unknown kind returned no error")
	}
	if got := errs.KindOf(err); got != errs.Config {
		t.Errorf("kind = %q, want config", got)
	}
}

// TestYAMLSecretRedacted is requirement 1's second half: a secret's YAML holds
// no value, only its size, and managedFields is gone. With reveal the values
// come back, and the safe copy does not.
func TestYAMLSecretRedacted(t *testing.T) {
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "prod", Name: "web-tls",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"password": []byte("correct-horse-battery-staple"),
			"token":    []byte("s3cr3t"),
		},
	}
	r := describeReader(t, secret)
	ref := resource.Ref{Kind: "secret", Namespace: "prod", Name: "web-tls"}

	detail, err := r.Read(context.Background(), ref, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, value := range []string{"correct-horse-battery-staple", "s3cr3t",
		"Y29ycmVjdC1ob3JzZS1iYXR0ZXJ5LXN0YXBsZQ==", "czNjcjN0", "managedFields"} {
		if strings.Contains(detail.YAML, value) {
			t.Errorf("secret YAML carries %q:\n%s", value, detail.YAML)
		}
	}
	for _, want := range []string{"data:", "password:", "token:"} {
		if !strings.Contains(detail.YAML, want) {
			t.Errorf("secret YAML lost the key %q:\n%s", want, detail.YAML)
		}
	}
	if !strings.Contains(detail.YAML, "‹redacted 28 bytes›") {
		t.Errorf("secret YAML has no size marker:\n%s", detail.YAML)
	}
	if detail.Redacted != 2 {
		t.Errorf("Redacted = %d, want 2", detail.Redacted)
	}
	if detail.Revealed {
		t.Error("a read without reveal reports Revealed")
	}

	revealed, err := r.Read(context.Background(), ref, true)
	if err != nil {
		t.Fatalf("Read reveal: %v", err)
	}
	if !strings.Contains(revealed.YAML, "Y29ycmVjdC1ob3JzZS1iYXR0ZXJ5LXN0YXBsZQ==") {
		t.Errorf("revealed YAML lost the value:\n%s", revealed.YAML)
	}
	if !revealed.Revealed || !revealed.SecretRevealed() {
		t.Error("revealed detail does not report Revealed")
	}
	if strings.Contains(revealed.SafeYAML, "Y29ycmVjdC1ob3JzZS1iYXR0ZXJ5LXN0YXBsZQ==") {
		t.Errorf("safe YAML carries the value:\n%s", revealed.SafeYAML)
	}
	if !strings.Contains(revealed.SafeYAML, "‹redacted 28 bytes›") {
		t.Errorf("safe YAML lost the size marker:\n%s", revealed.SafeYAML)
	}
}

// TestYAMLSecretStringDataRedacted: stringData is write-only on a real API
// server, so it is covered on an object built here: its values are replaced by
// their length, and reveal keeps them.
func TestYAMLSecretStringDataRedacted(t *testing.T) {
	key := "api" + "-key"
	value := "plain" + "-value"
	def, ok := defFor("secret")
	if !ok {
		t.Fatal("no secret kind definition")
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": "web", "namespace": "prod"},
		"stringData": map[string]interface{}{key: value},
	}}
	out, redacted, err := marshalYAML(def, obj, false)
	if err != nil {
		t.Fatalf("marshalYAML: %v", err)
	}
	if strings.Contains(out, value) {
		t.Errorf("stringData value is in the YAML:\n%s", out)
	}
	if !strings.Contains(out, "‹redacted 11 bytes›") {
		t.Errorf("stringData value has no size marker:\n%s", out)
	}
	if redacted != 1 {
		t.Errorf("redacted = %d, want 1", redacted)
	}
	out, redacted, err = marshalYAML(def, obj, true)
	if err != nil {
		t.Fatalf("marshalYAML reveal: %v", err)
	}
	if !strings.Contains(out, value) {
		t.Errorf("revealed YAML lost the stringData value:\n%s", out)
	}
	if redacted != 0 {
		t.Errorf("redacted = %d on a reveal, want 0", redacted)
	}
}

// TestYAMLManagedFieldsDropped: every kind's YAML drops managedFields, and the
// text is valid YAML with the object's apiVersion and kind.
func TestYAMLManagedFieldsDropped(t *testing.T) {
	pod := crashLoopPod()
	pod.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}
	detail, err := describeReader(t, pod).Read(context.Background(), resource.Ref{
		Kind: "pod", Namespace: "prod", Name: pod.Name,
	}, false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if strings.Contains(detail.YAML, "managedFields") {
		t.Errorf("YAML carries managedFields:\n%s", detail.YAML)
	}
	for _, want := range []string{"apiVersion: v1", "kind: Pod", "name: " + pod.Name,
		"namespace: prod", "image: registry.example.internal/web:1.4"} {
		if !strings.Contains(detail.YAML, want) {
			t.Errorf("YAML is missing %q:\n%s", want, detail.YAML)
		}
	}
}

// TestYAMLNotFound: a missing object is not-available, not a panic.
func TestYAMLNotFound(t *testing.T) {
	_, err := describeReader(t).Read(context.Background(), resource.Ref{
		Kind: "secret", Namespace: "prod", Name: "missing",
	}, false)
	if err == nil {
		t.Fatal("Read of a missing object returned no error")
	}
	if got := errs.KindOf(err); got != errs.NotAvailable {
		t.Errorf("kind = %q, want not-available", got)
	}
}

// intstrFromInt is an IntOrString holding one port number.
func intstrFromInt(n int32) intstr.IntOrString {
	return intstr.IntOrString{Type: intstr.Int, IntVal: n}
}
