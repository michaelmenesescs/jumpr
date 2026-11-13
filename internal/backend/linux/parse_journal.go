package linux

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/shq"
)

// Entry is one parsed journal record.
type Entry struct {
	Time     time.Time
	Priority int
	Source   string
	PID      string
	Unit     string
	Message  string
	Cursor   string
}

// JournalOptions describes one journalctl invocation. Sudo is carried by the
// caller (shq.Wrap), never by the command string. MaxPriority 0 means "unset"
// and is treated as 7 (every priority).
type JournalOptions struct {
	Unit        string
	MaxPriority int
	Window      resource.TimeWindow
	Now         time.Time
	Limit       int
	Follow      bool
	Kernel      bool
	Sudo        bool
}

// ParseJournalLine parses one journalctl -o json line.
func ParseJournalLine(line []byte) (Entry, error) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Entry{}, errs.New(errs.Parse, "journal.parse", "empty line")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return Entry{}, errs.Wrap(errs.Parse, "journal.parse", err)
	}

	usec, err := jrnlTimestamp(fields["__REALTIME_TIMESTAMP"])
	if err != nil {
		return Entry{}, err
	}

	e := Entry{
		Time:     time.UnixMicro(usec).UTC(),
		Priority: jrnlPriority(fields["PRIORITY"]),
		Source:   jrnlSource(fields),
		PID:      jrnlFirstString(fields["_PID"], fields["PID"]),
		Unit:     jrnlFirstString(fields["_SYSTEMD_UNIT"], fields["UNIT"]),
		Message:  strings.TrimRight(jrnlMessage(fields["MESSAGE"]), "\r\n"),
		Cursor:   jrnlString(fields["__CURSOR"]),
	}
	return e, nil
}

// ParseJournal parses every line of out. It returns the entries in order and
// the number of lines that did not parse; blank lines are ignored silently.
func ParseJournal(out []byte) ([]Entry, int) {
	var (
		entries []Entry
		skipped int
	)
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		e, err := ParseJournalLine(line)
		if err != nil {
			skipped++
			continue
		}
		entries = append(entries, e)
	}
	return entries, skipped
}

// PriorityName is the syslog name for a journald priority; unknown values
// report "info".
func PriorityName(p int) string {
	names := [...]string{"emerg", "alert", "crit", "err", "warn", "notice", "info", "debug"}
	if p < 0 || p >= len(names) {
		return "info"
	}
	return names[p]
}

// PriorityStatus maps a journald priority to a resource status: 0–3 critical,
// 4 warning, everything else OK.
func PriorityStatus(p int) resource.Status {
	switch {
	case p <= 3:
		return resource.StatusCrit
	case p == 4:
		return resource.StatusWarn
	default:
		return resource.StatusOK
	}
}

// JournalCommand builds the remote journalctl line with shq. The output is the
// raw command (no sh -c wrapper, no sudo); the caller wraps it.
func JournalCommand(o JournalOptions) string {
	args := []string{"journalctl", "-o", "json", "--no-pager"}
	if o.Unit != "" {
		args = append(args, "-u", o.Unit)
	}
	if o.MaxPriority > 0 && o.MaxPriority < 7 {
		args = append(args, "-p", strconv.Itoa(o.MaxPriority))
	}
	from, to := o.Window.Bounds(o.Now)
	if !from.IsZero() {
		args = append(args, "--since", "@"+strconv.FormatInt(from.Unix(), 10))
		if !o.Follow && !to.IsZero() {
			args = append(args, "--until", "@"+strconv.FormatInt(to.Unix(), 10))
		}
	}
	limit := o.Limit
	switch {
	case o.Follow:
		limit = 50
	case limit <= 0:
		limit = 500
	}
	args = append(args, "-n", strconv.Itoa(limit))
	if o.Follow {
		args = append(args, "-f")
	}
	if o.Kernel {
		args = append(args, "-k")
	}
	return shq.Join(args...)
}

// jrnlTimestamp decodes __REALTIME_TIMESTAMP (microseconds since the epoch) as
// a decimal string, a JSON number or a single-element array.
func jrnlTimestamp(raw json.RawMessage) (int64, error) {
	s := jrnlString(raw)
	if s == "" {
		return 0, errs.New(errs.Parse, "journal.parse", "missing __REALTIME_TIMESTAMP")
	}
	usec, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errs.Wrap(errs.Parse, "journal.parse", err)
	}
	return usec, nil
}

// jrnlPriority reads PRIORITY, defaulting to 6 (info) when absent or invalid.
func jrnlPriority(raw json.RawMessage) int {
	p, err := strconv.Atoi(jrnlString(raw))
	if err != nil || p < 0 || p > 7 {
		return 6
	}
	return p
}

// jrnlSource applies the SYSLOG_IDENTIFIER → _COMM → kernel → "?" fallback.
func jrnlSource(fields map[string]json.RawMessage) string {
	if s := jrnlString(fields["SYSLOG_IDENTIFIER"]); s != "" {
		return s
	}
	if s := jrnlString(fields["_COMM"]); s != "" {
		return s
	}
	if jrnlString(fields["_TRANSPORT"]) == "kernel" {
		return "kernel"
	}
	return "?"
}

// jrnlFirstString returns the first non-empty value among raw.
func jrnlFirstString(raw ...json.RawMessage) string {
	for _, r := range raw {
		if s := jrnlString(r); s != "" {
			return s
		}
	}
	return ""
}

// jrnlString decodes a field that may be a string, null, a number or an array
// of strings (repeated field: the first element wins).
func jrnlString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] == 'n' {
		return ""
	}
	if raw[0] == '[' {
		var parts []json.RawMessage
		if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
			return ""
		}
		return jrnlString(parts[0])
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// jrnlMessage decodes MESSAGE: a string, null, an array of strings (first
// wins) or an array of byte values for a non-UTF-8 payload, where invalid
// UTF-8 is replaced with U+FFFD.
func jrnlMessage(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return jrnlString(raw)
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return ""
	}
	if first := bytes.TrimSpace(parts[0]); len(first) > 0 && first[0] == '"' {
		return jrnlString(first)
	}
	var data []byte
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}
