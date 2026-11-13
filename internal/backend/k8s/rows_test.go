package k8s

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"jumper/internal/resource"
)

// rowNow is the clock every row test runs against.
var rowNow = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

// ago is a timestamp rowNow-d.
func ago(d time.Duration) metav1.Time { return metav1.NewTime(rowNow.Add(-d)) }

// containerRunning is one running container named name.
func containerRunning(name string, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:         name,
		Ready:        true,
		RestartCount: restarts,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
}

// containerWaiting is one waiting container named name.
func containerWaiting(name, reason string, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:         name,
		RestartCount: restarts,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: reason},
		},
	}
}

// containerTerminated is one terminated container named name.
func containerTerminated(name, reason string, exit int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: exit},
		},
	}
}

// TestPodStatus ports kubectl's printer cases: the STATUS text, the ready
// count, the container total and the restart count.
func TestPodStatus(t *testing.T) {
	initRunning := corev1.ContainerStatus{
		Name:  "init",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
	initWaiting := containerWaiting("init", "CrashLoopBackOff", 3)
	initDone := containerTerminated("init", "Completed", 0)

	tests := []struct {
		name     string
		pod      *corev1.Pod
		status   string
		ready    int
		total    int
		restarts int
		colour   resource.Status
	}{
		{
			name: "running and ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:             corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{containerRunning("app", 2)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "Running", ready: 1, total: 1, restarts: 2, colour: resource.StatusOK,
		},
		{
			name: "container creating",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:             corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{containerWaiting("app", "ContainerCreating", 0)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "ContainerCreating", ready: 0, total: 1, colour: resource.StatusWarn,
		},
		{
			name: "crash loop backoff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:             corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{containerWaiting("app", "CrashLoopBackOff", 7)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "CrashLoopBackOff", ready: 0, total: 1, restarts: 7, colour: resource.StatusCrit,
		},
		{
			name: "oom killed",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:             corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{containerTerminated("app", "OOMKilled", 137)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "OOMKilled", ready: 0, total: 1, colour: resource.StatusCrit,
		},
		{
			name: "image pull backoff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:             corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{containerWaiting("app", "ImagePullBackOff", 0)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "ImagePullBackOff", ready: 0, total: 1, colour: resource.StatusCrit,
		},
		{
			name: "terminated with an error",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:             corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{containerTerminated("app", "Error", 1)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "Error", ready: 0, total: 1, colour: resource.StatusCrit,
		},
		{
			name: "init container running",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:                 corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{initRunning},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init"}},
					Containers:     []corev1.Container{{Name: "app"}},
				},
			},
			status: "Init:0/1", ready: 0, total: 1, colour: resource.StatusWarn,
		},
		{
			name: "init container crashing",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:                 corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{initWaiting},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init"}},
					Containers:     []corev1.Container{{Name: "app"}},
				},
			},
			// kubectl counts the init container restarts while initializing.
			status: "Init:CrashLoopBackOff", ready: 0, total: 1, restarts: 3, colour: resource.StatusCrit,
		},
		{
			name: "init done and the app running",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:                 corev1.PodRunning,
					InitContainerStatuses: []corev1.ContainerStatus{initDone},
					ContainerStatuses:     []corev1.ContainerStatus{containerRunning("app", 1)},
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init"}},
					Containers:     []corev1.Container{{Name: "app"}},
				},
			},
			status: "Running", ready: 1, total: 1, restarts: 1, colour: resource.StatusOK,
		},
		{
			name: "completed job pod",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:             corev1.PodSucceeded,
					ContainerStatuses: []corev1.ContainerStatus{containerTerminated("app", "Completed", 0)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "Completed", ready: 0, total: 1, colour: resource.StatusOK,
		},
		{
			name: "completed with a running sidecar",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						containerTerminated("job", "Completed", 0),
						containerRunning("sidecar", 0),
					},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "job"}, {Name: "sidecar"}}},
			},
			status: "Running", ready: 1, total: 2, colour: resource.StatusWarn,
		},
		{
			name: "terminating",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: ptrTime(ago(time.Minute))},
				Status: corev1.PodStatus{
					Phase:             corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{containerRunning("app", 0)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "Terminating", ready: 1, total: 1, colour: resource.StatusWarn,
		},
		{
			name: "pending with no statuses",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodPending},
				Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "Pending", ready: 0, total: 1, colour: resource.StatusWarn,
		},
		{
			name: "restarts are summed over the containers",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						containerRunning("a", 2),
						containerRunning("b", 3),
					},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
			},
			status: "Running", ready: 2, total: 2, restarts: 5, colour: resource.StatusOK,
		},
		{
			name: "node lost",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: ptrTime(ago(time.Minute))},
				Status: corev1.PodStatus{
					Phase:  corev1.PodRunning,
					Reason: "NodeLost",
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			status: "Unknown", ready: 0, total: 1, colour: resource.StatusWarn,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, ready, total, restarts := podStatus(tc.pod)
			if status != tc.status {
				t.Errorf("status = %q, want %q", status, tc.status)
			}
			if ready != tc.ready {
				t.Errorf("ready = %d, want %d", ready, tc.ready)
			}
			if total != tc.total {
				t.Errorf("total = %d, want %d", total, tc.total)
			}
			if restarts != tc.restarts {
				t.Errorf("restarts = %d, want %d", restarts, tc.restarts)
			}
			if got := podColour(status, ready, total); got != tc.colour {
				t.Errorf("colour = %v, want %v", got, tc.colour)
			}
		})
	}
}

// TestRows builds one row of every kind and checks the columns kubectl's
// -o wide view shows.
func TestRows(t *testing.T) {
	tests := []struct {
		name  string
		row   rowFunc
		obj   any
		check map[string]string
	}{
		{
			name: "pod",
			row:  podRow,
			obj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "prod", Name: "web-1", CreationTimestamp: ago(90 * time.Second),
					Labels: map[string]string{"app": "web"},
				},
				Spec: corev1.PodSpec{
					NodeName:   "node-a",
					Containers: []corev1.Container{{Name: "app"}},
				},
				Status: corev1.PodStatus{
					Phase:             corev1.PodRunning,
					PodIP:             "10.0.0.7",
					ContainerStatuses: []corev1.ContainerStatus{containerRunning("app", 4)},
				},
			},
			check: map[string]string{
				"namespace": "prod",
				"name":      "web-1",
				"ready":     "1/1",
				"status":    "Running",
				"ip":        "10.0.0.7",
				"node":      "node-a",
			},
		},
		{
			name: "deployment",
			row:  deploymentRow,
			obj: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web", CreationTimestamp: ago(time.Hour)},
				Spec:       appsv1.DeploymentSpec{Replicas: ptrTo[int32](3)},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: 2, UpdatedReplicas: 3, AvailableReplicas: 2},
			},
			check: map[string]string{"namespace": "prod", "name": "web", "ready": "2/3"},
		},
		{
			name: "statefulset",
			row:  statefulSetRow,
			obj: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg"},
				Spec:       appsv1.StatefulSetSpec{Replicas: ptrTo[int32](3)},
				Status:     appsv1.StatefulSetStatus{ReadyReplicas: 3},
			},
			check: map[string]string{"name": "pg", "ready": "3/3"},
		},
		{
			name: "daemonset",
			row:  daemonSetRow,
			obj: &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "agent"},
				Spec: appsv1.DaemonSetSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{NodeSelector: map[string]string{"role": "web"}},
					},
				},
				Status: appsv1.DaemonSetStatus{
					DesiredNumberScheduled: 4, CurrentNumberScheduled: 4,
					NumberReady: 3, UpdatedNumberScheduled: 4, NumberAvailable: 3,
				},
			},
			check: map[string]string{"name": "agent", "node_selector": "role=web"},
		},
		{
			name: "job",
			row:  jobRow,
			obj: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "import"},
				Spec:       batchv1.JobSpec{Completions: ptrTo[int32](2)},
				Status: batchv1.JobStatus{
					Succeeded: 1,
					StartTime: ptrTime(ago(10 * time.Minute)),
				},
			},
			check: map[string]string{"name": "import", "completions": "1/2"},
		},
		{
			name: "cronjob",
			row:  cronJobRow,
			obj: &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "nightly"},
				Spec:       batchv1.CronJobSpec{Schedule: "0 3 * * *"},
			},
			check: map[string]string{"name": "nightly", "schedule": "0 3 * * *", "suspend": "False"},
		},
		{
			name: "node",
			row:  nodeRow,
			obj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-a",
					Labels: map[string]string{
						"node-role.kubernetes.io/control-plane": "",
						"node-role.kubernetes.io/worker":        "",
					},
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
					},
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
					},
					NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.31.2"},
				},
			},
			check: map[string]string{
				"name": "node-a", "status": "Ready", "version": "v1.31.2",
				"internal_ip": "10.0.0.1", "roles": "control-plane,worker",
			},
		},
		{
			name: "service",
			row:  serviceRow,
			obj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeNodePort,
					ClusterIP: "10.96.0.10",
					Ports: []corev1.ServicePort{
						{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
					},
				},
			},
			check: map[string]string{
				"name": "web", "type": "NodePort", "cluster_ip": "10.96.0.10",
				"ports": "80:30080/TCP",
			},
		},
		{
			name: "endpoints",
			row:  endpointsRow,
			obj: &corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{IP: "10.0.0.7"}},
					Ports:     []corev1.EndpointPort{{Port: 8080}},
				}},
			},
			check: map[string]string{"name": "web", "endpoints": "10.0.0.7:8080"},
		},
		{
			name: "ingress",
			row:  ingressRow,
			obj: &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"},
				Spec: networkingv1.IngressSpec{
					IngressClassName: ptrTo("nginx"),
					Rules: []networkingv1.IngressRule{{
						Host: "web.example.internal",
						IngressRuleValue: networkingv1.IngressRuleValue{
							HTTP: &networkingv1.HTTPIngressRuleValue{
								Paths: []networkingv1.HTTPIngressPath{{
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: "web",
											Port: networkingv1.ServiceBackendPort{Number: 80},
										},
									},
								}},
							},
						},
					}},
				},
			},
			check: map[string]string{
				"name": "web", "class": "nginx", "hosts": "web.example.internal", "ports": "80",
			},
		},
		{
			name: "configmap",
			row:  configMapRow,
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web-conf"},
				Data:       map[string]string{"a": "1", "b": "2"},
			},
			check: map[string]string{"name": "web-conf"},
		},
		{
			name: "event",
			row:  eventRow,
			obj: &corev1.Event{
				ObjectMeta:     metav1.ObjectMeta{Namespace: "prod", Name: "web-1.abc"},
				Type:           corev1.EventTypeWarning,
				Reason:         "BackOff",
				Count:          12,
				LastTimestamp:  ago(5 * time.Minute),
				Message:        "Back-off  restarting  failed  container",
				InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-1"},
			},
			check: map[string]string{
				"type": "Warning", "reason": "BackOff", "object": "Pod/web-1",
				"message": "Back-off restarting failed container",
			},
		},
		{
			name: "namespace",
			row:  namespaceRow,
			obj: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "prod"},
				Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
			},
			check: map[string]string{"name": "prod", "status": "Active"},
		},
		{
			name: "persistentvolumeclaim",
			row:  pvcRow,
			obj: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg-data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					VolumeName:       "pv-01",
					StorageClassName: ptrTo("fast"),
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase:    corev1.ClaimBound,
					Capacity: corev1.ResourceList{corev1.ResourceStorage: apiresource.MustParse("10Gi")},
				},
			},
			check: map[string]string{
				"name": "pg-data", "status": "Bound", "volume": "pv-01",
				"capacity": "10Gi", "access_modes": "RWO", "storage_class": "fast",
			},
		},
		{
			name: "persistentvolume",
			row:  pvRow,
			obj: &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "pv-01"},
				Spec: corev1.PersistentVolumeSpec{
					Capacity:                      corev1.ResourceList{corev1.ResourceStorage: apiresource.MustParse("10Gi")},
					AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					StorageClassName:              "fast",
				},
				Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
			},
			check: map[string]string{
				"name": "pv-01", "capacity": "10Gi", "access_modes": "RWO",
				"reclaim_policy": "Retain", "status": "Bound", "storage_class": "fast",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.row(tc.obj, rowNow)
			if r.Ref.Backend != backendName {
				t.Errorf("Ref.Backend = %q, want k8s", r.Ref.Backend)
			}
			if r.Ref.Kind != resource.KindName(tc.name) {
				t.Errorf("Ref.Kind = %q, want %q", r.Ref.Kind, tc.name)
			}
			if r.Ref.Name == "" {
				t.Error("Ref.Name is empty")
			}
			for id, want := range tc.check {
				got, ok := r.Fields[id]
				if !ok {
					t.Errorf("field %q missing", id)
					continue
				}
				if s, _ := got.(string); s != want {
					t.Errorf("field %q = %v, want %q", id, got, want)
				}
			}
			if r.Raw == nil {
				t.Error("Raw is nil")
			}
		})
	}
}

// TestRowsAge checks the AGE field is the age of the object.
func TestRowsAge(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web-1", CreationTimestamp: ago(3 * time.Minute)},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	r := podRow(pod, rowNow)
	if got := r.Fields["age"]; got != 3*time.Minute {
		t.Errorf("age = %v, want 3m", got)
	}
}

// TestRowsColour covers the status mapping of requirement 4: a pod that is
// not Ready/Running is Warn, the fatal reasons are Crit, a NotReady node is
// Crit and a Warning event is Warn.
func TestRowsColour(t *testing.T) {
	pod := func(phase corev1.PodPhase, statuses ...corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "p"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status:     corev1.PodStatus{Phase: phase, ContainerStatuses: statuses},
		}
	}
	node := func(ready corev1.ConditionStatus) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "n"},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}},
			},
		}
	}
	tests := []struct {
		name string
		row  resource.Resource
		want resource.Status
	}{
		{"pod running", podRow(pod(corev1.PodRunning, containerRunning("app", 0)), rowNow), resource.StatusOK},
		{"pod pending", podRow(pod(corev1.PodPending, containerWaiting("app", "ContainerCreating", 0)), rowNow), resource.StatusWarn},
		{"pod crashloop", podRow(pod(corev1.PodRunning, containerWaiting("app", "CrashLoopBackOff", 1)), rowNow), resource.StatusCrit},
		{"node ready", nodeRow(node(corev1.ConditionTrue), rowNow), resource.StatusOK},
		{"node not ready", nodeRow(node(corev1.ConditionFalse), rowNow), resource.StatusCrit},
		{
			"event warning",
			eventRow(&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "e"}, Type: corev1.EventTypeWarning}, rowNow),
			resource.StatusWarn,
		},
		{
			"event normal",
			eventRow(&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "e"}, Type: corev1.EventTypeNormal}, rowNow),
			resource.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.row.Status != tc.want {
				t.Errorf("status = %v, want %v", tc.row.Status, tc.want)
			}
		})
	}
}

// TestSecretRowExposesNoData proves a secret row carries names and metadata
// only: no value, no key name, nothing that could be logged.
func TestSecretRowExposesNoData(t *testing.T) {
	m := &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "prod",
			Name:              "db-password",
			CreationTimestamp: ago(time.Hour),
			Labels:            map[string]string{"app": "db"},
		},
	}
	r := secretRow(m, rowNow)
	if r.Ref.Name != "db-password" || r.Ref.Namespace != "prod" {
		t.Fatalf("ref = %+v", r.Ref)
	}
	allowed := map[string]bool{"namespace": true, "name": true, "age": true}
	for id, v := range r.Fields {
		if !allowed[id] {
			t.Errorf("field %q = %v must not be set on a secret row", id, v)
		}
	}
	if r.Labels["app"] != "db" {
		t.Errorf("labels = %v, want app=db", r.Labels)
	}
}

// TestRowsSkipUnknownObject keeps a parser crash out of the UI: a row builder
// handed the wrong type returns the zero Resource instead of panicking.
func TestRowsSkipUnknownObject(t *testing.T) {
	rows := []rowFunc{
		podRow, deploymentRow, statefulSetRow, daemonSetRow, jobRow, cronJobRow,
		nodeRow, serviceRow, endpointsRow, ingressRow, configMapRow, secretRow,
		eventRow, namespaceRow, pvcRow, pvRow,
	}
	for _, row := range rows {
		if got := row(struct{}{}, rowNow); got.Ref.Name != "" {
			t.Errorf("row(%T) = %+v, want the zero resource", struct{}{}, got)
		}
		if got := row(nil, rowNow); got.Ref.Name != "" {
			t.Errorf("row(nil) = %+v, want the zero resource", got)
		}
	}
}

// TestKindSpecs checks the kind table itself: kubectl's aliases, unique names
// and columns that a row builder fills.
func TestKindSpecs(t *testing.T) {
	want := map[resource.KindName][]string{
		"pod":                   {"po"},
		"deployment":            {"deploy"},
		"statefulset":           {"sts"},
		"daemonset":             {"ds"},
		"job":                   {"job"},
		"cronjob":               {"cj"},
		"node":                  {"no"},
		"service":               {"svc"},
		"endpoints":             {"ep"},
		"ingress":               {"ing"},
		"configmap":             {"cm"},
		"secret":                {"secret"},
		"event":                 {"ev"},
		"namespace":             {"ns"},
		"persistentvolumeclaim": {"pvc"},
		"persistentvolume":      {"pv"},
	}
	seen := map[string]bool{}
	for _, spec := range kindSpecs() {
		if seen[string(spec.Name)] {
			t.Errorf("kind %q appears twice", spec.Name)
		}
		seen[string(spec.Name)] = true
		aliases, ok := want[spec.Name]
		if !ok {
			t.Errorf("unexpected kind %q", spec.Name)
			continue
		}
		if len(spec.Aliases) != len(aliases) {
			t.Errorf("%s aliases = %v, want %v", spec.Name, spec.Aliases, aliases)
		}
		for i, alias := range aliases {
			if i < len(spec.Aliases) && spec.Aliases[i] != alias {
				t.Errorf("%s alias %d = %q, want %q", spec.Name, i, spec.Aliases[i], alias)
			}
		}
		if len(spec.Columns) == 0 {
			t.Errorf("%s has no columns", spec.Name)
		}
		if spec.DefaultSort == "" {
			t.Errorf("%s has no default sort", spec.Name)
		}
		if !spec.Watchable {
			t.Errorf("%s is not watchable", spec.Name)
		}
		if _, ok := defFor(spec.Name); !ok {
			t.Errorf("defFor(%q) not found", spec.Name)
		}
		for _, alias := range spec.Aliases {
			def, ok := defFor(resource.KindName(alias))
			if !ok || def.spec.Name != spec.Name {
				t.Errorf("defFor(%q) = %v, %v", alias, def.spec.Name, ok)
			}
		}
	}
	if len(seen) != len(want) {
		t.Errorf("serves %d kinds, want %d", len(seen), len(want))
	}
}

// ptrTo returns a pointer to v.
func ptrTo[T any](v T) *T { return &v }

// ptrTime returns a pointer to t.
func ptrTime(t metav1.Time) *metav1.Time { return &t }
