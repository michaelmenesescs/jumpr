package mux_test

import (
	"strings"
	"testing"

	"jumper/internal/mux"
	"jumper/internal/render"
)

// typeKeys feeds keys in order and returns the first yank it produces.
func typeKeys(t *testing.T, c *mux.CopyMode, keys ...string) (string, bool) {
	t.Helper()
	for _, k := range keys {
		done, yank := c.Update(k)
		if done {
			return yank, true
		}
	}
	return "", false
}

func TestCopyModeMovementBounds(t *testing.T) {
	c := mux.NewCopyMode([]string{"alpha", "bravo", "charlie"}, 20, 5)

	if y, x := c.Cursor(); y != 2 || x != 0 {
		t.Fatalf("cursor starts at %d,%d, want the last line 2,0", y, x)
	}

	c.Update("k")
	c.Update("k")
	c.Update("k")
	if y, _ := c.Cursor(); y != 0 {
		t.Fatalf("k past the first line left the cursor at %d, want 0", y)
	}

	for i := 0; i < 10; i++ {
		c.Update("j")
	}
	if y, _ := c.Cursor(); y != 2 {
		t.Fatalf("j past the last line left the cursor at %d, want 2", y)
	}

	c.Update("l")
	if _, x := c.Cursor(); x != 1 {
		t.Fatalf("l moved to column %d, want 1", x)
	}
	c.Update("h")
	c.Update("h")
	if _, x := c.Cursor(); x != 0 {
		t.Fatalf("h past the first column left the cursor at %d, want 0", x)
	}

	c.Update("$")
	if _, x := c.Cursor(); x != len("charlie")-1 {
		t.Fatalf("$ went to column %d, want %d", x, len("charlie")-1)
	}
	c.Update("0")
	if _, x := c.Cursor(); x != 0 {
		t.Fatalf("0 left the cursor at column %d, want 0", x)
	}

	c.Update("g")
	if y, _ := c.Cursor(); y != 0 {
		t.Fatalf("g went to line %d, want 0", y)
	}
	c.Update("G")
	if y, _ := c.Cursor(); y != 2 {
		t.Fatalf("G went to line %d, want the last line", y)
	}

	c.Update("ctrl+u")
	if y, _ := c.Cursor(); y != 0 {
		t.Fatalf("ctrl+u went to line %d, want 0", y)
	}
	c.Update("ctrl+d")
	if y, _ := c.Cursor(); y != 2 {
		t.Fatalf("ctrl+d went to line %d, want the last line", y)
	}

	// Narrow lines clamp the column instead of leaving it past the end.
	c.Update("k")
	c.Update("$")
	if _, x := c.Cursor(); x != len("bravo")-1 {
		t.Fatalf("$ on a short line went to column %d, want %d", x, len("bravo")-1)
	}
}

func TestCopyModeCharSelectionAcrossLines(t *testing.T) {
	c := mux.NewCopyMode([]string{"alpha", "bravo"}, 20, 5)
	yank, done := typeKeys(t, c, "k", "v", "j", "l", "l", "y")
	if !done {
		t.Fatal("y did not finish copy mode")
	}
	if want := "alpha\nbra"; yank != want {
		t.Fatalf("yank = %q, want %q", yank, want)
	}
}

func TestCopyModeLineSelection(t *testing.T) {
	c := mux.NewCopyMode([]string{"alpha", "bravo", "charlie"}, 20, 5)
	yank, done := typeKeys(t, c, "V", "k", "y")
	if !done {
		t.Fatal("y did not finish copy mode")
	}
	if want := "bravo\ncharlie"; yank != want {
		t.Fatalf("line yank = %q, want %q", yank, want)
	}

	t.Run("V again stops selecting", func(t *testing.T) {
		c := mux.NewCopyMode([]string{"alpha", "bravo"}, 20, 5)
		c.Update("V")
		c.Update("V")
		yank, _ := typeKeys(t, c, "y")
		if yank != "bravo" {
			t.Fatalf("yank after cancelling the selection = %q, want the current line", yank)
		}
	})
}

func TestCopyModeSearchWraps(t *testing.T) {
	c := mux.NewCopyMode([]string{"needle one", "x", "needle two"}, 20, 5)
	if _, done := typeKeys(t, c, "/", "n", "e", "e", "d", "l", "e", "enter"); done {
		t.Fatal("enter while searching finished copy mode")
	}
	if y, x := c.Cursor(); y != 0 || x != 0 {
		t.Fatalf("search wrapped to %d,%d, want line 0 column 0", y, x)
	}

	c.Update("n")
	if y, _ := c.Cursor(); y != 2 {
		t.Fatalf("n went to line %d, want 2", y)
	}
	c.Update("N")
	if y, _ := c.Cursor(); y != 0 {
		t.Fatalf("N went to line %d, want 0", y)
	}

	t.Run("no match keeps the cursor", func(t *testing.T) {
		c := mux.NewCopyMode([]string{"alpha", "bravo"}, 20, 5)
		typeKeys(t, c, "/", "z", "z", "enter")
		if y, x := c.Cursor(); y != 1 || x != 0 {
			t.Fatalf("cursor moved to %d,%d on a failed search", y, x)
		}
	})
}

func TestCopyModeYankTextExact(t *testing.T) {
	c := mux.NewCopyMode([]string{"  indented  ", "second"}, 20, 5)
	yank, done := typeKeys(t, c, "y")
	if !done {
		t.Fatal("y did not finish copy mode")
	}
	if want := "second"; yank != want {
		t.Fatalf("yank = %q, want %q", yank, want)
	}

	c = mux.NewCopyMode([]string{"  indented  ", "second"}, 20, 5)
	yank, _ = typeKeys(t, c, "k", "y")
	if want := "  indented  "; yank != want {
		t.Fatalf("yank of the current line = %q, want %q", yank, want)
	}

	t.Run("empty yank on quit", func(t *testing.T) {
		c := mux.NewCopyMode([]string{"alpha"}, 20, 5)
		yank, done := typeKeys(t, c, "q")
		if !done || yank != "" {
			t.Fatalf("q returned (%q, %v), want (\"\", true)", yank, done)
		}
	})

	t.Run("first esc clears the selection", func(t *testing.T) {
		c := mux.NewCopyMode([]string{"alpha", "bravo"}, 20, 5)
		c.Update("v")
		yank, done := typeKeys(t, c, "esc")
		if done {
			t.Fatal("esc with a selection finished copy mode instead of clearing it")
		}
		if yank != "" {
			t.Fatalf("clearing returned yank %q", yank)
		}
		yank, done = typeKeys(t, c, "y")
		if !done || yank != "bravo" {
			t.Fatalf("yank after clearing = (%q, %v), want the current line", yank, done)
		}
	})
}

func TestCopyModeWideRunes(t *testing.T) {
	// "a日本b": a=cell 0, 日=1-2, 本=3-4, b=5.
	c := mux.NewCopyMode([]string{"a日本b"}, 10, 3)
	yank, done := typeKeys(t, c, "v", "l", "l", "y")
	if !done {
		t.Fatal("y did not finish copy mode")
	}
	if want := "a日"; yank != want {
		t.Fatalf("yank ending inside a wide rune = %q, want %q", yank, want)
	}

	c = mux.NewCopyMode([]string{"a日本b"}, 10, 3)
	yank, _ = typeKeys(t, c, "v", "$", "y")
	if want := "a日本b"; yank != want {
		t.Fatalf("yank to the end = %q, want %q", yank, want)
	}

	c = mux.NewCopyMode([]string{"日本", "語"}, 10, 4)
	yank, _ = typeKeys(t, c, "V", "k", "y")
	if want := "日本\n語"; yank != want {
		t.Fatalf("line yank of wide runes = %q, want %q", yank, want)
	}
}

func TestCopyModeViewSize(t *testing.T) {
	c := mux.NewCopyMode([]string{"alpha", "bravo", "charlie"}, 48, 6)
	view := c.View()
	lines := strings.Split(view, "\n")
	if len(lines) != 6 {
		t.Fatalf("View has %d lines, want 6", len(lines))
	}
	for i, line := range lines {
		if w := render.Width(line); w != 48 {
			t.Fatalf("line %d is %d cells, want 48", i, w)
		}
	}
	status := lines[len(lines)-1]
	for _, want := range []string{"[copy]", "3/3", "v:select", "y:yank", "q:quit"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status %q is missing %q", status, want)
		}
	}

	t.Run("cursor scrolls into view", func(t *testing.T) {
		lines := make([]string, 40)
		for i := range lines {
			lines[i] = "line"
		}
		c := mux.NewCopyMode(lines, 20, 5)
		if !strings.Contains(c.View(), "40/40") {
			t.Fatalf("View does not show the cursor on the last line:\n%s", c.View())
		}
	})

	t.Run("selection is reverse video", func(t *testing.T) {
		c := mux.NewCopyMode([]string{"alpha"}, 10, 3)
		c.Update("V")
		if !strings.Contains(c.View(), "\x1b[7m") {
			t.Fatal("a line selection is not drawn in reverse video")
		}
	})
}
