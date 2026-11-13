package shq

import (
	"math/rand"
	"os/exec"
	"strings"
	"testing"
)

func TestQuoteTable(t *testing.T) {
	cases := map[string]string{
		"":          "''",
		"abc":       "abc",
		"a-b/c.d:e": "a-b/c.d:e",
		"a b":       "'a b'",
		"it's":      `'it'\''s'`,
		`say "hi"`:  `'say "hi"'`,
		"$VAR":      "'$VAR'",
		"`id`":      "'`id`'",
		"a\nb":      "'a\nb'",
		"a;b":       "'a;b'",
		"héllo":     "'héllo'",
		"a'b'c":     `'a'\''b'\''c'`,
		"a\x00b":    "ab",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q)=%q want %q", in, got, want)
		}
	}
}

func TestJoin(t *testing.T) {
	if got := Join("ls", "-l", "a b", ""); got != "ls -l 'a b' ''" {
		t.Fatal(got)
	}
	if Join() != "" {
		t.Fatal("empty join")
	}
}

func TestQuoteRoundTripLocal(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	rng := rand.New(rand.NewSource(1))
	alphabet := []rune("abcXYZ019 _-./'\"\\$`;&|<>(){}*?!#~\n\t")
	for i := 0; i < 500; i++ {
		n := rng.Intn(20)
		var b strings.Builder
		for j := 0; j < n; j++ {
			b.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		s := b.String()
		out, err := exec.Command("sh", "-c", "printf %s "+Quote(s)).Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(out) != s {
			t.Fatalf("round trip %q -> %q", s, out)
		}
	}
}

func TestWrapSudo(t *testing.T) {
	if got, want := Wrap("id", false), "sh -c 'export LC_ALL=C LANG=C; id'"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got, want := Wrap("id", true), "sudo -n -- sh -c 'export LC_ALL=C LANG=C; id'"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestValidUnitName(t *testing.T) {
	for _, s := range []string{"nginx.service", "getty@tty1.service", "a:b_c-d"} {
		if !ValidUnitName(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "a b", "a;b", "a$b", "a\nb", strings.Repeat("a", 257)} {
		if ValidUnitName(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
	if !ValidUnitName(strings.Repeat("a", 256)) {
		t.Error("256 should be valid")
	}
}

func TestValidSignal(t *testing.T) {
	for _, s := range []string{"TERM", "KILL", "HUP", "INT", "USR1", "USR2", "QUIT", "STOP", "CONT", "1", "9", "64"} {
		if !ValidSignal(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "0", "65", "-9", "term", "SIGTERM", "9;ls", "+9", "1 "} {
		if ValidSignal(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}
