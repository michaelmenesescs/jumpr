package linux

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport/transporttest"
)

// cgtopOut is a real two-frame `systemd-cgtop -b -n 2` capture: the first frame
// seeds the rates, the second is the one the parser keeps.
const cgtopOut = `Control Group                    Tasks   %CPU   Memory  Input/s Output/s
/                                  231    0.9   963.4M       0B    2.1K
init.scope                           1    0.0    13.6M        -       -
system.slice                        98    2.4     1.1G     4.2K  512.0K

Control Group                    Tasks   %CPU   Memory  Input/s Output/s
/                                  233    1.1   964.1M       0B    2.3K
init.scope                           1    0.0    13.6M        -       -
system.slice                        99    2.6     1.2G     8.0K  640.0K
user.slice                          41    0.7   210.5M     1.0K    0B
`

func TestParseCgtopKeepsSecondFrame(t *testing.T) {
	cgs, err := ParseCgtop(cgtopOut)
	if err != nil {
		t.Fatalf("ParseCgtop: %v", err)
	}
	if got, want := len(cgs), 4; got != want {
		t.Fatalf("cgroups = %d, want %d: %+v", got, want, cgs)
	}
	if got, want := cgs[0].Path, "/"; got != want {
		t.Errorf("cgs[0].Path = %q, want %q", got, want)
	}
	if got, want := cgs[0].Tasks, int64(233); got != want {
		t.Errorf("root tasks = %d, want %d (second frame)", got, want)
	}
	if !nearly(cgs[0].CPU, 1.1) {
		t.Errorf("root cpu = %v, want 1.1", cgs[0].CPU)
	}
	if !cgs[0].HaveMemory || !nearly(cgs[0].Memory, 964.1*(1<<20)) {
		t.Errorf("root memory = %v (have %v), want 964.1M", cgs[0].Memory, cgs[0].HaveMemory)
	}
	if !cgs[0].HaveOutput || !nearly(cgs[0].Output, 2.3*(1<<10)) {
		t.Errorf("root output = %v (have %v), want 2.3K", cgs[0].Output, cgs[0].HaveOutput)
	}
	// init.scope prints "-" for both rates: those cells stay nil.
	init := cgs[1]
	if init.HaveInput || init.HaveOutput {
		t.Errorf("init.scope rates = %v/%v (have %v/%v), want unset",
			init.Input, init.Output, init.HaveInput, init.HaveOutput)
	}
	if init.Path != "init.scope" || init.Tasks != 1 {
		t.Errorf("init.scope = %+v", init)
	}
	if !nearly(cgs[2].Memory, 1.2*(1<<30)) {
		t.Errorf("system.slice memory = %v, want 1.2G", cgs[2].Memory)
	}
	if !nearly(cgs[3].Input, 1.0*(1<<10)) {
		t.Errorf("user.slice input = %v, want 1.0K", cgs[3].Input)
	}
}

func TestParseCgtopSkipsGarbageAndEmpty(t *testing.T) {
	cgs, err := ParseCgtop("")
	if err != nil || len(cgs) != 0 {
		t.Fatalf("empty output = %+v, %v; want none, nil", cgs, err)
	}
	garbage := "Control Group  Tasks  %CPU  Memory  Input/s Output/s\n" +
		"not a row\n" +
		"/  x  1.0  1M  0B  0B\n" +
		"/good  4  0.5  2M  0B  0B\n"
	cgs, err = ParseCgtop(garbage)
	if err != nil {
		t.Fatalf("ParseCgtop: %v", err)
	}
	if len(cgs) != 1 || cgs[0].Path != "/good" || cgs[0].Tasks != 4 {
		t.Fatalf("garbage parse = %+v, want only /good", cgs)
	}
}

func TestCgroupRowsSortAndNils(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindCgroup)
	ex.On("", scriptReply(batch, map[string]string{secCgroup: cgtopOut}, nil, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindCgroup})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	items := results[0].Items
	if len(items) != 4 {
		t.Fatalf("rows = %d, want 4", len(items))
	}
	// Biggest memory first.
	if got, want := items[0].Ref.Name, "system.slice"; got != want {
		t.Errorf("first row = %q, want %q", got, want)
	}
	if got, ok := items[0].Fields["memory"].(int64); !ok || got != 1288490188 {
		t.Errorf("system.slice memory = %v, want 1.2G in bytes", items[0].Fields["memory"])
	}
	for _, r := range items {
		if r.Ref.Kind != KindCgroup || r.Ref.Backend != backendName || r.Ref.Host != "web-01" {
			t.Errorf("bad ref %+v", r.Ref)
		}
		if r.Fields["cgroup"] == nil || r.Fields["tasks"] == nil || r.Fields["cpu"] == nil {
			t.Errorf("%s: missing field in %+v", r.Ref.Name, r.Fields)
		}
		if r.Ref.Name == "init.scope" {
			if got, ok := r.Fields["input"]; ok || got != nil {
				t.Errorf("init.scope input = %v, want nil", got)
			}
		}
	}
}

func TestPkgFamilyFromOSRelease(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{"rocky", "NAME=\"Rocky Linux\"\nID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n", FamilyRPM, true},
		{"fedora", "NAME=Fedora\nID=fedora\n", FamilyRPM, true},
		{"ubuntu", "NAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\n", FamilyDeb, true},
		{"debian", "ID=debian\n", FamilyDeb, true},
		{"suse", "ID=\"opensuse-leap\"\nID_LIKE=\"suse opensuse\"\n", FamilyRPM, true},
		{"unknown", "ID=alpine\n", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := OSFamily(tc.out)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("OSFamily(%q) = %q, %v; want %q, %v", tc.out, got, ok, tc.want, tc.ok)
			}
		})
	}
}

const pkgRpmOut = familyPrefix + `rpm
bash	5.1.8-6.el9_1	x86_64	1670000000
openssl-libs	3.0.7-27.el9	x86_64	1675000000
broken	line-without-tabs
gpg-pubkey	1.0-1	noarch	(none)
`

const pkgDebOut = familyPrefix + `deb
bash	5.1-6ubuntu1	amd64	ii 
libssl3	3.0.2-0ubuntu1.10	amd64	ii 
oldconf	1.0	all	rc 
`

func TestParsePkgRpm(t *testing.T) {
	pkgs, err := ParsePkg(pkgRpmOut)
	if err != nil {
		t.Fatalf("ParsePkg: %v", err)
	}
	// "broken" has no tabs, "gpg-pubkey" has no install time; both are kept but
	// the second has no Installed value.
	if got, want := len(pkgs), 3; got != want {
		t.Fatalf("packages = %d, want %d: %+v", got, want, pkgs)
	}
	first := pkgs[0]
	if first.Name != "bash" || first.Version != "5.1.8-6.el9_1" || first.Arch != "x86_64" {
		t.Errorf("first = %+v", first)
	}
	if first.Family != FamilyRPM || first.Status != "" {
		t.Errorf("family/status = %q/%q", first.Family, first.Status)
	}
	if got, want := first.Installed.Unix(), int64(1670000000); got != want {
		t.Errorf("installed = %d, want %d", got, want)
	}
	if !pkgs[2].Installed.IsZero() {
		t.Errorf("gpg-pubkey installed = %v, want zero", pkgs[2].Installed)
	}
}

func TestParsePkgDpkg(t *testing.T) {
	pkgs, err := ParsePkg(pkgDebOut)
	if err != nil {
		t.Fatalf("ParsePkg: %v", err)
	}
	if got, want := len(pkgs), 3; got != want {
		t.Fatalf("packages = %d, want %d: %+v", got, want, pkgs)
	}
	if pkgs[0].Name != "bash" || pkgs[0].Version != "5.1-6ubuntu1" || pkgs[0].Arch != "amd64" {
		t.Errorf("first = %+v", pkgs[0])
	}
	if pkgs[0].Family != FamilyDeb || pkgs[0].Status != "ii" {
		t.Errorf("family/status = %q/%q, want deb/ii", pkgs[0].Family, pkgs[0].Status)
	}
	if !pkgs[0].Installed.IsZero() {
		t.Errorf("deb installed = %v, want zero (rpm only)", pkgs[0].Installed)
	}
	if pkgs[2].Status != "rc" {
		t.Errorf("oldconf status = %q, want rc", pkgs[2].Status)
	}
	pkgs, err = ParsePkg("")
	if err != nil || len(pkgs) != 0 {
		t.Fatalf("empty output = %+v, %v", pkgs, err)
	}
}

func TestPkgCommandIsReadOnly(t *testing.T) {
	for _, bad := range []string{"rm ", " > ", ">>", "tee ", "mv ", "dd ", "<<", "sed -i"} {
		if strings.Contains(cmdPkg, bad) {
			t.Fatalf("cmdPkg contains %q: %s", bad, cmdPkg)
		}
	}
	for _, want := range []string{". /etc/os-release", "ID_LIKE", "rpm -qa --qf", "dpkg-query -W -f", familyPrefix + "rpm", familyPrefix + "deb"} {
		if !strings.Contains(cmdPkg, want) {
			t.Errorf("cmdPkg is missing %q: %s", want, cmdPkg)
		}
	}
}

func TestRpmPkgRowsAndTimeout(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("db-01"))
	batch := batchOf(t, b, KindPkg)
	ex.On("", scriptReply(batch, map[string]string{secPkg: pkgRpmOut}, nil, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindPkg, Hosts: []resource.HostID{"db-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	items := results[0].Items
	if got, want := len(items), 3; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if got, want := items[0].Ref.Name, "bash"; got != want {
		t.Errorf("first row = %q, want %q (sorted by name)", got, want)
	}
	if got := items[0].Fields["version"]; got != "5.1.8-6.el9_1" {
		t.Errorf("version = %v", got)
	}
	if got, ok := items[0].Fields["installed"].(time.Time); !ok || got.Unix() != 1670000000 {
		t.Errorf("installed = %v", items[0].Fields["installed"])
	}
	for _, r := range items {
		if r.Fields["name"] == "gpg-pubkey" {
			if _, ok := r.Fields["installed"]; ok {
				t.Errorf("gpg-pubkey has an installed value: %v", r.Fields["installed"])
			}
		}
	}
	calls := ex.Calls()
	if len(calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(calls))
	}
	if got, want := calls[0].Command.Timeout, pkgTimeout; got != want {
		t.Errorf("pkg timeout = %v, want %v", got, want)
	}
}

func TestDpkgPkgRows(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-02"))
	batch := batchOf(t, b, KindPkg)
	ex.On("", scriptReply(batch, map[string]string{secPkg: pkgDebOut}, nil, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindPkg, Hosts: []resource.HostID{"web-02"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	items := results[0].Items
	if got, want := len(items), 3; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	for _, r := range items {
		if _, ok := r.Fields["installed"]; ok {
			t.Errorf("%s: deb rows must not carry an install time", r.Ref.Name)
		}
	}
	if got := items[0].Fields["arch"]; got != "amd64" {
		t.Errorf("arch = %v", got)
	}
}

func TestPkgVersionCommandQuotesName(t *testing.T) {
	name := "libc; rm -rf /"
	script := PackageVersionCommand(name)
	quoted := shq.Quote(name)
	// The name appears twice (one branch per package manager), always inside the
	// shq quotes and never bare.
	if got := strings.Count(script, name); got != 2 {
		t.Fatalf("name appears %d times, want 2: %s", got, script)
	}
	if got := strings.Count(script, quoted); got != 2 {
		t.Fatalf("quoted name appears %d times, want 2: %s", got, script)
	}
	if strings.Contains(script, " "+name) || strings.Contains(script, name+" ") {
		t.Errorf("package name reached the command line unquoted: %s", script)
	}
	if _, err := ParsePackageVersion(familyPrefix + "rpm\n" + "1.2.3-1\n"); err != nil {
		t.Errorf("ParsePackageVersion: %v", err)
	}
}

func TestKernelKindUsesJournalFlagsAndSudo(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	ex.On("journalctl", transporttest.Reply{Stdout: `{"__REALTIME_TIMESTAMP":"1700000000000000","PRIORITY":"3","_TRANSPORT":"kernel","MESSAGE":"i915: GPU hang"}
{"__REALTIME_TIMESTAMP":"1700000001000000","PRIORITY":"6","_TRANSPORT":"kernel","MESSAGE":"eth0: link up"}
`})

	ch, err := b.List(context.Background(), resource.Query{Kind: KindKernel, Hosts: []resource.HostID{"web-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	items := results[0].Items
	if got, want := len(items), 2; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if items[0].Ref.Kind != KindKernel || items[0].Ref.Host != "web-01" {
		t.Errorf("bad ref %+v", items[0].Ref)
	}
	if got := items[0].Fields["message"]; got != "i915: GPU hang" {
		t.Errorf("message = %v", got)
	}
	calls := ex.Calls()
	if len(calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(calls))
	}
	cmd := calls[0].Command
	if !cmd.Sudo {
		t.Error("kernel log must run under sudo")
	}
	for _, want := range []string{"journalctl", "-k", "-p 4", "-n 300", "-o json", "--no-pager"} {
		if !strings.Contains(cmd.Script, want) {
			t.Errorf("kernel command is missing %q: %s", want, cmd.Script)
		}
	}
}

func TestParseFileStat(t *testing.T) {
	st, err := ParseFileStat("4096 root:adm 640 2026-11-12 10:31:02.123456789 -0500\n")
	if err != nil {
		t.Fatalf("ParseFileStat: %v", err)
	}
	if st.Size != 4096 || st.Owner != "root" || st.Group != "adm" || st.Mode != "640" {
		t.Errorf("stat = %+v", st)
	}
	if got, want := st.MTime.UTC().Format(time.RFC3339), "2026-11-12T15:31:02Z"; got != want {
		t.Errorf("mtime = %s, want %s", got, want)
	}
	if _, err := ParseFileStat("nonsense"); err == nil {
		t.Error("ParseFileStat accepted nonsense")
	}
}

func TestParseChecksumAndBinary(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	got, path, ok := ParseChecksumLine(sum + "  /etc/nginx/nginx.conf")
	if !ok || got != sum || path != "/etc/nginx/nginx.conf" {
		t.Fatalf("ParseChecksumLine = %q, %q, %v", got, path, ok)
	}
	if _, _, ok := ParseChecksumLine("not a checksum"); ok {
		t.Error("ParseChecksumLine accepted garbage")
	}
	if !IsBinaryData([]byte("abc\x00def")) {
		t.Error("NUL in the first KiB is binary")
	}
	if IsBinaryData([]byte("plain text\n")) {
		t.Error("text reported as binary")
	}
	if IsBinaryData([]byte(strings.Repeat("a", 9<<10) + "\x00")) {
		t.Error("NUL after the first 8 KiB is not sniffed")
	}
}

// TestKindSpecsHaveMiscKinds keeps the new kinds wired into the specs, aliases
// and normalization.
func TestKindSpecsHaveMiscKinds(t *testing.T) {
	byName := map[resource.KindName]resource.KindSpec{}
	for _, spec := range kindSpecs() {
		byName[spec.Name] = spec
	}
	for _, kind := range []resource.KindName{KindCgroup, KindKernel, KindPkg} {
		spec, ok := byName[kind]
		if !ok {
			t.Fatalf("kind %s has no spec", kind)
		}
		if len(spec.Columns) == 0 {
			t.Errorf("kind %s has no columns", kind)
		}
	}
	cases := map[string]resource.KindName{
		"cgtop":    KindCgroup,
		"cg":       KindCgroup,
		"dmesg":    KindKernel,
		"packages": KindPkg,
		"pkg":      KindPkg,
	}
	for alias, want := range cases {
		if got, ok := normalizeKind(resource.KindName(alias)); !ok || got != want {
			t.Errorf("normalizeKind(%q) = %q, %v; want %q", alias, got, ok, want)
		}
	}
}

// TestPkgFailureFailsHost makes sure a probe error is not silently swallowed.
func TestPkgFailureFailsHost(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindPkg)
	ex.On("", scriptReply(batch, nil, map[string]int{secPkg: 127}, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindPkg, Hosts: []resource.HostID{"web-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want one error", results)
	}
	// `dpkg-query`/`rpm` missing is rc 127, which the probe layer classifies as
	// not-available; any non-empty error fails the host.
	if k := errs.KindOf(results[0].Err); k != errs.NotAvailable && k != errs.Remote {
		t.Errorf("error kind = %s, want not-available or remote", k)
	}
}

// nearly compares two human-scaled figures with a small relative tolerance: the
// parser parses a decimal and multiplies it, so exact bit equality is the wrong
// test.
func nearly(a, b float64) bool {
	d := math.Abs(a - b)
	return d <= 1e-6*math.Max(1, math.Abs(b))
}
