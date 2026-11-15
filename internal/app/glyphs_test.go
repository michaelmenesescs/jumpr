package app

import (
	"strings"
	"testing"

	"jumpr/internal/errs"
)

// TestGlyphsResolve pins the precedence between ui.glyphs and JUMPR_GLYPHS:
// a non-empty environment value wins, "auto" means unicode, and an unknown
// value on either side is a config error naming its source.
func TestGlyphsResolve(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		env  string
		want string
	}{
		{"config auto", "auto", "", "unicode"},
		{"empty config", "", "", "unicode"},
		{"config unicode", "unicode", "", "unicode"},
		{"config nerd", "nerd", "", "nerd"},
		{"config ascii", "ascii", "", "ascii"},
		{"env overrides auto", "auto", "ascii", "ascii"},
		{"env overrides nerd", "nerd", "ascii", "ascii"},
		{"env unicode", "ascii", "unicode", "unicode"},
		{"env auto", "", "auto", "unicode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			set, err := resolveGlyphs(c.cfg, c.env)
			if err != nil {
				t.Fatalf("resolveGlyphs(%q, %q): %v", c.cfg, c.env, err)
			}
			if set.Name != c.want {
				t.Fatalf("resolveGlyphs(%q, %q).Name = %q, want %q", c.cfg, c.env, set.Name, c.want)
			}
		})
	}
}

// TestGlyphsResolveErrors checks that a bad value is an errs.Config error that
// names the source it came from, in both directions.
func TestGlyphsResolveErrors(t *testing.T) {
	cases := []struct {
		cfg, env string
		source   string
	}{
		{"bogus", "", "ui.glyphs"},
		{"ascii", "bogus", "JUMPR_GLYPHS"},
		{"auto", "blocks", "JUMPR_GLYPHS"},
	}
	for _, c := range cases {
		_, err := resolveGlyphs(c.cfg, c.env)
		if err == nil {
			t.Fatalf("resolveGlyphs(%q, %q) returned no error", c.cfg, c.env)
		}
		if !errs.Is(err, errs.Config) {
			t.Fatalf("resolveGlyphs(%q, %q) kind = %q, want %q", c.cfg, c.env, errs.KindOf(err), errs.Config)
		}
		if msg := err.Error(); !strings.Contains(msg, c.source) {
			t.Errorf("resolveGlyphs(%q, %q) error %q does not name %s", c.cfg, c.env, msg, c.source)
		}
	}
}
