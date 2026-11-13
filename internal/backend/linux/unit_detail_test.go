package linux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport/transporttest"
)

// unitDetailStatus is an active nginx.service's `systemctl status` output.
const unitDetailStatus = `● nginx.service - A high performance web server and a reverse proxy server
     Loaded: loaded (/usr/lib/systemd/system/nginx.service; enabled; preset: disabled)
     Active: active (running) since Thu 2026-11-12 09:12:41 EST; 2h ago
   Main PID: 2231 (nginx)
      Tasks: 3 (limit: 100000)
     Memory: 12.3M
        CPU: 1.204s
     CGroup: /system.slice/nginx.service
             ├─2231 nginx: master process /usr/sbin/nginx
             └─2232 nginx: worker process`

// unitDetailShow is the property list `systemctl show` prints for it.
const unitDetailShow = `ActiveState=active
SubState=running
Result=success
NRestarts=0
ExecMainStatus=0
ExecMainPID=2231
MemoryCurrent=12890112
TasksCurrent=3
ActiveEnterTimestamp=Thu 2026-11-12 09:12:41 EST
InactiveEnterTimestamp=
FragmentPath=/usr/lib/systemd/system/nginx.service
DropInPaths=/etc/systemd/system/nginx.service.d/override.conf
UnitFileState=enabled
`

// unitDetailCat is `systemctl cat` with one drop-in.
const unitDetailCat = `# /usr/lib/systemd/system/nginx.service
[Unit]
Description=A high performance web server and a reverse proxy server
After=network-online.target

[Service]
ExecStart=/usr/sbin/nginx

# /etc/systemd/system/nginx.service.d/override.conf
[Service]
LimitNOFILE=65535

# /etc/systemd/system/nginx.service.d/limits.conf
[Service]
TasksMax=512`

// unitMark writes one probe marker pair exactly like the batch script does.
func unitMark(sb *strings.Builder, nonce, id string, rc int, body string) {
	fmt.Fprintf(sb, "\n@@SITREP:%s:%s:B@@\n%s\n@@SITREP:%s:%s:E:%d@@\n", nonce, id, body, nonce, id, rc)
}

// unitReply scripts the detail batch reply from per-section output and code.
func unitReply(bodies map[string]string, rcs map[string]int) transporttest.Reply {
	const nonce = "deadbeef"
	var out strings.Builder
	for _, id := range []string{secUnitStatus, secUnitShow, secUnitCat} {
		unitMark(&out, nonce, id, rcs[id], bodies[id])
	}
	return transporttest.Reply{Stdout: out.String()}
}

// unitDetailBodies is the fixture above, keyed by section.
func unitDetailBodies() map[string]string {
	return map[string]string{
		secUnitStatus: unitDetailStatus,
		secUnitShow:   unitDetailShow,
		secUnitCat:    unitDetailCat,
	}
}

// newUnitDetailBackend builds a real backend over a scripted transport.
func newUnitDetailBackend(ex *transporttest.Fake) *Backend {
	return New(Options{
		Exec:    ex,
		Timeout: time.Second,
		Nonce:   func() string { return "deadbeef" },
		Now:     func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	})
}

func TestUnitInfoGolden(t *testing.T) {
	ex := transporttest.New()
	ex.On("", unitReply(unitDetailBodies(), map[string]int{}))
	b := newUnitDetailBackend(ex)

	info, err := b.UnitInfo(context.Background(), "web-01", "nginx.service")
	if err != nil {
		t.Fatalf("UnitInfo: %v", err)
	}
	if !strings.Contains(info.Status, "active (running)") {
		t.Errorf("status = %q, want the status text", info.Status)
	}
	if got := info.Props["MemoryCurrent"]; got != "12890112" {
		t.Errorf("MemoryCurrent = %q, want 12890112", got)
	}
	if info.Detail.ActiveState != "active" || info.Detail.SubState != "running" {
		t.Errorf("detail state = %q/%q, want active/running", info.Detail.ActiveState, info.Detail.SubState)
	}
	if info.Detail.NRestarts != 0 || info.Detail.ExecMainPID != "2231" {
		t.Errorf("detail restarts/pid = %d/%q", info.Detail.NRestarts, info.Detail.ExecMainPID)
	}
	if info.Detail.MemoryCurrent != 12890112 || info.Detail.TasksCurrent != 3 {
		t.Errorf("detail memory/tasks = %d/%d", info.Detail.MemoryCurrent, info.Detail.TasksCurrent)
	}
	if got, want := info.Detail.ActiveEnterTimestamp, time.Date(2026, 11, 12, 9, 12, 41, 0, time.FixedZone("EST", -5*3600)); !got.Equal(want) {
		t.Errorf("ActiveEnterTimestamp = %v, want %v", got, want)
	}
	if info.Detail.UnitFileState != "enabled" || info.Detail.FragmentPath != "/usr/lib/systemd/system/nginx.service" {
		t.Errorf("unit file = %q %q", info.Detail.UnitFileState, info.Detail.FragmentPath)
	}
	for _, want := range []string{"# /usr/lib/systemd/system/nginx.service", "LimitNOFILE=65535"} {
		if !strings.Contains(info.Cat, want) {
			t.Errorf("cat does not contain %q:\n%s", want, info.Cat)
		}
	}
	wantDropIns := []string{
		"/etc/systemd/system/nginx.service.d/override.conf",
		"/etc/systemd/system/nginx.service.d/limits.conf",
	}
	if strings.Join(info.DropIns, ",") != strings.Join(wantDropIns, ",") {
		t.Errorf("DropIns = %v, want %v", info.DropIns, wantDropIns)
	}

	calls := ex.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d exec calls, want one batch", len(calls))
	}
	script := calls[0].Command.Script
	for _, want := range []string{
		"systemctl status nginx.service --no-pager -n 0 --full",
		"systemctl show nginx.service --no-pager",
		"systemctl cat nginx.service --no-pager",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("batch script does not contain %q:\n%s", want, script)
		}
	}
}

func TestUnitInfoInactiveRC3(t *testing.T) {
	ex := transporttest.New()
	bodies := unitDetailBodies()
	bodies[secUnitStatus] = "○ nginx.service - A high performance web server\n     Loaded: loaded\n     Active: inactive (dead)"
	bodies[secUnitShow] = "ActiveState=inactive\nSubState=dead\nResult=success\nMemoryCurrent=[not set]\nTasksCurrent=[not set]\n"
	ex.On("", unitReply(bodies, map[string]int{secUnitStatus: 3}))
	b := newUnitDetailBackend(ex)

	info, err := b.UnitInfo(context.Background(), "web-01", "nginx.service")
	if err != nil {
		t.Fatalf("UnitInfo on an inactive unit: %v", err)
	}
	if info.Detail.ActiveState != "inactive" {
		t.Errorf("ActiveState = %q, want inactive", info.Detail.ActiveState)
	}
	if info.Detail.MemoryCurrent != -1 {
		t.Errorf("MemoryCurrent = %d, want -1 for [not set]", info.Detail.MemoryCurrent)
	}
	if !strings.Contains(info.Status, "inactive (dead)") {
		t.Errorf("status = %q, want the inactive text", info.Status)
	}
}

func TestUnitInfoUnitWithoutFile(t *testing.T) {
	ex := transporttest.New()
	bodies := unitDetailBodies()
	bodies[secUnitCat] = ""
	ex.On("", unitReply(bodies, map[string]int{secUnitCat: 1}))
	b := newUnitDetailBackend(ex)

	info, err := b.UnitInfo(context.Background(), "web-01", "nginx.service")
	if err != nil {
		t.Fatalf("UnitInfo without a unit file: %v", err)
	}
	if info.Cat != "" {
		t.Errorf("Cat = %q, want empty", info.Cat)
	}
	if len(info.DropIns) != 1 || info.DropIns[0] != "/etc/systemd/system/nginx.service.d/override.conf" {
		t.Errorf("DropIns = %v, want the one systemd reported", info.DropIns)
	}
}

func TestUnitInfoNoSuchUnit(t *testing.T) {
	ex := transporttest.New()
	ex.On("", unitReply(map[string]string{}, map[string]int{
		secUnitStatus: 4,
		secUnitShow:   4,
		secUnitCat:    4,
	}))
	b := newUnitDetailBackend(ex)

	_, err := b.UnitInfo(context.Background(), "web-01", "ghost.service")
	if errs.KindOf(err) != errs.NotAvailable {
		t.Fatalf("err = %v (kind %q), want %q", err, errs.KindOf(err), errs.NotAvailable)
	}
}

func TestUnitInfoInvalidName(t *testing.T) {
	ex := transporttest.New()
	b := newUnitDetailBackend(ex)

	for _, name := range []string{"nginx; rm -rf /", "", "nginx service", "$(id)"} {
		if _, err := b.UnitInfo(context.Background(), "web-01", name); errs.KindOf(err) != errs.Config {
			t.Errorf("UnitInfo(%q) err = %v, want %q", name, err, errs.Config)
		}
	}
	if calls := ex.Calls(); len(calls) != 0 {
		t.Fatalf("got %d exec calls for invalid unit names, want none", len(calls))
	}
	if _, err := b.UnitInfo(context.Background(), "", "nginx.service"); errs.KindOf(err) != errs.Config {
		t.Errorf("UnitInfo without a host err = %v, want %q", err, errs.Config)
	}
}

func TestUnitDetailLink(t *testing.T) {
	b := newUnitDetailBackend(transporttest.New())
	links := b.Links(resource.Resource{Ref: resource.Ref{Backend: "linux", Kind: KindUnit, Host: "web-01", Name: "nginx.service"}})
	if len(links) != 2 {
		t.Fatalf("got %d links, want detail and journal", len(links))
	}
	if links[0].Key != "enter" || links[0].To.Kind != KindUnitDetail {
		t.Errorf("first link = %+v, want enter → %q", links[0], KindUnitDetail)
	}
	if got := links[0].To.Args["unit"]; got != "nginx.service" {
		t.Errorf("link unit arg = %q", got)
	}
	if links[1].Key != "j" || links[1].To.Kind != KindJournal {
		t.Errorf("second link = %+v, want j → journal", links[1])
	}
	if !hasKind(b.Kinds(), KindUnitDetail) {
		t.Error("Kinds() does not register the unit-detail kind")
	}
}

// hasKind reports whether specs contains name.
func hasKind(specs []resource.KindSpec, name resource.KindName) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}
