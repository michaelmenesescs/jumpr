package salt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/shq"
)

// saltPrefix is the label namespace the inventory adds to grain keys. A
// selector term written as `salt.role=web` targets the `role` grain.
const saltPrefix = "salt."

// hostKey is the one selector key that targets minion IDs instead of grains.
const hostKey = "host"

// smallSetLimit is the largest resolved set that is still clearer as a list of
// minion IDs than as a compound expression.
const smallSetLimit = 20

// Compiled is the targeting plan for one operation: the Salt targets to
// submit, whether they were compiled from a selector, the minions the client
// expects to match, and one line explaining the choice for the plan dialog.
type Compiled struct {
	Targets []Target
	Native  bool
	Minions []string
	Explain string
}

// CompileSelector turns a jumper selector into one compound Salt target.
// It succeeds only when the selector has no sets and every term targets a
// known grain or the minion ID. Values Salt cannot take in a compound
// expression (empty, a space, a quote, or parentheses) make it fail so the
// caller falls back to a list.
func CompileSelector(sel resource.Selector, grainKeys map[string]bool) (Target, bool) {
	parts := make([]string, 0, len(sel.Terms))
	for _, t := range sel.Terms {
		expr, reason := nativeTerm(t, grainKeys)
		if reason != "" {
			return Target{}, false
		}
		parts = append(parts, expr)
	}
	if len(sel.Sets) > 0 || len(parts) == 0 {
		return Target{}, false
	}
	expr := strings.Join(parts, " and ")
	// A compound expression may not start with "not"; an unconditional glob
	// term in front makes it valid without changing its meaning.
	if strings.HasPrefix(parts[0], "not ") {
		expr = "* and " + expr
	}
	return Target{Type: "compound", Expr: expr}, true
}

// nativeTerm renders one term as a compound expression, or returns a short
// reason (for the plan dialog) why the term cannot be expressed natively.
func nativeTerm(t resource.Term, grainKeys map[string]bool) (string, string) {
	key := strings.TrimPrefix(t.Key, saltPrefix)
	host := key == hostKey
	if !host && !grainKeys[key] {
		return "", fmt.Sprintf("selector uses non-grain key %q", key)
	}

	value := t.Value
	if t.Op != resource.OpExists && t.Op != resource.OpNotExists {
		if value == "" {
			return "", fmt.Sprintf("selector value for %q is empty", key)
		}
		if strings.ContainsAny(value, " \"'()") {
			return "", fmt.Sprintf("selector value for %q has characters Salt cannot take", key)
		}
	}

	if host {
		switch t.Op {
		case resource.OpEq:
			return "L@" + value, ""
		case resource.OpMatch:
			return "E@" + value, ""
		case resource.OpNotMatch:
			return "not E@" + value, ""
		}
		return "", "selector uses a host term Salt cannot express"
	}

	switch t.Op {
	case resource.OpEq:
		return "G@" + key + ":" + value, ""
	case resource.OpNotEq:
		return "not G@" + key + ":" + value, ""
	case resource.OpMatch:
		return "P@" + key + ":" + value, ""
	case resource.OpNotMatch:
		return "not P@" + key + ":" + value, ""
	case resource.OpExists:
		return "G@" + key + ":*", ""
	case resource.OpNotExists:
		return "not G@" + key + ":*", ""
	}
	return "", fmt.Sprintf("selector uses an operator Salt cannot express on %q", key)
}

// CompileHosts turns minion IDs into sorted, de-duplicated list targets of at
// most chunk IDs each. IDs that cannot live in a comma-joined list (comma,
// whitespace, empty) are dropped.
func CompileHosts(minions []string, chunk int) []Target {
	if chunk < 1 {
		chunk = 1
	}
	ids := sortedMinions(minions)
	var targets []Target
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		targets = append(targets, Target{Type: "list", Expr: strings.Join(ids[start:end], ",")})
	}
	return targets
}

// sortedMinions sorts and de-duplicates minion IDs, dropping the ones a list
// target cannot carry.
func sortedMinions(minions []string) []string {
	seen := make(map[string]bool, len(minions))
	ids := make([]string, 0, len(minions))
	for _, id := range minions {
		if id == "" || strings.ContainsAny(id, ", \t\r\n") {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Compile picks the best target form for a selector and the minions the client
// resolved for it. Native compilation is used only for sets larger than
// smallSetLimit; small sets stay readable as lists. Minions always carries the
// client-side resolution, which the job engine compares against Salt's own
// match to report extra and missing minions.
func Compile(sel resource.Selector, resolved []string, grainKeys map[string]bool, chunk int) Compiled {
	minions := append([]string(nil), resolved...)
	target, native := CompileSelector(sel, grainKeys)
	if native && len(minions) > smallSetLimit {
		return Compiled{
			Targets: []Target{target},
			Native:  true,
			Minions: minions,
			Explain: "compound: " + target.Expr,
		}
	}

	targets := CompileHosts(minions, chunk)
	explain := fmt.Sprintf("list of %d minions in %d %s",
		len(minions), len(targets), plural(len(targets), "chunk", "chunks"))
	if reason := nativeBlocker(sel, grainKeys); reason != "" {
		explain += " (" + reason + ")"
	}
	return Compiled{Targets: targets, Minions: minions, Explain: explain}
}

// nativeBlocker returns the first reason CompileSelector would reject sel, or
// "" when the selector compiles natively.
func nativeBlocker(sel resource.Selector, grainKeys map[string]bool) string {
	if len(sel.Sets) > 0 {
		return fmt.Sprintf("selector references set %q", "@"+sel.Sets[0])
	}
	if len(sel.Terms) == 0 {
		return "selector is empty"
	}
	for _, t := range sel.Terms {
		if _, reason := nativeTerm(t, grainKeys); reason != "" {
			return reason
		}
	}
	return ""
}

// plural returns one or many depending on n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// nativeFlags maps a Target.Type to the flag a user types for it. A glob is
// the Salt default and takes no flag.
var nativeFlags = map[string]string{
	"compound":  "-C",
	"grain":     "-G",
	"pcre":      "-E",
	"list":      "-L",
	"nodegroup": "-N",
}

// ParseNative parses the target a user typed after `:salt`: -C, -G, -E, -L,
// -N or -I (pillar, compiled to a compound I@ term), or a bare glob. Quotes
// around the expression are optional.
func ParseNative(s string) (Target, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return Target{}, errs.New(errs.Config, "salt.ParseNative", "empty target")
	}
	if !strings.HasPrefix(in, "-") {
		expr := unquote(in)
		if expr == "" {
			return Target{}, errs.New(errs.Config, "salt.ParseNative", "empty target expression")
		}
		return Target{Type: "glob", Expr: expr}, nil
	}

	flag, rest := splitFlag(in)
	expr := unquote(strings.TrimSpace(rest))
	if expr == "" {
		return Target{}, errs.New(errs.Config, "salt.ParseNative",
			fmt.Sprintf("%s needs a target expression", flag))
	}

	switch flag {
	case "-C":
		return Target{Type: "compound", Expr: expr}, nil
	case "-G":
		return Target{Type: "grain", Expr: expr}, nil
	case "-E":
		return Target{Type: "pcre", Expr: expr}, nil
	case "-L":
		return Target{Type: "list", Expr: expr}, nil
	case "-N":
		return Target{Type: "nodegroup", Expr: expr}, nil
	case "-I":
		return Target{Type: "compound", Expr: "I@" + expr}, nil
	}
	return Target{}, errs.New(errs.Config, "salt.ParseNative", fmt.Sprintf("unknown target flag %q", flag))
}

// splitFlag splits the leading flag word from the rest of the input.
func splitFlag(s string) (flag, rest string) {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// unquote strips one pair of matching single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// String renders the target in the form a user would type on a Salt command
// line, quoted for display.
func (t Target) String() string {
	flag, ok := nativeFlags[t.Type]
	if !ok {
		return t.Expr
	}
	return shq.Join(flag, t.Expr)
}

// ParsePreview parses the output of `--preview-target`: either a JSON list or
// the text form with one `- minion` line each. The result is sorted and
// de-duplicated; unrelated lines are skipped.
func ParsePreview(out []byte) ([]string, error) {
	if strings.TrimSpace(string(out)) == "" {
		return nil, nil
	}
	if data, err := FirstJSON(out); err == nil {
		var list []string
		if json.Unmarshal(data, &list) == nil {
			return cleanMinions(list), nil
		}
	}

	minions := make([]string, 0, 8)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-") {
			continue
		}
		if id := strings.TrimSpace(strings.TrimPrefix(line, "-")); id != "" {
			minions = append(minions, unquote(id))
		}
	}
	if len(minions) == 0 {
		return nil, errs.New(errs.Parse, "salt.preview", "no minions in preview output")
	}
	return cleanMinions(minions), nil
}

// cleanMinions trims, sorts and de-duplicates minion IDs.
func cleanMinions(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return sortedMinions(out)
}
