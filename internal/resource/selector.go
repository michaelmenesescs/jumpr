package resource

import (
	"fmt"
	"regexp"
	"strings"

	"jumper/internal/errs"
)

// Op is a selector operator.
type Op int

const (
	OpEq        Op = iota // key=value
	OpNotEq               // key!=value
	OpMatch               // key~regex   (unanchored, RE2)
	OpNotMatch            // key!~regex
	OpExists              // key
	OpNotExists           // !key
)

// Term is one selector condition.
type Term struct {
	Key   string
	Op    Op
	Value string

	// re holds the compiled regexp for OpMatch and OpNotMatch.
	re *regexp.Regexp
}

// Selector is a conjunction (AND) of terms plus named sets.
// Text form: `role=web env=prod dc~ewr|lax !maint host~^db @saved-set @investigation`.
// Terms are separated by spaces. Values with spaces use double quotes: name="a b".
type Selector struct {
	Terms []Term
	Sets  []string // names without the leading "@"
	Raw   string
}

// keyPattern is the accepted shape of a selector key.
var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-/]*$`)

// ParseSelector parses the selector text form. Empty or all-space input yields
// the zero Selector. Errors are errs.Config and name the offending token.
func ParseSelector(s string) (Selector, error) {
	sel := Selector{Raw: s}
	tokens, err := splitTokens(s)
	if err != nil {
		return Selector{}, err
	}
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "@") {
			name := tok[1:]
			if name == "" {
				return Selector{}, badToken(tok, "empty set name")
			}
			sel.Sets = append(sel.Sets, name)
			continue
		}
		term, err := parseTerm(tok)
		if err != nil {
			return Selector{}, err
		}
		sel.Terms = append(sel.Terms, term)
	}
	return sel, nil
}

// IsZero reports whether the selector has no terms and no sets.
func (s Selector) IsZero() bool { return len(s.Terms) == 0 && len(s.Sets) == 0 }

// Matches reports whether labels satisfy every term. A missing key fails
// OpEq, OpMatch and OpExists and satisfies the negated forms. Sets are ignored
// here; internal/targets expands them. Regexes are unanchored.
func (s Selector) Matches(labels map[string]string) bool {
	for _, t := range s.Terms {
		v, ok := labels[t.Key]
		var held bool
		switch t.Op {
		case OpEq:
			held = ok && v == t.Value
		case OpNotEq:
			held = !ok || v != t.Value
		case OpMatch:
			held = ok && t.re.MatchString(v)
		case OpNotMatch:
			held = !ok || !t.re.MatchString(v)
		case OpExists:
			held = ok
		case OpNotExists:
			held = !ok
		}
		if !held {
			return false
		}
	}
	return true
}

// String renders the canonical text form: terms in input order, then sets.
// Values containing a space or a quote are quoted.
func (s Selector) String() string {
	parts := make([]string, 0, len(s.Terms)+len(s.Sets))
	for _, t := range s.Terms {
		switch t.Op {
		case OpEq:
			parts = append(parts, t.Key+"="+quoteValue(t.Value))
		case OpNotEq:
			parts = append(parts, t.Key+"!="+quoteValue(t.Value))
		case OpMatch:
			parts = append(parts, t.Key+"~"+quoteValue(t.Value))
		case OpNotMatch:
			parts = append(parts, t.Key+"!~"+quoteValue(t.Value))
		case OpExists:
			parts = append(parts, t.Key)
		case OpNotExists:
			parts = append(parts, "!"+t.Key)
		}
	}
	for _, name := range s.Sets {
		parts = append(parts, "@"+name)
	}
	return strings.Join(parts, " ")
}

// splitTokens splits on spaces, honouring double quotes. Inside quotes a
// backslash escapes a quote so `\"` yields a literal quote; other backslashes
// are kept as-is for regexes.
func splitTokens(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	pending := false
	flush := func() {
		if pending {
			tokens = append(tokens, cur.String())
			cur.Reset()
			pending = false
		}
	}

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if inQuote {
			switch {
			case r == '\\' && i+1 < len(runes) && runes[i+1] == '"':
				cur.WriteRune('"')
				pending = true
				i++
			case r == '"':
				inQuote = false
			default:
				cur.WriteRune(r)
				pending = true
			}
			continue
		}
		switch r {
		case '"':
			inQuote = true
			pending = true
		case ' ', '	', '\n', '\r':
			flush()
		default:
			cur.WriteRune(r)
			pending = true
		}
	}
	if inQuote {
		return nil, badToken(strings.TrimSpace(s), "unterminated quote")
	}
	flush()
	return tokens, nil
}

// parseTerm parses one non-set token. Operators are recognised at their
// earliest position, with two-character operators winning ties.
func parseTerm(tok string) (Term, error) {
	if strings.HasPrefix(tok, "!") && !strings.ContainsAny(tok, "=~") {
		key := tok[1:]
		if err := checkKey(key); err != nil {
			return Term{}, badToken(tok, err.Error())
		}
		return Term{Key: key, Op: OpNotExists}, nil
	}

	key, op, raw, found := splitOperator(tok)
	if !found {
		if err := checkKey(tok); err != nil {
			return Term{}, badToken(tok, err.Error())
		}
		return Term{Key: tok, Op: OpExists}, nil
	}
	if err := checkKey(key); err != nil {
		return Term{}, badToken(tok, err.Error())
	}

	term := Term{Key: key, Op: op, Value: raw}
	if op == OpMatch || op == OpNotMatch {
		re, err := regexp.Compile(raw)
		if err != nil {
			return Term{}, badToken(tok, "invalid regexp: "+err.Error())
		}
		term.re = re
	}
	return term, nil
}

// splitOperator finds the first operator in tok.
func splitOperator(tok string) (key string, op Op, value string, found bool) {
	for i := 0; i < len(tok); i++ {
		switch {
		case strings.HasPrefix(tok[i:], "!="):
			return tok[:i], OpNotEq, tok[i+2:], true
		case strings.HasPrefix(tok[i:], "!~"):
			return tok[:i], OpNotMatch, tok[i+2:], true
		case tok[i] == '=':
			return tok[:i], OpEq, tok[i+1:], true
		case tok[i] == '~':
			return tok[:i], OpMatch, tok[i+1:], true
		}
	}
	return tok, 0, "", false
}

func checkKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("invalid key %q", key)
	}
	return nil
}

// quoteValue quotes a value that contains a space or a quote.
func quoteValue(v string) string {
	if !strings.ContainsAny(v, " \"") {
		return v
	}
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

func badToken(tok, why string) error {
	return errs.New(errs.Config, "resource.ParseSelector", fmt.Sprintf("bad token %q: %s", tok, why))
}
