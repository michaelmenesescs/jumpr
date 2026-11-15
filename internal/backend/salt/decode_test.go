package salt

import (
	"encoding/json"
	"strings"
	"testing"

	"jumpr/internal/errs"
)

func TestFirstJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"object first", "{\"a\": 1}\n", "{\"a\": 1}\n"},
		{"array first", "[1, 2]\n", "[1, 2]\n"},
		{"indented object", "  {\"a\": 1}", "{\"a\": 1}"},
		{"noise line before", "WARNING: whatever\n{\"a\": 1}\n", "{\"a\": 1}\n"},
		{"many noise lines before", "salt: warning\n\nINFO x\n\t[\"a\"]\n", "[\"a\"]\n"},
		{"keeps the rest verbatim", "noise\n{\"a\": 1}{\"b\": 2}\n", "{\"a\": 1}{\"b\": 2}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FirstJSON([]byte(tt.in))
			if err != nil {
				t.Fatalf("FirstJSON: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("FirstJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFirstJSONNoJSON(t *testing.T) {
	for _, in := range []string{"", "ERROR: boom\nnothing here\n", "   \n\t\n"} {
		_, err := FirstJSON([]byte(in))
		if err == nil {
			t.Fatalf("FirstJSON(%q) succeeded, want an error", in)
		}
		if !errs.Is(err, errs.Parse) {
			t.Errorf("kind = %q, want parse (%v)", errs.KindOf(err), err)
		}
	}
}

func TestFirstJSONMentionsOutput(t *testing.T) {
	_, err := FirstJSON([]byte("ERROR: no master\n"))
	if err == nil || !strings.Contains(err.Error(), "ERROR: no master") {
		t.Fatalf("error = %v, want it to carry the output", err)
	}
}

func TestDecodeObject(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "single object",
			in:   "{\"web-01\": {\"ok\": true}}",
			want: map[string]string{"web-01": "{\"ok\": true}"},
		},
		{
			name: "two concatenated objects",
			in:   "{\"web-01\": 1}{\"web-02\": 2}",
			want: map[string]string{"web-01": "1", "web-02": "2"},
		},
		{
			name: "pretty printed after noise",
			in:   "salt: WARNING\n{\n  \"web-01\": {\n    \"rc\": 0\n  }\n}\n",
			want: map[string]string{"web-01": "{\n    \"rc\": 0\n  }"},
		},
		{
			name: "empty object",
			in:   "{}",
			want: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeObject([]byte(tt.in))
			if err != nil {
				t.Fatalf("DecodeObject: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d keys, want %d (%v)", len(got), len(tt.want), got)
			}
			for k, want := range tt.want {
				raw, ok := got[k]
				if !ok {
					t.Fatalf("missing key %q in %v", k, got)
				}
				if string(raw) != want {
					t.Errorf("key %q = %q, want %q", k, raw, want)
				}
			}
		})
	}
}

func TestDecodeObjectErrors(t *testing.T) {
	for _, in := range []string{"no json at all\n", "[1, 2]"} {
		if _, err := DecodeObject([]byte(in)); !errs.Is(err, errs.Parse) {
			t.Errorf("DecodeObject(%q) kind = %q, want parse (%v)", in, errs.KindOf(err), err)
		}
	}
}

func TestDecodeInto(t *testing.T) {
	type ret struct {
		Value int    `json:"value"`
		Name  string `json:"name"`
	}
	var got ret
	if err := DecodeInto([]byte("salt: notice\n{\"value\": 7, \"name\": \"web\"}\ntrailing\n"), &got); err != nil {
		t.Fatalf("DecodeInto: %v", err)
	}
	if got.Value != 7 || got.Name != "web" {
		t.Errorf("decoded %+v, want {7 web}", got)
	}
}

func TestDecodeIntoList(t *testing.T) {
	var got []string
	if err := DecodeInto([]byte("noise\n[\"a\", \"b\"]"), &got); err != nil {
		t.Fatalf("DecodeInto: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("decoded %v, want [a b]", got)
	}
}

func TestDecodeIntoErrors(t *testing.T) {
	var got map[string]int
	if err := DecodeInto([]byte("{\"a\": \"not a number\"}"), &got); !errs.Is(err, errs.Parse) {
		t.Errorf("kind = %q, want parse (%v)", errs.KindOf(err), err)
	}
	if err := DecodeInto([]byte("nothing"), &got); !errs.Is(err, errs.Parse) {
		t.Errorf("no json: kind = %q, want parse (%v)", errs.KindOf(err), err)
	}
}

func TestNoReturn(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"false", true},
		{"true", false},
		{"null", false},
		{"0", false},
		{`{"a": 1}`, false},
		{`"Minion did not return. [No response]"`, true},
		{`"Minion did not return"`, true},
		{`"No response"`, true},
		{`"everything is fine"`, false},
		{"not json", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := NoReturn(json.RawMessage(tt.raw)); got != tt.want {
			t.Errorf("NoReturn(%s) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestParseJID(t *testing.T) {
	const jid = "20261112135501123456"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"cli sentence", "Executed command with job ID: " + jid + "\n", jid},
		{"json form", `{"jid": "` + jid + `"}`, jid},
		{"json number", `{"jid": ` + jid + `}`, jid},
		{"noise around it", "salt: started job [" + jid + "] at 13:55\n", jid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseJID([]byte(tt.in))
			if err != nil {
				t.Fatalf("ParseJID: %v", err)
			}
			if got != jid {
				t.Errorf("jid = %q, want %q", got, jid)
			}
		})
	}
}

func TestParseJIDMissing(t *testing.T) {
	for _, in := range []string{"", "no job id here", `{"jid": "2026111213550112345"}`, "202611121355011234567"} {
		if got, err := ParseJID([]byte(in)); err == nil {
			t.Errorf("ParseJID(%q) = %q, nil; want an error", in, got)
		} else if !errs.Is(err, errs.Parse) {
			t.Errorf("ParseJID(%q) kind = %q, want parse", in, errs.KindOf(err))
		}
	}
}
