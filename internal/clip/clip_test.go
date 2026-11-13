package clip_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"jumper/internal/clip"
)

// fakeClip is a Clipboard that records what it was given or fails on purpose.
type fakeClip struct {
	text string
	err  error
	used bool
}

func (f *fakeClip) Copy(text string) error {
	f.used = true
	f.text = text
	return f.err
}

func TestOSC52ExactBytes(t *testing.T) {
	cases := []struct {
		name string
		tmux bool
		want string
	}{
		{"plain", false, "\x1b]52;c;aGk=\x07"},
		{"tmux", true, "\x1bPtmux;\x1b\x1b]52;c;aGk=\x07\x1b\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := clip.NewOSC52(&buf, tc.tmux)
			if err := c.Copy("hi"); err != nil {
				t.Fatalf("Copy: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("OSC52 bytes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOSC52TooLong(t *testing.T) {
	var buf bytes.Buffer
	c := clip.NewOSC52(&buf, false)
	err := c.Copy(strings.Repeat("a", 60000))
	if err == nil {
		t.Fatal("Copy of a 60,000 byte text returned no error")
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote %d bytes after refusing, want 0", buf.Len())
	}
}

// errWriter always fails, so the write error is propagated.
type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errors.New("closed") }

func TestOSC52WriteError(t *testing.T) {
	c := clip.NewOSC52(errWriter{}, false)
	if err := c.Copy("hi"); err == nil {
		t.Fatal("Copy with a failing writer returned no error")
	}
}

func TestClipMultiFallsThrough(t *testing.T) {
	t.Run("second succeeds", func(t *testing.T) {
		first := &fakeClip{err: errors.New("no pbcopy")}
		second := &fakeClip{}
		if err := clip.Multi(first, second).Copy("hello"); err != nil {
			t.Fatalf("Multi: %v", err)
		}
		if !first.used || !second.used {
			t.Fatalf("used = %v/%v, want both tried", first.used, second.used)
		}
		if second.text != "hello" {
			t.Fatalf("second clipboard got %q, want %q", second.text, "hello")
		}
	})

	t.Run("first succeeds", func(t *testing.T) {
		first := &fakeClip{}
		second := &fakeClip{err: errors.New("must not run")}
		if err := clip.Multi(first, second).Copy("hi"); err != nil {
			t.Fatalf("Multi: %v", err)
		}
		if second.used {
			t.Fatal("Multi kept going after a success")
		}
	})

	t.Run("all fail", func(t *testing.T) {
		err := clip.Multi(&fakeClip{err: errors.New("a")}, &fakeClip{err: errors.New("b")}).Copy("hi")
		if err == nil {
			t.Fatal("Multi of two failing clipboards returned no error")
		}
		if !strings.Contains(err.Error(), "b") {
			t.Fatalf("error %q does not name the last failure", err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if err := clip.Multi().Copy("hi"); err == nil {
			t.Fatal("Multi() of no clipboards returned no error")
		}
	})
}

func TestClipPbcopy(t *testing.T) {
	var name string
	var stdin []byte
	c := clip.NewPbcopy(func(n string, in []byte) error {
		name, stdin = n, in
		return nil
	})
	if err := c.Copy("hello"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if name != "pbcopy" {
		t.Fatalf("ran %q, want pbcopy", name)
	}
	if string(stdin) != "hello" {
		t.Fatalf("stdin = %q, want %q", stdin, "hello")
	}

	t.Run("failure", func(t *testing.T) {
		c := clip.NewPbcopy(func(string, []byte) error { return errors.New("boom") })
		if err := c.Copy("hi"); err == nil {
			t.Fatal("a failing runner returned no error")
		}
	})

	t.Run("no runner", func(t *testing.T) {
		if err := clip.NewPbcopy(nil).Copy("hi"); err == nil {
			t.Fatal("a nil runner returned no error")
		}
	})
}
