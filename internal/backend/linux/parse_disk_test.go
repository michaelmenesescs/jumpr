package linux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/probe"
	"jumper/internal/resource"
	"jumper/internal/transport/transporttest"
)

// readDiskTestdata reads one fixture of the disk capture.
func readDiskTestdata(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", "testdata", "linux", "disk", "rocky9", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// section builds a successful section result with the given stdout.
func section(id, out string) probe.SectionResult {
	return probe.SectionResult{ID: id, Stdout: out}
}

func TestParseDFGolden(t *testing.T) {
	mounts, err := ParseDF(readDiskTestdata(t, "df.txt"))
	if err != nil {
		t.Fatalf("ParseDF: %v", err)
	}
	if got, want := len(mounts), 6; got != want {
		t.Fatalf("mounts = %d, want %d: %+v", got, want, mounts)
	}

	want := Mount{
		Mountpoint: "/",
		FSType:     "xfs",
		Source:     "/dev/mapper/rl-root",
		Size:       41943040 * 1024,
		Used:       20971520 * 1024,
		Avail:      20971520 * 1024,
		UsePct:     50,
	}
	if got := mounts[0]; got != want {
		t.Errorf("mounts[0] = %+v, want %+v", got, want)
	}

	// A mount point with a space is the rest of the line, not a field.
	last := mounts[len(mounts)-1]
	if got, want := last.Mountpoint, "/mnt/backup disk"; got != want {
		t.Errorf("last mountpoint = %q, want %q", got, want)
	}
	if got, want := last.UsePct, 90; got != want {
		t.Errorf("last use = %d, want %d", got, want)
	}
	if got, want := last.Size, int64(209715200)*1024; got != want {
		t.Errorf("last size = %d, want %d", got, want)
	}
}

func TestParseDFWithoutTypeColumn(t *testing.T) {
	out := "" +
		"Filesystem     1K-blocks    Used Available Use% Mounted on\n" +
		"/dev/sda1        1038336  312000    726336  31% /boot\n" +
		"/dev/sdb1      209715200 188743680 20971520  90% /mnt/backup disk\n" +
		"short line\n" +
		"/dev/bad 12 nope 3 4% /bad\n"
	mounts, err := ParseDF(out)
	if err != nil {
		t.Fatalf("ParseDF: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mounts = %d, want 2: %+v", len(mounts), mounts)
	}
	if got, want := mounts[0], (Mount{
		Mountpoint: "/boot",
		Source:     "/dev/sda1",
		Size:       1038336 * 1024,
		Used:       312000 * 1024,
		Avail:      726336 * 1024,
		UsePct:     31,
	}); got != want {
		t.Errorf("mounts[0] = %+v, want %+v", got, want)
	}
	if mounts[1].Mountpoint != "/mnt/backup disk" {
		t.Errorf("mounts[1] mountpoint = %q", mounts[1].Mountpoint)
	}
}

func TestParseDFGarbageDoesNotPanic(t *testing.T) {
	for _, out := range []string{"", "\n\n", "Filesystem Type 1K-blocks Used Available Capacity Mounted on\n", "garbage"} {
		mounts, err := ParseDF(out)
		if err != nil {
			t.Errorf("ParseDF(%q) error: %v", out, err)
		}
		if len(mounts) != 0 {
			t.Errorf("ParseDF(%q) = %+v, want none", out, mounts)
		}
	}
}

func TestParseDFInodesGolden(t *testing.T) {
	inodes, err := ParseDFInodes(readDiskTestdata(t, "df-inodes.txt"))
	if err != nil {
		t.Fatalf("ParseDFInodes: %v", err)
	}
	if got, want := len(inodes), 6; got != want {
		t.Fatalf("inodes = %d, want %d: %v", got, want, inodes)
	}
	if got, want := inodes["/data"], 98; got != want {
		t.Errorf("inodes[/data] = %d, want %d", got, want)
	}
	if got, want := inodes["/mnt/backup disk"], 8; got != want {
		t.Errorf("inodes[/mnt/backup disk] = %d, want %d", got, want)
	}
}

func TestParseMountsJoinsInodeUse(t *testing.T) {
	mounts, err := ParseMounts(readDiskTestdata(t, "df.txt"), readDiskTestdata(t, "df-inodes.txt"))
	if err != nil {
		t.Fatalf("ParseMounts: %v", err)
	}
	byMount := make(map[string]Mount, len(mounts))
	for _, m := range mounts {
		byMount[m.Mountpoint] = m
	}
	if got, want := byMount["/data"].IUsePct, 98; got != want {
		t.Errorf("/data iuse = %d, want %d", got, want)
	}
	if got, want := byMount["/boot"].IUsePct, 3; got != want {
		t.Errorf("/boot iuse = %d, want %d", got, want)
	}
	if got, want := byMount["/data"].UsePct, 95; got != want {
		t.Errorf("/data use = %d, want %d", got, want)
	}
}

func TestMountStatusThresholds(t *testing.T) {
	cases := []struct {
		name string
		use  int
		iuse int
		want resource.Status
	}{
		{"quiet", 10, 20, resource.StatusOK},
		{"space warn", 85, 10, resource.StatusWarn},
		{"inode warn", 10, 85, resource.StatusWarn},
		{"space crit", 96, 10, resource.StatusCrit},
		{"inode crit", 10, 95, resource.StatusCrit},
	}
	for _, tc := range cases {
		if got := (Mount{UsePct: tc.use, IUsePct: tc.iuse}).Status(); got != tc.want {
			t.Errorf("%s: status = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFilterMounts(t *testing.T) {
	mounts, err := ParseDF(readDiskTestdata(t, "df.txt"))
	if err != nil {
		t.Fatalf("ParseDF: %v", err)
	}
	kept := FilterMounts(append([]Mount(nil), mounts...), false)
	if got, want := len(kept), 4; got != want {
		t.Fatalf("default mounts = %d, want %d: %+v", got, want, kept)
	}
	for _, m := range kept {
		switch m.FSType {
		case "tmpfs", "devtmpfs", "overlay", "squashfs":
			t.Errorf("pseudo filesystem %q not filtered", m.FSType)
		}
	}
	if got, want := len(FilterMounts(append([]Mount(nil), mounts...), true)), 6; got != want {
		t.Errorf("all mounts = %d, want %d", got, want)
	}
}

func TestParseLsblkGolden(t *testing.T) {
	disks, err := ParseLsblk(readDiskTestdata(t, "lsblk.json"))
	if err != nil {
		t.Fatalf("ParseLsblk: %v", err)
	}
	if got, want := len(disks), 6; got != want {
		t.Fatalf("disks = %d, want %d: %+v", got, want, disks)
	}

	// The tree is flattened depth-first: sda, sda1, sda2, nvme0n1, nvme0n1p1, sr0.
	names := []string{"sda", "sda1", "sda2", "nvme0n1", "nvme0n1p1", "sr0"}
	depths := []int{0, 1, 1, 0, 1, 0}
	for i, d := range disks {
		if d.Name != names[i] || d.Depth != depths[i] {
			t.Errorf("disks[%d] = %s depth %d, want %s depth %d", i, d.Name, d.Depth, names[i], depths[i])
		}
	}
	if got, want := disks[1].IndentedName(), "  sda1"; got != want {
		t.Errorf("indented name = %q, want %q", got, want)
	}
	// Size came through as a quoted string.
	if got, want := disks[1].Size, int64(1073741824); got != want {
		t.Errorf("sda1 size = %d, want %d", got, want)
	}
	// A null mountpoint with a mountpoints array.
	if got, want := disks[2].Mountpoint(), "/"; got != want {
		t.Errorf("sda2 mountpoint = %q, want %q", got, want)
	}
	if got, want := len(disks[2].Mountpoints), 2; got != want {
		t.Errorf("sda2 mountpoints = %v", disks[2].Mountpoints)
	}
	// A plain array-free mountpoint.
	if got, want := disks[4].Mountpoint(), "/boot/efi"; got != want {
		t.Errorf("nvme0n1p1 mountpoint = %q, want %q", got, want)
	}
	if disks[3].Rota {
		t.Error("nvme0n1 reported rotational")
	}
	if !disks[0].Rota || disks[0].ReadOnly {
		t.Errorf("sda rota/ro = %v/%v, want true/false", disks[0].Rota, disks[0].ReadOnly)
	}
	if !disks[5].ReadOnly {
		t.Error("sr0 should be read-only")
	}
	if got, want := disks[3].Model, "Samsung SSD 980 PRO 500GB"; got != want {
		t.Errorf("nvme0n1 model = %q, want %q", got, want)
	}
}

func TestParseLsblkEmptyAndMalformed(t *testing.T) {
	disks, err := ParseLsblk("")
	if err != nil || len(disks) != 0 {
		t.Errorf("empty lsblk = %+v, %v; want none, nil", disks, err)
	}
	disks, err = ParseLsblk(`{"blockdevices":[]}`)
	if err != nil || len(disks) != 0 {
		t.Errorf("empty blockdevices = %+v, %v; want none, nil", disks, err)
	}
	if _, err := ParseLsblk("{"); errs.KindOf(err) != errs.Parse {
		t.Errorf("malformed json kind = %s, want parse", errs.KindOf(err))
	}
}

func TestParseDiskstatsSingleSample(t *testing.T) {
	stats, err := ParseDiskstats(readDiskTestdata(t, "diskstats.txt"))
	if err != nil {
		t.Fatalf("ParseDiskstats: %v", err)
	}
	if got, want := len(stats), 4; got != want {
		t.Fatalf("stats = %d, want %d: %v", got, want, stats)
	}
	// The last line for each device wins when two samples are present.
	if got, want := stats["nvme0n1"].Reads, uint64(1100); got != want {
		t.Errorf("nvme0n1 reads = %d, want %d", got, want)
	}
	if got, want := stats["sda1"].Reads, uint64(5); got != want {
		t.Errorf("sda1 reads = %d, want %d", got, want)
	}
}

func TestParseDiskstatsPairRates(t *testing.T) {
	rates, err := ParseDiskstatsPair(readDiskTestdata(t, "diskstats.txt"), time.Second)
	if err != nil {
		t.Fatalf("ParseDiskstatsPair: %v", err)
	}
	if got, want := len(rates), 4; got != want {
		t.Fatalf("rates = %d, want %d: %v", got, want, rates)
	}
	nvme := rates["nvme0n1"]
	if nvme.RPS != 100 || nvme.WPS != 300 {
		t.Errorf("nvme0n1 r/s, w/s = %v, %v; want 100, 300", nvme.RPS, nvme.WPS)
	}
	if nvme.Util != 40 {
		t.Errorf("nvme0n1 util = %v, want 40", nvme.Util)
	}
	if got, want := rates["nvme0n1p1"].RPS, 20.0; got != want {
		t.Errorf("nvme0n1p1 r/s = %v, want %v", got, want)
	}
	if got, want := rates["nvme0n1p1"].Util, 0.2; got != want {
		t.Errorf("nvme0n1p1 util = %v, want %v", got, want)
	}
	// Unchanged counters and a counter reset both yield zero, not negative.
	if rates["sda"] != (DiskRate{}) {
		t.Errorf("sda rate = %+v, want zero", rates["sda"])
	}
	if rates["sda1"].RPS != 0 {
		t.Errorf("sda1 r/s = %v, want 0 after counter reset", rates["sda1"].RPS)
	}
}

func TestDiskstatsNeedsTwoSamples(t *testing.T) {
	if _, err := ParseDiskstatsPair("   8 0 sda 1 0 1 0 1 0 1 0 0 1 1\n", time.Second); errs.KindOf(err) != errs.Parse {
		t.Errorf("single sample kind = %s, want parse", errs.KindOf(err))
	}
}

func TestDiskRatesSkipsDevicesWithoutBaseline(t *testing.T) {
	before := map[string]DiskStat{"a": {Reads: 1}}
	after := map[string]DiskStat{"a": {Reads: 2}, "b": {Reads: 5}}
	rates := DiskRates(before, after, time.Second)
	if _, ok := rates["b"]; ok {
		t.Error("device without a baseline should be omitted")
	}
	if got, want := rates["a"].RPS, 1.0; got != want {
		t.Errorf("a r/s = %v, want %v", got, want)
	}
	if DiskRates(before, after, 0) != nil {
		t.Error("zero interval should yield no rates")
	}
}

func TestMountRowsFromSections(t *testing.T) {
	b := New(Options{Now: func() time.Time { return time.Unix(1700000000, 0) }})
	sec := map[string]probe.SectionResult{
		secDF:      section(secDF, readDiskTestdata(t, "df.txt")),
		secDFInode: section(secDFInode, readDiskTestdata(t, "df-inodes.txt")),
	}
	rows, err := b.rows(KindMount, "web-01", resource.Query{Kind: KindMount}, sec, nil, nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if got, want := len(rows), 4; got != want {
		t.Fatalf("mount rows = %d, want %d: %+v", got, want, rows)
	}
	byName := make(map[string]resource.Resource, len(rows))
	for _, r := range rows {
		byName[r.Ref.Name] = r
	}
	data, ok := byName["/data"]
	if !ok {
		t.Fatalf("no row for /data: %+v", rows)
	}
	if data.Ref.Kind != KindMount || data.Ref.Host != "web-01" || data.Ref.Backend != "linux" {
		t.Errorf("bad ref %+v", data.Ref)
	}
	if got, want := data.Fields["mount"], "/data"; got != want {
		t.Errorf("mount = %v, want %v", got, want)
	}
	if got, want := data.Fields["size"], int64(104857600)*1024; got != want {
		t.Errorf("size = %v, want %v", got, want)
	}
	if got, want := data.Fields["use"], int64(95); got != want {
		t.Errorf("use = %v, want %v", got, want)
	}
	if got, want := data.Fields["iuse"], int64(98); got != want {
		t.Errorf("iuse = %v, want %v", got, want)
	}
	if got, want := data.Fields["source"], "/dev/mapper/rl-data"; got != want {
		t.Errorf("source = %v, want %v", got, want)
	}
	if data.Status != resource.StatusCrit {
		t.Errorf("/data status = %v, want crit", data.Status)
	}
	if got, want := byName["/mnt/backup disk"].Status, resource.StatusWarn; got != want {
		t.Errorf("backup status = %v, want warn", got)
	}
	for _, r := range rows {
		if r.Fields["fstype"] == "tmpfs" || r.Fields["fstype"] == "overlay" {
			t.Errorf("pseudo filesystem row leaked: %+v", r.Fields)
		}
	}
}

func TestMountRowsAllArgAndInodeDegrade(t *testing.T) {
	b := New(Options{})
	sec := map[string]probe.SectionResult{
		secDF:      section(secDF, readDiskTestdata(t, "df.txt")),
		secDFInode: section(secDFInode, readDiskTestdata(t, "df-inodes.txt")),
	}
	rows, err := b.rows(KindMount, "web-01", resource.Query{Kind: KindMount, Args: map[string]string{"all": "1"}}, sec, nil, nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if got, want := len(rows), 6; got != want {
		t.Fatalf("all rows = %d, want %d", got, want)
	}

	bad := map[string]error{secDFInode: errs.New(errs.Remote, secDFInode, "exit 1")}
	rows, err = b.rows(KindMount, "web-01", resource.Query{Kind: KindMount}, sec, bad, nil)
	if err != nil {
		t.Fatalf("rows with a failed inode section: %v", err)
	}
	if got, want := len(rows), 4; got != want {
		t.Fatalf("degraded rows = %d, want %d", got, want)
	}
	for _, r := range rows {
		if _, ok := r.Fields["iuse"]; ok {
			t.Errorf("iuse should be absent when the inode sample failed: %+v", r.Fields)
		}
		if got := r.Labels["_degraded"]; !strings.Contains(got, secDFInode) {
			t.Errorf("_degraded = %q, want it to name %s", got, secDFInode)
		}
	}
}

func TestDiskRowsFromSections(t *testing.T) {
	b := New(Options{})
	sec := map[string]probe.SectionResult{
		secLsblk:     section(secLsblk, readDiskTestdata(t, "lsblk.json")),
		secDiskstats: section(secDiskstats, readDiskTestdata(t, "diskstats.txt")),
	}
	rows, err := b.rows(KindDisk, "web-01", resource.Query{Kind: KindDisk}, sec, nil, nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if got, want := len(rows), 6; got != want {
		t.Fatalf("disk rows = %d, want %d", got, want)
	}
	byName := make(map[string]resource.Resource, len(rows))
	for _, r := range rows {
		byName[r.Ref.Name] = r
	}
	nvme, ok := byName["nvme0n1"]
	if !ok {
		t.Fatalf("no row for nvme0n1: %+v", rows)
	}
	if nvme.Ref.Kind != KindDisk || nvme.Ref.Host != "web-01" {
		t.Errorf("bad ref %+v", nvme.Ref)
	}
	if got, want := nvme.Fields["r/s"], 100.0; got != want {
		t.Errorf("r/s = %v, want %v", got, want)
	}
	if got, want := nvme.Fields["w/s"], 300.0; got != want {
		t.Errorf("w/s = %v, want %v", got, want)
	}
	if got, want := nvme.Fields["util"], 40.0; got != want {
		t.Errorf("util = %v, want %v", got, want)
	}
	if got, want := nvme.Fields["rota"], false; got != want {
		t.Errorf("rota = %v, want %v", got, want)
	}
	if got, want := nvme.Fields["size"], int64(500107862016); got != want {
		t.Errorf("size = %v, want %v", got, want)
	}
	sda1 := byName["sda1"]
	if got, want := sda1.Fields["name"], "  sda1"; got != want {
		t.Errorf("indented name = %v, want %v", got, want)
	}
	if got, want := sda1.Fields["mount"], "/boot"; got != want {
		t.Errorf("sda1 mount = %v, want %v", got, want)
	}
}

func TestDiskRowsStatsFailureDegrades(t *testing.T) {
	b := New(Options{})
	sec := map[string]probe.SectionResult{
		secLsblk:     section(secLsblk, readDiskTestdata(t, "lsblk.json")),
		secDiskstats: section(secDiskstats, readDiskTestdata(t, "diskstats.txt")),
	}
	bad := map[string]error{secDiskstats: errs.New(errs.Remote, secDiskstats, "exit 1")}
	rows, err := b.rows(KindDisk, "web-01", resource.Query{Kind: KindDisk}, sec, bad, nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if got, want := len(rows), 6; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	for _, r := range rows {
		if _, ok := r.Fields["r/s"]; ok {
			t.Errorf("rates should be absent when diskstats failed: %+v", r.Fields)
		}
		if got := r.Labels["_degraded"]; !strings.Contains(got, secDiskstats) {
			t.Errorf("_degraded = %q, want it to name %s", got, secDiskstats)
		}
	}
}

func TestMountUsageCommandQuotesPath(t *testing.T) {
	cmd := MountUsageCommand("/mnt/backup disk")
	if !cmd.Sudo {
		t.Error("du must run under sudo")
	}
	if cmd.Timeout != 20*time.Second {
		t.Errorf("timeout = %v, want 20s", cmd.Timeout)
	}
	for _, want := range []string{"du -xh", "--max-depth=1", "'/mnt/backup disk'", "sort -rh", "head -25"} {
		if !strings.Contains(cmd.Script, want) {
			t.Errorf("script %q does not contain %q", cmd.Script, want)
		}
	}
}

func TestMountUsage(t *testing.T) {
	ex := transporttest.New()
	ex.On("du -xh", transporttest.Reply{Stdout: "4.0K\t/mnt/backup disk/keep\n0\t/mnt/backup disk/logs\n"})
	b := New(Options{Exec: ex})

	lines, err := b.MountUsage(context.Background(), "web-01", "/mnt/backup disk")
	if err != nil {
		t.Fatalf("MountUsage: %v", err)
	}
	if got, want := len(lines), 2; got != want {
		t.Fatalf("lines = %d, want %d: %v", got, want, lines)
	}
	if got, want := lines[0], "4.0K\t/mnt/backup disk/keep"; got != want {
		t.Errorf("lines[0] = %q, want %q", got, want)
	}
	calls := ex.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Host != "web-01" || !calls[0].Command.Sudo {
		t.Errorf("call = %+v, want sudo on web-01", calls[0])
	}

	if _, err := b.MountUsage(context.Background(), "web-01", "../../etc"); errs.KindOf(err) != errs.Config {
		t.Errorf("relative path kind = %s, want config", errs.KindOf(err))
	}
}

func TestKindsServeDiskAndMount(t *testing.T) {
	b := New(Options{})
	specs := make(map[resource.KindName]resource.KindSpec)
	for _, s := range b.Kinds() {
		specs[s.Name] = s
	}
	mount, ok := specs[KindMount]
	if !ok {
		t.Fatal("mount kind not registered")
	}
	if mount.Scope != resource.ScopeHost {
		t.Errorf("mount scope = %v, want host", mount.Scope)
	}
	ids := make([]string, len(mount.Columns))
	for i, c := range mount.Columns {
		ids[i] = c.ID
	}
	want := []string{"mount", "fstype", "size", "used", "avail", "use", "iuse", "source"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("mount columns = %v, want %v", ids, want)
	}
	disk, ok := specs[KindDisk]
	if !ok {
		t.Fatal("disk kind not registered")
	}
	if disk.Scope != resource.ScopeHost {
		t.Errorf("disk scope = %v, want host", disk.Scope)
	}

	aliases := map[string]resource.KindName{
		"mounts": KindMount, "df": KindMount,
		"disks": KindDisk, "lsblk": KindDisk,
	}
	for alias, want := range aliases {
		got, ok := normalizeKind(resource.KindName(alias))
		if !ok || got != want {
			t.Errorf("normalizeKind(%q) = %q, %v; want %q", alias, got, ok, want)
		}
	}
}

func TestBackendListMountsOneRoundTrip(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindMount)
	ex.On("", scriptReply(batch, map[string]string{
		secDF:      readDiskTestdata(t, "df.txt"),
		secDFInode: readDiskTestdata(t, "df-inodes.txt"),
	}, nil, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindMount})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var rows []resource.Resource
	for r := range ch {
		if r.Err != nil {
			t.Fatalf("host %s: %v", r.Host, r.Err)
		}
		rows = append(rows, r.Items...)
	}
	if got, want := len(rows), 4; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if got, want := len(ex.Calls()), 1; got != want {
		t.Errorf("Exec calls = %d, want %d (one batch per host)", got, want)
	}
	if !strings.Contains(ex.Calls()[0].Command.Script, "df -PTk") {
		t.Errorf("not the mount batch: %q", ex.Calls()[0].Command.Script)
	}
}

func TestBackendListDisksOneRoundTrip(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindDisk)
	ex.On("", scriptReply(batch, map[string]string{
		secLsblk:     readDiskTestdata(t, "lsblk.json"),
		secDiskstats: readDiskTestdata(t, "diskstats.txt"),
	}, nil, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindDisk})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var rows []resource.Resource
	for r := range ch {
		if r.Err != nil {
			t.Fatalf("host %s: %v", r.Host, r.Err)
		}
		rows = append(rows, r.Items...)
	}
	if got, want := len(rows), 6; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if got, want := len(ex.Calls()), 1; got != want {
		t.Errorf("Exec calls = %d, want %d (one batch per host)", got, want)
	}
	if !strings.Contains(ex.Calls()[0].Command.Script, "lsblk -J -b") {
		t.Errorf("not the disk batch: %q", ex.Calls()[0].Command.Script)
	}
}
