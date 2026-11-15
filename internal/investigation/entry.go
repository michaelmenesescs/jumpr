package investigation

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"jumpr/internal/action"
	"jumpr/internal/redact"
)

// EntryKind classifies one timeline bullet (BUILD-PLAN §4.10).
type EntryKind string

const (
	EntryOpen    EntryKind = "open"
	EntryView    EntryKind = "view"
	EntryProbe   EntryKind = "probe"
	EntryJournal EntryKind = "journal"
	EntryAction  EntryKind = "action"
	EntryDenied  EntryKind = "denied"
	EntryNote    EntryKind = "note"
	EntryFinding EntryKind = "finding"
	EntryResolve EntryKind = "resolve"
	EntryInput   EntryKind = "input" // synchronized input lines
)

// Entry is one timeline bullet. Text is Markdown, one line. Output is attached collapsed.
type Entry struct {
	Time   time.Time
	Kind   EntryKind
	Actor  action.Actor
	Text   string
	Output string // optional; redacted and size-capped by the writer
}

// Log appends entries to the open investigation, or to the daily audit file when none is open
// (audit files only receive action, denied and input entries). Safe for concurrent use.
// Log implements bus.Recorder.
type Log interface {
	Append(e Entry) error
	Current() (Meta, bool)
}

// maxOutputBytes caps an attached output block. Anything longer is cut on a
// line boundary and the reader is told so.
const maxOutputBytes = 16 << 10

// entryTimeLayout is the wall-clock form of a bullet's timestamp.
const entryTimeLayout = "15:04:05"

// FormatEntry renders one timeline bullet.
//
//   - 14:02:40 **probe** `failed-units` on 24 hosts → 22 identical, 2 differ
//
// The time is the entry's own zone in 15:04:05 form, the kind is bold, then
// Text verbatim. A non-human actor is credited at the end of the line. Output,
// when present, is attached as a collapsed <details> block indented two spaces
// so it nests under the bullet (SPEC §10).
func FormatEntry(e Entry, red *redact.Redactor) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(e.Time.Format(entryTimeLayout))
	b.WriteString(" **")
	b.WriteString(string(e.Kind))
	b.WriteString("**")
	if text := strings.TrimRight(redText(red, e.Text), "\n"); text != "" {
		b.WriteString(" ")
		b.WriteString(text)
	}
	if e.Actor != "" && e.Actor != action.Human {
		b.WriteString(" by **")
		b.WriteString(string(e.Actor))
		b.WriteString("**")
	}
	if body, lines, ok := formatOutput(e.Output, red); ok {
		b.WriteString("\n")
		b.WriteString(detailsBlock(body, lines))
	}
	return b.String()
}

// redText runs text through the redactor. A nil redactor leaves it alone.
func redText(red *redact.Redactor, s string) string {
	if red == nil {
		return s
	}
	return red.String(s)
}

// formatOutput normalizes, redacts and caps an attached output block. It
// reports the block body, the number of lines in it and whether there is
// anything to show at all.
func formatOutput(raw string, red *redact.Redactor) (body string, lines int, ok bool) {
	if strings.TrimSpace(raw) == "" {
		return "", 0, false
	}
	s := redText(red, raw)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimRight(s, "\n")
	if capped, cut := capOutput(s); cut {
		s = capped + "\n… (output truncated at 16 KiB)"
	}
	return s, strings.Count(s, "\n") + 1, true
}

// capOutput cuts s to at most maxOutputBytes, preferring a line boundary and
// never splitting a UTF-8 sequence.
func capOutput(s string) (string, bool) {
	if len(s) <= maxOutputBytes {
		return s, false
	}
	cut := s[:maxOutputBytes]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		return cut[:i], true
	}
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	if len(cut) == 0 {
		return "", true
	}
	// Drop a partial rune at the end, if any.
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r == utf8.RuneError && size <= 1 {
			cut = cut[:len(cut)-1]
			continue
		}
		break
	}
	return cut, true
}

// detailsBlock renders the collapsed output attachment. Every line is
// indented two spaces so the whole block belongs to the bullet.
func detailsBlock(body string, lines int) string {
	fence := strings.Repeat("`", fenceLength(body))
	var b strings.Builder
	b.WriteString("  <details><summary>output (")
	b.WriteString(strconv.Itoa(lines))
	b.WriteString(" lines)</summary>\n\n")
	b.WriteString("  ")
	b.WriteString(fence)
	b.WriteString("\n")
	for _, ln := range strings.Split(body, "\n") {
		if ln == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  ")
		b.WriteString(ln)
		b.WriteString("\n")
	}
	b.WriteString("  ")
	b.WriteString(fence)
	b.WriteString("\n")
	b.WriteString("  </details>")
	return b.String()
}

// fenceLength returns a fence longer than any run of backticks in s, so output
// that contains a Markdown fence cannot break out of the code block.
func fenceLength(s string) int {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	if longest < 2 {
		return 3
	}
	return longest + 1
}
