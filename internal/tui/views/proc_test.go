package views

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/action"
	"jumpr/internal/backend"
	"jumpr/internal/backend/linux"
	"jumpr/internal/errs"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
)

// procFake is a backend that answers the /proc batch, the socket list, the
// process journal, the kill planner and nothing else.
type procFake struct {
	info    linux.ProcDetail
	infoErr error

	sockets []resource.Resource
	sockErr error

	entries []linux.Entry
	jourErr error
	jourPid string

	specs   []action.Spec
	plan    action.Plan
	planErr error
	planned []action.Request
}

var (
	_ backend.Backend = (*procFake)(nil)
	_ action.Planner  = (*procFake)(nil)
	_ procInfoer      = (*procFake)(nil)
	_ procJournaler   = (*procFake)(nil)
)

func (f *procFake) Name() string                            { return "linux" }
func (f *procFake) Kinds() []resource.KindSpec              { return (&linux.Backend{}).Kinds() }
func (f *procFake) Links(resource.Resource) []resource.Link { return nil }
func (f *procFake) Health(context.Context) backend.Health   { return backend.Health{} }
func (f *procFake) Get(context.Context, resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, nil
}

func (f *procFake) List(context.Context, resource.Query) (<-chan backend.Result, error) {
	ch := make(chan backend.Result, 1)
	if f.sockErr != nil {
		ch <- backend.Result{Err: f.sockErr}
	} else if len(f.sockets) > 0 {
		ch <- backend.Result{Items: f.sockets, Done: 1, Total: 1}
	}
	close(ch)
	return ch, nil
}

func (f *procFake) ProcInfo(_ context.Context, _ resource.HostID, pid string) (linux.ProcDetail, error) {
	if f.infoErr != nil {
		return linux.ProcDetail{PID: 1234}, f.infoErr
	}
	return f.info, nil
}

func (f *procFake) ProcJournal(_ context.Context, _ resource.HostID, pid string, _ linux.JournalOptions) ([]linux.Entry, error) {
	f.jourPid = pid
	return f.entries, f.jourErr
}

func (f *procFake) Actions([]resource.Resource) []action.Spec { return f.specs }

func (f *procFake) Plan(_ context.Context, req action.Request) (action.Plan, error) {
	f.planned = append(f.planned, req)
	if f.planErr != nil {
		return action.Plan{}, f.planErr
	}
	return f.plan, nil
}

// procViewInfo is an sshd at 77 % of its open-file limit.
func procViewInfo() linux.ProcDetail {
	start := time.Date(2026, 11, 12, 9, 12, 41, 0, time.UTC)
	return linux.ProcDetail{
		PID:      1234,
		Status:   map[string]string{"Name": "sshd", "State": "S (sleeping)", "Threads": "1"},
		Name:     "sshd",
		State:    "S (sleeping)",
		Threads:  1,
		RSS:      3456 * 1024,
		VMSize:   11244 * 1024,
		FDs:      900,
		FDSoft:   1024,
		FDWarn:   true,
		OOMScore: 0,
		Start:    start,
		Exe:      "/usr/sbin/sshd",
		Cwd:      "/",
		Cgroup:   linux.ProcCgroup{Path: "/system.slice/sshd.service", Unit: "sshd.service"},
		Limits: []linux.ProcLimit{
			{Name: "Max open files", Soft: "1024", Hard: "1048576", Units: "files"},
			{Name: "Max processes", Soft: "31151", Hard: "31151", Units: "processes"},
		},
		EnvKeys: []string{"HOME", "LANG", "PATH"},
		IO:      linux.ProcIO{RChar: 12345, WChar: 6789, ReadBytes: 4096, WriteBytes: 2048},
		Failed:  map[string]error{},
	}
}

// procSocketRow is one socket list row held by pid.
func procSocketRow(local, peer string, pids ...int) resource.Resource {
	s := linux.Socket{Proto: "tcp", State: "LISTEN", LocalPort: portOf(local), PeerPort: portOf(peer)}
	for _, pid := range pids {
		s.Procs = append(s.Procs, linux.SockProc{Name: "proc" + string(rune('0'+pid%10)), PID: pid, FD: 3})
	}
	return resource.Resource{
		Ref: resource.Ref{Backend: "linux", Kind: linux.KindSocket, Host: "web-01"},
		Fields: map[string]any{
			"proto":   "tcp",
			"state":   "LISTEN",
			"local":   local,
			"peer":    peer,
			"process": "sshd",
		},
		Raw: s,
	}
}

// portOf is the text after the last colon of an address.
func portOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// procJournalEntry is one journal record of the process.
func procJournalEntry(at time.Time, pri int, msg string) linux.Entry {
	return linux.Entry{Time: at, Priority: pri, Source: "sshd", PID: "1234", Message: msg}
}

// newProcFake is the fixture backend: a live sshd, one of its sockets and two
// journal records, plus the kill spec.
func newProcFake() *procFake {
	base := fixedNow.Add(-10 * time.Minute)
	return &procFake{
		info: procViewInfo(),
		sockets: []resource.Resource{
			procSocketRow("0.0.0.0:22", "0.0.0.0:*", 1234),
			procSocketRow("0.0.0.0:80", "0.0.0.0:*", 2000),
		},
		entries: []linux.Entry{
			procJournalEntry(base, 6, "Accepted publickey for sre"),
			procJournalEntry(base.Add(time.Minute), 3, "error: kex_exchange_identification"),
		},
		specs: []action.Spec{{ID: "linux.kill", Title: "kill", Risk: action.RiskDestructive,
			Params: []action.Param{{Name: "signal", Default: "TERM"}}}},
		plan: action.Plan{
			Title: "kill -TERM 1234 on 1 host",
			Steps: []action.Step{{Target: action.Target{Host: "web-01"}, Display: "sudo -n kill -TERM 1234"}},
		},
	}
}

// drainProc runs a command and everything it asks for next, as the root would.
func drainProc(t *testing.T, p *Proc, cmd tea.Cmd) {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i > 5000 {
			t.Fatal("commands did not settle")
		}
		msg := cmd()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if c != nil {
					drainProc(t, p, c)
				}
			}
			return
		}
		_, next := p.Update(msg)
		cmd = next
	}
}

// procFixture builds a loaded drill-in over the fake backend.
func procFixture(t *testing.T) (*Proc, *procFake) {
	t.Helper()
	f := newProcFake()
	p := NewProc(testEnv(), f, "web-01", "1234")
	drainProc(t, p, p.Init())
	return p, f
}

// sendProcKey routes one key and returns its command.
func sendProcKey(t *testing.T, p *Proc, key string) tea.Cmd {
	t.Helper()
	v, cmd := p.Update(keyMsg(key))
	if _, ok := v.(*Proc); !ok {
		t.Fatalf("Update returned %T, want *Proc", v)
	}
	return cmd
}

func TestProcOverviewTab(t *testing.T) {
	p, _ := procFixture(t)
	frame := stripHostANSI(p.Render(120, 30))

	for _, want := range []string{
		"pid", "1234",
		"name", "sshd",
		"state", "S (sleeping)",
		"threads",
		"rss", "3.4 MiB",
		"virtual", "11.0 MiB",
		"fds", "900 / 1024", "above 80 %",
		"oom score",
		"started", "2026-11-12 09:12:41 UTC",
		"exe", "/usr/sbin/sshd",
		"cwd", "/",
		"cgroup", "/system.slice/sshd.service", "sshd.service",
		"1 overview", "2 limits", "3 env", "4 sockets", "5 journal",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame does not contain %q:\n%s", want, frame)
		}
	}
	if p.Crumb() != "pid 1234" {
		t.Errorf("Crumb = %q, want the pid", p.Crumb())
	}
	sub := p.Subject()
	if sub.Kind != linux.KindProcessDetail || sub.Ref.Name != "1234" || len(sub.Hosts) != 1 || sub.Hosts[0] != "web-01" {
		t.Errorf("Subject = %+v", sub)
	}
	if p.CommandLine() != "process-detail --host web-01 --arg pid=1234" {
		t.Errorf("CommandLine = %q", p.CommandLine())
	}
}

func TestProcTabsSwitch(t *testing.T) {
	p, f := procFixture(t)

	sendProcKey(t, p, "2")
	if p.tabs.Active() != p.limits {
		t.Errorf("after 2 Active = %d, want the limits tab", p.tabs.Active())
	}
	frame := stripHostANSI(p.Render(110, 20))
	for _, want := range []string{"LIMIT", "SOFT", "HARD", "Max open files", "1048576", "files"} {
		if !strings.Contains(frame, want) {
			t.Errorf("limits tab does not contain %q:\n%s", want, frame)
		}
	}

	sendProcKey(t, p, "3")
	if p.tabs.Active() != p.envKeys {
		t.Errorf("after 3 Active = %d, want the env tab", p.tabs.Active())
	}
	frame = stripHostANSI(p.Render(110, 20))
	for _, want := range []string{"keys", "values are cut off", "HOME", "LANG", "PATH"} {
		if !strings.Contains(frame, want) {
			t.Errorf("env tab does not contain %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "=") {
		t.Errorf("env tab shows a value:\n%s", frame)
	}

	sendProcKey(t, p, "4")
	if p.tabs.Active() != p.sockets {
		t.Errorf("after 4 Active = %d, want the sockets tab", p.tabs.Active())
	}
	frame = stripHostANSI(p.Render(120, 20))
	if !strings.Contains(frame, "0.0.0.0:22") {
		t.Errorf("sockets tab lost this pid's socket:\n%s", frame)
	}
	if strings.Contains(frame, "0.0.0.0:80") {
		t.Errorf("sockets tab shows another pid's socket:\n%s", frame)
	}

	drainProc(t, p, sendProcKey(t, p, "5"))
	if p.tabs.Active() != p.journal {
		t.Errorf("after 5 Active = %d, want the journal tab", p.tabs.Active())
	}
	if f.jourPid != "1234" {
		t.Errorf("journal asked for pid %q, want 1234", f.jourPid)
	}
	frame = stripHostANSI(p.Render(120, 20))
	for _, want := range []string{"_PID=1234", "Accepted publickey for sre", "kex_exchange_identification"} {
		if !strings.Contains(frame, want) {
			t.Errorf("journal tab does not contain %q:\n%s", want, frame)
		}
	}

	sendProcKey(t, p, "tab")
	if p.tabs.Active() != p.overview {
		t.Errorf("tab cycled to %d, want the overview", p.tabs.Active())
	}
}

func TestProcExited(t *testing.T) {
	f := newProcFake()
	f.infoErr = errs.New(errs.NotAvailable, "linux.ProcInfo", "process exited").WithHost("web-01")
	p := NewProc(testEnv(), f, "web-01", "1234")
	drainProc(t, p, p.Init())

	frame := stripHostANSI(p.Render(100, 20))
	for _, want := range []string{"process exited", "pid 1234", "esc"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame does not contain %q:\n%s", want, frame)
		}
	}
	hints := p.Hints()
	if len(hints) == 0 || hints[0].Key != "esc" || hints[0].Text != "back" {
		t.Errorf("Hints = %+v, want esc → back", hints)
	}
	cmd := sendProcKey(t, p, "esc")
	if cmd == nil {
		t.Fatal("esc on a gone process returned no command")
	}
	if _, ok := cmd().(tui.PopViewMsg); !ok {
		t.Errorf("esc returned %T, want tui.PopViewMsg", cmd())
	}
}

func TestProcDetailErrorScreen(t *testing.T) {
	f := newProcFake()
	f.infoErr = errs.New(errs.Sudo, "linux.ProcInfo", "sudo -n needs a password").WithHost("web-01")
	p := NewProc(testEnv(), f, "web-01", "1234")
	drainProc(t, p, p.Init())

	frame := stripHostANSI(p.Render(100, 20))
	if !strings.Contains(frame, "sudo") {
		t.Errorf("error screen does not name the kind:\n%s", frame)
	}
	if !strings.Contains(frame, "esc") {
		t.Errorf("error screen does not offer the way back:\n%s", frame)
	}
}

func TestProcActionsKey(t *testing.T) {
	p, _ := procFixture(t)
	cmd := sendProcKey(t, p, "x")
	if cmd == nil {
		t.Fatal("x returned no command")
	}
	msg, ok := cmd().(tui.OpenDialogMsg)
	if !ok {
		t.Fatalf("x returned %T, want tui.OpenDialogMsg", cmd())
	}
	if !strings.Contains(msg.Dialog.Title(), "1234") {
		t.Errorf("dialog title = %q, want the pid", msg.Dialog.Title())
	}
	body := stripHostANSI(msg.Dialog.View(60, 10))
	if !strings.Contains(body, "! kill") || !strings.Contains(body, "linux.kill") {
		t.Errorf("actions menu does not offer the destructive kill:\n%s", body)
	}

	// Picking the spec shows the read-only plan.
	_, pick := msg.Dialog.Update(keyMsg("enter"))
	if pick == nil {
		t.Fatal("picking a spec returned no command")
	}
	pickMsg := pick()
	batch, ok := pickMsg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("pick returned %T, want tea.BatchMsg", pickMsg)
	}
	var preview tui.OpenDialogMsg
	for _, c := range batch {
		if c == nil {
			continue
		}
		if m, ok := c().(tui.OpenDialogMsg); ok {
			preview = m
		}
	}
	if preview.Dialog == nil {
		t.Fatal("picking a spec did not show the plan")
	}
	if got := stripHostANSI(preview.Dialog.View(60, 10)); !strings.Contains(got, "sudo -n kill -TERM 1234") {
		t.Errorf("plan preview does not show the command:\n%s", got)
	}
}

func TestProcExactSize(t *testing.T) {
	p, _ := procFixture(t)
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		frame := p.Render(size[0], size[1])
		lines := strings.Split(frame, "\n")
		if len(lines) != size[1] {
			t.Fatalf("%dx%d: %d lines, want %d", size[0], size[1], len(lines), size[1])
		}
		for i, line := range lines {
			if got := render.Width(line); got != size[0] {
				t.Fatalf("%dx%d: line %d is %d cells, want %d", size[0], size[1], i, got, size[0])
			}
		}
	}
}
