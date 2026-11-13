// Package linux holds the Linux (SSH) backend: probes, parsers, kinds and
// actions. Everything in this file is pure: it takes the stdout of one probe
// section and returns typed data, never running a command itself.
package linux

import (
	"math"
	"strconv"
	"strings"
	"time"

	"jumper/internal/errs"
)

// Probe names. They double as the Op of a parser error so the UI can say which
// section failed.
const (
	ovProbeLoadavg     = "loadavg"
	ovProbeMeminfo     = "meminfo"
	ovProbePSI         = "psi"
	ovProbeOSRelease   = "os-release"
	ovProbeUptime      = "uptime"
	ovProbeSystemState = "system-state"
	ovProbeNproc       = "nproc"
)

// Load is /proc/loadavg: the one, five and fifteen minute averages plus the
// runnable and total task counts.
type Load struct {
	L1, L5, L15    float64
	Running, Total int
}

// Mem is the subset of /proc/meminfo the host overview shows, in kB.
type Mem struct {
	TotalKB     int64
	AvailableKB int64
	FreeKB      int64
	BuffersKB   int64
	CachedKB    int64
	SwapTotalKB int64
	SwapFreeKB  int64
}

// UsedPercent returns the used share of memory, (Total − Available) / Total ×
// 100, and 0 when Total is 0.
func (m Mem) UsedPercent() float64 {
	if m.TotalKB == 0 {
		return 0
	}
	return float64(m.TotalKB-m.AvailableKB) / float64(m.TotalKB) * 100
}

// PSILine is one resource's pressure stall line. Present is false when the
// kernel (or the probe) reported no line for it.
type PSILine struct {
	SomeAvg10 float64
	FullAvg10 float64
	Present   bool
}

// PSI is the parsed /proc/pressure sections the probe concatenates.
type PSI struct {
	CPU PSILine
	Mem PSILine
	IO  PSILine
}

// OS is the subset of /etc/os-release the host overview shows.
type OS struct {
	ID         string
	VersionID  string
	PrettyName string
}

// ovXErr builds a parse error naming the probe section.
func ovXErr(probe, msg string) *errs.Error {
	return errs.New(errs.Parse, probe, msg)
}

// ovXFloat parses a finite, non-negative float.
func ovXFloat(s string) (float64, bool) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// ParseLoadavg parses /proc/loadavg, for example
// "0.52 0.41 0.30 2/413 99123". Fields after the fifth are ignored.
func ParseLoadavg(s string) (Load, error) {
	f := strings.Fields(s)
	if len(f) < 4 {
		return Load{}, ovXErr(ovProbeLoadavg, "expected at least 4 fields")
	}
	var l Load
	var ok [3]bool
	l.L1, ok[0] = ovXFloat(f[0])
	l.L5, ok[1] = ovXFloat(f[1])
	l.L15, ok[2] = ovXFloat(f[2])
	if !ok[0] || !ok[1] || !ok[2] {
		return Load{}, ovXErr(ovProbeLoadavg, "bad load average")
	}
	running, total, found := strings.Cut(f[3], "/")
	if !found {
		return Load{}, ovXErr(ovProbeLoadavg, "bad task count")
	}
	r, err1 := strconv.Atoi(running)
	n, err2 := strconv.Atoi(total)
	if err1 != nil || err2 != nil || r < 0 || n < 0 {
		return Load{}, ovXErr(ovProbeLoadavg, "bad task count")
	}
	l.Running, l.Total = r, n
	return l, nil
}

// ParseMeminfo parses /proc/meminfo. Values are read with strings.Fields, so
// column drift and unknown keys do not matter. Kernels without MemAvailable
// fall back to MemFree + Buffers + Cached.
func ParseMeminfo(s string) (Mem, error) {
	var m Mem
	var haveTotal, haveAvailable bool
	for _, line := range strings.Split(s, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil || v < 0 {
			continue
		}
		switch strings.TrimSpace(key) {
		case "MemTotal":
			m.TotalKB, haveTotal = v, true
		case "MemAvailable":
			m.AvailableKB, haveAvailable = v, true
		case "MemFree":
			m.FreeKB = v
		case "Buffers":
			m.BuffersKB = v
		case "Cached":
			m.CachedKB = v
		case "SwapTotal":
			m.SwapTotalKB = v
		case "SwapFree":
			m.SwapFreeKB = v
		}
	}
	if !haveTotal {
		return Mem{}, ovXErr(ovProbeMeminfo, "no MemTotal")
	}
	if !haveAvailable {
		m.AvailableKB = m.FreeKB + m.BuffersKB + m.CachedKB
	}
	return m, nil
}

// ParsePSI parses the probe's PSI output: "== <resource>" headers followed by
// "some"/"full" stall lines. A resource with no lines is simply not Present;
// unknown headers and malformed lines are skipped, so a kernel without PSI is
// not an error.
func ParsePSI(s string) (PSI, error) {
	var p PSI
	var cur *PSILine
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "=="); ok {
			switch strings.ToLower(strings.TrimSpace(rest)) {
			case "cpu":
				cur = &p.CPU
			case "memory", "mem":
				cur = &p.Mem
			case "io":
				cur = &p.IO
			default:
				cur = nil
			}
			continue
		}
		if cur == nil {
			continue
		}
		f := strings.Fields(line)
		var dst *float64
		switch f[0] {
		case "some":
			dst = &cur.SomeAvg10
		case "full":
			dst = &cur.FullAvg10
		default:
			continue
		}
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k != "avg10" {
				continue
			}
			x, valid := ovXFloat(v)
			if !valid {
				break
			}
			*dst = x
			cur.Present = true
			break
		}
	}
	return p, nil
}

// ParseOSRelease parses /etc/os-release, unquoting values quoted with either
// " or '. Unknown keys are ignored.
func ParseOSRelease(s string) (OS, error) {
	var o OS
	seen := false
	for _, line := range strings.Split(s, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v := ovXUnquote(strings.TrimSpace(val))
		switch strings.TrimSpace(key) {
		case "ID":
			o.ID, seen = v, true
		case "VERSION_ID":
			o.VersionID, seen = v, true
		case "PRETTY_NAME":
			o.PrettyName, seen = v, true
		}
	}
	if !seen {
		return OS{}, ovXErr(ovProbeOSRelease, "no ID, VERSION_ID or PRETTY_NAME")
	}
	return o, nil
}

// ovXUnquote strips one matching pair of surrounding single or double quotes.
func ovXUnquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// ParseUptime parses the first field of /proc/uptime, seconds since boot, into
// a duration.
func ParseUptime(s string) (time.Duration, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, ovXErr(ovProbeUptime, "empty input")
	}
	sec, ok := ovXFloat(f[0])
	if !ok || sec < 0 {
		return 0, ovXErr(ovProbeUptime, "bad seconds")
	}
	const maxSeconds = float64(math.MaxInt64) / float64(time.Second)
	if sec > maxSeconds {
		return 0, ovXErr(ovProbeUptime, "seconds out of range")
	}
	return time.Duration(math.Round(sec * float64(time.Second))), nil
}

// ParseSystemState parses `systemctl is-system-running`. Anything outside the
// known set maps to "unknown"; empty output is an error.
func ParseSystemState(s string) (string, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return "", ovXErr(ovProbeSystemState, "empty input")
	}
	switch v := strings.ToLower(f[0]); v {
	case "running", "degraded", "starting", "maintenance", "stopping", "offline", "unknown":
		return v, nil
	}
	return "unknown", nil
}

// ParseNproc parses `nproc` output.
func ParseNproc(s string) (int, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, ovXErr(ovProbeNproc, "empty input")
	}
	n, err := strconv.Atoi(f[0])
	if err != nil || n <= 0 {
		return 0, ovXErr(ovProbeNproc, "bad processor count")
	}
	return n, nil
}
