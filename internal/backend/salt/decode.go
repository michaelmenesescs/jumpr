package salt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"jumpr/internal/errs"
)

// snippetLen is how much unparsable output an error message carries.
const snippetLen = 200

// FirstJSON skips leading non-JSON lines and returns the input from the first
// line that starts (after spaces) with { or [. Salt CLIs print warnings and
// banners before the JSON.
func FirstJSON(b []byte) ([]byte, error) {
	for i := 0; i < len(b); {
		j := i
		for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\r') {
			j++
		}
		if j < len(b) && (b[j] == '{' || b[j] == '[') {
			return b[j:], nil
		}
		nl := bytes.IndexByte(b[i:], '\n')
		if nl < 0 {
			break
		}
		i += nl + 1
	}
	return nil, errs.New(errs.Parse, "salt.decode", "no JSON in output: "+snippet(b))
}

// snippet is the first snippetLen bytes of b with trailing whitespace removed.
func snippet(b []byte) string {
	if len(b) > snippetLen {
		b = b[:snippetLen]
	}
	return strings.TrimSpace(string(b))
}

// DecodeObject decodes one or more concatenated top-level JSON objects and
// merges their keys. Non-static salt output is one object per minion
// ({"web-01": …}{"web-02": …}), so callers get every minion in one map.
func DecodeObject(b []byte) (map[string]json.RawMessage, error) {
	data, err := FirstJSON(b)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	out := make(map[string]json.RawMessage)
	for {
		var m map[string]json.RawMessage
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, errs.New(errs.Parse, "salt.decode", "object: "+err.Error())
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out, nil
}

// DecodeInto decodes the first JSON value in b into v.
func DecodeInto(b []byte, v any) error {
	data, err := FirstJSON(b)
	if err != nil {
		return err
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(v); err != nil {
		return errs.New(errs.Parse, "salt.decode", err.Error())
	}
	return nil
}

// NoReturn reports whether raw is Salt's "the minion did not answer" marker:
// JSON false, or a string starting with "Minion did not return" or
// "No response". JSON null is a legitimate return value, not a no-return.
func NoReturn(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		return strings.HasPrefix(t, "Minion did not return") || strings.HasPrefix(t, "No response")
	}
	return false
}

// jidRe matches exactly 20 digits, not part of a longer run.
var jidRe = regexp.MustCompile(`(?:^|[^0-9])([0-9]{20})(?:[^0-9]|$)`)

// ParseJID returns the first Salt job id in stdout. It works for the plain
// "Executed command with job ID: …" form and for JSON forms.
func ParseJID(stdout []byte) (string, error) {
	if m := jidRe.FindSubmatch(stdout); m != nil {
		return string(m[1]), nil
	}
	return "", errs.New(errs.Parse, "salt.jid", "no job id in output")
}
