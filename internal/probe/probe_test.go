package probe

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
)

const testNonce = "deadbeef"

func mustBatch(t *testing.T, nonce string, s ...Section) Batch {
	t.Helper()
	b, err := NewBatch(nonce, s...)
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}
	return b
}

func TestNewBatch(t *testing.T) {
	ok := []Section{
		{ID: "loadavg", Cmd: "cat /proc/loadavg"},
		{ID: "sockets", Cmd: "ss -H -tuanp", Sudo: true},
		{ID: "a.b-c_1", Cmd: "true"},
	}
	if _, err := NewBatch(testNonce, ok...); err != nil {
		t.Fatalf("valid batch: %v", err)
	}

	cases := []struct {
		name   string
		nonce  string
		sects  []Section
		reason string
	}{
		{"duplicate id", testNonce, []Section{{ID: "a", Cmd: "true"}, {ID: "a", Cmd: "true"}}, "duplicate"},
		{"upper id", testNonce, []Section{{ID: "Load", Cmd: "true"}}, "id"},
		{"space id", testNonce, []Section{{ID: "lo ad", Cmd: "true"}}, "id"},
		{"empty id", testNonce, []Section{{ID: "", Cmd: "true"}}, "id"},
		{"colon id", testNonce, []Section{{ID: "a:b", Cmd: "true"}}, "id"},
		{"empty cmd", testNonce, []Section{{ID: "a", Cmd: "  "}}, "command"},
		{"heredoc", testNonce, []Section{{ID: "a", Cmd: "cat <<EOF"}}, "heredoc"},
		{"short nonce", "abc", []Section{{ID: "a", Cmd: "true"}}, "nonce"},
		{"non-hex nonce", "zzzzzzzz", []Section{{ID: "a", Cmd: "true"}}, "nonce"},
		{"nonce with colon", "dead:bef", []Section{{ID: "a", Cmd: "true"}}, "nonce"},
	}
	for _, c := range cases {
		_, err := NewBatch(c.nonce, c.sects...)
		if err == nil {
			t.Fatalf("%s: want error", c.name)
		}
		if got := errs.KindOf(err); got != errs.Internal {
			t.Errorf("%s: kind %q want %q", c.name, got, errs.Internal)
		}
		if !strings.Contains(strings.ToLower(err.Error()), c.reason) {
			t.Errorf("%s: %q does not mention %q", c.name, err.Error(), c.reason)
		}
	}
}

func TestScriptShape(t *testing.T) {
	b := mustBatch(t, testNonce,
		Section{ID: "loadavg", Cmd: "cat /proc/loadavg"},
		Section{ID: "sockets", Cmd: "ss -H -tuanp", Sudo: true},
	)
	want := "b() { printf '\\n@@SITREP:deadbeef:%s:B@@\\n' \"$1\"; " +
		"printf '\\n@@SITREP:deadbeef:%s:B@@\\n' \"$1\" >&2; }\n" +
		"e() { printf '\\n@@SITREP:deadbeef:%s:E:%s@@\\n' \"$1\" \"$2\"; " +
		"printf '\\n@@SITREP:deadbeef:%s:E:%s@@\\n' \"$1\" \"$2\" >&2; }\n" +
		"b loadavg; cat /proc/loadavg; e loadavg $?\n" +
		"b sockets; sudo -n -- sh -c 'ss -H -tuanp'; e sockets $?\n"
	if got := b.Script(); got != want {
		t.Fatalf("script mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestCommand(t *testing.T) {
	b := mustBatch(t, testNonce, Section{ID: "a", Cmd: "echo a"})
	c := b.Command(3 * time.Second)
	if c.Sudo {
		t.Error("batch command must not be sudo on the transport level")
	}
	if c.Script != b.Script() {
		t.Error("Command.Script must be Batch.Script()")
	}
	if c.Timeout != 3*time.Second {
		t.Errorf("timeout %v want 3s", c.Timeout)
	}
}

// TestSplitRoundTripLocal runs the generated script with sh and splits its real
// output. It needs no network, no lab and no SSH agent.
func TestSplitRoundTripLocal(t *testing.T) {
	b := mustBatch(t, testNonce,
		Section{ID: "a", Cmd: "echo a"},
		Section{ID: "err", Cmd: "( echo err >&2; exit 3 )"},
		Section{ID: "nonl", Cmd: "printf 'no newline'"},
		Section{ID: "missing", Cmd: "jumpr-no-such-command-xyz"},
	)
	cmd := exec.Command("sh", "-c", b.Script())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The script's own exit status is that of the last `e` printf, which
		// succeeds even when a section failed.
		t.Fatalf("script run: %v", err)
	}

	res := b.Split(stdout.Bytes(), stderr.Bytes())
	if len(res) != 4 {
		t.Fatalf("got %d results want 4: %v", len(res), res)
	}

	if r := res["a"]; r.Missing || r.RC != 0 || r.Stdout != "a\n" || r.Err != nil {
		t.Errorf("a: %+v", r)
	}
	if r := res["err"]; r.Missing || r.RC != 3 || r.Stdout != "" || r.Stderr != "err\n" || r.Err != nil {
		t.Errorf("err: %+v", r)
	}
	if r := res["nonl"]; r.Missing || r.RC != 0 || r.Stdout != "no newline" || r.Err != nil {
		t.Errorf("nonl: %+v", r)
	}
	r := res["missing"]
	if r.Missing {
		t.Errorf("missing: unexpected Missing: %+v", r)
	}
	if r.RC != 127 {
		t.Errorf("missing: rc %d want 127", r.RC)
	}
	if got := errs.KindOf(r.Err); got != errs.NotAvailable {
		t.Errorf("missing: kind %q want %q", got, errs.NotAvailable)
	}
}

func TestSplitTruncated(t *testing.T) {
	b := mustBatch(t, testNonce, Section{ID: "a", Cmd: "echo a"}, Section{ID: "b", Cmd: "echo b"})
	stdout := []byte("\n@@SITREP:deadbeef:a:B@@\nhello\n\n@@SITREP:deadbeef:a:E:0@@\n" +
		"\n@@SITREP:deadbeef:b:B@@\npart")
	res := b.Split(stdout, nil)
	if len(res) != 2 {
		t.Fatalf("got %d results want 2", len(res))
	}
	if r := res["a"]; r.Missing || r.Stdout != "hello\n" || r.RC != 0 {
		t.Errorf("a (complete): %+v", r)
	}
	r := res["b"]
	if !r.Missing {
		t.Fatalf("b: want Missing: %+v", r)
	}
	if got := errs.KindOf(r.Err); got != errs.Remote {
		t.Errorf("b: kind %q want %q", got, errs.Remote)
	}
	if r.Err == nil || !strings.Contains(r.Err.Error(), "no result") {
		t.Errorf("b: err %v", r.Err)
	}

	// A section with no B marker at all is missing too.
	none := b.Split([]byte("no markers here\n"), nil)
	if !none["a"].Missing || !none["b"].Missing {
		t.Errorf("no markers: want both missing: %v", none)
	}
}

func TestSplitIgnoresForeignMarkers(t *testing.T) {
	b := mustBatch(t, testNonce, Section{ID: "a", Cmd: "echo a"})
	inner := "@@SITREP:cafebabe:x:B@@\nfake\n@@SITREP:cafebabe:x:E:0@@\n"
	stdout := []byte("\n@@SITREP:deadbeef:a:B@@\n" + inner + "\n@@SITREP:deadbeef:a:E:0@@\n")
	stderr := []byte("\n@@SITREP:cafebabe:x:B@@\njunk\n@@SITREP:cafebabe:x:E:1@@\n")
	res := b.Split(stdout, stderr)
	if len(res) != 1 {
		t.Fatalf("got %d results want 1: %v", len(res), res)
	}
	r := res["a"]
	if r.Missing {
		t.Fatalf("a: unexpected Missing")
	}
	if r.Stdout != inner {
		t.Errorf("a stdout %q want %q", r.Stdout, inner)
	}
	if r.Stderr != "" || r.RC != 0 {
		t.Errorf("a: foreign stderr leaked: %+v", r)
	}
}

func TestSudoSectionClassified(t *testing.T) {
	b := mustBatch(t, testNonce, Section{ID: "sockets", Cmd: "ss -H -tuanp", Sudo: true})
	stdout := []byte("\n@@SITREP:deadbeef:sockets:B@@\n\n@@SITREP:deadbeef:sockets:E:1@@\n")

	t.Run("stderr with markers", func(t *testing.T) {
		stderr := []byte("\n@@SITREP:deadbeef:sockets:B@@\nsudo: a password is required\n" +
			"\n@@SITREP:deadbeef:sockets:E:1@@\n")
		r := b.Split(stdout, stderr)["sockets"]
		if r.Missing || r.RC != 1 {
			t.Fatalf("result: %+v", r)
		}
		if r.Stderr != "sudo: a password is required\n" {
			t.Errorf("stderr %q", r.Stderr)
		}
		if got := errs.KindOf(r.Err); got != errs.Sudo {
			t.Errorf("kind %q want %q", got, errs.Sudo)
		}
	})

	t.Run("bare stderr", func(t *testing.T) {
		r := b.Split(stdout, []byte("sudo: a password is required\n"))["sockets"]
		if got := errs.KindOf(r.Err); got != errs.Sudo {
			t.Errorf("kind %q want %q", got, errs.Sudo)
		}
	})
}

func TestSplitKeepsExactBytes(t *testing.T) {
	b := mustBatch(t, testNonce, Section{ID: "x", Cmd: "true"})
	stdout := []byte("\n@@SITREP:deadbeef:x:B@@\n  spaced  \n\n\n@@SITREP:deadbeef:x:E:0@@\n")
	r := b.Split(stdout, nil)["x"]
	if r.Stdout != "  spaced  \n\n" {
		t.Errorf("stdout %q want %q", r.Stdout, "  spaced  \n\n")
	}
}
