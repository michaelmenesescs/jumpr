package fake

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumper/internal/resource"
)

// hostsPerDC is the fleet size per datacenter; the demo world has three DCs.
const hostsPerDC = 504

// fleetSize is the number of web hosts in the fake world (1,512).
const fleetSize = 3 * hostsPerDC

// datacenters in generation order.
var datacenters = []string{"ewr", "lax", "ord"}

// failedHosts are the hosts whose nginx.service is failed in the mockup.
var failedHosts = map[resource.HostID]bool{
	"web-ewr-007": true,
	"web-ewr-012": true,
}

// unitDef describes a unit every fake host runs.
type unitDef struct {
	name string
	desc string
}

var hostUnits = []unitDef{
	{"cron.service", "Regular background program processing daemon"},
	{"haproxy.service", "HAProxy Load Balancer"},
	{"nginx.service", "The nginx HTTP and reverse proxy server"},
	{"sshd.service", "OpenSSH server daemon"},
	{"systemd-journald.service", "Journal Service"},
}

// alertDef describes one alert in the fake world.
type alertDef struct {
	name   string
	sev    string
	state  string
	age    time.Duration
	labels map[string]string
}

var alertDefs = []alertDef{
	{"HighErrorRate", "critical", "firing", 18 * time.Minute, map[string]string{"service": "api", "dc": "ewr"}},
	{"NginxUnitFailed", "critical", "firing", 14 * time.Minute, map[string]string{"unit": "nginx.service", "dc": "ewr"}},
	{"PodCrashLooping", "warning", "firing", 26 * time.Minute, map[string]string{"namespace": "shop", "pod": "checkout-7d9f8c"}},
	{"DiskFillingUp", "warning", "firing", 41 * time.Minute, map[string]string{"host": "web-ewr-019", "mount": "/var"}},
	{"HighLatency", "critical", "silenced", 2*time.Hour + 5*time.Minute, map[string]string{"service": "checkout", "dc": "lax"}},
	{"CertificateExpiry", "warning", "pending", 5 * time.Minute, map[string]string{"domain": "www.example.internal"}},
}

// rnd is the deterministic pseudo-random source: fnv64a of "name:key". Every
// value in the world is derived from it, so data is a pure function of the name.
func rnd(name, key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

// fleetNames returns every host name in generation order (ewr, lax, ord).
func fleetNames() []string {
	names := make([]string, 0, fleetSize)
	for _, dc := range datacenters {
		for i := 1; i <= hostsPerDC; i++ {
			names = append(names, fmt.Sprintf("web-%s-%03d", dc, i))
		}
	}
	return names
}

// dcOf extracts the datacenter from a host name, defaulting to ewr.
func dcOf(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) == 3 {
		for _, dc := range datacenters {
			if parts[1] == dc {
				return dc
			}
		}
	}
	return datacenters[0]
}

func isFailedHost(id resource.HostID) bool { return failedHosts[id] }

// hostLabels are the labels every host row carries.
func hostLabels(name, dc string) map[string]string {
	return map[string]string{
		"host": name,
		"role": "web",
		"dc":   dc,
		"env":  "prod",
	}
}

// hostRow builds one host resource. It depends only on the name (and the
// snapshot time), never on iteration order.
func hostRow(now time.Time, name string) resource.Resource {
	dc := dcOf(name)
	id := resource.HostID(name)

	load := 0.50 + float64(rnd(name, "load")%400)/100
	mem := int64(30 + rnd(name, "mem")%61)
	psi := float64(rnd(name, "psi_io")%100) / 10
	errRate := float64(rnd(name, "err_rate")%100) / 1000

	var failed int64
	var units any
	status := resource.StatusOK
	if isFailedHost(id) {
		failed = 1
		units = "nginx"
		errRate = 0.41
		status = resource.StatusCrit
	}

	return resource.Resource{
		Ref: resource.Ref{
			Backend: "fake",
			Kind:    "host",
			Name:    name,
		},
		Fields: map[string]any{
			"host":     name,
			"load":     load,
			"mem":      mem,
			"psi_io":   psi,
			"failed":   failed,
			"units":    units,
			"err_rate": errRate,
			"spark":    sparkline(name),
		},
		Labels:    hostLabels(name, dc),
		Status:    status,
		FetchedAt: now,
	}
}

// sparkline builds twelve points around the host's load.
func sparkline(name string) []float64 {
	points := make([]float64, 12)
	for i := range points {
		points[i] = 0.50 + float64(rnd(name, fmt.Sprintf("spark-%d", i))%400)/100
	}
	return points
}

// unitRows builds the unit list for one host. The failed nginx.service (and the
// active haproxy.service) on the mockup's faulted hosts are explicit.
func unitRows(now time.Time, host resource.HostID) []resource.Resource {
	rows := make([]resource.Resource, 0, len(hostUnits))
	for _, def := range hostUnits {
		load, active, sub := "loaded", "active", "running"
		status := resource.StatusOK
		if isFailedHost(host) {
			switch def.name {
			case "nginx.service":
				active, sub = "failed", "failed"
				status = resource.StatusCrit
			case "haproxy.service":
				active, sub = "active", "running"
			}
		}
		rows = append(rows, resource.Resource{
			Ref: resource.Ref{
				Backend: "fake",
				Kind:    "unit",
				Host:    host,
				Name:    def.name,
			},
			Fields: map[string]any{
				"unit":        def.name,
				"load":        load,
				"active":      active,
				"sub":         sub,
				"description": def.desc,
			},
			Labels:    hostLabels(string(host), dcOf(string(host))),
			Status:    status,
			FetchedAt: now,
		})
	}
	return rows
}

// alertRows builds the six alerts, stamped relative to now.
func alertRows(now time.Time) []resource.Resource {
	rows := make([]resource.Resource, 0, len(alertDefs))
	for _, def := range alertDefs {
		labels := map[string]string{
			"alertname": def.name,
			"severity":  def.sev,
		}
		for k, v := range def.labels {
			labels[k] = v
		}
		rows = append(rows, resource.Resource{
			Ref: resource.Ref{
				Backend: "fake",
				Kind:    "alert",
				Name:    def.name,
			},
			Fields: map[string]any{
				"sev":    def.sev,
				"alert":  def.name,
				"labels": labelText(def.labels),
				"source": "alertmanager",
				"start":  now.Add(-def.age),
				"state":  def.state,
			},
			Labels:    labels,
			Status:    alertStatus(def.sev, def.state),
			FetchedAt: now,
		})
	}
	return rows
}

// labelText renders a label map as sorted key="value" pairs.
func labelText(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Quote(labels[k]))
	}
	return strings.Join(parts, ", ")
}

func alertStatus(sev, state string) resource.Status {
	if state != "firing" {
		return resource.StatusWarn
	}
	if sev == "critical" {
		return resource.StatusCrit
	}
	return resource.StatusWarn
}
