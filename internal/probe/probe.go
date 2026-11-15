// Package probe compiles several small read-only commands into one POSIX sh
// script (one round trip per host) and splits the reply back into per-section
// output, exit code and classified error. Sections are delimited by
// @@SITREP:<nonce>:<id>:B@@ / @@SITREP:<nonce>:<id>:E:<rc>@@ markers printed on
// both stdout and stderr so one lost stream does not lose the boundaries.
package probe

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

const markerPrefix = "@@SITREP:"

// Op names used on errors built here.
const opBatch = "probe.batch"

var (
	idRe    = regexp.MustCompile(`^[a-z0-9_.-]+$`)
	nonceRe = regexp.MustCompile(`^[0-9a-f]{8}$`)
)

// Section is one read-only command in a batch. ID is the marker name and must
// match ^[a-z0-9_.-]+$; Cmd is run as written (or under sudo -n when Sudo).
type Section struct {
	ID   string
	Cmd  string
	Sudo bool
}

// Batch is a compiled set of sections sharing one nonce (8 hex characters,
// random in production, fixed in tests).
type Batch struct {
	Nonce    string
	Sections []Section
}

// SectionResult is one section's outcome. Missing is set when the marker pair
// for the section never arrived (script died, output truncated).
type SectionResult struct {
	ID      string
	Stdout  string
	Stderr  string
	RC      int
	Err     error
	Missing bool
}

// NewBatch validates the nonce and sections. Duplicate or invalid IDs, an empty
// command, a heredoc and a malformed nonce are programmer errors: errs.Internal.
func NewBatch(nonce string, s ...Section) (Batch, error) {
	if !nonceRe.MatchString(nonce) {
		return Batch{}, errs.New(errs.Internal, opBatch, "nonce must be 8 hex characters")
	}
	seen := make(map[string]struct{}, len(s))
	for _, sec := range s {
		if !idRe.MatchString(sec.ID) {
			return Batch{}, errs.New(errs.Internal, opBatch, "invalid section id "+strconv.Quote(sec.ID))
		}
		if _, dup := seen[sec.ID]; dup {
			return Batch{}, errs.New(errs.Internal, opBatch, "duplicate section id "+strconv.Quote(sec.ID))
		}
		seen[sec.ID] = struct{}{}
		if strings.TrimSpace(sec.Cmd) == "" {
			return Batch{}, errs.New(errs.Internal, opBatch, "section "+strconv.Quote(sec.ID)+" has an empty command")
		}
		if strings.Contains(sec.Cmd, "<<") {
			return Batch{}, errs.New(errs.Internal, opBatch, "section "+strconv.Quote(sec.ID)+" contains a heredoc")
		}
	}
	return Batch{Nonce: nonce, Sections: append([]Section(nil), s...)}, nil
}

// Script is the POSIX sh script for the whole batch. It is meant to run with
// transport.Command{Script: b.Script(), Sudo: false}; the markers go to stdout
// and stderr, and every exit code is read immediately after its command.
func (b Batch) Script() string {
	pre := markerPrefix + b.Nonce + ":"
	var sb strings.Builder
	sb.WriteString("b() { printf '\\n" + pre + "%s:B@@\\n' \"$1\"; " +
		"printf '\\n" + pre + "%s:B@@\\n' \"$1\" >&2; }\n")
	sb.WriteString("e() { printf '\\n" + pre + "%s:E:%s@@\\n' \"$1\" \"$2\"; " +
		"printf '\\n" + pre + "%s:E:%s@@\\n' \"$1\" \"$2\" >&2; }\n")
	for _, sec := range b.Sections {
		cmd := sec.Cmd
		if sec.Sudo {
			cmd = "sudo -n -- sh -c " + shq.Quote(sec.Cmd)
		}
		sb.WriteString("b " + sec.ID + "; " + cmd + "; e " + sec.ID + " $?\n")
	}
	return sb.String()
}

// Command wraps Script in one non-sudo transport command. The batch handles
// privilege per section, so the transport must not wrap the whole script in sudo.
func (b Batch) Command(timeout time.Duration) transport.Command {
	return transport.Command{Script: b.Script(), Sudo: false, Timeout: timeout}
}

// mark is one parsed marker in a stream.
type mark struct {
	id    string
	kind  byte // 'B' or 'E'
	rc    int
	start int
	end   int
}

// scan parses markers carrying exactly this nonce; anything else stays content.
func scan(data []byte, nonce string) []mark {
	pre := []byte(markerPrefix + nonce + ":")
	var out []mark
	i := 0
	for {
		j := bytes.Index(data[i:], pre)
		if j < 0 {
			return out
		}
		p := i + j
		k := p + len(pre)
		c := bytes.IndexByte(data[k:], ':')
		if c < 0 {
			return out
		}
		id := string(data[k : k+c])
		q := k + c + 1
		if q >= len(data) {
			return out
		}
		switch data[q] {
		case 'B':
			if bytes.HasPrefix(data[q:], []byte("B@@")) {
				out = append(out, mark{id: id, kind: 'B', start: p, end: q + 3})
				i = q + 3
				continue
			}
		case 'E':
			if bytes.HasPrefix(data[q:], []byte("E:")) {
				r := q + 2
				e := bytes.Index(data[r:], []byte("@@"))
				if e < 0 {
					return out
				}
				rc, err := strconv.Atoi(string(data[r : r+e]))
				if err != nil {
					rc = -1
				}
				out = append(out, mark{id: id, kind: 'E', rc: rc, start: p, end: r + e + 2})
				i = r + e + 2
				continue
			}
		}
		i = q + 1
	}
}

// pair finds the first B marker for id and the first E marker after it.
func pair(ms []mark, id string) (mark, mark, bool) {
	var bm mark
	haveB := false
	for _, m := range ms {
		if m.id != id {
			continue
		}
		if m.kind == 'B' && !haveB {
			bm, haveB = m, true
			continue
		}
		if m.kind == 'E' && haveB {
			return bm, m, true
		}
	}
	return mark{}, mark{}, false
}

// body returns the bytes between a B and an E marker minus the single leading
// and trailing newline the markers themselves print.
func body(data []byte, bm, em mark) string {
	if bm.end > em.start || em.start > len(data) {
		return ""
	}
	s := string(data[bm.end:em.start])
	s = strings.TrimPrefix(s, "\n")
	s = strings.TrimSuffix(s, "\n")
	return s
}

// Split parses both streams by markers with this batch's nonce only. Every
// section of the batch gets an entry. RC comes from the stdout E marker; a
// section whose B marker is absent, or whose E marker never arrived, is Missing
// with an errs.Remote error. Stderr is the section's own stderr text; when the
// stderr stream carries no markers at all, its whole text is attributed to the
// sections, so a bare "sudo: a password is required" still classifies.
func (b Batch) Split(stdout, stderr []byte) map[string]SectionResult {
	outMarks := scan(stdout, b.Nonce)
	errMarks := scan(stderr, b.Nonce)
	bareStderr := len(errMarks) == 0 && !bytes.Contains(stderr, []byte(markerPrefix)) &&
		len(bytes.TrimSpace(stderr)) > 0
	bare := strings.TrimRight(string(stderr), "\n")

	res := make(map[string]SectionResult, len(b.Sections))
	for _, sec := range b.Sections {
		r := SectionResult{ID: sec.ID, RC: -1}
		if bm, em, ok := pair(outMarks, sec.ID); ok {
			r.Stdout = body(stdout, bm, em)
			r.RC = em.rc
		} else {
			r.Missing = true
		}
		if bm, em, ok := pair(errMarks, sec.ID); ok {
			r.Stderr = body(stderr, bm, em)
		} else if bareStderr && !r.Missing {
			r.Stderr = bare
		}
		if r.Missing {
			r.Err = errs.New(errs.Remote, "probe", "no result (truncated or aborted)")
		} else {
			r.Err = shq.Classify(r.RC, r.Stderr, sec.Sudo)
		}
		res[sec.ID] = r
	}
	return res
}
