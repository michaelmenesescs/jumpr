package salt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"jumper/internal/errs"
)

// Pillar reads (SPEC §8.6 "Pillar"): pillar.items and pillar.get.
//
// jumper's promise is narrow and it is kept here: a pillar value is turned
// into a type and a length the moment it arrives, and the bytes it came in
// are zeroed before this file returns. Nothing in this file logs, records or
// returns a value — except PillarValue, which hands exactly one value to the
// dialog that asked for it.
const (
	opPillarItems = "salt.pillar.items"
	opPillarGet   = "salt.pillar.get"

	// pillarModule is the Salt module whose calls carry values.
	pillarModule = "pillar."

	// PillarHidden is what the jobs browser and the log show in place of a
	// pillar.* call's arguments and returns.
	PillarHidden = "‹pillar data hidden›"
)

var (
	// minionIDRe is the shape of a minion id jumper will address: no shell
	// metacharacters, no slashes, no leading dash.
	minionIDRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	// pillarPathRe is the shape of a colon-separated pillar key path.
	pillarPathRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*(:[A-Za-z0-9_][A-Za-z0-9_-]*)*$`)
)

// PillarNode is one node of a pillar tree: the full key path, the JSON type of
// the value (dict, list, str, int, float, bool or null), the length of a
// string or list and, for a dict, its children. The value itself never
// survives the conversion.
type PillarNode struct {
	Key      string // full path, e.g. "nginx:tls:key"
	Type     string
	Len      int // characters of a str, items of a list; 0 otherwise
	Children []PillarNode
}

// PillarTree is one minion's pillar as structure only.
type PillarTree struct {
	Minion string
	Nodes  []PillarNode
}

// PillarDiff is one key path whose type differs between two minions. A side
// that does not have the key is "" (missing).
type PillarDiff struct {
	Path  string
	Left  string
	Right string
}

// PillarKeys returns one minion's pillar as a tree of types and lengths. It
// runs pillar.items as a Salt job, converts the return as soon as it arrives
// and zeroes the raw JSON: no value is kept, logged or returned.
func (b *Backend) PillarKeys(ctx context.Context, minion string) (PillarTree, error) {
	tree := PillarTree{}
	minion = strings.TrimSpace(minion)
	if minion == "" {
		return tree, errs.New(errs.Config, opPillarItems, "pillar needs a minion")
	}
	if !minionIDRe.MatchString(minion) {
		return tree, errs.New(errs.Config, opPillarItems, fmt.Sprintf("invalid minion id %q", minion))
	}
	if b.runner == nil {
		return tree, errs.New(errs.Config, opPillarItems, "salt backend not configured: no runner")
	}

	job, err := b.runner.Start(ctx, Submit{
		Targets: []Target{{Type: "list", Expr: minion}},
		Fun:     "pillar.items",
		Expect:  []string{minion},
	})
	if err != nil {
		return tree, err
	}
	defer job.Cancel()

	var (
		raw     json.RawMessage
		missing error
	)
	for mr := range job.Results() {
		returned := bytes.TrimSpace(mr.Return)
		if mr.NoReturn || len(returned) == 0 || isNull(returned) {
			if missing == nil {
				missing = errs.New(errs.Timeout, opPillarItems, "no return from "+mr.Minion)
			}
			continue
		}
		// A string or list return is Salt reporting a failure, not a pillar.
		// Its text can carry values, so it never reaches the error message.
		if !bytes.HasPrefix(returned, []byte("{")) {
			if missing == nil {
				missing = errs.New(errs.Remote, opPillarItems, "pillar.items did not return an object").WithHost(mr.Minion)
			}
			continue
		}
		raw = mr.Return
	}
	if len(raw) == 0 {
		if missing != nil {
			return tree, missing
		}
		return tree, errs.New(errs.Parse, opPillarItems, "pillar.items returned nothing").WithHost(minion)
	}

	tree.Minion = minion
	tree.Nodes = pillarNodes(raw, "")
	// Every byte of the return is a value: drop it before returning.
	clear(raw)
	return tree, nil
}

// PillarValue reveals one pillar value of one minion. The value is returned to
// the caller and nowhere else: the call is Sensitive, so it is not logged, and
// the caller shows it in a dialog without recording it.
func (b *Backend) PillarValue(ctx context.Context, minion, keyPath string) (string, error) {
	minion = strings.TrimSpace(minion)
	keyPath = strings.TrimSpace(keyPath)
	if minion == "" || !minionIDRe.MatchString(minion) {
		return "", errs.New(errs.Config, opPillarGet, fmt.Sprintf("invalid minion id %q", minion))
	}
	if !pillarPathRe.MatchString(keyPath) {
		return "", errs.New(errs.Config, opPillarGet, fmt.Sprintf("invalid pillar key %q", keyPath))
	}
	if b.runner == nil {
		return "", errs.New(errs.Config, opPillarGet, "salt backend not configured: no runner")
	}
	out, err := b.runner.Run(ctx, Call{
		Tool:      toolSalt,
		Target:    &Target{Type: "list", Expr: minion},
		Fun:       "pillar.get",
		Args:      []string{keyPath},
		Sensitive: true,
	})
	if err != nil {
		return "", err
	}
	top, err := DecodeObject(out)
	if err != nil {
		clear(out)
		return "", err
	}
	raw, ok := top[minion]
	if !ok && len(top) == 1 {
		for _, v := range top {
			raw, ok = v, true
		}
	}
	if !ok {
		return "", errs.New(errs.NotAvailable, opPillarGet, "the minion did not answer").WithHost(minion)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isNull(trimmed) || NoReturn(trimmed) {
		clear(out)
		return "", errs.New(errs.NotAvailable, opPillarGet, "no pillar value at "+keyPath).WithHost(minion)
	}
	value := formatPillarValue(trimmed)
	// The answer is now a string the caller owns; drop the JSON it came in.
	clear(out)
	return value, nil
}

// DiffPillars compares two pillar trees by key path and type. Values are not
// in a tree, so they cannot — and never do — take part. Rows are sorted by
// path; an empty result means the two pillars have the same shape.
func DiffPillars(a, b PillarTree) []PillarDiff {
	left, right := flattenPillar(a), flattenPillar(b)
	paths := make([]string, 0, len(left)+len(right))
	seen := make(map[string]bool, len(left)+len(right))
	for p := range left {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range right {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	var out []PillarDiff
	for _, p := range paths {
		l, lok := left[p]
		r, rok := right[p]
		switch {
		case !rok:
			out = append(out, PillarDiff{Path: p, Left: l})
		case !lok:
			out = append(out, PillarDiff{Path: p, Right: r})
		case l != r:
			out = append(out, PillarDiff{Path: p, Left: l, Right: r})
		}
	}
	return out
}

// flattenPillar maps every key path of a tree onto its type.
func flattenPillar(t PillarTree) map[string]string {
	out := make(map[string]string)
	var walk func([]PillarNode)
	walk = func(nodes []PillarNode) {
		for _, n := range nodes {
			out[n.Key] = n.Type
			walk(n.Children)
		}
	}
	walk(t.Nodes)
	return out
}

// HiddenFunction reports whether a Salt function's arguments and returns stay
// hidden in the jobs browser and the log: every pillar.* call does.
func HiddenFunction(fun string) bool {
	return strings.HasPrefix(strings.TrimSpace(fun), pillarModule)
}

// HideJobInfo replaces a pillar.* job's arguments and returns with
// PillarHidden. A job of any other function is returned unchanged.
func HideJobInfo(info JobInfo) JobInfo {
	if !HiddenFunction(info.Function) {
		return info
	}
	info.Arguments = nil
	if len(info.Result) > 0 {
		// The marker is deliberately not JSON: a consumer that cannot decode
		// a return renders it verbatim, so it shows without quotes.
		hidden := json.RawMessage(PillarHidden)
		results := make(map[string]MinionReturn, len(info.Result))
		for minion, mr := range info.Result {
			results[minion] = MinionReturn{Return: hidden, Retcode: mr.Retcode, Success: mr.Success}
		}
		info.Result = results
	}
	return info
}

// pillarNodes converts one JSON object into nodes. prefix is the path of the
// object itself ("" at the root).
func pillarNodes(raw json.RawMessage, prefix string) []PillarNode {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &obj); err != nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	nodes := make([]PillarNode, 0, len(keys))
	for _, k := range keys {
		nodes = append(nodes, pillarNode(pillarKey(prefix, k), obj[k]))
	}
	return nodes
}

// pillarKey joins a parent path and a key.
func pillarKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + ":" + key
}

// pillarNode converts one value into a node. The value is decoded only to
// classify it or to measure it, and is dropped when this returns.
func pillarNode(path string, raw json.RawMessage) PillarNode {
	node := PillarNode{Key: path}
	value := bytes.TrimSpace(raw)
	if len(value) == 0 {
		node.Type = "null"
		return node
	}
	switch value[0] {
	case '{':
		node.Type = "dict"
		node.Children = pillarNodes(value, path)
	case '[':
		node.Type = "list"
		var items []json.RawMessage
		if err := json.Unmarshal(value, &items); err == nil {
			node.Len = len(items)
		}
	case '"':
		node.Type = "str"
		var s string
		if err := json.Unmarshal(value, &s); err == nil {
			node.Len = utf8.RuneCountInString(s)
		}
	case 't', 'f':
		node.Type = "bool"
	case 'n':
		node.Type = "null"
	default:
		if bytes.ContainsAny(value, ".eE") {
			node.Type = "float"
			return node
		}
		node.Type = "int"
	}
	return node
}

// formatPillarValue renders a revealed value for its dialog: a string as it
// is, anything else as indented JSON.
func formatPillarValue(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(raw)
	}
	if out, err := json.MarshalIndent(v, "", "  "); err == nil {
		return string(out)
	}
	return string(raw)
}
