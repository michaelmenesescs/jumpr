package linux

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/fanout"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
)

// journalOp is the Op of every error built here.
const journalOp = "linux.journal"

// journalOptions turns query arguments into one journalctl invocation. The
// keys are the frozen ones of the task: unit, priority (0–7), limit, kernel.
func (b *Backend) journalOptions(q resource.Query) JournalOptions {
	o := JournalOptions{
		Unit:   strings.TrimSpace(q.Args["unit"]),
		Window: q.Window,
		Now:    b.now(),
		Sudo:   true,
	}
	if v := strings.TrimSpace(q.Args["priority"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 7 {
			o.MaxPriority = n
		}
	}
	if v := strings.TrimSpace(q.Args["limit"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			o.Limit = n
		}
	}
	switch strings.TrimSpace(q.Args["kernel"]) {
	case "1", "true", "yes":
		o.Kernel = true
	}
	return o
}

// journalErr turns a journalctl exit code into an error, or nil when the command
// succeeded. Remote stderr is classified by internal/shq, never parsed here.
func journalErr(host resource.HostID, res transport.ExecResult) error {
	if res.RC == 0 {
		return nil
	}
	if e := shq.Classify(res.RC, string(res.Stderr), true); e != nil {
		if typed, ok := e.(*errs.Error); ok {
			return typed.WithHost(string(host))
		}
		return errs.Wrap(errs.Remote, journalOp, e).WithHost(string(host))
	}
	return errs.New(errs.Remote, journalOp, fmt.Sprintf("journalctl exit %d", res.RC)).WithHost(string(host))
}

// listJournal runs one journalctl per host through the fan-out and emits the
// parsed entries as resources, oldest first. It closes out when every host has
// reported or ctx is done.
func (b *Backend) listJournal(ctx context.Context, out chan<- backend.Result, hosts []resource.HostID, q resource.Query) {
	defer close(out)
	o := b.journalOptions(q)
	cmd := transport.Command{
		Script:  JournalCommand(o),
		Sudo:    o.Sudo,
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
			// journalctl prints oldest first; a single host's order is kept.
			items := make([]resource.Resource, 0, len(entries))
			for _, e := range entries {
				items = append(items, e.Resource(hr.Host))
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

// FollowJournal starts journalctl -f on one host and streams parsed entries.
// The channel closes when ctx is canceled or the command ends; lines that do not
// parse are skipped. The caller must cancel ctx (or Close the view) to stop it.
func (b *Backend) FollowJournal(ctx context.Context, host resource.HostID, o JournalOptions) (<-chan Entry, error) {
	if b.stream == nil {
		return nil, errs.New(errs.Config, "linux.FollowJournal", "no transport streamer configured")
	}
	host = resource.NormalizeHostID(string(host))
	if host == "" {
		return nil, errs.New(errs.Config, "linux.FollowJournal", "ref needs a host")
	}
	o.Follow = true
	if o.Now.IsZero() {
		o.Now = b.now()
	}
	// A follow runs until it is canceled, so it carries no exec timeout.
	st, err := b.stream.Start(ctx, host, transport.Command{
		Script: JournalCommand(o),
		Sudo:   o.Sudo,
	})
	if err != nil {
		return nil, err
	}

	out := make(chan Entry)
	go func() {
		defer close(out)
		lines := st.Lines()
		for {
			select {
			case <-ctx.Done():
				drainLines(st, lines)
				return
			case line, ok := <-lines:
				if !ok {
					return
				}
				e, err := ParseJournalLine(line)
				if err != nil {
					continue
				}
				select {
				case out <- e:
				case <-ctx.Done():
					drainLines(st, lines)
					return
				}
			}
		}
	}()
	return out, nil
}

// drainLines closes the stream and consumes what is left, so the producer is
// never left blocked on a send nobody reads.
func drainLines(st transport.Stream, lines <-chan []byte) {
	_ = st.Close()
	for range lines {
	}
}

// Resource converts one journal entry into a row. The view uses it for followed
// entries too, so List and follow render identically.
func (e Entry) Resource(host resource.HostID) resource.Resource {
	host = resource.NormalizeHostID(string(host))
	name := e.Cursor
	if name == "" {
		name = fmt.Sprintf("%s@%d", e.Source, e.Time.UnixMicro())
	}
	fields := map[string]any{
		"host":    string(host),
		"time":    e.Time,
		"pri":     PriorityName(e.Priority),
		"source":  e.Source,
		"message": e.Message,
	}
	if e.Unit != "" {
		fields["unit"] = e.Unit
	}
	return resource.Resource{
		Ref:       resource.Ref{Backend: backendName, Kind: KindJournal, Host: host, Name: name},
		Fields:    fields,
		Labels:    map[string]string{"host": string(host)},
		Status:    PriorityStatus(e.Priority),
		Raw:       e,
		FetchedAt: e.Time,
	}
}
