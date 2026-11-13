package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/itchyny/gojq"

	"jumper/internal/errs"
)

const (
	// parseTimeout bounds a jq expression. A plugin expression runs on the
	// client against already-received output, so it needs a ceiling of its own.
	parseTimeout = 2 * time.Second
	// parseMaxRows caps how many rows one parse may produce, so a runaway jq
	// expression (`repeat(1)`, `range(0;1e9)`) cannot exhaust memory.
	parseMaxRows = 10000
	// parseOp is the errs.Error op of every failure in this file.
	parseOp = "plugin.parse"
)

// numberRe matches a plain decimal literal. Only a token that matches is
// converted to a number, so "1e", "NaN", "Inf", "0x10" and "1,000" stay
// strings.
var numberRe = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// Parse turns command or HTTP output into rows according to def (SPEC §12).
// Every row is a map from a view column name to a value. Parse never panics:
// output that does not fit the definition is an *errs.Error of kind
// errs.Parse, and lines a definition does not match are skipped.
func Parse(def ParseDef, out []byte) ([]map[string]any, error) {
	if def.Type != ParseRaw && len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	switch def.Type {
	case ParseJSON:
		return parseJSON(out)
	case ParseJQ:
		return parseJQ(def.Expr, out)
	case ParseRegex:
		return parseRegex(def, out)
	case ParseColumns:
		return parseColumns(def, out)
	case ParseKV:
		return parseKV(def, out)
	case ParseLines:
		return parseLines(out), nil
	case ParseRaw:
		return []map[string]any{{"output": strings.TrimRight(string(out), "\r\n")}}, nil
	default:
		return nil, errs.New(errs.Config, parseOp, fmt.Sprintf("unknown parse type %q", def.Type))
	}
}

// parseJSON accepts an object (one row) or an array (one row per element).
// Numbers keep their shape: an integral literal becomes int64, anything else
// float64, so a JSON `200` renders as 200 and not 200.0.
func parseJSON(out []byte) ([]map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errs.Wrap(errs.Parse, parseOp, fmt.Errorf("json: %w", err))
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errs.New(errs.Parse, parseOp, "json: unexpected data after the top-level value")
	}
	v = jsonTyped(v)

	switch x := v.(type) {
	case map[string]any:
		return []map[string]any{x}, nil
	case []any:
		rows := make([]map[string]any, 0, len(x))
		for _, e := range x {
			if len(rows) >= parseMaxRows {
				break
			}
			if m, ok := e.(map[string]any); ok {
				rows = append(rows, m)
				continue
			}
			rows = append(rows, map[string]any{"value": e})
		}
		return rows, nil
	default:
		return []map[string]any{{"value": x}}, nil
	}
}

// jsonTyped converts json.Number values (and the numbers inside them, at any
// depth) into int64 or float64.
func jsonTyped(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case map[string]any:
		for k, e := range x {
			x[k] = jsonTyped(e)
		}
		return x
	case []any:
		for i := range x {
			x[i] = jsonTyped(x[i])
		}
		return x
	}
	return v
}

// parseJQ runs a gojq expression over the JSON output. Every emitted object is
// a row; any other value becomes {"value": …}. The expression has
// parseTimeout to finish and may not produce more than parseMaxRows rows.
func parseJQ(expr string, out []byte) ([]map[string]any, error) {
	query, err := gojq.Parse(expr)
	if err != nil {
		return nil, errs.Wrap(errs.Parse, parseOp, fmt.Errorf("jq: %w", err))
	}
	var input any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&input); err != nil {
		return nil, errs.Wrap(errs.Parse, parseOp, fmt.Errorf("jq: input is not JSON: %w", err))
	}
	input = jsonTyped(input)

	ctx, cancel := context.WithTimeout(context.Background(), parseTimeout)
	defer cancel()

	iter := query.RunWithContext(ctx, input)
	rows := make([]map[string]any, 0, 16)
	for len(rows) < parseMaxRows {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return nil, errs.Wrap(errs.Timeout, parseOp,
					fmt.Errorf("jq: expression did not finish within %s", parseTimeout))
			case errors.Is(err, context.Canceled):
				return nil, errs.Wrap(errs.Canceled, parseOp, fmt.Errorf("jq: %w", err))
			default:
				return nil, errs.Wrap(errs.Parse, parseOp, fmt.Errorf("jq: %w", err))
			}
		}
		rows = append(rows, jqRow(v))
	}
	return rows, nil
}

// jqRow keeps an object as it is and wraps anything else under "value".
func jqRow(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{"value": v}
}

// parseRegex fills one row per match from the pattern's named groups. Lines the
// pattern does not match are skipped; with multiline the pattern runs over the
// whole output and every match is a row.
func parseRegex(def ParseDef, out []byte) ([]map[string]any, error) {
	if strings.TrimSpace(def.Pattern) == "" {
		return nil, errs.New(errs.Config, parseOp, "regex: parse.pattern is empty")
	}
	re, err := regexp.Compile(def.Pattern)
	if err != nil {
		return nil, errs.Wrap(errs.Config, parseOp, fmt.Errorf("regex: %w", err))
	}
	names := namedGroups(re)
	if len(names) == 0 {
		return nil, errs.New(errs.Config, parseOp, "regex: parse.pattern has no named groups, like (?P<name>...)")
	}

	var matches [][]string
	if def.Multiline {
		matches = re.FindAllStringSubmatch(string(out), -1)
	} else {
		for _, line := range splitLines(out) {
			matches = append(matches, re.FindAllStringSubmatch(line, -1)...)
			if len(matches) >= parseMaxRows {
				break
			}
		}
	}

	rows := make([]map[string]any, 0, len(matches))
	for _, m := range matches {
		if len(rows) >= parseMaxRows {
			break
		}
		row := make(map[string]any, len(names))
		for i, name := range names {
			var v string
			if i+1 < len(m) {
				v = m[i+1]
			}
			row[name] = typedValue(v, def.Strings)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseColumns splits whitespace-separated lines into columns. The column names
// come from names or from the first line (header: true); skip lines are dropped
// first. The last column receives everything that is left on the line, so
// values with spaces survive.
func parseColumns(def ParseDef, out []byte) ([]map[string]any, error) {
	lines := splitLines(out)
	if def.Skip > 0 {
		if def.Skip >= len(lines) {
			return nil, nil
		}
		lines = lines[def.Skip:]
	}

	names := def.Names
	if def.Header {
		if len(lines) == 0 {
			return nil, nil
		}
		names = strings.Fields(lines[0])
		lines = lines[1:]
		if len(names) == 0 {
			return nil, errs.New(errs.Parse, parseOp, "columns: header line has no column names")
		}
	}
	if len(names) == 0 {
		return nil, errs.New(errs.Config, parseOp, "columns: parse needs header: true or names")
	}

	rows := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if len(rows) >= parseMaxRows {
			break
		}
		fields, ok := splitColumns(line, len(names))
		if !ok {
			continue
		}
		row := make(map[string]any, len(names))
		for i, name := range names {
			row[name] = typedValue(fields[i], def.Strings)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// splitColumns cuts line into exactly n fields on runs of spaces and tabs; the
// nth field is the trimmed remainder of the line. It reports false when the
// line has fewer than n fields, so a truncated or garbage line is skipped
// rather than becoming a row of empty columns.
func splitColumns(line string, n int) ([]string, bool) {
	if n <= 0 || fieldCount(line) < n {
		return nil, false
	}
	fields := make([]string, 0, n)
	i := 0
	for len(fields) < n-1 {
		for i < len(line) && isFieldSpace(line[i]) {
			i++
		}
		start := i
		for i < len(line) && !isFieldSpace(line[i]) {
			i++
		}
		fields = append(fields, line[start:i])
	}
	for i < len(line) && isFieldSpace(line[i]) {
		i++
	}
	fields = append(fields, strings.TrimRight(line[i:], " 	"))
	return fields, true
}

// fieldCount counts the whitespace-separated fields of line.
func fieldCount(line string) int {
	n := 0
	inField := false
	for i := 0; i < len(line); i++ {
		switch {
		case isFieldSpace(line[i]):
			inField = false
		case !inField:
			inField = true
			n++
		}
	}
	return n
}

func isFieldSpace(b byte) bool { return b == ' ' || b == '\t' }

// parseKV reads `key=value` (or the configured separator) lines into a single
// row. Lines without the separator and lines with an empty key are skipped.
func parseKV(def ParseDef, out []byte) ([]map[string]any, error) {
	sep := def.Separator
	if sep == "" {
		sep = "="
	}
	row := make(map[string]any)
	for _, line := range splitLines(out) {
		i := strings.Index(line, sep)
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		if key == "" {
			continue
		}
		row[key] = typedValue(strings.TrimSpace(line[i+len(sep):]), def.Strings)
	}
	if len(row) == 0 {
		return nil, nil
	}
	return []map[string]any{row}, nil
}

// parseLines returns one row per non-empty line.
func parseLines(out []byte) []map[string]any {
	lines := splitLines(out)
	rows := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(rows) >= parseMaxRows {
			break
		}
		rows = append(rows, map[string]any{"line": line})
	}
	return rows
}

// splitLines splits output into lines, accepting CRLF, and drops the empty
// remainder after a final newline.
func splitLines(out []byte) []string {
	if len(out) == 0 {
		return nil
	}
	text := strings.ReplaceAll(string(out), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// typedValue returns the token as int64 (integral literal) or float64
// (fractional or exponent), or as a string when it is not a plain decimal
// literal or asString is set.
func typedValue(s string, asString bool) any {
	if asString || !numberRe.MatchString(s) {
		return s
	}
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}
