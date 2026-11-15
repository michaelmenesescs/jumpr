package salt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// StateRow is one state in a minion's state.apply return.
type StateRow struct {
	Module   string
	ID       string
	Name     string
	Function string
	SLS      string
	RunNum   int
	Result   *bool // tri-state: true, false, nil (JSON null)
	Changed  bool  // changes is a non-empty object
	Changes  json.RawMessage
	Comment  string
	Duration float64
}

// StateRun is one minion's state.apply return.
type StateRun struct {
	Rows   []StateRow
	Errors []string
	Test   bool
}

// stateRet is the per-state object inside a return dict.
type stateRet struct {
	Result   *bool           `json:"result"`
	Changes  json.RawMessage `json:"changes"`
	Comment  string          `json:"comment"`
	Name     string          `json:"name"`
	SLS      string          `json:"__sls__"`
	RunNum   int             `json:"__run_num__"`
	ID       string          `json:"__id__"`
	Fun      string          `json:"fun"`
	Duration float64         `json:"duration"`
}

// ParseStateReturn decodes one minion's state return, which is a dict keyed by
// module_|-id_|-name_|-function, a list of error strings, or a plain string
// such as "ERROR: …". Rows come back sorted by run number.
func ParseStateReturn(raw json.RawMessage) (StateRun, error) {
	var run StateRun
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return run, errs.New(errs.Parse, "salt.state", "empty state return")
	}
	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return run, errs.Wrap(errs.Parse, "salt.state", err)
		}
		for key, val := range obj {
			row, err := parseStateRow(key, val)
			if err != nil {
				// Remote output never panics or aborts a whole run; skip what
				// does not look like a state result.
				continue
			}
			run.Rows = append(run.Rows, row)
		}
		sort.SliceStable(run.Rows, func(i, j int) bool { return run.Rows[i].RunNum < run.Rows[j].RunNum })
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return run, errs.Wrap(errs.Parse, "salt.state", err)
		}
		for _, item := range list {
			run.Errors = append(run.Errors, errorText(item))
		}
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return run, errs.Wrap(errs.Parse, "salt.state", err)
		}
		run.Errors = append(run.Errors, s)
	case 'f':
		// Salt's "the minion did not answer" marker.
		run.Errors = append(run.Errors, "no return")
	case 'n':
		// A null return carries nothing; an empty run is the honest answer.
	case 't':
		return run, errs.New(errs.Parse, "salt.state", "unexpected boolean state return")
	default:
		return run, errs.New(errs.Parse, "salt.state", snippet(trimmed))
	}
	return run, nil
}

// parseStateRow builds one row from a return key and value. It errors when the
// value is not an object, which is how non-state keys get skipped.
func parseStateRow(key string, val json.RawMessage) (StateRow, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(val), []byte("{")) {
		return StateRow{}, fmt.Errorf("not an object")
	}
	var ret stateRet
	if err := json.Unmarshal(val, &ret); err != nil {
		return StateRow{}, err
	}
	var row StateRow
	// The key is module, id, name, function. Split with a limit: names may
	// themselves contain "_|-"-like text.
	if parts := strings.SplitN(key, "_|-", 4); len(parts) == 4 {
		row.Module, row.ID, row.Name, row.Function = parts[0], parts[1], parts[2], parts[3]
	}
	// Prefer the return's own fields; they are authoritative when the key is odd.
	if ret.ID != "" {
		row.ID = ret.ID
	}
	if ret.Name != "" {
		row.Name = ret.Name
	}
	if row.Function == "" && ret.Fun != "" {
		row.Function = ret.Fun
	}
	row.SLS = ret.SLS
	row.RunNum = ret.RunNum
	row.Result = ret.Result
	row.Comment = ret.Comment
	row.Duration = ret.Duration
	row.Changes = ret.Changes
	row.Changed = nonEmptyObject(ret.Changes)
	return row, nil
}

// nonEmptyObject reports whether raw is a JSON object with at least one key.
func nonEmptyObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return false
	}
	return len(obj) > 0
}

// errorText renders one element of an error list: a string verbatim, anything
// else as compact JSON.
func errorText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.String()
	}
	return strings.TrimSpace(string(raw))
}

// Counts returns the number of states in each outcome. With Test set, a null
// result means "would change" and is counted as pending.
func (r StateRun) Counts() (ok, changed, failed, pending int) {
	for _, row := range r.Rows {
		switch {
		case row.Result != nil && !*row.Result:
			failed++
		case row.Result == nil && r.Test:
			pending++
		case row.Changed:
			changed++
		default:
			ok++
		}
	}
	return ok, changed, failed, pending
}

// Status is the worst outcome in the run.
func (r StateRun) Status() resource.Status {
	_, changed, failed, pending := r.Counts()
	switch {
	case len(r.Errors) > 0 || failed > 0:
		return resource.StatusCrit
	case changed > 0 || pending > 0:
		return resource.StatusWarn
	default:
		return resource.StatusOK
	}
}

// Normalized renders the grouping text: one block per state that failed, would
// change or changed, in run order. Identical outcomes on different hosts
// produce identical text, so they group together. Durations, start times and
// run numbers never appear.
func (r StateRun) Normalized() string {
	var b strings.Builder
	if len(r.Errors) > 0 {
		b.WriteString("ERROR")
		for _, msg := range r.Errors {
			for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
				b.WriteString("\n  ")
				b.WriteString(strings.TrimRight(line, " \t\r"))
			}
		}
		return b.String()
	}
	var blocks []string
	for _, row := range r.Rows {
		word, show := r.rowWord(row)
		if !show {
			continue
		}
		var block strings.Builder
		block.WriteString(strings.TrimRight(fmt.Sprintf("%s %s :: %s (%s.%s) %s",
			word, row.SLS, row.ID, row.Module, row.Function, row.Name), " "))
		for i, line := range commentLines(row.Comment) {
			if i == 3 {
				break
			}
			block.WriteString("\n  ")
			block.WriteString(strings.TrimRight(line, " \t\r"))
		}
		if changes := renderChanges(row.Changes); changes != "" {
			block.WriteString("\n  ")
			block.WriteString(changes)
		}
		blocks = append(blocks, block.String())
	}
	if len(blocks) == 0 {
		return fmt.Sprintf("clean (%d states)", len(r.Rows))
	}
	return strings.Join(blocks, "\n")
}

// rowWord returns the normalization word for a row and whether the row appears
// in the normalized text at all.
func (r StateRun) rowWord(row StateRow) (string, bool) {
	switch {
	case row.Result != nil && !*row.Result:
		return "FAILED", true
	case row.Result == nil && r.Test:
		return "WOULD-CHANGE", true
	case row.Changed:
		return "CHANGED", true
	default:
		return "", false
	}
}

// commentLines splits a comment into lines, dropping a trailing empty line.
func commentLines(comment string) []string {
	if comment == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(comment, "\n"), "\n")
	return lines
}

// renderChanges renders a changes object: a string "diff" field verbatim,
// otherwise compact JSON with sorted keys.
func renderChanges(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		if diff, ok := obj["diff"]; ok {
			var s string
			if err := json.Unmarshal(diff, &s); err == nil {
				return s
			}
		}
	}
	// json.Marshal sorts object keys, so the text is stable across hosts.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(trimmed)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(trimmed)
	}
	return string(out)
}

// Summary renders "42 ok · 2 would change · 1 failed". Zero parts are omitted,
// except "ok", which is always shown.
func (r StateRun) Summary() string {
	ok, changed, failed, pending := r.Counts()
	parts := []string{fmt.Sprintf("%d ok", ok)}
	if changed > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", changed))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d would change", pending))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	return strings.Join(parts, " · ")
}

// RenderRows renders rows for a table: sls, id, function, result, comment
// (first line only).
func RenderRows(r StateRun) [][]string {
	rows := make([][]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		rows = append(rows, []string{
			row.SLS,
			row.ID,
			row.Function,
			r.resultWord(row),
			firstLine(row.Comment),
		})
	}
	return rows
}

// resultWord is the short outcome word shown in a table cell.
func (r StateRun) resultWord(row StateRow) string {
	switch {
	case row.Result != nil && !*row.Result:
		return "failed"
	case row.Result == nil && r.Test:
		return "would change"
	case row.Changed:
		return "changed"
	default:
		return "ok"
	}
}
