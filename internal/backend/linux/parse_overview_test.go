package linux

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"jumpr/internal/errs"
)

// ovXReadTestdata reads a captured overview section from the shared,
// sanitized testdata tree.
func ovXReadTestdata(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "testdata", "linux", "overview", "rocky9", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ovXGarbage is a table of inputs that must never panic a parser: binary
// bytes, half lines, separators without values and other noise.
var ovXGarbage = []string{
	"\x00\x01\x02\xff\xfe",
	"\x00",
	"MemTotal",
	":",
	":::",
	"MemTotal:",
	"MemTotal: kB",
	"0.5 0.4",
	"0.52 0.41 0.30 2",
	"0.52 0.41 0.30 /413",
	"0.52 0.41 0.30 x/y 99123",
	"NaN Inf 1e999 1/2 3",
	"== cpu",
	"== cpu\nsome avg10=",
	"some avg10=1.0",
	"== memory",
	"'",
	`PRETTY_NAME="unterminated`,
	"-1",
	"-1.5 2 3",
	"99999999999999999999999999999999999999 2 3 1/2 3",
	"\n\n\n",
	"   \t  ",
}

func TestParseLoadavg(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Load
		wantErr bool
	}{
		{
			name:  "inline sample",
			input: "0.52 0.41 0.30 2/413 99123",
			want:  Load{L1: 0.52, L5: 0.41, L15: 0.30, Running: 2, Total: 413},
		},
		{
			name:  "golden rocky9",
			input: ovXReadTestdata(t, "loadavg.txt"),
			want:  Load{L1: 0.52, L5: 0.41, L15: 0.30, Running: 2, Total: 413},
		},
		{
			name:  "trailing newline and extra fields",
			input: "12.00 3.50 1.25 1/2048 4242\n",
			want:  Load{L1: 12, L5: 3.5, L15: 1.25, Running: 1, Total: 2048},
		},
		{name: "empty", input: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLoadavg(tt.input)
			if tt.wantErr {
				if err == nil || !errs.Is(err, errs.Parse) {
					t.Fatalf("want errs.Parse, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseMeminfo(t *testing.T) {
	fallback := Mem{
		TotalKB:     32653820,
		AvailableKB: 1234567 + 123456 + 9876543,
		FreeKB:      1234567,
		BuffersKB:   123456,
		CachedKB:    9876543,
		SwapTotalKB: 8388604,
		SwapFreeKB:  8388604,
	}
	tests := []struct {
		name    string
		input   string
		want    Mem
		wantErr bool
	}{
		{
			name:  "golden rocky9",
			input: ovXReadTestdata(t, "meminfo.txt"),
			want: Mem{
				TotalKB:     32653820,
				AvailableKB: 20123456,
				FreeKB:      1234567,
				BuffersKB:   123456,
				CachedKB:    9876543,
				SwapTotalKB: 8388604,
				SwapFreeKB:  8388604,
			},
		},
		{
			name: "no MemAvailable falls back to free+buffers+cached",
			input: "MemTotal:       32653820 kB\n" +
				"MemFree:         1234567 kB\n" +
				"Buffers:          123456 kB\n" +
				"Cached:          9876543 kB\n" +
				"SwapCached:        4321 kB\n" +
				"SwapTotal:       8388604 kB\n" +
				"SwapFree:        8388604 kB\n",
			want: fallback,
		},
		{
			name:  "unknown lines ignored",
			input: "MemTotal:       1000 kB\nHugePages_Total:   0\nTriggers: (0)\nMemAvailable:    500 kB\n",
			want:  Mem{TotalKB: 1000, AvailableKB: 500},
		},
		{name: "empty", input: "", wantErr: true},
		{name: "no MemTotal", input: "MemFree: 10 kB\nBool: yes\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMeminfo(tt.input)
			if tt.wantErr {
				if !errs.Is(err, errs.Parse) {
					t.Fatalf("want errs.Parse, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMemUsedPercent(t *testing.T) {
	tests := []struct {
		name string
		mem  Mem
		want float64
	}{
		{name: "zero total", mem: Mem{}, want: 0},
		{name: "sixty percent", mem: Mem{TotalKB: 100, AvailableKB: 40}, want: 60},
		{name: "full", mem: Mem{TotalKB: 1000, AvailableKB: 0}, want: 100},
		{name: "all available", mem: Mem{TotalKB: 1000, AvailableKB: 1000}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mem.UsedPercent(); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParsePSI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  PSI
	}{
		{
			name:  "inline sample",
			input: "== cpu\nsome avg10=1.23 avg60=0.80 avg300=0.40 total=123456\n== memory\nsome avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n== io\nsome avg10=3.20 avg60=1.10 avg300=0.50 total=987654\nfull avg10=2.90 avg60=0.90 avg300=0.40 total=876543\n",
			want: PSI{
				CPU: PSILine{SomeAvg10: 1.23, FullAvg10: 0, Present: true},
				Mem: PSILine{SomeAvg10: 0, FullAvg10: 0, Present: true},
				IO:  PSILine{SomeAvg10: 3.20, FullAvg10: 2.90, Present: true},
			},
		},
		{
			name:  "golden rocky9",
			input: ovXReadTestdata(t, "psi.txt"),
			want: PSI{
				CPU: PSILine{SomeAvg10: 1.23, FullAvg10: 0, Present: true},
				Mem: PSILine{SomeAvg10: 0, FullAvg10: 0, Present: true},
				IO:  PSILine{SomeAvg10: 3.20, FullAvg10: 2.90, Present: true},
			},
		},
		{
			name:  "kernel without psi",
			input: "",
			want:  PSI{},
		},
		{
			name:  "missing sections stay not present",
			input: "== cpu\nsome avg10=2.50 avg60=1.00 avg300=0.50 total=7\n",
			want:  PSI{CPU: PSILine{SomeAvg10: 2.50, Present: true}},
		},
		{
			name:  "full line only",
			input: "== io\nfull avg10=0.75 avg60=0.50 avg300=0.25 total=9\n",
			want:  PSI{IO: PSILine{FullAvg10: 0.75, Present: true}},
		},
		{
			name:  "unknown section ignored",
			input: "== bogus\nsome avg10=9.99 avg60=0 avg300=0 total=1\n== cpu\nsome avg10=1.00 avg60=0 avg300=0 total=1\n",
			want:  PSI{CPU: PSILine{SomeAvg10: 1, Present: true}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePSI(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseOSRelease(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    OS
		wantErr bool
	}{
		{
			name:  "golden rocky9",
			input: ovXReadTestdata(t, "os-release.txt"),
			want:  OS{ID: "rocky", VersionID: "9.3", PrettyName: "Rocky Linux 9.3 (Blue Onyx)"},
		},
		{
			name:  "single quotes",
			input: "ID='ubuntu'\nVERSION_ID='22.04'\nPRETTY_NAME='Ubuntu 22.04.3 LTS'\n",
			want:  OS{ID: "ubuntu", VersionID: "22.04", PrettyName: "Ubuntu 22.04.3 LTS"},
		},
		{
			name:  "unquoted and unknown keys",
			input: "NAME=Debian\nID=debian\nVERSION_ID=12\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n",
			want:  OS{ID: "debian", VersionID: "12", PrettyName: "Debian GNU/Linux 12 (bookworm)"},
		},
		{name: "empty", input: "", wantErr: true},
		{name: "no recognised keys", input: "NAME=\"X\"\nLOGO=x\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOSRelease(tt.input)
			if tt.wantErr {
				if !errs.Is(err, errs.Parse) {
					t.Fatalf("want errs.Parse, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseUptime(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{
			name:  "inline sample",
			input: "3561234.56 28012345.11",
			want:  3561234*time.Second + 560*time.Millisecond,
		},
		{
			name:  "golden rocky9",
			input: ovXReadTestdata(t, "uptime.txt"),
			want:  3561234*time.Second + 560*time.Millisecond,
		},
		{
			name:  "whole seconds",
			input: "42.00 100.00\n",
			want:  42 * time.Second,
		},
		{name: "empty", input: "", wantErr: true},
		{name: "not a number", input: "junk 1.0", wantErr: true},
		{name: "negative", input: "-5.0 1.0", wantErr: true},
		{name: "overflow", input: "1e300 1.0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUptime(tt.input)
			if tt.wantErr {
				if !errs.Is(err, errs.Parse) {
					t.Fatalf("want errs.Parse, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSystemState(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "running", input: "running\n", want: "running"},
		{name: "degraded", input: "degraded\n", want: "degraded"},
		{name: "starting", input: "starting", want: "starting"},
		{name: "maintenance", input: "maintenance\n", want: "maintenance"},
		{name: "stopping", input: " stopping \n", want: "stopping"},
		{name: "offline", input: "offline\n", want: "offline"},
		{name: "unknown passthrough", input: "unknown\n", want: "unknown"},
		{name: "anything else is unknown", input: "initializing\n", want: "unknown"},
		{name: "garbage is unknown", input: "\x00\xff", want: "unknown"},
		{name: "empty", input: "", wantErr: true},
		{name: "whitespace only", input: " \n\t", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSystemState(tt.input)
			if tt.wantErr {
				if !errs.Is(err, errs.Parse) {
					t.Fatalf("want errs.Parse, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseNproc(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{name: "eight", input: "8\n", want: 8},
		{name: "one", input: "1", want: 1},
		{name: "large", input: "256\n", want: 256},
		{name: "empty", input: "", wantErr: true},
		{name: "not a number", input: "many\n", wantErr: true},
		{name: "zero", input: "0\n", wantErr: true},
		{name: "negative", input: "-4\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseNproc(tt.input)
			if tt.wantErr {
				if !errs.Is(err, errs.Parse) {
					t.Fatalf("want errs.Parse, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// TestOverviewParsersGarbage feeds every parser binary and truncated input;
// the only requirement is that nothing panics.
func TestOverviewParsersGarbage(t *testing.T) {
	for i, in := range ovXGarbage {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("input %d (%q) panicked: %v", i, in, r)
				}
			}()
			_, _ = ParseLoadavg(in)
			_, _ = ParseMeminfo(in)
			_, _ = ParsePSI(in)
			_, _ = ParseOSRelease(in)
			_, _ = ParseUptime(in)
			_, _ = ParseSystemState(in)
			_, _ = ParseNproc(in)
			_ = Mem{TotalKB: 1}.UsedPercent()
		}()
	}
}

// TestParseEmptyIsParseError asserts the empty-input contract for every
// parser except PSI, which reports every line as not present instead.
func TestParseEmptyIsParseError(t *testing.T) {
	checks := []struct {
		name string
		err  error
	}{
		{"loadavg", func() error { _, err := ParseLoadavg(""); return err }()},
		{"meminfo", func() error { _, err := ParseMeminfo(""); return err }()},
		{"os-release", func() error { _, err := ParseOSRelease(""); return err }()},
		{"uptime", func() error { _, err := ParseUptime(""); return err }()},
		{"system-state", func() error { _, err := ParseSystemState(""); return err }()},
		{"nproc", func() error { _, err := ParseNproc(""); return err }()},
	}
	for _, c := range checks {
		if !errs.Is(c.err, errs.Parse) {
			t.Errorf("%s: want errs.Parse, got %v", c.name, c.err)
		}
	}
	if _, err := ParsePSI(""); err != nil {
		t.Errorf("psi: want no error, got %v", err)
	}
}
