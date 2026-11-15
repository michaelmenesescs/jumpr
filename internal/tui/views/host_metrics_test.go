package views

import (
	"strings"
	"testing"

	"jumpr/internal/backend/prom"
	"jumpr/internal/investigation"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
)

// TestHostMetricsSection renders one configured column with its value and
// sparkline, and a dash for a column the host has no series for.
func TestHostMetricsSection(t *testing.T) {
	h := &Host{
		env:  testEnv(),
		host: "web-ewr-001",
		metricCols: []prom.HostColumn{
			{ID: "load", Title: "LOAD", Expr: `node_load1{instance=~"$instance"}`},
			{ID: "mem", Title: "MEM", Expr: `node_memory_used_percent{instance=~"$instance"}`},
		},
		metricValues: map[resource.HostID]map[string]prom.HostValue{
			"web-ewr-001": {"load": {Value: 1.25, Spark: []float64{1, 2, 3}}},
		},
	}
	line := h.metricsLine(120)
	for _, want := range []string{"METRICS", "LOAD 1.25", "MEM"} {
		if !strings.Contains(line, want) {
			t.Fatalf("metrics line %q is missing %q", line, want)
		}
	}
	if !strings.Contains(line, "—") {
		t.Fatalf("metrics line %q has no dash for the missing column", line)
	}
}

// TestHostMetricsSectionEmpty: with no configured columns the section does not
// appear at all (metrics.host_columns defaults to none).
func TestHostMetricsSectionEmpty(t *testing.T) {
	h := &Host{env: testEnv(), host: "web-ewr-001"}
	if line := h.metricsLine(120); line != "" {
		t.Fatalf("metrics line = %q, want none", line)
	}
	if line := h.alertExprLine(120); line != "" {
		t.Fatalf("alert line = %q, want none", line)
	}
}

// TestHostAlertExpression: an investigation opened from an alert shows the
// alert's own rule expression in the overview.
func TestHostAlertExpression(t *testing.T) {
	env := testEnv()
	env.Session = &tui.SessionState{
		Investigation: &investigation.Meta{Title: "HighErrorRate", Alerts: []string{"HighErrorRate"}},
	}
	h := &Host{
		env:         env,
		host:        "web-ewr-001",
		metricAlert: `sum(rate(http_requests_total{status=~"5.."}[5m])) > 0.5`,
	}
	line := h.alertExprLine(120)
	for _, want := range []string{"ALERT", "HighErrorRate", "http_requests_total"} {
		if !strings.Contains(line, want) {
			t.Fatalf("alert line %q is missing %q", line, want)
		}
	}
}
