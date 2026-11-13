package linux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jumper/internal/resource"
)

func readUnitTestdata(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", "testdata", "linux", "units", "rocky9", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestParseListUnitsGolden(t *testing.T) {
	units, err := ParseListUnits(readUnitTestdata(t, "list-units.txt"))
	if err != nil {
		t.Fatalf("ParseListUnits: %v", err)
	}
	if got, want := len(units), 12; got != want {
		t.Fatalf("len(units) = %d, want %d: %+v", got, want, units)
	}

	// The bullet is printed for failed units even with --plain; it must be dropped.
	if got, want := units[0], (Unit{
		Name:        "nginx.service",
		Load:        "loaded",
		Active:      "failed",
		Sub:         "failed",
		Description: "The nginx HTTP and reverse proxy server",
	}); got != want {
		t.Errorf("units[0] = %+v, want %+v", got, want)
	}

	// not-found unit: still four fields, description repeats the name.
	if got, want := units[9], (Unit{
		Name:        "display-manager.service",
		Load:        "not-found",
		Active:      "inactive",
		Sub:         "dead",
		Description: "display-manager.service",
	}); got != want {
		t.Errorf("units[9] = %+v, want %+v", got, want)
	}

	// A description with a dash and inner single spaces stays intact.
	if got, want := units[4].Description, "firewalld - dynamic firewall daemon"; got != want {
		t.Errorf("units[4].Description = %q, want %q", got, want)
	}
	if got, want := units[6].Name, "logrotate.timer"; got != want {
		t.Errorf("units[6].Name = %q, want %q", got, want)
	}
}

func TestParseListUnitsPrefixesAndSpacing(t *testing.T) {
	out := "● nginx.service   loaded    failed    failed    The nginx HTTP    and reverse proxy server\n" +
		"* broken.service  loaded    failed    failed    broken\n" +
		"  indented.service  loaded  active  running  leading spaces in the line\n"
	units, err := ParseListUnits(out)
	if err != nil {
		t.Fatalf("ParseListUnits: %v", err)
	}
	if got, want := len(units), 3; got != want {
		t.Fatalf("len(units) = %d, want %d: %+v", got, want, units)
	}
	if got, want := units[0].Name, "nginx.service"; got != want {
		t.Errorf("units[0].Name = %q, want %q", got, want)
	}
	if got, want := units[0].Description, "The nginx HTTP and reverse proxy server"; got != want {
		t.Errorf("units[0].Description = %q, want %q (fields joined with one space)", got, want)
	}
	if got, want := units[1].Name, "broken.service"; got != want {
		t.Errorf("units[1].Name = %q, want %q", got, want)
	}
	if got, want := units[2].Name, "indented.service"; got != want {
		t.Errorf("units[2].Name = %q, want %q", got, want)
	}
}

func TestParseListUnitsShortAndEmptyLines(t *testing.T) {
	out := "\n" +
		"nginx.service loaded failed\n" + // three fields: skipped
		"   \n" +
		"onlyname\n" +
		"sshd.service loaded active running OpenSSH server daemon\n"
	units, err := ParseListUnits(out)
	if err != nil {
		t.Fatalf("ParseListUnits: %v", err)
	}
	if got, want := len(units), 1; got != want {
		t.Fatalf("len(units) = %d, want %d: %+v", got, want, units)
	}
	if got, want := units[0].Name, "sshd.service"; got != want {
		t.Errorf("units[0].Name = %q, want %q", got, want)
	}
}

func TestParseListUnitsEmpty(t *testing.T) {
	for _, in := range []string{"", "\n", "  \n\t\n"} {
		units, err := ParseListUnits(in)
		if err != nil {
			t.Fatalf("ParseListUnits(%q): %v", in, err)
		}
		if len(units) != 0 {
			t.Errorf("ParseListUnits(%q) = %+v, want empty", in, units)
		}
		if units == nil {
			// nil is acceptable for an empty result, but callers must not need a guard.
			continue
		}
	}
}

func TestParseListUnitsGarbageDoesNotPanic(t *testing.T) {
	in := "\x00\x01\xff\xfe garbage\n\x00\x00\nshort line\n● \n   a   b   c   d   \n"
	units, err := ParseListUnits(in)
	if err != nil {
		t.Fatalf("ParseListUnits: %v", err)
	}
	// Only "a b c <empty description>" has four fields.
	if got, want := len(units), 1; got != want {
		t.Fatalf("len(units) = %d, want %d: %+v", got, want, units)
	}
	if got, want := units[0].Name, "a"; got != want {
		t.Errorf("units[0].Name = %q, want %q", got, want)
	}
}

func TestListUnitsTypeSuffix(t *testing.T) {
	cases := []struct{ name, want string }{
		{"nginx.service", "service"},
		{"logrotate.timer", "timer"},
		{"dbus.socket", "socket"},
		{"mnt-data.mount", "mount"},
		{"systemd-udevd.service", "service"},
		{"nodot", ""},
		{"", ""},
		{"weird.name.with.dots.target", "target"},
	}
	for _, c := range cases {
		if got := (Unit{Name: c.name}).Type(); got != c.want {
			t.Errorf("Unit{%q}.Type() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestListUnitsStatusMapping(t *testing.T) {
	cases := []struct {
		active, sub string
		want        resource.Status
	}{
		{"failed", "failed", resource.StatusCrit},
		{"failed", "auto-restart", resource.StatusCrit},
		{"activating", "start", resource.StatusWarn},
		{"deactivating", "stop", resource.StatusWarn},
		{"reloading", "reload", resource.StatusWarn},
		{"active", "auto-restart", resource.StatusWarn},
		{"active", "running", resource.StatusOK},
		{"active", "waiting", resource.StatusOK},
		{"active", "exited", resource.StatusOK},
		{"inactive", "dead", resource.StatusUnknown},
		{"inactive", "exited", resource.StatusUnknown},
		{"", "", resource.StatusUnknown},
	}
	for _, c := range cases {
		if got := (Unit{Name: "x.service", Active: c.active, Sub: c.sub}).Status(); got != c.want {
			t.Errorf("Unit{active:%q sub:%q}.Status() = %v, want %v", c.active, c.sub, got, c.want)
		}
	}
}

func TestParseShowGolden(t *testing.T) {
	props := ParseShow(readUnitTestdata(t, "show-nginx.txt"))
	if got, want := len(props), 28; got != want {
		t.Fatalf("len(props) = %d, want %d", got, want)
	}
	if got, want := props["ActiveState"], "active"; got != want {
		t.Errorf("props[ActiveState] = %q, want %q", got, want)
	}
	if got, want := props["FragmentPath"], "/usr/lib/systemd/system/nginx.service"; got != want {
		t.Errorf("props[FragmentPath] = %q, want %q", got, want)
	}
	// A value that contains "=" keeps everything after the first one.
	exec := props["ExecStart"]
	if !strings.HasPrefix(exec, "{ path=/usr/sbin/nginx ; argv[]=/usr/sbin/nginx -c /etc/nginx/nginx.conf ;") {
		t.Errorf("props[ExecStart] = %q, want the path=... value intact", exec)
	}
	if !strings.Contains(exec, "ignore_errors=no") {
		t.Errorf("props[ExecStart] = %q, want inner key=value pairs kept", exec)
	}
	// Repeated keys are joined with a newline, in order.
	if got, want := props["Environment"], "NGINX_PORT=8080\nNGINX_WORKERS=4"; got != want {
		t.Errorf("props[Environment] = %q, want %q", got, want)
	}
	// An empty value is kept as an empty string.
	if v, ok := props["InactiveEnterTimestamp"]; !ok || v != "" {
		t.Errorf("props[InactiveEnterTimestamp] = %q, %v; want \"\", true", v, ok)
	}
}

func TestParseShowEdgeCases(t *testing.T) {
	if props := ParseShow(""); len(props) != 0 {
		t.Errorf("ParseShow(\"\") = %+v, want empty", props)
	}
	props := ParseShow("A=1\nno equals here\n\nB=2\nA=3\r\nC=x=y\r\n")
	if got, want := props["A"], "1\n3"; got != want {
		t.Errorf("props[A] = %q, want %q", got, want)
	}
	if got, want := props["C"], "x=y"; got != want {
		t.Errorf("props[C] = %q, want %q", got, want)
	}
	if _, ok := props["no equals here"]; ok {
		t.Errorf("a line without '=' must be skipped: %+v", props)
	}
	if len(props) != 3 {
		t.Errorf("len(props) = %d, want 3: %+v", len(props), props)
	}
}

func TestUnitDetailGolden(t *testing.T) {
	d := UnitDetail(ParseShow(readUnitTestdata(t, "show-nginx.txt")))

	if got, want := d.ActiveState, "active"; got != want {
		t.Errorf("ActiveState = %q, want %q", got, want)
	}
	if got, want := d.SubState, "running"; got != want {
		t.Errorf("SubState = %q, want %q", got, want)
	}
	if got, want := d.Result, "success"; got != want {
		t.Errorf("Result = %q, want %q", got, want)
	}
	if got, want := d.NRestarts, 0; got != want {
		t.Errorf("NRestarts = %d, want %d", got, want)
	}
	if got, want := d.ExecMainStatus, 0; got != want {
		t.Errorf("ExecMainStatus = %d, want %d", got, want)
	}
	if got, want := d.ExecMainPID, "1842"; got != want {
		t.Errorf("ExecMainPID = %q, want %q", got, want)
	}
	if got, want := d.MemoryCurrent, int64(5242880); got != want {
		t.Errorf("MemoryCurrent = %d, want %d", got, want)
	}
	if got, want := d.TasksCurrent, 3; got != want {
		t.Errorf("TasksCurrent = %d, want %d", got, want)
	}
	// The zone abbreviation may resolve to a fixed offset or to zero (see the task
	// pitfalls); the wall-clock fields are what matters.
	if got, want := d.ActiveEnterTimestamp.Format("Mon 2006-01-02 15:04:05"), "Thu 2026-11-12 09:14:02"; got != want {
		t.Errorf("ActiveEnterTimestamp = %v, want wall clock %q", d.ActiveEnterTimestamp, want)
	}
	if !d.InactiveEnterTimestamp.IsZero() {
		t.Errorf("InactiveEnterTimestamp = %v, want zero", d.InactiveEnterTimestamp)
	}
	if got, want := d.FragmentPath, "/usr/lib/systemd/system/nginx.service"; got != want {
		t.Errorf("FragmentPath = %q, want %q", got, want)
	}
	wantDrops := []string{
		"/etc/systemd/system/nginx.service.d/override.conf",
		"/etc/systemd/system/nginx.service.d/limits.conf",
	}
	if len(d.DropInPaths) != len(wantDrops) {
		t.Fatalf("DropInPaths = %v, want %v", d.DropInPaths, wantDrops)
	}
	for i := range wantDrops {
		if d.DropInPaths[i] != wantDrops[i] {
			t.Errorf("DropInPaths[%d] = %q, want %q", i, d.DropInPaths[i], wantDrops[i])
		}
	}
	if got, want := d.UnitFileState, "enabled"; got != want {
		t.Errorf("UnitFileState = %q, want %q", got, want)
	}
}

func TestUnitDetailMemoryCurrentNotSet(t *testing.T) {
	cases := map[string]int64{
		"[not set]":            -1,
		"18446744073709551615": -1, // (uint64 max, systemd's "infinity")
		"":                     -1,
		"18446744073709551614": -1, // out of int64 range
		"not-a-number":         -1,
		"0":                    0,
		"1048576":              1048576,
	}
	for in, want := range cases {
		d := UnitDetail(map[string]string{"MemoryCurrent": in})
		if got := d.MemoryCurrent; got != want {
			t.Errorf("MemoryCurrent(%q) = %d, want %d", in, got, want)
		}
	}
	if got := UnitDetail(nil).MemoryCurrent; got != -1 {
		t.Errorf("MemoryCurrent(absent) = %d, want -1", got)
	}
}

func TestUnitDetailPartialAndBadValues(t *testing.T) {
	d := UnitDetail(map[string]string{
		"NRestarts":              "7",
		"ExecMainStatus":         "1",
		"TasksCurrent":           "nope",
		"ActiveEnterTimestamp":   "garbage",
		"InactiveEnterTimestamp": "Thu 2026-13-45 99:99:99 UTC",
		"DropInPaths":            "",
		"ExecMainPID":            "",
	})
	if got, want := d.NRestarts, 7; got != want {
		t.Errorf("NRestarts = %d, want %d", got, want)
	}
	if got, want := d.ExecMainStatus, 1; got != want {
		t.Errorf("ExecMainStatus = %d, want %d", got, want)
	}
	if got, want := d.TasksCurrent, 0; got != want {
		t.Errorf("TasksCurrent = %d, want %d", got, want)
	}
	if !d.ActiveEnterTimestamp.IsZero() || !d.InactiveEnterTimestamp.IsZero() {
		t.Errorf("bad timestamps must be zero: %+v", d)
	}
	if len(d.DropInPaths) != 0 {
		t.Errorf("DropInPaths = %v, want empty", d.DropInPaths)
	}

	empty := UnitDetail(nil)
	if empty.ActiveState != "" || empty.NRestarts != 0 || !empty.ActiveEnterTimestamp.IsZero() {
		t.Errorf("UnitDetail(nil) = %+v, want zero values except MemoryCurrent", empty)
	}
}

func TestSortUnits(t *testing.T) {
	us := []Unit{
		{Name: "sshd.service", Active: "active", Sub: "running"},
		{Name: "dbus.socket", Active: "active", Sub: "running"},
		{Name: "logrotate.timer", Active: "active", Sub: "waiting"},
		{Name: "nginx.service", Active: "failed", Sub: "failed"},
		{Name: "haproxy.service", Active: "failed", Sub: "failed"},
		{Name: "chronyd.service", Active: "inactive", Sub: "dead"},
		{Name: "mnt-data.mount", Active: "active", Sub: "mounted"},
	}
	SortUnits(us)
	got := make([]string, len(us))
	for i, u := range us {
		got[i] = u.Name
	}
	want := []string{
		"haproxy.service", // failed first, services before other types
		"nginx.service",   // failed, then by name
		"chronyd.service", // services next, by name (chronyd < sshd)
		"sshd.service",
		"mnt-data.mount", // remaining types, alphabetically by type
		"dbus.socket",
		"logrotate.timer",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SortUnits order = %v, want %v", got, want)
	}

	// Sorting an empty slice must not panic.
	SortUnits(nil)
}
