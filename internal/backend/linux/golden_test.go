package linux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Inputs a capture run writes each probe to (task 053). A parser task may add
// more files to the same directory; the golden test only reads the ones it
// knows.
const (
	goldenOverview = "overview"
	goldenUnits    = "units"
	goldenPS       = "ps"
	goldenSS       = "ss"
	goldenDisk     = "disk"
	goldenIP       = "ip"
	goldenJournal  = "journal"
	goldenTimers   = "timers"
	goldenDeps     = "deps"
	goldenCgroup   = "cgroup"
	goldenPkg      = "pkg"
)

// goldenDistros are the three lab distros a capture records (task 053). A
// directory that does not exist yet is skipped, so the test is meaningful
// before the lab has produced every capture.
var goldenDistros = []string{"rocky9", "ubuntu2204", "ubuntu2404"}

// TestGoldenLinuxCaptures runs the matching parser over every captured probe
// directory and asserts invariants that hold on any healthy host. It needs no
// lab, no network and no SSH agent: it only reads testdata/.
func TestGoldenLinuxCaptures(t *testing.T) {
	root := goldenRoot(t)
	probes, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	for _, p := range probes {
		if !p.IsDir() {
			continue
		}
		probeDir := filepath.Join(root, p.Name())
		distros, err := os.ReadDir(probeDir)
		if err != nil {
			t.Fatalf("read %s: %v", probeDir, err)
		}
		seen := make(map[string]bool, len(distros))
		for _, d := range distros {
			if d.IsDir() {
				seen[d.Name()] = true
			}
		}
		for _, distro := range goldenDistros {
			if !seen[distro] {
				t.Run(p.Name()+"/"+distro, func(t *testing.T) {
					t.Skipf("no capture for %s/%s yet", p.Name(), distro)
				})
				continue
			}
			t.Run(p.Name()+"/"+distro, func(t *testing.T) {
				goldenProbe(t, p.Name(), filepath.Join(probeDir, distro))
			})
		}
		// Distro directories that do not follow the capture names (an older
		// sample, for instance) are still real output: run them too.
		for _, d := range distros {
			if !d.IsDir() || isGoldenDistro(d.Name()) {
				continue
			}
			t.Run(p.Name()+"/"+d.Name(), func(t *testing.T) {
				goldenProbe(t, p.Name(), filepath.Join(probeDir, d.Name()))
			})
		}
	}
}

// isGoldenDistro reports whether name is one of the three capture directories.
func isGoldenDistro(name string) bool {
	for _, d := range goldenDistros {
		if d == name {
			return true
		}
	}
	return false
}

// goldenProbe dispatches on the probe directory. A probe without a parser here
// is not an error: the capture tool records more than the goldens check.
func goldenProbe(t *testing.T, probe, dir string) {
	t.Helper()
	switch probe {
	case goldenOverview:
		goldenOverviewFiles(t, dir)
	case goldenUnits:
		goldenUnitsFiles(t, dir)
	case goldenPS:
		goldenPSFiles(t, dir)
	case goldenSS:
		goldenSSFiles(t, dir)
	case goldenDisk:
		goldenDiskFiles(t, dir)
	case goldenIP:
		goldenIPFiles(t, dir)
	case goldenJournal:
		goldenJournalFiles(t, dir)
	case goldenTimers:
		goldenTimersFiles(t, dir)
	case goldenDeps:
		goldenDepsFiles(t, dir)
	case goldenCgroup:
		goldenCgroupFiles(t, dir)
	case goldenPkg:
		goldenPkgFiles(t, dir)
	default:
		t.Logf("no golden parser for probe %q", probe)
	}
}

func goldenOverviewFiles(t *testing.T, dir string) {
	t.Helper()
	if out, ok := goldenFile(t, dir, "loadavg"); ok {
		if _, err := ParseLoadavg(string(out)); err != nil {
			t.Errorf("ParseLoadavg: %v", err)
		}
	}
	if out, ok := goldenFile(t, dir, "meminfo"); ok {
		mem, err := ParseMeminfo(string(out))
		if err != nil {
			t.Fatalf("ParseMeminfo: %v", err)
		}
		if mem.TotalKB <= 0 {
			t.Errorf("meminfo total = %d kB; want > 0", mem.TotalKB)
		}
	}
	if out, ok := goldenFile(t, dir, "psi"); ok {
		if _, err := ParsePSI(string(out)); err != nil {
			t.Errorf("ParsePSI: %v", err)
		}
	}
	if out, ok := goldenFile(t, dir, "os-release"); ok {
		osrel, err := ParseOSRelease(string(out))
		if err != nil {
			t.Fatalf("ParseOSRelease: %v", err)
		}
		if strings.TrimSpace(osrel.ID) == "" {
			t.Errorf("os-release ID is empty")
		}
	}
	if out, ok := goldenFile(t, dir, "uptime"); ok {
		if _, err := ParseUptime(string(out)); err != nil {
			t.Errorf("ParseUptime: %v", err)
		}
	}
	if out, ok := goldenFile(t, dir, "nproc"); ok {
		n, err := ParseNproc(string(out))
		if err != nil {
			t.Fatalf("ParseNproc: %v", err)
		}
		if n <= 0 {
			t.Errorf("nproc = %d; want > 0", n)
		}
	}
	if out, ok := goldenFile(t, dir, "sysstate"); ok {
		if _, err := ParseSystemState(string(out)); err != nil {
			t.Errorf("ParseSystemState: %v", err)
		}
	}
	if out, ok := goldenFile(t, dir, "failed"); ok {
		if _, err := ParseListUnits(string(out)); err != nil {
			t.Errorf("ParseListUnits (failed): %v", err)
		}
	}
}

func goldenUnitsFiles(t *testing.T, dir string) {
	t.Helper()
	out, ok := goldenFile(t, dir, "list-units")
	if !ok {
		return
	}
	units, err := ParseListUnits(string(out))
	if err != nil {
		t.Fatalf("ParseListUnits: %v", err)
	}
	found := false
	for _, u := range units {
		if u.Name == "sshd.service" || u.Name == "ssh.service" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("units list has neither sshd.service nor ssh.service (%d units)", len(units))
	}
	// A `systemctl show` capture may share the directory; it must parse.
	if out, ok := goldenFile(t, dir, "show-nginx"); ok {
		if props := ParseShow(string(out)); len(props) == 0 {
			t.Errorf("ParseShow returned no properties")
		}
	}
}

func goldenPSFiles(t *testing.T, dir string) {
	t.Helper()
	out, ok := goldenFile(t, dir, "ps")
	if !ok {
		return
	}
	procs, _ := ParsePS(string(out))
	if len(procs) == 0 {
		t.Fatalf("ParsePS returned no processes")
	}
	for _, p := range procs {
		if p.PID == 1 {
			return
		}
	}
	t.Errorf("ps output has no PID 1 (%d processes)", len(procs))
}

func goldenSSFiles(t *testing.T, dir string) {
	t.Helper()
	out, ok := goldenFile(t, dir, "ss")
	if !ok {
		return
	}
	socks, _ := ParseSS(string(out))
	if len(socks) == 0 {
		t.Fatalf("ParseSS returned no sockets")
	}
	for _, s := range socks {
		if s.LocalPort == "22" {
			return
		}
	}
	t.Errorf("ss output has no listener on 22 (%d sockets)", len(socks))
}

func goldenDiskFiles(t *testing.T, dir string) {
	t.Helper()
	if out, ok := goldenFile(t, dir, "df"); ok {
		if _, err := ParseDF(string(out)); err != nil {
			t.Errorf("ParseDF: %v", err)
		}
	}
	if out, ok := goldenFile(t, dir, "df-inodes"); ok {
		if _, err := ParseDFInodes(string(out)); err != nil {
			t.Errorf("ParseDFInodes: %v", err)
		}
	}
	if out, ok := goldenFile(t, dir, "lsblk"); ok {
		if _, err := ParseLsblk(string(out)); err != nil {
			t.Errorf("ParseLsblk: %v", err)
		}
	}
	if out, ok := goldenFile(t, dir, "diskstats"); ok {
		samples := ParseDiskstatsSamples(string(out))
		if len(samples) == 0 {
			t.Errorf("ParseDiskstatsSamples returned no sample")
		}
		if len(samples) >= 2 {
			if _, err := ParseDiskstatsPair(string(out), time.Second); err != nil {
				t.Errorf("ParseDiskstatsPair: %v", err)
			}
		}
	}
}

func goldenIPFiles(t *testing.T, dir string) {
	t.Helper()
	parsers := []struct {
		stem  string
		parse func(string) error
	}{
		{"link", func(s string) error { _, err := ParseIPLinks(s); return err }},
		{"addr", func(s string) error { _, err := ParseIPAddrs(s); return err }},
		{"route", func(s string) error { _, err := ParseIPRoute(s); return err }},
		{"route6", func(s string) error { _, err := ParseIPRoute6(s); return err }},
		{"neigh", func(s string) error { _, err := ParseIPNeigh(s); return err }},
	}
	for _, p := range parsers {
		out, ok := goldenFile(t, dir, p.stem)
		if !ok {
			continue
		}
		if err := p.parse(string(out)); err != nil {
			t.Errorf("parse %s: %v", p.stem, err)
		}
	}
}

func goldenJournalFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !jsonlName(e.Name()) {
			continue
		}
		out, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		_, skipped := ParseJournal(out)
		lines := 0
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) != "" {
				lines++
			}
		}
		// The 1 % bound is a property of a real capture (500 entries): a short
		// hand-made sample may contain one deliberately truncated line.
		if lines >= 100 && skipped*100 > lines {
			t.Errorf("%s: %d of %d journal lines skipped; want at most 1%%", e.Name(), skipped, lines)
		}
	}
}

func goldenTimersFiles(t *testing.T, dir string) {
	t.Helper()
	if out, ok := goldenFile(t, dir, "list-timers"); ok {
		_ = ParseTimers(string(out))
	}
}

func goldenDepsFiles(t *testing.T, dir string) {
	t.Helper()
	for _, stem := range []string{"list-dependencies", "list-dependencies-reverse"} {
		if out, ok := goldenFile(t, dir, stem); ok {
			_ = ParseDependencies(string(out))
		}
	}
}

func goldenCgroupFiles(t *testing.T, dir string) {
	t.Helper()
	if out, ok := goldenFile(t, dir, "cgroup"); ok {
		if _, err := ParseCgtop(string(out)); err != nil {
			t.Errorf("ParseCgtop: %v", err)
		}
	}
}

func goldenPkgFiles(t *testing.T, dir string) {
	t.Helper()
	if out, ok := goldenFile(t, dir, "pkg"); ok {
		if _, err := ParsePkg(string(out)); err != nil {
			t.Errorf("ParsePkg: %v", err)
		}
	}
}

// goldenFile reads dir/<stem>.<any extension>, so a `.json` and a captured
// `.txt` file are found the same way.
func goldenFile(t *testing.T, dir, stem string) ([]byte, bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())) != stem {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		return b, true
	}
	return nil, false
}

// jsonlName reports whether a journal capture is one of the shapes the journal
// probe produces.
func jsonlName(name string) bool {
	switch filepath.Ext(name) {
	case ".jsonl", ".json", ".txt":
		return true
	}
	return false
}

// goldenRoot finds testdata/linux above the test's working directory.
func goldenRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "testdata", "linux")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
