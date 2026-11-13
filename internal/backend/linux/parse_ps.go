package linux

import (
	"path"
	"strconv"
	"strings"
	"time"
)

// Proc is one row of `ps -eo pid=,ppid=,user:32=,stat=,pcpu=,pmem=,rss=,etimes=,args=`.
type Proc struct {
	PID     int
	PPID    int
	User    string
	Stat    string
	CPU     float64
	Mem     float64
	RSSKB   int64
	Elapsed time.Duration
	Args    string
}

// psFields splits line into at most n whitespace-separated fields and returns
// the untouched remainder after the n-th field (leading whitespace trimmed).
// It returns a nil field slice when the line has fewer than n fields.
func psFields(line string, n int) ([]string, string) {
	fields := make([]string, 0, n)
	rest := line
	for len(fields) < n {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			return nil, ""
		}
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			fields = append(fields, rest)
			rest = ""
			break
		}
		fields = append(fields, rest[:i])
		rest = rest[i:]
	}
	if len(fields) < n {
		return nil, ""
	}
	return fields, rest
}

// ParsePS parses `ps -eo pid=,ppid=,user:32=,stat=,pcpu=,pmem=,rss=,etimes=,args=`
// output. The first eight fields are whitespace-separated; everything after
// them (trimmed) is Args. Lines that do not carry at least those eight valid
// fields are skipped and counted.
func ParsePS(out string) ([]Proc, int) {
	var procs []Proc
	skipped := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields, rest := psFields(line, 8)
		if fields == nil {
			skipped++
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			skipped++
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			skipped++
			continue
		}
		cpu, err := strconv.ParseFloat(fields[4], 64)
		if err != nil {
			skipped++
			continue
		}
		mem, err := strconv.ParseFloat(fields[5], 64)
		if err != nil {
			skipped++
			continue
		}
		rss, err := strconv.ParseInt(fields[6], 10, 64)
		if err != nil {
			skipped++
			continue
		}
		elapsed, err := strconv.Atoi(fields[7])
		if err != nil {
			skipped++
			continue
		}
		procs = append(procs, Proc{
			PID:     pid,
			PPID:    ppid,
			User:    fields[2],
			Stat:    fields[3],
			CPU:     cpu,
			Mem:     mem,
			RSSKB:   rss,
			Elapsed: time.Duration(elapsed) * time.Second,
			Args:    strings.TrimSpace(rest),
		})
	}
	return procs, skipped
}

// Name is the short process name: the bracketed text for a kernel thread
// (`[kworker/0:1]` -> `kworker/0:1`), otherwise the base name of the first
// word of Args with a trailing colon removed (`nginx: worker process` -> `nginx`).
func (p Proc) Name() string {
	args := strings.TrimSpace(p.Args)
	if args == "" {
		return ""
	}
	if strings.HasPrefix(args, "[") {
		if i := strings.IndexByte(args, ']'); i > 1 {
			return args[1:i]
		}
	}
	first := args
	if i := strings.IndexAny(first, " \t"); i >= 0 {
		first = first[:i]
	}
	name := path.Base(first)
	return strings.TrimSuffix(name, ":")
}
