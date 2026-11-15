package k8s

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"jumpr/internal/resource"
)

// rowFunc turns one informer object into a table row. now is the injected
// clock, so AGE is deterministic in tests.
type rowFunc func(obj any, now time.Time) resource.Resource

// kindDef is everything the backend needs to serve one kind: the spec the
// registry publishes, the informer that lists it and the row builder. The
// contract (resource.KindSpec) is BUILD-PLAN §4.2.
type kindDef struct {
	spec resource.KindSpec
	// gvr is the resource the API server serves; the metadata client needs it.
	gvr schema.GroupVersionResource
	// object is the prototype of the typed informer. Nil means the kind is
	// served by a metadata-only informer (secrets): values never leave the
	// API server.
	object runtime.Object
	// namespaced is false for cluster-scoped kinds.
	namespaced bool
	// nodeArg maps Args["node"] onto the field selector spec.nodeName.
	nodeArg bool
	row     rowFunc
}

// columns is sugar for the column list of a kind.
func columns(c ...resource.Column) []resource.Column { return c }

// text is a left-aligned text column.
func text(id, title string, width int) resource.Column {
	return resource.Column{ID: id, Title: title, Width: width}
}

// right is a right-aligned numeric column.
func right(id, title string, width int) resource.Column {
	return resource.Column{ID: id, Title: title, Width: width, Align: resource.AlignRight, Kind: resource.ColNumber}
}

// age is the AGE column: a duration since creation.
func age(id, title string, width int) resource.Column {
	return resource.Column{ID: id, Title: title, Width: width, Align: resource.AlignRight, Kind: resource.ColDuration}
}

// kindDefs returns every kind the backend serves, in the order SPEC §8.2
// lists them. Aliases are kubectl's.
func kindDefs() []kindDef {
	return []kindDef{
		{
			spec: resource.KindSpec{
				Name:    "pod",
				Aliases: []string{"po"},
				Title:   "Pods",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					text("ready", "READY", 7),
					text("status", "STATUS", 20),
					right("restarts", "RESTARTS", 9),
					age("age", "AGE", 6),
					text("ip", "IP", 15),
					text("node", "NODE", 16),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			object:     podPrototype(),
			namespaced: true,
			nodeArg:    true,
			row:        podRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "deployment",
				Aliases: []string{"deploy"},
				Title:   "Deployments",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					text("ready", "READY", 7),
					right("up_to_date", "UP-TO-DATE", 10),
					right("available", "AVAILABLE", 9),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			object:     deploymentPrototype(),
			namespaced: true,
			row:        deploymentRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "statefulset",
				Aliases: []string{"sts"},
				Title:   "StatefulSets",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					text("ready", "READY", 7),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"},
			object:     statefulSetPrototype(),
			namespaced: true,
			row:        statefulSetRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "daemonset",
				Aliases: []string{"ds"},
				Title:   "DaemonSets",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					right("desired", "DESIRED", 8),
					right("current", "CURRENT", 8),
					right("ready", "READY", 6),
					right("up_to_date", "UP-TO-DATE", 10),
					right("available", "AVAILABLE", 9),
					text("node_selector", "SELECTOR", 16),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"},
			object:     daemonSetPrototype(),
			namespaced: true,
			row:        daemonSetRow,
		},
		{
			spec: resource.KindSpec{
				Name:  "job",
				Title: "Jobs",
				Scope: resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					text("completions", "COMPLETIONS", 12),
					resource.Column{ID: "duration", Title: "DURATION", Width: 9, Align: resource.AlignRight, Kind: resource.ColDuration},
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"},
			object:     jobPrototype(),
			namespaced: true,
			row:        jobRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "cronjob",
				Aliases: []string{"cj"},
				Title:   "CronJobs",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					text("schedule", "SCHEDULE", 14),
					text("suspend", "SUSPEND", 8),
					right("active", "ACTIVE", 7),
					age("last_schedule", "LAST SCHEDULE", 13),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"},
			object:     cronJobPrototype(),
			namespaced: true,
			row:        cronJobRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "node",
				Aliases: []string{"no"},
				Title:   "Nodes",
				Scope:   resource.ScopeGlobal,
				Columns: columns(
					text("name", "NAME", 0),
					text("status", "STATUS", 9),
					text("roles", "ROLES", 18),
					age("age", "AGE", 6),
					text("version", "VERSION", 11),
					text("internal_ip", "INTERNAL-IP", 15),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"},
			object: nodePrototype(),
			row:    nodeRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "service",
				Aliases: []string{"svc"},
				Title:   "Services",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					text("type", "TYPE", 12),
					text("cluster_ip", "CLUSTER-IP", 15),
					text("external_ip", "EXTERNAL-IP", 15),
					text("ports", "PORTS", 20),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"},
			object:     servicePrototype(),
			namespaced: true,
			row:        serviceRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "endpoints",
				Aliases: []string{"ep"},
				Title:   "Endpoints",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					text("endpoints", "ENDPOINTS", 0),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "endpoints"},
			object:     endpointsPrototype(),
			namespaced: true,
			row:        endpointsRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "ingress",
				Aliases: []string{"ing"},
				Title:   "Ingresses",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					text("class", "CLASS", 10),
					text("hosts", "HOSTS", 24),
					text("address", "ADDRESS", 16),
					text("ports", "PORTS", 8),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
			object:     ingressPrototype(),
			namespaced: true,
			row:        ingressRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "configmap",
				Aliases: []string{"cm"},
				Title:   "ConfigMaps",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					right("data", "DATA", 6),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
			object:     configMapPrototype(),
			namespaced: true,
			row:        configMapRow,
		},
		{
			// Secrets are served by a metadata-only informer: the API server
			// never sends the values, so they cannot reach Fields, a log or a
			// recording. That also means TYPE and the key names are not in the
			// cache; task 108's describe view reads them, keys only, on demand.
			spec: resource.KindSpec{
				Name:  "secret",
				Title: "Secrets",
				Scope: resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
			object:     nil,
			namespaced: true,
			row:        secretRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "event",
				Aliases: []string{"ev"},
				Title:   "Events",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("type", "TYPE", 9),
					text("reason", "REASON", 20),
					text("object", "OBJECT", 24),
					right("count", "COUNT", 6),
					age("age", "LAST SEEN", 10),
					text("message", "MESSAGE", 0),
				),
				DefaultSort: "age",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"},
			object:     eventPrototype(),
			namespaced: true,
			row:        eventRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "namespace",
				Aliases: []string{"ns"},
				Title:   "Namespaces",
				Scope:   resource.ScopeGlobal,
				Columns: columns(
					text("name", "NAME", 0),
					text("status", "STATUS", 10),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"},
			object: namespacePrototype(),
			row:    namespaceRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "persistentvolumeclaim",
				Aliases: []string{"pvc"},
				Title:   "PersistentVolumeClaims",
				Scope:   resource.ScopeNamespace,
				Columns: columns(
					text("namespace", "NAMESPACE", 14),
					text("name", "NAME", 0),
					text("status", "STATUS", 9),
					text("volume", "VOLUME", 20),
					text("capacity", "CAPACITY", 9),
					text("access_modes", "ACCESS MODES", 12),
					text("storage_class", "STORAGECLASS", 13),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
			object:     pvcPrototype(),
			namespaced: true,
			row:        pvcRow,
		},
		{
			spec: resource.KindSpec{
				Name:    "persistentvolume",
				Aliases: []string{"pv"},
				Title:   "PersistentVolumes",
				Scope:   resource.ScopeGlobal,
				Columns: columns(
					text("name", "NAME", 0),
					text("capacity", "CAPACITY", 9),
					text("access_modes", "ACCESS MODES", 12),
					text("reclaim_policy", "RECLAIM POLICY", 14),
					text("status", "STATUS", 9),
					text("storage_class", "STORAGECLASS", 13),
					age("age", "AGE", 6),
				),
				DefaultSort: "name",
				Watchable:   true,
			},
			gvr:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumes"},
			object: pvPrototype(),
			row:    pvRow,
		},
	}
}

// defFor resolves a kind name or alias to its definition.
func defFor(name resource.KindName) (kindDef, bool) {
	want := strings.ToLower(strings.TrimSpace(string(name)))
	for _, def := range kindDefs() {
		if strings.ToLower(string(def.spec.Name)) == want {
			return def, true
		}
		for _, alias := range def.spec.Aliases {
			if strings.ToLower(alias) == want {
				return def, true
			}
		}
	}
	return kindDef{}, false
}

// kindSpecs is what Backend.Kinds publishes.
func kindSpecs() []resource.KindSpec {
	defs := kindDefs()
	out := make([]resource.KindSpec, len(defs))
	for i, def := range defs {
		out[i] = def.spec
	}
	return out
}

// storeKey is the informer store key of one object: "namespace/name" for a
// namespaced kind, "name" otherwise.
func (d kindDef) storeKey(namespace, name string) string {
	if d.namespaced {
		return namespace + "/" + name
	}
	return name
}

// created is the creation time of an object's metadata, or the zero time.
func created(obj metav1.Object) time.Time {
	return obj.GetCreationTimestamp().Time
}
