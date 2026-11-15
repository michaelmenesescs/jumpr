package views

import (
	"strings"
	"testing"
	"time"

	teatest "github.com/charmbracelet/x/exp/teatest"

	"jumpr/internal/backend"
	"jumpr/internal/backend/fake"
	"jumpr/internal/errs"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// asciiSet returns the ascii glyph set every assertion here renders with.
func asciiSet(t *testing.T) render.GlyphSet {
	t.Helper()
	set, err := render.Glyphs("ascii")
	if err != nil {
		t.Fatalf("render.Glyphs(ascii): %v", err)
	}
	return set
}

// sparkColumn returns the fake host kind's spark column.
func sparkColumn(t *testing.T) resource.Column {
	t.Helper()
	b := fake.New(fake.Options{})
	for _, c := range hostKind(t, b).Columns {
		if c.Kind == resource.ColSpark {
			return c
		}
	}
	t.Fatal("fake host kind has no spark column")
	return resource.Column{}
}

func TestTableAsciiGlyphsSparkCell(t *testing.T) {
	col := sparkColumn(t)
	row := resource.Resource{Fields: map[string]any{col.ID: []float64{1, 4, 2, 8, 3}}}
	got := cellTextWith(asciiSet(t), col, row, fixedNow, col.Width)
	want := render.SparkWith(asciiSet(t), []float64{1, 4, 2, 8, 3}, col.Width)
	if got != want {
		t.Fatalf("spark cell = %q, want %q", got, want)
	}
	if w := render.Width(got); w != col.Width {
		t.Errorf("spark cell is %d cells, want %d", w, col.Width)
	}
	for _, r := range got {
		if r >= 0x80 {
			t.Errorf("spark cell %q contains %U", got, r)
		}
	}
	// The unicode default is untouched: cellText still draws Unicode blocks.
	unicodeCell := cellText(col, row, fixedNow, col.Width)
	if unicodeCell != render.Spark([]float64{1, 4, 2, 8, 3}, col.Width) {
		t.Errorf("unicode spark cell = %q, want the task 009 rendering", unicodeCell)
	}
}

func TestTableAsciiGlyphsFrame(t *testing.T) {
	b := fake.New(fake.Options{})
	tbl := NewTable(testEnv(), b, hostKind(t, b), resource.Query{Kind: "host"}).WithGlyphs(asciiSet(t))
	tbl = drain(t, tbl)

	for _, size := range [][2]int{{120, 30}, {80, 20}, {40, 10}, {160, 5}} {
		out := tbl.Render(size[0], size[1])
		for _, r := range out {
			if r > 0x7f {
				t.Fatalf("frame %dx%d contains %U\n%s", size[0], size[1], r, out)
			}
		}
		lines := strings.Split(out, "\n")
		if len(lines) != size[1] {
			t.Fatalf("frame %dx%d has %d lines", size[0], size[1], len(lines))
		}
		for _, line := range lines {
			if w := render.Width(line); w != size[0] {
				t.Fatalf("frame %dx%d line %q is %d cells", size[0], size[1], line, w)
			}
		}
	}
}

func TestTableAsciiGlyphsErrorFrame(t *testing.T) {
	opts := fake.Options{FailHosts: map[resource.HostID]errs.Kind{
		"web-ewr-007": errs.Unreachable,
		"web-ewr-012": errs.Timeout,
	}}
	b := fake.New(opts)
	tbl := NewTable(testEnv(), b, hostKind(t, b), resource.Query{Kind: "host"}).WithGlyphs(asciiSet(t))
	tbl = drain(t, tbl)
	if len(tbl.errs) == 0 {
		t.Fatal("no failed hosts to render")
	}

	for _, size := range [][2]int{{120, 30}, {60, 8}, {40, 6}} {
		out := tbl.Render(size[0], size[1])
		if !strings.Contains(out, "unreachable") {
			t.Fatalf("frame %dx%d does not show the error rows:\n%s", size[0], size[1], out)
		}
		for _, r := range out {
			if r > 0x7f {
				t.Fatalf("frame %dx%d contains %U\n%s", size[0], size[1], r, out)
			}
		}
		lines := strings.Split(out, "\n")
		if len(lines) != size[1] {
			t.Fatalf("frame %dx%d has %d lines", size[0], size[1], len(lines))
		}
		for _, line := range lines {
			if w := render.Width(line); w != size[0] {
				t.Fatalf("frame %dx%d line %q is %d cells", size[0], size[1], line, w)
			}
		}
	}
}

func TestTableAsciiGlyphsBannerSeparator(t *testing.T) {
	b := fake.New(fake.Options{})
	tbl := NewTable(testEnv(), b, hostKind(t, b), resource.Query{Kind: "host"}).WithGlyphs(asciiSet(t))
	tbl = drain(t, tbl)
	if got := tbl.banner(); !strings.Contains(got, "1,512 hosts | 0 errors |") {
		t.Errorf("banner = %q, want ascii separators", got)
	}
}

// TestGlyphsTableFromEnv checks that a table takes the active set from the
// environment instead of defaulting to unicode: no WithGlyphs call needed.
func TestGlyphsTableFromEnv(t *testing.T) {
	env := testEnv()
	env.Glyphs = asciiSet(t)
	b := fake.New(fake.Options{})
	tbl := drain(t, NewTable(env, b, hostKind(t, b), resource.Query{Kind: "host"}))
	out := tbl.Render(80, 12)
	if strings.IndexFunc(out, func(r rune) bool { return r > 0x7f }) != -1 {
		t.Fatalf("table from an ascii env is not ASCII:\n%s", out)
	}
	if !strings.Contains(out, "1,512 hosts |") {
		t.Fatalf("table from an ascii env has no ascii banner:\n%s", out)
	}
}

// newAsciiRoot builds the whole application model over the fake backend with
// the ascii set in the environment, exactly as internal/app wires it.
func newAsciiRoot(t *testing.T) *tui.Root {
	t.Helper()
	theme.SetProfile("none")
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		t.Fatalf("register fake backend: %v", err)
	}
	env := &tui.Env{
		Registry:  reg,
		Session:   &tui.SessionState{Name: "main"},
		Theme:     theme.Dark(),
		Glyphs:    asciiSet(t),
		Keys:      keymap.Default(),
		Now:       func() time.Time { return fixedNow },
		Factories: make(map[resource.KindName]tui.Factory),
	}
	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	factory := func(env *tui.Env, q resource.Query) (tui.View, error) {
		b, spec, ok := env.Registry.Lookup(string(q.Kind))
		if !ok {
			return nil, errs.New(errs.Config, "views.newAsciiRoot", "unknown kind "+string(q.Kind))
		}
		return NewTable(env, b, spec, q), nil
	}
	return tui.NewRoot(env, cmds, factory, resource.Query{Kind: "host"}, "")
}

// TestAsciiFrame renders one full application frame at 120x34 with the ascii
// set and requires zero runes above 0x7f: status line, crumbs, banner, header,
// rows, empty cells and the hint bar.
func TestAsciiFrame(t *testing.T) {
	root := newAsciiRoot(t)
	tm := teatest.NewTestModel(t, root, teatest.WithInitialTermSize(120, 34))
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return strings.Contains(string(b), "1,512 hosts")
	}, teatest.WithDuration(5*time.Second))
	tm.Quit()
	m := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
	final, ok := m.(*tui.Root)
	if !ok {
		t.Fatalf("final model is %T, want *tui.Root", m)
	}

	frame := final.View()
	if frame == "" {
		t.Fatal("frame is empty")
	}
	if i := strings.IndexFunc(frame, func(r rune) bool { return r > 0x7f }); i != -1 {
		line := strings.Count(frame[:i], "\n") + 1
		t.Fatalf("frame has a non-ASCII rune %U on line %d:\n%s", []rune(frame[i:])[0], line, frame)
	}
	lines := strings.Split(frame, "\n")
	if len(lines) != 34 {
		t.Fatalf("frame has %d lines, want 34", len(lines))
	}
	for i, line := range lines {
		if w := render.Width(line); w != 120 {
			t.Errorf("line %d is %d cells, want 120", i, w)
		}
	}
	// The frame is the real thing: the banner, host rows, the ascii health dot
	// and the ascii hint bar all appear.
	for _, want := range []string{"1,512 hosts", "web-", "+fake", "<:> cmd"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame does not contain %q:\n%s", want, frame)
		}
	}
}
