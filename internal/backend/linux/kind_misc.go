package linux

import (
	"context"
	"sort"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/fanout"
	"jumpr/internal/probe"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
)

const (
	// pkgTimeout bounds the package list. `rpm -qa` on a host with thousands of
	// packages is slow, so the kind gets its own budget.
	pkgTimeout = 60 * time.Second
	// kernelLogLimit is how many kernel records the kernel kind asks for.
	kernelLogLimit = 300
	// kernelLogPriority is the lowest-severity record the kernel kind keeps
	// (4 = warning and above).
	kernelLogPriority = 4
)

// cgroupRows builds one row per cgroup of cgtop's second iteration, biggest
// memory first. A cell cgtop printed as "-" stays nil.
func (b *Backend) cgroupRows(host resource.HostID, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secCgroup]; e != nil {
		return nil, e
	}
	cgs, err := ParseCgtop(sec[secCgroup].Stdout)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cgs, func(i, j int) bool {
		if cgs[i].HaveMemory != cgs[j].HaveMemory {
			return cgs[i].HaveMemory
		}
		if cgs[i].Memory != cgs[j].Memory {
			return cgs[i].Memory > cgs[j].Memory
		}
		return cgs[i].Path < cgs[j].Path
	})

	out := make([]resource.Resource, 0, len(cgs))
	for _, cg := range cgs {
		if cg.Path == "" {
			continue
		}
		fields := map[string]any{
			"host":   string(host),
			"cgroup": cg.Path,
			"tasks":  cg.Tasks,
			"cpu":    cg.CPU,
		}
		if cg.HaveMemory {
			fields["memory"] = int64(cg.Memory)
		}
		if cg.HaveInput {
			fields["input"] = int64(cg.Input)
		}
		if cg.HaveOutput {
			fields["output"] = int64(cg.Output)
		}
		labels := b.labels(host)
		applyDegraded(labels, nil, retried)
		out = append(out, resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: KindCgroup, Host: host, Name: cg.Path},
			Fields:    fields,
			Labels:    labels,
			Status:    resource.StatusUnknown,
			Raw:       cg,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

// pkgRows builds one row per installed package, sorted by name. rpm rows carry
// their install time; deb rows leave the column nil.
func (b *Backend) pkgRows(host resource.HostID, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secPkg]; e != nil {
		return nil, e
	}
	pkgs, err := ParsePkg(sec[secPkg].Stdout)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(pkgs, func(i, j int) bool {
		if pkgs[i].Name != pkgs[j].Name {
			return pkgs[i].Name < pkgs[j].Name
		}
		return pkgs[i].Arch < pkgs[j].Arch
	})

	out := make([]resource.Resource, 0, len(pkgs))
	seen := make(map[string]int, len(pkgs))
	for _, p := range pkgs {
		if p.Name == "" {
			continue
		}
		name := p.Name
		seen[name]++
		if n := seen[name]; n > 1 && p.Arch != "" {
			// Multi-arch hosts list one name twice; the Ref name must stay
			// unique.
			name = p.Name + "." + p.Arch
		}
		fields := map[string]any{
			"host":    string(host),
			"name":    p.Name,
			"version": p.Version,
			"arch":    p.Arch,
		}
		if !p.Installed.IsZero() {
			fields["installed"] = p.Installed
		}
		labels := b.labels(host)
		applyDegraded(labels, nil, retried)
		out = append(out, resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: KindPkg, Host: host, Name: name},
			Fields:    fields,
			Labels:    labels,
			Status:    resource.StatusUnknown,
			Raw:       p,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

// listKernel runs one kernel-only journalctl per host and emits its records as
// kernel rows. It reuses the journal parser and the journal row shape; only the
// query (and the sudo) differs.
func (b *Backend) listKernel(ctx context.Context, out chan<- backend.Result, hosts []resource.HostID, q resource.Query) {
	defer close(out)
	cmd := transport.Command{
		Script: JournalCommand(JournalOptions{
			Kernel:      true,
			MaxPriority: kernelLogPriority,
			Limit:       kernelLogLimit,
			Window:      q.Window,
			Now:         b.now(),
			Sudo:        true,
		}),
		Sudo:    true,
		Timeout: b.timeoutDuration(),
	}
	total, done := len(hosts), 0
	for hr := range fanout.Run(ctx, b.exec, hosts, cmd, fanout.Options{Concurrency: b.concurrency}) {
		done++
		res := backend.Result{Host: hr.Host, Done: done, Total: total}
		if hr.Err != nil {
			b.record(true)
			res.Err = hr.Err
		} else if err := journalErr(hr.Host, hr.Result); err != nil {
			b.record(false)
			res.Err = err
		} else {
			b.record(false)
			entries, _ := ParseJournal(hr.Result.Stdout)
			items := make([]resource.Resource, 0, len(entries))
			for _, e := range entries {
				row := e.Resource(hr.Host)
				row.Ref.Kind = KindKernel
				items = append(items, row)
			}
			res.Items = items
		}
		select {
		case <-ctx.Done():
			return
		case out <- res:
		}
	}
}

// kindTimeout is the exec timeout of one kind. Only the package list has its
// own budget; everything else uses the backend's.
func (b *Backend) kindTimeout(kind resource.KindName) time.Duration {
	if kind == KindPkg {
		return pkgTimeout
	}
	return b.timeoutDuration()
}

// ExecReadOnly runs one already-quoted read-only command on hosts through the
// backend's executor and returns one result per host. It is the narrow seam the
// TUI's file viewer and pkg probe use: the caller builds the command text with
// internal/shq, and the backend supplies the executor, the concurrency limit and
// the timeout. A zero command timeout becomes the backend default.
func (b *Backend) ExecReadOnly(ctx context.Context, hosts []resource.HostID, cmd transport.Command) <-chan transport.HostResult {
	if cmd.Timeout <= 0 {
		cmd.Timeout = b.timeoutDuration()
	}
	if b.exec == nil {
		out := make(chan transport.HostResult)
		close(out)
		return out
	}
	return fanout.Run(ctx, b.exec, hosts, cmd, fanout.Options{Concurrency: b.concurrency})
}
