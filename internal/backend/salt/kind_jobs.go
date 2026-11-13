package salt

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"jumper/internal/resource"
)

// Job states as the jobs browser shows them.
const (
	JobRunning = "running"
	JobDone    = "done"
)

const (
	// jobDefaultWindow is the history window a query without a session
	// window asks for. jobs.list_jobs on a busy master is large, so the
	// window is always passed.
	jobDefaultWindow = time.Hour
	// jobTargetPreview is how many minions of a target list the compact
	// target keeps before counting the rest.
	jobTargetPreview = 2
)

// jobRow is one job accumulated from jobs.active and jobs.list_jobs.
type jobRow struct {
	jid      string
	function string
	target   json.RawMessage
	user     string
	start    time.Time
	running  bool
	returned int
	total    int
	// reported holds the minions that have answered so far; it is only
	// known for a running job (jobs.active).
	reported []string
}

// jobRows merges the running jobs with the history of the query window.
// Running jobs come first, then the rest newest first.
func (b *Backend) jobRows(ctx context.Context, q resource.Query) ([]resource.Resource, error) {
	now := b.now()
	w := q.Window
	if w.IsZero() {
		w = resource.TimeWindow{From: now.Add(-jobDefaultWindow), ToNow: true, Label: "-1h"}
	}

	var (
		activeErr, historyErr error
		running               map[string]ActiveJob
		past                  []JobSummary
	)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		running, activeErr = b.runner.Active(ctx)
	}()
	go func() {
		defer wg.Done()
		past, historyErr = b.runner.ListJobs(ctx, w, now)
	}()
	wg.Wait()
	if activeErr != nil {
		return nil, activeErr
	}
	if historyErr != nil {
		return nil, historyErr
	}

	rows := make(map[string]*jobRow, len(past)+len(running))
	for _, h := range past {
		rows[h.JID] = &jobRow{
			jid:      h.JID,
			function: h.Function,
			target:   h.Target,
			user:     h.User,
			start:    h.Start,
		}
	}
	for jid, a := range running {
		r := rows[jid]
		if r == nil {
			r = &jobRow{jid: jid}
			rows[jid] = r
		}
		r.function = a.Function
		r.target = a.Target
		r.user = a.User
		if r.start.IsZero() {
			if t, ok := JIDTime(jid); ok {
				r.start = t
			}
		}
		r.running = true
		r.reported = a.Returned
		r.total = len(a.Returned) + len(a.Running)
		r.returned = len(a.Returned)
	}

	minion := strings.TrimSpace(q.Args["minion"])
	fun := strings.TrimSpace(q.Args["fun"])

	out := make([]*jobRow, 0, len(rows))
	for _, r := range rows {
		if internalJob(r.function) {
			continue
		}
		if fun != "" && !strings.HasPrefix(r.function, fun) {
			continue
		}
		if minion != "" && !jobMentions(r, minion) {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, c := out[i], out[j]
		if a.running != c.running {
			return a.running
		}
		if !a.start.Equal(c.start) {
			return a.start.After(c.start)
		}
		return a.jid > c.jid
	})

	items := make([]resource.Resource, 0, len(out))
	for _, r := range out {
		items = append(items, b.jobResource(r))
	}
	return items, nil
}

// jobMentions reports whether the job's target list, its targets or the
// minions that already returned name minion. targets is json.RawMessage.
func jobMentions(r *jobRow, minion string) bool {
	if contains(r.reported, minion) {
		return true
	}
	return targetMentions(r.target, minion)
}

// targetMentions reports whether a Salt target names minion: a list holds it
// exactly, a glob matches it, a single name equals it.
func targetMentions(raw json.RawMessage, minion string) bool {
	if isNull(raw) {
		return false
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return contains(list, minion)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	s = strings.TrimSpace(s)
	if s == minion {
		return true
	}
	if ok, err := path.Match(s, minion); err == nil && ok {
		return true
	}
	return false
}

// internalJob reports whether a job is jumper's own polling noise rather than
// something an operator asked for.
func internalJob(fun string) bool {
	return strings.HasPrefix(fun, "runner.jobs.") || fun == "saltutil.find_job"
}

// compactTarget renders a Salt target for one cell: a list keeps its first
// two minions and counts the rest, a glob is shown as it was given.
func compactTarget(raw json.RawMessage) string {
	if isNull(raw) {
		return ""
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		switch {
		case len(list) == 0:
			return ""
		case len(list) <= jobTargetPreview:
			return strings.Join(list, ",")
		default:
			return fmt.Sprintf("%s (+%d)", strings.Join(list[:jobTargetPreview], ","), len(list)-jobTargetPreview)
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// jobResource is one jobs-browser row.
func (b *Backend) jobResource(r *jobRow) resource.Resource {
	state := JobDone
	st := resource.StatusOK
	if r.running {
		state, st = JobRunning, resource.StatusWarn
	}
	fields := map[string]any{
		"jid":      r.jid,
		"state":    state,
		"function": r.function,
	}
	if !r.start.IsZero() {
		fields["start"] = r.start
	}
	if target := compactTarget(r.target); target != "" {
		fields["target"] = target
	}
	if r.user != "" {
		fields["user"] = r.user
	}
	// History has no reliable return count, so the cell stays blank there.
	if r.running && r.total > 0 {
		fields["returned"] = fmt.Sprintf("%d/%d", r.returned, r.total)
	}
	return resource.Resource{
		Ref:       resource.Ref{Backend: backendName, Kind: KindJobs, Name: r.jid},
		Fields:    fields,
		Status:    st,
		FetchedAt: b.now(),
	}
}

// jobLinks are the pivots of one job row: the grouped job detail.
func (b *Backend) jobLinks(r resource.Resource) []resource.Link {
	jid := strings.TrimSpace(r.Ref.Name)
	if jid == "" {
		return nil
	}
	return []resource.Link{{
		Label: "job detail",
		Key:   "enter",
		To:    resource.Query{Kind: "salt-job-detail", Args: map[string]string{"jid": jid}},
	}}
}
