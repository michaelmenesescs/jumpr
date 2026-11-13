package linux

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func psReadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "linux", "ps", "rocky9", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func TestParsePSInline(t *testing.T) {
	out := "" +
		"      1       0 root     Ss     0.0  0.1  18932  864000 /usr/lib/systemd/systemd --switched-root --system\n" +
		"      2       0 root     S      0.0  0.0      0  863900 [kworker/0:1]\n" +
		"    900     1 systemd-timesync Ss   1.5  2.5   4096      60 /usr/bin/foo    --bar   baz\n" +
		"not a ps line\n" +
		"  77    x root S 0.0 0.0 1 2 bad ppid\n"
	procs, skipped := ParsePS(out)
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
	if len(procs) != 3 {
		t.Fatalf("procs = %d, want 3", len(procs))
	}
	first := procs[0]
	want := Proc{
		PID:     1,
		PPID:    0,
		User:    "root",
		Stat:    "Ss",
		CPU:     0,
		Mem:     0.1,
		RSSKB:   18932,
		Elapsed: 864000 * time.Second,
		Args:    "/usr/lib/systemd/systemd --switched-root --system",
	}
	if first != want {
		t.Errorf("first = %+v, want %+v", first, want)
	}
	if got := procs[1].Name(); got != "kworker/0:1" {
		t.Errorf("kernel thread Name = %q, want kworker/0:1", got)
	}
	if procs[2].User != "systemd-timesync" {
		t.Errorf("long user = %q", procs[2].User)
	}
	if procs[2].CPU != 1.5 || procs[2].Mem != 2.5 || procs[2].RSSKB != 4096 {
		t.Errorf("numeric fields = %+v", procs[2])
	}
	if procs[2].Elapsed != 60*time.Second {
		t.Errorf("Elapsed = %v, want 1m0s", procs[2].Elapsed)
	}
	if got, wantArgs := procs[2].Args, "/usr/bin/foo    --bar   baz"; got != wantArgs {
		t.Errorf("Args = %q, want %q (inner spaces kept)", got, wantArgs)
	}
}

func TestParsePSFixture(t *testing.T) {
	procs, skipped := ParsePS(psReadFixture(t, "ps.txt"))
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if len(procs) != 6 {
		t.Fatalf("procs = %d, want 6", len(procs))
	}
	byPID := make(map[int]Proc, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	nginxMaster, ok := byPID[2010]
	if !ok {
		t.Fatalf("pid 2010 missing")
	}
	if nginxMaster.PPID != 1 || nginxMaster.User != "nginx" || nginxMaster.Stat != "Ss" {
		t.Errorf("nginx master = %+v", nginxMaster)
	}
	if got := nginxMaster.Name(); got != "nginx" {
		t.Errorf("nginx master Name = %q, want nginx", got)
	}
	if got := byPID[88901].Name(); got != "sshd" {
		t.Errorf("sshd Name = %q, want sshd", got)
	}
	if got := byPID[2].Name(); got != "kworker/0:1" {
		t.Errorf("kworker Name = %q", got)
	}
}

func TestParsePSGarbageDoesNotPanic(t *testing.T) {
	for _, in := range []string{"", "\n\n", "x", "-- header --\n1 2", "\x00\x01\x02", "1\n2\n3\n"} {
		procs, skipped := ParsePS(in)
		if len(procs) != 0 {
			t.Errorf("input %q produced %d procs", in, len(procs))
		}
		if skipped < 0 {
			t.Errorf("input %q gave negative skipped count", in)
		}
	}
}

func TestProcName(t *testing.T) {
	cases := []struct {
		args string
		want string
	}{
		{"[kworker/0:1]", "kworker/0:1"},
		{"[kworker/u8:2-events_unbound]", "kworker/u8:2-events_unbound"},
		{"/usr/sbin/nginx -g daemon on;", "nginx"},
		{"nginx: worker process", "nginx"},
		{"nginx: master process /usr/sbin/nginx -g daemon on;", "nginx"},
		{"systemd", "systemd"},
		{"  /usr/lib/systemd/systemd-journald  ", "systemd-journald"},
		{"", ""},
		{"[unterminated", "[unterminated"},
	}
	for _, c := range cases {
		if got := (Proc{Args: c.args}).Name(); got != c.want {
			t.Errorf("Name(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
