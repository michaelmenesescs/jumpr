// Package group collapses per-host command output into groups of identical results.
package group

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

// Group is a set of hosts that produced the same normalized output and exit code.
type Group struct {
	Key    string // hash of the normalized output + rc
	Output string // representative output (first seen)
	RC     int
	Hosts  []resource.HostID // sorted
}

// HostErr is a transport failure for one host.
type HostErr struct {
	Host resource.HostID
	Err  error
}

// Summary is a snapshot of a Grouper.
type Summary struct {
	Total  int                     // hosts expected
	Done   int                     // hosts reported (ok + errors)
	Groups []Group                 // largest first; Groups[0] is the majority. Ties: lexical by Key.
	Errors map[errs.Kind][]HostErr // transport failures, not part of any group
}

// Majority returns the largest group.
func (s Summary) Majority() (Group, bool) {
	if len(s.Groups) == 0 {
		return Group{}, false
	}
	return s.Groups[0], true
}

// Differing returns every group except the majority.
func (s Summary) Differing() []Group {
	if len(s.Groups) <= 1 {
		return nil
	}
	return append([]Group(nil), s.Groups[1:]...)
}

// Headline renders "1,481 identical · 19 differ · 4 errors". Parts with a zero count are omitted,
// except "identical". While Done < Total it is prefixed with "812/1,512 · ".
func (s Summary) Headline() string {
	if s.Done == 0 && s.Total > 0 {
		return "0/" + thousands(s.Total)
	}
	identical, differ := 0, 0
	if m, ok := s.Majority(); ok {
		identical = len(m.Hosts)
	}
	for _, g := range s.Differing() {
		differ += len(g.Hosts)
	}
	errCount := 0
	for _, l := range s.Errors {
		errCount += len(l)
	}
	parts := []string{thousands(identical) + " identical"}
	if differ > 0 {
		parts = append(parts, thousands(differ)+" differ")
	}
	if errCount > 0 {
		parts = append(parts, thousands(errCount)+" errors")
	}
	if s.Done < s.Total {
		parts = append([]string{thousands(s.Done) + "/" + thousands(s.Total)}, parts...)
	}
	return strings.Join(parts, " · ")
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Grouper accumulates results. Not safe for concurrent use; feed it from one goroutine.
type Grouper struct {
	total     int
	normalize func(string) string
	groups    map[string]*Group
	errors    map[errs.Kind][]HostErr
}

// NewGrouper: normalize may be nil. It is applied to output before hashing (e.g. strip timestamps).
func NewGrouper(total int, normalize func(string) string) *Grouper {
	if normalize == nil {
		normalize = defaultNormalize
	}
	return &Grouper{
		total:     total,
		normalize: normalize,
		groups:    map[string]*Group{},
		errors:    map[errs.Kind][]HostErr{},
	}
}

func defaultNormalize(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r\f\v")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// Add records a host's output and exit code.
func (g *Grouper) Add(host resource.HostID, output string, rc int) {
	sum := sha256.Sum256([]byte(g.normalize(output) + "\x00" + strconv.Itoa(rc)))
	key := hex.EncodeToString(sum[:])
	grp, ok := g.groups[key]
	if !ok {
		grp = &Group{Key: key, Output: output, RC: rc}
		g.groups[key] = grp
	}
	grp.Hosts = append(grp.Hosts, host)
}

// AddError records a transport failure for a host.
func (g *Grouper) AddError(host resource.HostID, err error) {
	k := errs.KindOf(err)
	g.errors[k] = append(g.errors[k], HostErr{Host: host, Err: err})
}

// Summary returns a deterministic snapshot.
func (g *Grouper) Summary() Summary {
	s := Summary{Total: g.total, Errors: map[errs.Kind][]HostErr{}}
	for _, grp := range g.groups {
		c := *grp
		c.Hosts = append([]resource.HostID(nil), grp.Hosts...)
		sort.Slice(c.Hosts, func(i, j int) bool { return c.Hosts[i] < c.Hosts[j] })
		s.Done += len(c.Hosts)
		s.Groups = append(s.Groups, c)
	}
	sort.Slice(s.Groups, func(i, j int) bool {
		a, b := s.Groups[i], s.Groups[j]
		if len(a.Hosts) != len(b.Hosts) {
			return len(a.Hosts) > len(b.Hosts)
		}
		return a.Key < b.Key
	})
	for k, l := range g.errors {
		c := append([]HostErr(nil), l...)
		sort.SliceStable(c, func(i, j int) bool { return c[i].Host < c[j].Host })
		s.Errors[k] = c
		s.Done += len(c)
	}
	return s
}
