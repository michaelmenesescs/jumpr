package linux

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/probe"
	"jumpr/internal/resource"
	"jumpr/internal/transport/transporttest"
)

// fleetShow is one host's `systemctl show` reply for nginx.service.
const fleetShow = "ActiveState=active\n" +
	"SubState=running\n" +
	"ActiveEnterTimestamp=Wed 2026-11-12 09:30:00 UTC\n" +
	"NRestarts=2\n" +
	"UnitFileState=enabled\n" +
	"MemoryCurrent=52428800\n"

// fleetQuery is the unit-fleet query for unit.
func fleetQuery(unit string) resource.Query {
	return resource.Query{Kind: KindUnitFleet, Args: map[string]string{"unit": unit}}
}

// fleetBatch compiles the fleet batch of b for unit.
func fleetBatch(t *testing.T, b *Backend, unit string) probe.Batch {
	t.Helper()
	batch, err := b.batchFor(KindUnitFleet, fleetQuery(unit))
	if err != nil {
		t.Fatalf("batchFor(unit-fleet, %q): %v", unit, err)
	}
	return batch
}

// TestUnitFleetOneShowPerHost is the contract of the kind: one row per target
// host, each from exactly one `systemctl show -p ...` round trip.
func TestUnitFleetOneShowPerHost(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01", "web-02"))
	batch := fleetBatch(t, b, "nginx.service")

	var sec *probe.Section
	for i := range batch.Sections {
		if batch.Sections[i].ID == secUnitFleet {
			sec = &batch.Sections[i]
		}
	}
	if sec == nil {
		t.Fatalf("the fleet batch has no %q section", secUnitFleet)
	}
	if len(batch.Sections) != 1 {
		t.Fatalf("the fleet batch has %d sections, want 1", len(batch.Sections))
	}
	for _, want := range []string{"systemctl show nginx.service", "-p", unitFleetProps, "--no-pager"} {
		if !strings.Contains(sec.Cmd, want) {
			t.Fatalf("fleet command is missing %q: %s", want, sec.Cmd)
		}
	}
	if sec.Sudo {
		t.Error("systemctl show needs no sudo")
	}

	reply := scriptReply(batch, map[string]string{secUnitFleet: fleetShow}, nil, nil)
	ex.On("systemctl", reply)

	q := fleetQuery("nginx.service")
	q.Hosts = []resource.HostID{"web-01", "web-02"}
	ch, err := b.List(context.Background(), q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 2 {
		t.Fatalf("List returned %d results, want 2", len(results))
	}

	for _, res := range results {
		if res.Err != nil {
			t.Fatalf("%s failed: %v", res.Host, res.Err)
		}
		if len(res.Items) != 1 {
			t.Fatalf("%s has %d rows, want 1", res.Host, len(res.Items))
		}
		row := res.Items[0]
		if row.Ref.Kind != KindUnitFleet || row.Ref.Host != res.Host || row.Ref.Name != string(res.Host) {
			t.Errorf("%s ref = %+v, want unit-fleet on that host", res.Host, row.Ref)
		}
		if got := row.Str("host"); got != string(res.Host) {
			t.Errorf("%s host = %q", res.Host, got)
		}
		if got := row.Str("unit"); got != "nginx.service" {
			t.Errorf("%s unit = %q", res.Host, got)
		}
		if got := row.Str("active"); got != "active" {
			t.Errorf("%s active = %q", res.Host, got)
		}
		if got := row.Str("sub"); got != "running" {
			t.Errorf("%s sub = %q", res.Host, got)
		}
		if got := row.Str("enabled"); got != "enabled" {
			t.Errorf("%s enabled = %q", res.Host, got)
		}
		if got, ok := row.Fields["restarts"].(int64); !ok || got != 2 {
			t.Errorf("%s restarts = %v, want 2", res.Host, row.Fields["restarts"])
		}
		if got, ok := row.Fields["mem"].(int64); !ok || got != 52428800 {
			t.Errorf("%s mem = %v, want 52428800", res.Host, row.Fields["mem"])
		}
		since, ok := row.Fields["since"].(time.Time)
		if !ok {
			t.Fatalf("%s since = %v, want a time", res.Host, row.Fields["since"])
		}
		if want := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC); !since.Equal(want) {
			t.Errorf("%s since = %s, want %s", res.Host, since, want)
		}
		if row.Status != resource.StatusOK {
			t.Errorf("%s status = %v, want ok", res.Host, row.Status)
		}

		links := b.Links(row)
		if len(links) != 1 || links[0].Key != "enter" {
			t.Fatalf("%s links = %+v, want one enter link", res.Host, links)
		}
		to := links[0].To
		if to.Kind != KindUnitDetail || to.Args["unit"] != "nginx.service" {
			t.Errorf("%s enter opens %+v, want the unit detail of nginx.service", res.Host, to)
		}
		if len(to.Hosts) != 1 || to.Hosts[0] != res.Host {
			t.Errorf("%s enter hosts = %v, want just that host", res.Host, to.Hosts)
		}
	}

	if got := len(ex.Calls()); got != 2 {
		t.Fatalf("%d remote calls for 2 hosts, want one each", got)
	}
	for _, call := range ex.Calls() {
		if strings.Contains(call.Command.Script, "sudo") {
			t.Errorf("%s ran under sudo: %s", call.Host, call.Command.Script)
		}
	}
}

// TestUnitFleetMissingValues keeps a dead or unknown unit honest: no state is
// invented, and an unset MemoryCurrent has no mem cell.
func TestUnitFleetMissingValues(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := fleetBatch(t, b, "nginx.service")

	body := "ActiveState=inactive\nSubState=dead\nActiveEnterTimestamp=\nNRestarts=7\nUnitFileState=disabled\nMemoryCurrent=[not set]\n"
	ex.On("systemctl", scriptReply(batch, map[string]string{secUnitFleet: body}, nil, nil))

	q := fleetQuery("nginx.service")
	q.Hosts = []resource.HostID{"web-01"}
	ch, err := b.List(context.Background(), q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	row := results[0].Items[0]
	if _, ok := row.Fields["since"]; ok {
		t.Errorf("since = %v, want no cell for a unit that never entered active", row.Fields["since"])
	}
	if _, ok := row.Fields["mem"]; ok {
		t.Errorf("mem = %v, want no cell for MemoryCurrent=[not set]", row.Fields["mem"])
	}
	if got, ok := row.Fields["restarts"].(int64); !ok || got != 7 {
		t.Errorf("restarts = %v, want 7", row.Fields["restarts"])
	}
}

// TestUnitFleetRejectsBadUnit is the validation boundary: a unit name that
// could not survive one shell word never reaches a command line.
func TestUnitFleetRejectsBadUnit(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit string
	}{
		{"empty", ""},
		{"space", "nginx service"},
		{"semicolon", "nginx.service; rm -rf /"},
		{"path", "../../etc/passwd"},
		{"quote", "nginx'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := transporttest.New()
			b := newTestBackend(t, ex, inv("web-01"))
			q := fleetQuery(tc.unit)
			q.Hosts = []resource.HostID{"web-01"}
			ch, err := b.List(context.Background(), q)
			if err == nil {
				if ch != nil {
					drainResults(t, ch)
				}
				t.Fatalf("List(%q) returned no error", tc.unit)
			}
			if kind := errs.KindOf(err); kind != errs.Config {
				t.Errorf("List(%q) kind = %s, want config", tc.unit, kind)
			}
			if calls := ex.Calls(); len(calls) != 0 {
				t.Fatalf("List(%q) ran %d commands, want none", tc.unit, len(calls))
			}
		})
	}
}

// TestUnitFleetKindSpec pins the columns the fleet table renders.
func TestUnitFleetKindSpec(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	var spec *resource.KindSpec
	for _, s := range b.Kinds() {
		if s.Name == KindUnitFleet {
			cp := s
			spec = &cp
		}
	}
	if spec == nil {
		t.Fatal("the linux backend serves no unit-fleet kind")
	}
	want := []string{"host", "active", "sub", "since", "restarts", "enabled", "mem"}
	if len(spec.Columns) != len(want) {
		t.Fatalf("unit-fleet has %d columns, want %d", len(spec.Columns), len(want))
	}
	for i, id := range want {
		if spec.Columns[i].ID != id {
			t.Errorf("column %d is %q, want %q", i, spec.Columns[i].ID, id)
		}
	}
	if n, ok := normalizeKind("unit-fleet"); !ok || n != KindUnitFleet {
		t.Errorf("normalizeKind(unit-fleet) = %q, %v", n, ok)
	}
}
