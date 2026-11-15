package linux

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// Parser names. They double as the Op of a parser error so the UI can say which
// probe section failed.
const (
	diskProbeDF        = "df"
	diskProbeDFInodes  = "df-inodes"
	diskProbeLsblk     = "lsblk"
	diskProbeDiskstats = "diskstats"
)

// Mount is one row of `df -PTk`, joined with the inode use of `df -Pi`.
// Size, Used and Avail are bytes. IUsePct is zero when the inode sample is
// missing.
type Mount struct {
	Mountpoint string
	FSType     string
	Source     string
	Size       int64
	Used       int64
	Avail      int64
	UsePct     int
	IUsePct    int
}

// Status maps the fuller of the two percentages onto a resource status:
// Warn at 85 %, Crit at 95 %.
func (m Mount) Status() resource.Status {
	return mountStatus(m.UsePct, m.IUsePct)
}

// mountStatus is the threshold table shared by space and inode percentages.
func mountStatus(pcts ...int) resource.Status {
	worst := 0
	for _, p := range pcts {
		if p > worst {
			worst = p
		}
	}
	switch {
	case worst >= 95:
		return resource.StatusCrit
	case worst >= 85:
		return resource.StatusWarn
	default:
		return resource.StatusOK
	}
}

// ParseDF parses `df -PTk` (one line per mounted filesystem, 1K blocks) into
// mounts with byte sizes. Lines with too few fields or unparseable numbers are
// skipped, so unknown df dialects degrade instead of failing the host. The
// header decides whether the Type column is present; without a header the
// second field tells them apart. Mount points may contain spaces: the mount is
// everything after the fixed columns.
func ParseDF(out string) ([]Mount, error) {
	var mounts []Mount
	prefix := 0 // fixed fields before the mount point; 0 = not known yet
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if strings.EqualFold(f[0], "Filesystem") {
			prefix = 5
			for _, x := range f {
				if strings.EqualFold(x, "Type") {
					prefix = 6
					break
				}
			}
			continue
		}
		if prefix == 0 {
			prefix = 5
			if len(f) >= 7 && !dfNumber(f[1]) {
				prefix = 6
			}
		}
		if len(f) <= prefix {
			continue
		}
		sizeKB, ok := dfInt(f[prefix-4])
		if !ok {
			continue
		}
		used, ok := dfInt(f[prefix-3])
		if !ok {
			continue
		}
		avail, ok := dfInt(f[prefix-2])
		if !ok {
			continue
		}
		m := Mount{
			Source:     f[0],
			Mountpoint: strings.Join(f[prefix:], " "),
			Size:       sizeKB * 1024,
			Used:       used * 1024,
			Avail:      avail * 1024,
			UsePct:     dfPercent(f[prefix-1]),
		}
		if prefix == 6 {
			m.FSType = f[1]
		}
		mounts = append(mounts, m)
	}
	return mounts, nil
}

// ParseDFInodes parses `df -Pi` into inode-use percentages keyed by mount
// point. Lines that do not carry a percentage are skipped.
func ParseDFInodes(out string) (map[string]int, error) {
	inodes := make(map[string]int)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.EqualFold(f[0], "Filesystem") {
			continue
		}
		// Filesystem Inodes IUsed IFree IUse% Mounted on
		if len(f) < 6 {
			continue
		}
		inodes[strings.Join(f[5:], " ")] = dfPercent(f[4])
	}
	return inodes, nil
}

// ParseMounts parses the two df samples of the mount batch and joins inode use
// onto the space rows. A mount point missing from the inode sample keeps
// IUsePct at zero.
func ParseMounts(space, inodes string) ([]Mount, error) {
	mounts, err := ParseDF(space)
	if err != nil {
		return nil, err
	}
	pcts, err := ParseDFInodes(inodes)
	if err != nil {
		return nil, err
	}
	for i := range mounts {
		if p, ok := pcts[mounts[i].Mountpoint]; ok {
			mounts[i].IUsePct = p
		}
	}
	return mounts, nil
}

// isPseudoFS reports whether a mount type is one the mount kind hides by
// default: virtual filesystems and image mounts.
func isPseudoFS(fstype string) bool {
	switch strings.ToLower(fstype) {
	case "tmpfs", "devtmpfs", "overlay", "squashfs":
		return true
	default:
		return false
	}
}

// FilterMounts drops the pseudo filesystems unless all is set.
func FilterMounts(mounts []Mount, all bool) []Mount {
	if all {
		return mounts
	}
	kept := mounts[:0]
	for _, m := range mounts {
		if isPseudoFS(m.FSType) {
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// dfInt parses one df number. Thousands separators are accepted; anything else
// reports false.
func dfInt(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// dfNumber reports whether s parses as a df number.
func dfNumber(s string) bool {
	_, ok := dfInt(s)
	return ok
}

// dfPercent parses a df Capacity/IUse% cell ("95%", "1%").
func dfPercent(s string) int {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Disk is one flattened row of `lsblk -J`: a disk, partition, rom or lvm
// device. Depth is the nesting level inside the lsblk tree, used to indent
// children in the name column.
type Disk struct {
	Name        string
	Type        string
	Size        int64
	FSType      string
	Mountpoints []string
	Rota        bool
	ReadOnly    bool
	Model       string
	Depth       int
}

// IndentedName renders the name with two spaces per tree level.
func (d Disk) IndentedName() string {
	return strings.Repeat("  ", d.Depth) + d.Name
}

// Mountpoint returns the first mount point, or "" for an unmounted device.
func (d Disk) Mountpoint() string {
	if len(d.Mountpoints) == 0 {
		return ""
	}
	return d.Mountpoints[0]
}

// diskSize accepts both numbers and quoted numbers: util-linux prints sizes as
// strings on some versions and numbers on others.
type diskSize int64

func (d *diskSize) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*d = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		*d = 0
		return nil
	}
	*d = diskSize(n)
	return nil
}

// diskBool accepts true/false, 1/0 and their quoted forms.
type diskBool bool

func (d *diskBool) UnmarshalJSON(b []byte) error {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(string(b)), `"`)) {
	case "true", "1", "yes":
		*d = true
	default:
		*d = false
	}
	return nil
}

// lsblkNode is one node of the lsblk JSON tree.
type lsblkNode struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Size        diskSize    `json:"size"`
	FSType      string      `json:"fstype"`
	Mountpoint  *string     `json:"mountpoint"`
	Mountpoints []string    `json:"mountpoints"`
	Rota        diskBool    `json:"rota"`
	RO          diskBool    `json:"ro"`
	Model       string      `json:"model"`
	Children    []lsblkNode `json:"children"`
}

// mountpoints normalizes the two shapes lsblk uses for mount points: a single
// "mountpoint" (which may be null) or the newer "mountpoints" array.
func (n lsblkNode) mountpoints() []string {
	var out []string
	for _, m := range n.Mountpoints {
		if m != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 && n.Mountpoint != nil && *n.Mountpoint != "" {
		out = append(out, *n.Mountpoint)
	}
	return out
}

// ParseLsblk parses `lsblk -J -b -o …` and flattens the tree depth-first with
// Depth set. Unknown fields are ignored; empty output yields no rows; malformed
// JSON is an errs.Parse error.
func ParseLsblk(out string) ([]Disk, error) {
	s := strings.TrimSpace(out)
	if s == "" {
		return nil, nil
	}
	var doc struct {
		Blockdevices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, errs.New(errs.Parse, diskProbeLsblk, "invalid json: "+err.Error())
	}
	var disks []Disk
	var walk func(n lsblkNode, depth int)
	walk = func(n lsblkNode, depth int) {
		disks = append(disks, Disk{
			Name:        n.Name,
			Type:        n.Type,
			Size:        int64(n.Size),
			FSType:      n.FSType,
			Mountpoints: n.mountpoints(),
			Rota:        bool(n.Rota),
			ReadOnly:    bool(n.RO),
			Model:       n.Model,
			Depth:       depth,
		})
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	for _, n := range doc.Blockdevices {
		walk(n, 0)
	}
	return disks, nil
}

// DiskStat is one device's monotonic counters from /proc/diskstats.
type DiskStat struct {
	Reads   uint64 // field 4: reads completed
	Writes  uint64 // field 8: writes completed
	IOTicks uint64 // field 13: milliseconds spent doing IO
}

// DiskRate is the per-second rate and utilisation of one device over the
// sampling interval.
type DiskRate struct {
	RPS  float64 // reads per second
	WPS  float64 // writes per second
	Util float64 // percent of the interval spent doing IO
}

// ParseDiskstats parses /proc/diskstats, keeping the last value seen for each
// device name. Lines with fewer than thirteen fields are skipped.
func ParseDiskstats(out string) (map[string]DiskStat, error) {
	stats := make(map[string]DiskStat)
	for _, line := range strings.Split(out, "\n") {
		name, st, ok := parseDiskstatLine(line)
		if !ok {
			continue
		}
		stats[name] = st
	}
	return stats, nil
}

// ParseDiskstatsSamples splits the batch's two /proc/diskstats copies into
// samples. A device name that repeats starts a new sample, which is how the two
// `cat`s are told apart without an extra marker.
func ParseDiskstatsSamples(out string) []map[string]DiskStat {
	var samples []map[string]DiskStat
	cur := make(map[string]DiskStat)
	flush := func() {
		if len(cur) > 0 {
			samples = append(samples, cur)
			cur = make(map[string]DiskStat)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		name, st, ok := parseDiskstatLine(line)
		if !ok {
			continue
		}
		if _, dup := cur[name]; dup {
			flush()
		}
		cur[name] = st
	}
	flush()
	return samples
}

// parseDiskstatLine parses one /proc/diskstats line.
func parseDiskstatLine(line string) (string, DiskStat, bool) {
	f := strings.Fields(line)
	if len(f) < 13 {
		return "", DiskStat{}, false
	}
	reads, err := strconv.ParseUint(f[3], 10, 64)
	if err != nil {
		return "", DiskStat{}, false
	}
	writes, err := strconv.ParseUint(f[7], 10, 64)
	if err != nil {
		return "", DiskStat{}, false
	}
	ticks, err := strconv.ParseUint(f[12], 10, 64)
	if err != nil {
		return "", DiskStat{}, false
	}
	return f[2], DiskStat{Reads: reads, Writes: writes, IOTicks: ticks}, true
}

// ParseDiskstatsPair splits the two samples of one batch section and turns them
// into rates over interval.
func ParseDiskstatsPair(out string, interval time.Duration) (map[string]DiskRate, error) {
	samples := ParseDiskstatsSamples(out)
	if len(samples) < 2 {
		return nil, errs.New(errs.Parse, diskProbeDiskstats, "need two samples")
	}
	return DiskRates(samples[0], samples[1], interval), nil
}

// DiskRates computes per-device rates from two counter snapshots. Devices
// without a baseline are omitted; a counter that went backwards (device
// re-plugged, counter reset) contributes zero delta instead of a huge rate.
func DiskRates(before, after map[string]DiskStat, interval time.Duration) map[string]DiskRate {
	if interval <= 0 {
		return nil
	}
	secs := interval.Seconds()
	msecs := float64(interval.Milliseconds())
	out := make(map[string]DiskRate, len(after))
	for name, a := range after {
		b, ok := before[name]
		if !ok {
			continue
		}
		r := DiskRate{
			RPS:  float64(diskXDelta(a.Reads, b.Reads)) / secs,
			WPS:  float64(diskXDelta(a.Writes, b.Writes)) / secs,
			Util: float64(diskXDelta(a.IOTicks, b.IOTicks)) / msecs * 100,
		}
		if r.Util > 100 {
			r.Util = 100
		}
		out[name] = r
	}
	return out
}

// diskXDelta is after − before, or 0 when the counter went backwards.
func diskXDelta(after, before uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}
