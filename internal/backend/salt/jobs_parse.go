package salt

import (
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"jumpr/internal/errs"
)

// Operation names for job errors. BUILD-PLAN §4.1 uses dotted ops.
const (
	opListJob  = "salt.jobs.list_job"
	opListJobs = "salt.jobs.list_jobs"
	opActive   = "salt.jobs.active"
)

const (
	// startTimeLayout is jobs.list_job's StartTime format. The master reports
	// its own local time, so the value is kept as a naive local timestamp.
	startTimeLayout = "2006, Jan 02 15:04:05.000000"
	// jidLayout is the wall-clock prefix of a Salt job id.
	jidLayout = "20060102150405"
	// jidLen is the digit count of a Salt job id: YYYYMMDDhhmmss plus
	// microseconds.
	jidLen = 20
)

// JobInfo is one job as jobs.list_job describes it. Target is passed through
// untouched because Salt reports it either as a string or as a list.
type JobInfo struct {
	JID        string
	Function   string
	Arguments  []json.RawMessage
	Target     json.RawMessage
	TargetType string
	User       string
	Start      time.Time
	Minions    []string
	Result     map[string]MinionReturn
}

// MinionReturn is one minion's entry in JobInfo.Result.
type MinionReturn struct {
	Return  json.RawMessage
	Retcode int
	Success bool
}

// ActiveJob is one running job from jobs.active. Running lists the minions
// still executing, each with its process id on that minion.
type ActiveJob struct {
	Function   string
	Arguments  []json.RawMessage
	Target     json.RawMessage
	TargetType string
	User       string
	Running    []map[string]int
	Returned   []string
}

// JobSummary is one row of jobs.list_jobs.
type JobSummary struct {
	JID        string
	Function   string
	Arguments  []json.RawMessage
	Target     json.RawMessage
	TargetType string
	User       string
	Start      time.Time
}

// JIDTime returns the wall-clock time a job id encodes: YYYYMMDDhhmmss plus
// microseconds, read in the local zone. ok is false when jid is not a job id.
func JIDTime(jid string) (time.Time, bool) {
	if len(jid) != jidLen {
		return time.Time{}, false
	}
	for i := 0; i < len(jid); i++ {
		if jid[i] < '0' || jid[i] > '9' {
			return time.Time{}, false
		}
	}
	sec, err := time.ParseInLocation(jidLayout, jid[:14], time.Local)
	if err != nil {
		return time.Time{}, false
	}
	us, err := strconv.Atoi(jid[14:])
	if err != nil {
		return time.Time{}, false
	}
	return sec.Add(time.Duration(us) * time.Microsecond), true
}

// validJID reports whether jid looks like a Salt job id.
func validJID(jid string) bool {
	_, ok := JIDTime(jid)
	return ok
}

// listJobWire is the JSON shape of jobs.list_job. Error is raw because Salt
// has reported it both as a string and as an object.
type listJobWire struct {
	JID        string                     `json:"jid"`
	Function   string                     `json:"Function"`
	Arguments  []json.RawMessage          `json:"Arguments"`
	Target     json.RawMessage            `json:"Target"`
	TargetType string                     `json:"Target-type"`
	User       string                     `json:"User"`
	StartTime  string                     `json:"StartTime"`
	Minions    []string                   `json:"Minions"`
	Result     map[string]json.RawMessage `json:"Result"`
	Error      json.RawMessage            `json:"Error"`
}

// minionReturnWire is the JSON shape of one Result entry.
type minionReturnWire struct {
	Return  json.RawMessage `json:"return"`
	Retcode int             `json:"retcode"`
	Success bool            `json:"success"`
}

// activeJobWire is the JSON shape of one jobs.active entry.
type activeJobWire struct {
	Function   string            `json:"Function"`
	Arguments  []json.RawMessage `json:"Arguments"`
	Target     json.RawMessage   `json:"Target"`
	TargetType string            `json:"Target-type"`
	User       string            `json:"User"`
	Running    []map[string]int  `json:"Running"`
	Returned   []string          `json:"Returned"`
}

// jobSummaryWire is the JSON shape of one jobs.list_jobs entry.
type jobSummaryWire struct {
	Function   string            `json:"Function"`
	Arguments  []json.RawMessage `json:"Arguments"`
	Target     json.RawMessage   `json:"Target"`
	TargetType string            `json:"Target-type"`
	User       string            `json:"User"`
	StartTime  string            `json:"StartTime"`
}

// parseListJob decodes jobs.list_job output. An empty object or an object
// carrying "Error" means the master has no such job: errs.NotAvailable.
func parseListJob(b []byte) (JobInfo, error) {
	var w listJobWire
	if err := DecodeInto(b, &w); err != nil {
		return JobInfo{}, err
	}
	if msg, ok := errorField(w.Error); ok {
		return JobInfo{}, errs.New(errs.NotAvailable, opListJob, msg)
	}
	info := JobInfo{
		JID:        w.JID,
		Function:   w.Function,
		Arguments:  w.Arguments,
		Target:     w.Target,
		TargetType: w.TargetType,
		User:       w.User,
		Start:      parseStartTime(w.StartTime),
		Minions:    w.Minions,
	}
	if len(w.Result) > 0 {
		info.Result = make(map[string]MinionReturn, len(w.Result))
		for minion, raw := range w.Result {
			info.Result[minion] = parseMinionReturn(raw)
		}
	}
	if info.JID == "" && info.Function == "" && len(info.Minions) == 0 && len(info.Result) == 0 {
		return JobInfo{}, errs.New(errs.NotAvailable, opListJob, "no such job")
	}
	return info, nil
}

// errorField returns the message of a non-empty "Error" field.
func errorField(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, s != ""
	}
	return string(raw), true
}

// parseMinionReturn decodes one Result entry. Output that is not an object is
// kept whole as the return value rather than dropped.
func parseMinionReturn(raw json.RawMessage) MinionReturn {
	mr := MinionReturn{Return: raw}
	var w minionReturnWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return mr
	}
	mr.Retcode = w.Retcode
	mr.Success = w.Success
	if len(w.Return) > 0 {
		mr.Return = w.Return
	}
	return mr
}

// parseStartTime parses a jobs StartTime. Unparsable values stay zero: a job
// with an odd timestamp is still worth showing.
func parseStartTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{startTimeLayout, "2006, Jan 02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseActive decodes jobs.active into one entry per job id.
func parseActive(b []byte) (map[string]ActiveJob, error) {
	var wire map[string]activeJobWire
	if err := DecodeInto(b, &wire); err != nil {
		return nil, err
	}
	out := make(map[string]ActiveJob, len(wire))
	for jid, w := range wire {
		out[jid] = ActiveJob{
			Function:   w.Function,
			Arguments:  w.Arguments,
			Target:     w.Target,
			TargetType: w.TargetType,
			User:       w.User,
			Running:    w.Running,
			Returned:   w.Returned,
		}
	}
	return out, nil
}

// parseListJobs decodes jobs.list_jobs and sorts it newest first.
func parseListJobs(b []byte) ([]JobSummary, error) {
	var wire map[string]jobSummaryWire
	if err := DecodeInto(b, &wire); err != nil {
		return nil, err
	}
	out := make([]JobSummary, 0, len(wire))
	for jid, w := range wire {
		start := parseStartTime(w.StartTime)
		if start.IsZero() {
			if t, ok := JIDTime(jid); ok {
				start = t
			}
		}
		out = append(out, JobSummary{
			JID:        jid,
			Function:   w.Function,
			Arguments:  w.Arguments,
			Target:     w.Target,
			TargetType: w.TargetType,
			User:       w.User,
			Start:      start,
		})
	}
	sort.SliceStable(out, func(i, k int) bool {
		a, b := out[i], out[k]
		if !a.Start.Equal(b.Start) {
			return a.Start.After(b.Start)
		}
		return a.JID > b.JID
	})
	return out, nil
}
