// Package linux parsers for the remaining read-only kinds: cgroups, packages
// and the file viewer's stat/checksum helpers. Everything here is pure: it
// takes the stdout of one probe command and returns typed data.
package linux

import (
	"bytes"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/shq"
)

// Probe names. They double as the Op of a parser error.
const (
	miscProbeCgtop    = "cgroup"
	miscProbePkg      = "pkg"
	miscProbeFileStat = "file-stat"
	miscProbeChecksum = "checksum"
	miscProbeVersion  = "pkg-version"
)

// Package families, as systemd-cgtop and the package managers call them.
const (
	FamilyRPM = "rpm"
	FamilyDeb = "deb"
)

// familyPrefix starts the one line the package probe prints before the list, so
// the parser knows which package manager answered.
const familyPrefix = "@@family "

// Cgroup is one row of systemd-cgtop's second iteration. Bytes and rates are
// float64 because cgtop prints them human-scaled ("963.4M"). A column cgtop
// printed as "-" has its Have flag false.
type Cgroup struct {
	Path   string
	Tasks  int64
	CPU    float64
	Memory float64
	Input  float64
	Output float64

	HaveMemory bool
	HaveInput  bool
	HaveOutput bool
}

// Package is one installed package. Installed is only set for rpm, whose query
// prints %{INSTALLTIME}; Status is only set for deb (dpkg's status abbrev).
type Package struct {
	Name      string
	Version   string
	Arch      string
	Family    string
	Status    string
	Installed time.Time
}

// FileStat is the subset of `stat -c '%s %U:%G %a %y'` the viewer header shows.
type FileStat struct {
	Size  int64
	Owner string
	Group string
	Mode  string
	MTime time.Time
}

// ParseCgtop parses `systemd-cgtop -b -n 2` output and returns the second
// iteration. Frames are separated by a blank line; the header line is skipped.
// Lines that do not look like a cgroup row are ignored.
func ParseCgtop(out string) ([]Cgroup, error) {
	frames := cgtopFrames(out)
	if len(frames) == 0 {
		return nil, nil
	}
	// The last frame is the second iteration: the first one is only there to
	// seed the input/output rates.
	rows := frames[len(frames)-1]
	cgs := make([]Cgroup, 0, len(rows))
	for _, line := range rows {
		if cg, ok := parseCgtopLine(line); ok {
			cgs = append(cgs, cg)
		}
	}
	return cgs, nil
}

// cgtopFrames splits cgtop output into frames of non-blank lines. The inline
// header cgtop repeats per frame stays in the frame and is skipped by
// parseCgtopLine.
func cgtopFrames(out string) [][]string {
	var (
		frames [][]string
		cur    []string
	)
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				frames = append(frames, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		frames = append(frames, cur)
	}
	return frames
}

// parseCgtopLine parses one row: path, tasks, %CPU, memory, input/s, output/s.
func parseCgtopLine(line string) (Cgroup, bool) {
	f := strings.Fields(line)
	if len(f) < 6 {
		return Cgroup{}, false
	}
	if f[0] == "Control" || f[1] == "Tasks" || f[2] == "%CPU" {
		return Cgroup{}, false // the per-frame header
	}
	tasks, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return Cgroup{}, false
	}
	cpu, ok := cgtopFloat(f[2])
	if !ok {
		return Cgroup{}, false
	}
	cg := Cgroup{Path: f[0], Tasks: tasks, CPU: cpu}
	cg.Memory, cg.HaveMemory = cgtopSize(f[3])
	cg.Input, cg.HaveInput = cgtopSize(f[4])
	cg.Output, cg.HaveOutput = cgtopSize(f[5])
	return cg, true
}

// cgtopFloat parses a non-negative cgtop number; "-" and garbage report false.
func cgtopFloat(s string) (float64, bool) {
	if s == "-" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// cgtopSize parses cgtop's human-scaled byte figure: "963.4M", "2.1K", "0B",
// "1.1T". "-" reports false, so the cell renders as nil.
func cgtopSize(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0, false
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 'B', 'b':
		s = s[:len(s)-1]
	case 'K', 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		mult, s = 1<<30, s[:len(s)-1]
	case 'T', 't':
		mult, s = 1<<40, s[:len(s)-1]
	}
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v * mult, true
}

// ParsePkg parses the package probe's output. The first non-blank line is the
// "@@family rpm|deb" marker; every following line is one package with four
// tab-separated fields (name, version, arch, install time or status abbrev).
// Lines with the wrong shape are skipped.
func ParsePkg(out string) ([]Package, error) {
	var (
		pkgs   []Package
		family string
	)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, familyPrefix); ok {
			family = strings.TrimSpace(rest)
			continue
		}
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 || strings.TrimSpace(f[0]) == "" {
			continue
		}
		p := Package{
			Name:    f[0],
			Version: f[1],
			Arch:    f[2],
			Family:  family,
		}
		switch family {
		case FamilyRPM:
			if sec, err := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64); err == nil && sec > 0 {
				p.Installed = time.Unix(sec, 0).UTC()
			}
		default:
			p.Status = strings.TrimSpace(f[3])
		}
		pkgs = append(pkgs, p)
	}
	if family == "" && len(pkgs) == 0 {
		return nil, nil
	}
	return pkgs, nil
}

// ParsePackageVersion reads the version printed by the per-package probe. The
// family marker and blank lines are skipped; the first remaining line wins.
func ParsePackageVersion(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, familyPrefix) {
			continue
		}
		return strings.TrimSpace(line), nil
	}
	return "", errs.New(errs.Parse, miscProbeVersion, "no version in output")
}

// PackageVersionCommand builds the read-only probe that prints one package's
// installed version on either family. The package name is quoted with shq, so a
// hostile name can never reach the shell.
func PackageVersionCommand(name string) string {
	return `. /etc/os-release 2>/dev/null; ` +
		`case " $ID $ID_LIKE " in ` +
		`*rhel*|*fedora*|*centos*|*rocky*|*alma*|*suse*|*rpm*) ` +
		`echo "` + familyPrefix + `rpm"; ` +
		`rpm -q --qf '%{VERSION}-%{RELEASE}\n' ` + shq.Quote(name) + ` ;; ` +
		`*) echo "` + familyPrefix + `deb"; ` +
		`dpkg-query -W -f '${Version}\n' ` + shq.Quote(name) + ` ;; esac`
}

// OSFamily reads /etc/os-release and reports which package manager the host
// uses: "rpm" for RHEL/Fedora/SUSE families, "deb" for Debian/Ubuntu. It looks
// at ID first, then ID_LIKE.
func OSFamily(osRelease string) (string, bool) {
	var id, like string
	for _, line := range strings.Split(osRelease, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v := strings.ToLower(ovXUnquote(strings.TrimSpace(val)))
		switch strings.TrimSpace(key) {
		case "ID":
			id = v
		case "ID_LIKE":
			like = v
		}
	}
	for _, s := range []string{id, like} {
		for _, token := range strings.Fields(s) {
			switch token {
			case "rhel", "fedora", "centos", "rocky", "almalinux", "ol", "suse", "opensuse", "rpm":
				return FamilyRPM, true
			case "debian", "ubuntu", "devuan", "deb":
				return FamilyDeb, true
			}
		}
	}
	return "", false
}

// ParseFileStat parses `stat -c '%s %U:%G %a %y'`: the size, owner and group,
// the octal mode and the mtime with its zone.
func ParseFileStat(out string) (FileStat, error) {
	f := strings.Fields(out)
	if len(f) < 6 {
		return FileStat{}, errs.New(errs.Parse, miscProbeFileStat, "expected size, owner:group, mode and mtime")
	}
	size, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || size < 0 {
		return FileStat{}, errs.Wrap(errs.Parse, miscProbeFileStat, err)
	}
	owner, group, ok := strings.Cut(f[1], ":")
	if !ok || owner == "" {
		return FileStat{}, errs.New(errs.Parse, miscProbeFileStat, "owner and group are not owner:group")
	}
	st := FileStat{Size: size, Owner: owner, Group: group, Mode: f[2]}
	if t, err := time.Parse("2006-01-02 15:04:05.999999999 -0700", strings.Join(f[3:6], " ")); err == nil {
		st.MTime = t
	}
	return st, nil
}

// IsBinaryData reports whether b looks like binary content: a NUL byte within
// the first 8 KiB.
func IsBinaryData(b []byte) bool {
	const sniff = 8 << 10
	if len(b) > sniff {
		b = b[:sniff]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// ParseChecksumLine parses one `sha256sum` line ("<64 hex>  <path>"). The path
// may contain spaces; sha256sum separates it from the sum with two spaces.
func ParseChecksumLine(line string) (sum, path string, ok bool) {
	s := strings.TrimRight(line, "\r")
	i := strings.Index(s, "  ")
	if i != 64 || !isSHA256(s[:i]) {
		return "", "", false
	}
	path = strings.TrimSpace(s[i+2:])
	if path == "" {
		return "", "", false
	}
	return s[:i], path, true
}

// isSHA256 reports whether s is 64 lower-case hex characters.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
