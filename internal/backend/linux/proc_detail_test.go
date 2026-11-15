package linux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport/transporttest"
)

// procStatusFixture is an sshd's /proc/PID/status.
const procStatusFixture = `Name:	sshd
Umask:	0022
State:	S (sleeping)
Tgid:	1234
Pid:	1234
PPid:	1
Threads:	1
VmSize:	  11244 kB
VmRSS:	    3456 kB
`

// procEnvironFixture is what the remote pipeline prints: keys only.
const procEnvironFixture = `HOME
LANG
PATH
SSH_CONNECTION
`

// procIOFixture is an /proc/PID/io counter set.
const procIOFixture = `rchar: 12345
wchar: 6789
syscr: 10
syscw: 20
read_bytes: 4096
write_bytes: 2048
`

// procLimitsRow renders one /proc/PID/limits row in the kernel's fixed-width
// layout.
func procLimitsRow(name, soft, hard, units string) string {
	return fmt.Sprintf("%-25s %-19s %-19s %s", name, soft, hard, units)
}

// procLimitsFixture is a real /proc/PID/limits, including the header.
func procLimitsFixture() string {
	rows := [][4]string{
		{"Max cpu time", "unlimited", "unlimited", "seconds"},
		{"Max file size", "unlimited", "unlimited", "bytes"},
		{"Max open files", "1024", "1048576", "files"},
		{"Max locked memory", "8192", "8192", "bytes"},
		{"Max processes", "31151", "31151", "processes"},
	}
	body := []string{procLimitsRow("Limit", "Soft Limit", "Hard Limit", "Units")}
	for _, r := range rows {
		body = append(body, procLimitsRow(r[0], r[1], r[2], r[3]))
	}
	return strings.Join(body, "\n") + "\n"
}

// procBodies is the full fixture, keyed by section ID.
func procBodies() map[string]string {
	return map[string]string{
		secProcStatus:  procStatusFixture,
		secProcLimits:  procLimitsFixture(),
		secProcCgroup:  "0::/system.slice/sshd.service\n",
		secProcFDs:     "27\n",
		secProcExe:     "/usr/sbin/sshd\n",
		secProcCwd:     "/\n",
		secProcEnviron: procEnvironFixture,
		secProcIO:      procIOFixture,
		secProcOOM:     "0\n",
		secProcStart:   "Wed Nov 12 09:12:41 2026\n",
	}
}

// procSectionIDs is every section of the batch, in probe order.
func procSectionIDs() []string {
	return []string{
		secProcStatus, secProcLimits, secProcCgroup, secProcFDs, secProcExe,
		secProcCwd, secProcEnviron, secProcIO, secProcOOM, secProcStart,
	}
}

// procReply scripts the batch reply from per-section output, exit code and
// stderr.
func procReply(bodies map[string]string, rcs map[string]int, stderrs map[string]string) transporttest.Reply {
	const nonce = "deadbeef"
	var out strings.Builder
	for _, id := range procSectionIDs() {
		body := bodies[id]
		if e := stderrs[id]; e != "" {
			body = e
		}
		fmt.Fprintf(&out, "\n@@SITREP:%s:%s:B@@\n%s\n@@SITREP:%s:%s:E:%d@@\n", nonce, id, body, nonce, id, rcs[id])
	}
	return transporttest.Reply{Stdout: out.String()}
}

// newProcBackend builds a real backend over a scripted transport.
func newProcBackend(ex *transporttest.Fake) *Backend {
	return New(Options{
		Exec:    ex,
		Timeout: time.Second,
		Nonce:   func() string { return "deadbeef" },
		Now:     func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	})
}

// procFixture runs the batch against the full fixture.
func procFixture(t *testing.T) (ProcDetail, []transporttest.Call) {
	t.Helper()
	ex := transporttest.New()
	ex.On("", procReply(procBodies(), map[string]int{}, nil))
	b := newProcBackend(ex)
	info, err := b.ProcInfo(context.Background(), "web-01", "1234")
	if err != nil {
		t.Fatalf("ProcInfo: %v", err)
	}
	return info, ex.Calls()
}

func TestProcDetailGolden(t *testing.T) {
	info, calls := procFixture(t)

	if info.PID != 1234 || info.Name != "sshd" || info.State != "S (sleeping)" || info.Threads != 1 {
		t.Errorf("pid/name/state/threads = %d/%q/%q/%d", info.PID, info.Name, info.State, info.Threads)
	}
	if info.RSS != 3456*1024 || info.VMSize != 11244*1024 {
		t.Errorf("rss/vm = %d/%d, want 3456/11244 KiB in bytes", info.RSS, info.VMSize)
	}
	if info.FDs != 27 || info.FDSoft != 1024 || info.FDWarn {
		t.Errorf("fds = %d/%d warn=%v, want 27 of 1024 without a warning", info.FDs, info.FDSoft, info.FDWarn)
	}
	if info.OOMScore != 0 {
		t.Errorf("oom score = %d, want 0", info.OOMScore)
	}
	wantStart := time.Date(2026, 11, 12, 9, 12, 41, 0, time.UTC)
	if !info.Start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", info.Start, wantStart)
	}
	if info.Exe != "/usr/sbin/sshd" || info.Cwd != "/" {
		t.Errorf("exe/cwd = %q/%q", info.Exe, info.Cwd)
	}
	if info.Cgroup.Path != "/system.slice/sshd.service" || info.Cgroup.Unit != "sshd.service" {
		t.Errorf("cgroup = %+v, want the path and sshd.service", info.Cgroup)
	}
	if got := info.IO; got.RChar != 12345 || got.WChar != 6789 || got.ReadBytes != 4096 || got.WriteBytes != 2048 {
		t.Errorf("io = %+v", got)
	}
	if len(info.Failed) != 0 {
		t.Errorf("Failed = %v, want nothing for a complete fixture", info.Failed)
	}

	wantKeys := []string{"HOME", "LANG", "PATH", "SSH_CONNECTION"}
	if strings.Join(info.EnvKeys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("EnvKeys = %v, want %v", info.EnvKeys, wantKeys)
	}

	var open ProcLimit
	for _, lim := range info.Limits {
		if lim.Name == "Max open files" {
			open = lim
		}
	}
	if open.Soft != "1024" || open.Hard != "1048576" || open.Units != "files" {
		t.Errorf("Max open files = %+v", open)
	}
	if len(info.Limits) != 5 {
		t.Errorf("got %d limits, want 5", len(info.Limits))
	}

	if len(calls) != 1 {
		t.Fatalf("got %d exec calls, want one batch", len(calls))
	}
	script := calls[0].Command.Script
	for _, want := range []string{
		"cat /proc/1234/status",
		"cat /proc/1234/limits",
		"cat /proc/1234/cgroup",
		"ls /proc/1234/fd | wc -l",
		"readlink /proc/1234/exe",
		"readlink /proc/1234/cwd",
		"/proc/1234/environ | cut -d= -f1 | sort",
		"cat /proc/1234/io",
		"cat /proc/1234/oom_score",
		"ps -o lstart= -p 1234",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("batch script does not contain %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "cat /proc/1234/environ") {
		t.Errorf("environment values would reach the laptop; the batch must cut them:\n%s", script)
	}
	if n := strings.Count(script, "sudo -n -- sh -c"); n != len(procSectionIDs()) {
		t.Errorf("script runs %d sudo commands, want %d (one privileged batch)", n, len(procSectionIDs()))
	}
}

func TestProcDetailFDWarn(t *testing.T) {
	cases := []struct {
		fds   int
		soft  string
		warn  bool
		label string
	}{
		{819, "1024", false, "below 80 %"},
		{820, "1024", true, "above 80 %"},
		{1024, "1024", true, "at the limit"},
		{5, "unlimited", false, "unlimited"},
	}
	for _, c := range cases {
		bodies := procBodies()
		bodies[secProcFDs] = strconv.Itoa(c.fds) + "\n"
		bodies[secProcLimits] = strings.Replace(procLimitsFixture(), "1024                 1048576", c.soft+"                 1048576", 1)
		ex := transporttest.New()
		ex.On("", procReply(bodies, map[string]int{}, nil))
		info, err := newProcBackend(ex).ProcInfo(context.Background(), "web-01", "1234")
		if err != nil {
			t.Fatalf("%s: ProcInfo: %v", c.label, err)
		}
		if info.FDWarn != c.warn {
			t.Errorf("%s: FDWarn = %v (fds %d, soft %d), want %v", c.label, info.FDWarn, info.FDs, info.FDSoft, c.warn)
		}
	}
}

func TestProcDetailExited(t *testing.T) {
	ex := transporttest.New()
	gone := "cat: /proc/999/status: No such file or directory\n"
	stderrs := map[string]string{}
	for _, id := range procSectionIDs() {
		stderrs[id] = gone
	}
	rcs := map[string]int{}
	for _, id := range procSectionIDs() {
		rcs[id] = 1
	}
	ex.On("", procReply(map[string]string{}, rcs, stderrs))
	b := newProcBackend(ex)

	info, err := b.ProcInfo(context.Background(), "web-01", "999")
	if errs.KindOf(err) != errs.NotAvailable {
		t.Fatalf("err = %v (kind %q), want %q", err, errs.KindOf(err), errs.NotAvailable)
	}
	if !ProcessExited(err) {
		t.Errorf("ProcessExited(%v) = false, want true", err)
	}
	if !strings.Contains(err.Error(), "process exited") {
		t.Errorf("message %q does not say the process exited", err.Error())
	}
	if info.PID != 999 {
		t.Errorf("PID = %d, want 999 even when the process is gone", info.PID)
	}
	if ProcessExited(errs.New(errs.Timeout, "linux.ProcInfo", "process exited")) {
		t.Error("ProcessExited is true for a timeout")
	}
}

func TestProcDetailInvalidPID(t *testing.T) {
	ex := transporttest.New()
	b := newProcBackend(ex)

	for _, pid := range []string{"", "0", "-1", "1.5", "12; rm -rf /", "$(id)", "12 34", "pid"} {
		_, err := b.ProcInfo(context.Background(), "web-01", pid)
		if errs.KindOf(err) != errs.Config {
			t.Errorf("ProcInfo(%q) err = %v, want %q", pid, err, errs.Config)
		}
	}
	if calls := ex.Calls(); len(calls) != 0 {
		t.Fatalf("got %d exec calls for invalid pids, want none", len(calls))
	}
	if _, err := b.ProcInfo(context.Background(), "", "1234"); errs.KindOf(err) != errs.Config {
		t.Errorf("ProcInfo without a host err = %v, want %q", err, errs.Config)
	}
}

func TestProcDetailPartialFailure(t *testing.T) {
	ex := transporttest.New()
	bodies := procBodies()
	delete(bodies, secProcEnviron)
	ex.On("", procReply(bodies, map[string]int{secProcEnviron: 1},
		map[string]string{secProcEnviron: "cat: /proc/1234/environ: Permission denied\n"}))
	b := newProcBackend(ex)

	info, err := b.ProcInfo(context.Background(), "web-01", "1234")
	if err != nil {
		t.Fatalf("ProcInfo with one unreadable section: %v", err)
	}
	if info.Failed[secProcEnviron] == nil {
		t.Errorf("Failed does not record the environment section: %v", info.Failed)
	}
	if len(info.EnvKeys) != 0 {
		t.Errorf("EnvKeys = %v, want none", info.EnvKeys)
	}
	if info.Name != "sshd" || info.FDs != 27 {
		t.Errorf("an unreadable section hid the rest: name %q fds %d", info.Name, info.FDs)
	}
}

func TestProcDetailEnvNeverCarriesValues(t *testing.T) {
	// A hostile fixture: values the parser must never surface even if the
	// remote cut were skipped.
	ex := transporttest.New()
	bodies := procBodies()
	bodies[secProcEnviron] = "AWS_SECRET_ACCESS_KEY=s3cr3t\nHOME\nPATH\n"
	ex.On("", procReply(bodies, map[string]int{}, nil))

	info, err := newProcBackend(ex).ProcInfo(context.Background(), "web-01", "1234")
	if err != nil {
		t.Fatalf("ProcInfo: %v", err)
	}
	joined := strings.Join(info.EnvKeys, " ")
	if strings.Contains(joined, "s3cr3t") || strings.Contains(joined, "=") {
		t.Fatalf("EnvKeys carry a value: %v", info.EnvKeys)
	}
	if strings.Join(info.EnvKeys, ",") != "HOME,PATH" {
		t.Errorf("EnvKeys = %v, want HOME,PATH", info.EnvKeys)
	}
}

func TestParseProcCgroupV1PrefersTheUnit(t *testing.T) {
	out := "12:cpu,cpuacct:/docker/abc\n" +
		"1:name=systemd:/system.slice/nginx.service\n" +
		"0::/unified/path\n"
	cg, err := ParseProcCgroup(out)
	if err != nil {
		t.Fatalf("ParseProcCgroup: %v", err)
	}
	if cg.Path != "/unified/path" || cg.Controllers != "" {
		t.Errorf("cgroup v2 line was not preferred: %+v", cg)
	}

	cg, err = ParseProcCgroup("12:cpu,cpuacct:/docker/abc\n1:name=systemd:/system.slice/nginx.service\n")
	if err != nil {
		t.Fatalf("ParseProcCgroup without a unified line: %v", err)
	}
	if cg.Unit != "nginx.service" {
		t.Errorf("Unit = %q, want nginx.service", cg.Unit)
	}
	if _, err := ParseProcCgroup("garbage\n"); errs.KindOf(err) != errs.Parse {
		t.Errorf("unparsable cgroup err = %v, want %q", err, errs.Parse)
	}
}

func TestParseProcStartTimeLayouts(t *testing.T) {
	want := time.Date(2026, 11, 12, 9, 12, 41, 0, time.UTC)
	for _, out := range []string{
		"Wed Nov 12 09:12:41 2026\n",
		"Wed Nov  12 09:12:41 2026\n",
		"  Wed Nov 12 09:12:41 2026  \n",
	} {
		got, err := ParseProcStartTime(out)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseProcStartTime(%q) = %v, %v; want %v", out, got, err, want)
		}
	}
	if got, err := ParseProcStartTime("\n"); err != nil || !got.IsZero() {
		t.Errorf("empty lstart = %v, %v; want the zero time and no error", got, err)
	}
	if _, err := ParseProcStartTime("not a date\n"); errs.KindOf(err) != errs.Parse {
		t.Errorf("unparsable lstart err = %v, want %q", err, errs.Parse)
	}
}

func TestProcJournalCommandUsesThePIDMatch(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	o := JournalOptions{
		Window: resource.TimeWindow{From: now.Add(-time.Hour), ToNow: true, Label: "-1h"},
		Now:    now,
		Sudo:   true,
	}
	cmd := ProcJournalCommand("1234", o)
	if !strings.Contains(cmd, "journalctl") || !strings.Contains(cmd, "_PID=1234") {
		t.Errorf("command = %q, want a journalctl line with _PID=1234", cmd)
	}
	if !strings.Contains(cmd, "-o json") || !strings.Contains(cmd, "--no-pager") {
		t.Errorf("command = %q, want the journal shape", cmd)
	}
	if !strings.Contains(cmd, "-n "+strconv.Itoa(procJournalLimit)) {
		t.Errorf("command = %q, want the default limit", cmd)
	}
}
