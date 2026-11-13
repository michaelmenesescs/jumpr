package linux

import (
	"strings"
	"testing"
)

// TestCatalogBuiltinsReadOnly is the guard that keeps every catalog probe
// read-only: no deletion, no redirection, no tee.
func TestCatalogBuiltinsReadOnly(t *testing.T) {
	banned := []string{"rm", ">", "tee"}
	seen := make(map[string]bool)
	for _, p := range Catalog() {
		if p.Name == "" {
			t.Fatal("catalog has a probe with no name")
		}
		if seen[p.Name] {
			t.Fatalf("duplicate catalog probe %q", p.Name)
		}
		seen[p.Name] = true

		if strings.TrimSpace(p.Cmd) == "" {
			t.Fatalf("probe %q has an empty command", p.Name)
		}
		if strings.TrimSpace(p.Help) == "" {
			t.Fatalf("probe %q has no help line", p.Name)
		}
		for _, b := range banned {
			if strings.Contains(p.Cmd, b) {
				t.Fatalf("probe %q command contains %q: %s", p.Name, b, p.Cmd)
			}
		}
		for _, r := range p.Cmd {
			if r == '\n' || r == '\r' {
				t.Fatalf("probe %q command spans several lines: %q", p.Name, p.Cmd)
			}
		}
	}
}

// TestCatalogNames pins the built-ins the `:probe` command documents, their
// sudo flags and the one normalizer.
func TestCatalogNames(t *testing.T) {
	want := []string{
		"failed-units", "uptime", "kernel", "os",
		"listeners", "disk-full", "time-sync", "salt-minion",
	}
	probes := Catalog()
	if len(probes) != len(want) {
		t.Fatalf("catalog has %d probes, want %d", len(probes), len(want))
	}
	for i, name := range want {
		if probes[i].Name != name {
			t.Fatalf("probe %d is %q, want %q", i, probes[i].Name, name)
		}
	}

	byName := make(map[string]Probe, len(probes))
	for _, p := range probes {
		byName[p.Name] = p
	}
	if !byName["listeners"].Sudo {
		t.Error("listeners must run under sudo to see processes")
	}
	if byName["listeners"].Normalize == nil {
		t.Fatal("listeners needs the PID normalizer")
	}
	in, want2 := "LISTEN 0 511 0.0.0.0:443 0.0.0.0:* users:((\"nginx\",pid=812,fd=6))",
		"LISTEN 0 511 0.0.0.0:443 0.0.0.0:* users:((\"nginx\",pid=<n>,fd=6))"
	if got := byName["listeners"].Normalize(in); got != want2 {
		t.Fatalf("listeners normalize = %q, want %q", got, want2)
	}
	if byName["uptime"].Normalize != nil {
		t.Error("uptime must not normalize: differing load averages are the signal")
	}
	if byName["failed-units"].Sudo {
		t.Error("failed-units is readable without sudo")
	}
}

// TestCatalogDiskFullThreshold checks the read-only disk probe still reports
// the same filesystems the numeric comparison would.
func TestCatalogDiskFullThreshold(t *testing.T) {
	p := Catalog()[5]
	if p.Name != "disk-full" {
		t.Fatalf("probe 5 is %q", p.Name)
	}
	for _, want := range []string{"df -PT", "tmpfs", "devtmpfs", "$6", "$7"} {
		if !strings.Contains(p.Cmd, want) {
			t.Fatalf("disk-full command is missing %q: %s", want, p.Cmd)
		}
	}
}
