package resource

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"jumper/internal/errs"
)

func TestParseSelector(t *testing.T) {
	tests := []struct {
		name  string
		input string
		terms []Term
		sets  []string
	}{
		{
			name:  "full example",
			input: `role=web env=prod dc~ewr|lax !maint host~^db @saved @investigation`,
			terms: []Term{
				{Key: "role", Op: OpEq, Value: "web"},
				{Key: "env", Op: OpEq, Value: "prod"},
				{Key: "dc", Op: OpMatch, Value: "ewr|lax"},
				{Key: "maint", Op: OpNotExists},
				{Key: "host", Op: OpMatch, Value: "^db"},
			},
			sets: []string{"saved", "investigation"},
		},
		{
			name:  "every operator",
			input: `a=1 b!=2 c~x d!~y e !f`,
			terms: []Term{
				{Key: "a", Op: OpEq, Value: "1"},
				{Key: "b", Op: OpNotEq, Value: "2"},
				{Key: "c", Op: OpMatch, Value: "x"},
				{Key: "d", Op: OpNotMatch, Value: "y"},
				{Key: "e", Op: OpExists},
				{Key: "f", Op: OpNotExists},
			},
		},
		{
			name:  "quoted value with space",
			input: `name="a b" env=prod`,
			terms: []Term{
				{Key: "name", Op: OpEq, Value: "a b"},
				{Key: "env", Op: OpEq, Value: "prod"},
			},
		},
		{
			name:  "escaped quote inside quotes",
			input: `name="a\"b"`,
			terms: []Term{{Key: "name", Op: OpEq, Value: `a"b`}},
		},
		{
			name:  "backslash kept inside quotes",
			input: `host~"^db\d+"`,
			terms: []Term{{Key: "host", Op: OpMatch, Value: `^db\d+`}},
		},
		{
			name:  "regex may contain equals",
			input: `label~^a=b`,
			terms: []Term{{Key: "label", Op: OpMatch, Value: "^a=b"}},
		},
		{
			name:  "key punctuation allowed",
			input: `salt.role=db k8s.io/node-role=worker`,
			terms: []Term{
				{Key: "salt.role", Op: OpEq, Value: "db"},
				{Key: "k8s.io/node-role", Op: OpEq, Value: "worker"},
			},
		},
		{
			name:  "empty value",
			input: `note=`,
			terms: []Term{{Key: "note", Op: OpEq, Value: ""}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSelector(tt.input)
			if err != nil {
				t.Fatalf("ParseSelector(%q): %v", tt.input, err)
			}
			if got.Raw != tt.input {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.input)
			}
			assertTerms(t, got.Terms, tt.terms)
			if !reflect.DeepEqual(got.Sets, tt.sets) {
				t.Errorf("Sets = %v, want %v", got.Sets, tt.sets)
			}
		})
	}
}

func TestParseSelectorEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n "} {
		got, err := ParseSelector(in)
		if err != nil {
			t.Fatalf("ParseSelector(%q): %v", in, err)
		}
		if !got.IsZero() {
			t.Errorf("ParseSelector(%q) = %+v, want zero", in, got)
		}
	}
}

func TestParseSelectorErrors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantTok string
	}{
		{name: "bad regex", input: `host~(`, wantTok: "host~("},
		{name: "bad not-regex", input: `host!~[`, wantTok: "host!~["},
		{name: "bad key", input: `1abc=x`, wantTok: "1abc=x"},
		{name: "bad key punctuation", input: `a!b=x`, wantTok: "a!b=x"},
		{name: "empty not-exists key", input: `!`, wantTok: "!"},
		{name: "empty set name", input: `@`, wantTok: "@"},
		{name: "unterminated quote", input: `name="a b`, wantTok: `name="a b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSelector(tt.input)
			if err == nil {
				t.Fatalf("ParseSelector(%q) = nil error, want error", tt.input)
			}
			if !errs.Is(err, errs.Config) {
				t.Errorf("kind = %q, want config", errs.KindOf(err))
			}
			if !strings.Contains(err.Error(), strconv.Quote(tt.wantTok)) {
				t.Errorf("error %q does not name token %q", err, tt.wantTok)
			}
		})
	}
}

func TestSelectorMatches(t *testing.T) {
	labels := map[string]string{
		"role": "web",
		"env":  "prod",
		"dc":   "ewr",
		"host": "web-db-01",
		"zone": "a",
	}
	tests := []struct {
		name string
		sel  string
		want bool
	}{
		{name: "eq present", sel: `role=web`, want: true},
		{name: "eq present mismatch", sel: `role=db`, want: false},
		{name: "eq missing", sel: `missing=x`, want: false},
		{name: "not-eq present different", sel: `role!=db`, want: true},
		{name: "not-eq present equal", sel: `role!=web`, want: false},
		{name: "not-eq missing", sel: `missing!=x`, want: true},
		{name: "match present", sel: `dc~ewr`, want: true},
		{name: "match unanchored", sel: `host~db`, want: true},
		{name: "match alternation", sel: `dc~ewr|lax`, want: true},
		{name: "match present no match", sel: `dc~lax`, want: false},
		{name: "match missing", sel: `missing~x`, want: false},
		{name: "not-match present different", sel: `dc!~lax`, want: true},
		{name: "not-match present same", sel: `dc!~ewr`, want: false},
		{name: "not-match missing", sel: `missing!~x`, want: true},
		{name: "exists present", sel: `zone`, want: true},
		{name: "exists missing", sel: `maint`, want: false},
		{name: "not-exists missing", sel: `!maint`, want: true},
		{name: "not-exists present", sel: `!zone`, want: false},
		{name: "conjunction all hold", sel: `role=web env=prod dc~ewr|lax !maint host~^web-db`, want: true},
		{name: "conjunction one fails", sel: `role=web env=dev`, want: false},
		{name: "sets ignored", sel: `role=web @saved`, want: true},
		{name: "empty selector matches", sel: ``, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, err := ParseSelector(tt.sel)
			if err != nil {
				t.Fatalf("ParseSelector(%q): %v", tt.sel, err)
			}
			if got := sel.Matches(labels); got != tt.want {
				t.Errorf("Matches(%v) = %v, want %v", labels, got, tt.want)
			}
		})
	}

	sel, err := ParseSelector(`role=web`)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Matches(nil) {
		t.Errorf("Matches(nil) = true, want false")
	}
}

func TestSelectorStringRoundTrip(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: ``, want: ``},
		{input: `  role=web   env=prod  `, want: `role=web env=prod`},
		{input: `role=web env=prod dc~ewr|lax !maint host~^db @saved @investigation`,
			want: `role=web env=prod dc~ewr|lax !maint host~^db @saved @investigation`},
		{input: `name="a b"`, want: `name="a b"`},
		{input: `a=1 b!=2 c~x d!~y e !f @set`, want: `a=1 b!=2 c~x d!~y e !f @set`},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			first, err := ParseSelector(tt.input)
			if err != nil {
				t.Fatalf("ParseSelector(%q): %v", tt.input, err)
			}
			text := first.String()
			if text != tt.want {
				t.Fatalf("String() = %q, want %q", text, tt.want)
			}
			second, err := ParseSelector(text)
			if err != nil {
				t.Fatalf("ParseSelector(%q): %v", text, err)
			}
			assertTerms(t, second.Terms, first.Terms)
			if !reflect.DeepEqual(second.Sets, first.Sets) {
				t.Errorf("sets after round-trip = %v, want %v", second.Sets, first.Sets)
			}
			if got := second.String(); got != text {
				t.Errorf("second String() = %q, want %q", got, text)
			}
		})
	}
}

func TestSelectorQuotedValueRoundTrip(t *testing.T) {
	sel, err := ParseSelector(`name="a b" note="say \"hi\""`)
	if err != nil {
		t.Fatalf("ParseSelector: %v", err)
	}
	if got, want := sel.Terms[1].Value, `say "hi"`; got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}
	text := sel.String()
	back, err := ParseSelector(text)
	if err != nil {
		t.Fatalf("ParseSelector(%q): %v", text, err)
	}
	assertTerms(t, back.Terms, sel.Terms)
}

func assertTerms(t *testing.T, got, want []Term) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("terms = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Key != want[i].Key || got[i].Op != want[i].Op || got[i].Value != want[i].Value {
			t.Errorf("term %d = %+v, want %+v", i, got[i], want[i])
		}
		if want[i].Op == OpMatch || want[i].Op == OpNotMatch {
			if got[i].re == nil {
				t.Errorf("term %d has no compiled regexp", i)
			}
		}
	}
}
