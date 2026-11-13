package linux

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"jumper/internal/errs"
	"jumper/internal/probe"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
)

const (
	// mountUsageTimeout bounds the du drill-in. A huge tree is slow; the call is
	// cancelled when the view closes because it runs on the view's context.
	mountUsageTimeout = 20 * time.Second
	// mountUsageTail keeps the largest entries, biggest first, capped at 25.
	mountUsageTail = " | sort -rh | head -25"
)

// mountRows builds one row per real mounted filesystem. A failed space section
// fails the host; a failed inode section only drops the iuse column and marks
// the rows degraded.
func (b *Backend) mountRows(host resource.HostID, q resource.Query, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secDF]; e != nil {
		return nil, e
	}
	mounts, err := ParseDF(sec[secDF].Stdout)
	if err != nil {
		return nil, err
	}

	degraded := copyErrs(retried)
	inodesOK := true
	inodes := map[string]int{}
	if e := bad[secDFInode]; e != nil {
		inodesOK = false
		degraded[secDFInode] = e
	} else if inodes, err = ParseDFInodes(sec[secDFInode].Stdout); err != nil {
		inodesOK = false
		degraded[secDFInode] = err
	}

	mounts = FilterMounts(mounts, argsEnabled(q.Args["all"]))
	sort.SliceStable(mounts, func(i, j int) bool { return mounts[i].Mountpoint < mounts[j].Mountpoint })

	out := make([]resource.Resource, 0, len(mounts))
	for _, m := range mounts {
		fields := map[string]any{
			"host":   string(host),
			"mount":  m.Mountpoint,
			"fstype": m.FSType,
			"size":   m.Size,
			"used":   m.Used,
			"avail":  m.Avail,
			"use":    int64(m.UsePct),
			"source": m.Source,
		}
		status := m.Status()
		if inodesOK {
			pct := inodes[m.Mountpoint]
			fields["iuse"] = int64(pct)
			status = mountStatus(m.UsePct, pct)
		}
		labels := b.labels(host)
		applyDegraded(labels, nil, degraded)
		out = append(out, resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: KindMount, Host: host, Name: m.Mountpoint},
			Fields:    fields,
			Labels:    labels,
			Status:    status,
			Raw:       m,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

// diskRows builds one row per lsblk node in tree order. Block devices cannot be
// listed without lsblk; a failed diskstats section only drops the rate columns
// and marks the rows degraded.
func (b *Backend) diskRows(host resource.HostID, q resource.Query, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secLsblk]; e != nil {
		return nil, e
	}
	disks, err := ParseLsblk(sec[secLsblk].Stdout)
	if err != nil {
		return nil, err
	}

	degraded := copyErrs(retried)
	var rates map[string]DiskRate
	if e := bad[secDiskstats]; e != nil {
		degraded[secDiskstats] = e
	} else if rates, err = ParseDiskstatsPair(sec[secDiskstats].Stdout, time.Second); err != nil {
		degraded[secDiskstats] = err
	}

	out := make([]resource.Resource, 0, len(disks))
	for _, d := range disks {
		fields := map[string]any{
			"host":   string(host),
			"name":   d.IndentedName(),
			"type":   d.Type,
			"size":   d.Size,
			"fstype": d.FSType,
			"mount":  d.Mountpoint(),
			"rota":   d.Rota,
			"ro":     d.ReadOnly,
			"model":  d.Model,
		}
		if r, ok := rates[d.Name]; ok {
			fields["r/s"] = r.RPS
			fields["w/s"] = r.WPS
			fields["util"] = r.Util
		}
		labels := b.labels(host)
		applyDegraded(labels, nil, degraded)
		out = append(out, resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: KindDisk, Host: host, Name: d.Name},
			Fields:    fields,
			Labels:    labels,
			Status:    resource.StatusUnknown,
			Raw:       d,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

// MountUsageCommand is the one-shot drill-in for a mount point: the largest
// entries of the tree, biggest first, capped at 25.
func MountUsageCommand(mount string) transport.Command {
	return transport.Command{
		Script:  "du -xh --max-depth=1 " + shq.Quote(mount) + mountUsageTail,
		Sudo:    true,
		Timeout: mountUsageTimeout,
	}
}

// MountUsage runs the du drill-in for one mount point of one host. It is a
// single exec on the view's context, never a fan-out, so closing the view
// cancels it.
func (b *Backend) MountUsage(ctx context.Context, host resource.HostID, mount string) ([]string, error) {
	mount = strings.TrimSpace(mount)
	if !strings.HasPrefix(mount, "/") {
		return nil, errs.New(errs.Config, "linux.MountUsage", "mount must be an absolute path")
	}
	host = resource.NormalizeHostID(string(host))
	if host == "" {
		return nil, errs.New(errs.Config, "linux.MountUsage", "ref needs a host")
	}
	if b.exec == nil {
		return nil, errs.New(errs.Config, "linux.MountUsage", "no transport executor configured")
	}
	cctx, cancel := context.WithTimeout(ctx, mountUsageTimeout)
	defer cancel()
	res, err := b.exec.Exec(cctx, host, MountUsageCommand(mount))
	if err != nil {
		return nil, err
	}
	if res.RC != 0 {
		if e := shq.Classify(res.RC, string(res.Stderr), true); e != nil {
			return nil, e
		}
		return nil, errs.New(errs.Remote, "linux.MountUsage", fmt.Sprintf("du exit %d", res.RC)).WithHost(string(host))
	}
	out := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	if len(out) == 1 && out[0] == "" {
		return nil, nil
	}
	return out, nil
}

// copyErrs returns a fresh copy of a section-error map, so a row builder can
// add to it without touching the caller's.
func copyErrs(in map[string]error) map[string]error {
	out := make(map[string]error, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// argsEnabled reports whether a boolean query argument is on. Unset and "0"
// are off; anything else the user typed is treated as on.
func argsEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no":
		return false
	default:
		return true
	}
}
