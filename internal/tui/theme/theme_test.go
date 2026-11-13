package theme

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"jumper/internal/resource"
)

// allStyles returns every style field of the dark theme.
func allStyles(t *testing.T) []lipgloss.Style {
	t.Helper()
	th := Dark()
	if th == nil {
		t.Fatal("Dark() returned nil")
	}
	return []lipgloss.Style{
		th.Base, th.Dim, th.Faint, th.Bold, th.Accent, th.OK, th.Warn, th.Crit,
		th.Header, th.Selected, th.Border, th.BorderFocused, th.StatusBar,
		th.HintKey, th.HintText, th.Flash.Info, th.Flash.OK, th.Flash.Warn,
		th.Flash.Error, th.Title, th.Crumb,
	}
}

func TestDarkRendersEveryStyle(t *testing.T) {
	SetProfile("256")
	for i, s := range allStyles(t) {
		if got := s.Render("x"); !strings.Contains(got, "x") {
			t.Errorf("style %d: Render(%q) = %q, want it to contain x", i, "x", got)
		}
	}
}

func TestDarkColors(t *testing.T) {
	SetProfile("256")
	th := Dark()
	cases := []struct {
		name  string
		style lipgloss.Style
		code  string
	}{
		{"OK", th.OK, "38;5;42"},
		{"Warn", th.Warn, "38;5;214"},
		{"Crit", th.Crit, "38;5;203"},
		{"Accent", th.Accent, "38;5;75"},
		{"Dim", th.Dim, "38;5;245"},
		{"Faint", th.Faint, "38;5;240"},
	}
	for _, c := range cases {
		if got := c.style.Render("x"); !strings.Contains(got, c.code) {
			t.Errorf("%s.Render(x) = %q, want colour %s", c.name, got, c.code)
		}
	}
	if got := th.Selected.Render("x"); !strings.Contains(got, "48;5;237") {
		t.Errorf("Selected.Render(x) = %q, want background 48;5;237", got)
	}
}

func TestStatusStyle(t *testing.T) {
	SetProfile("256")
	th := Dark()
	cases := []struct {
		status resource.Status
		want   lipgloss.Style
	}{
		{resource.StatusUnknown, th.Base},
		{resource.StatusOK, th.OK},
		{resource.StatusWarn, th.Warn},
		{resource.StatusCrit, th.Crit},
		{resource.StatusNA, th.Faint},
	}
	for _, c := range cases {
		got := th.StatusStyle(c.status).Render("host")
		if want := c.want.Render("host"); got != want {
			t.Errorf("StatusStyle(%v) = %q, want %q", c.status, got, want)
		}
	}
	// A status outside the known set falls back to Base rather than panicking.
	if got, want := th.StatusStyle(resource.Status(99)).Render("x"), th.Base.Render("x"); got != want {
		t.Errorf("StatusStyle(unknown value) = %q, want Base %q", got, want)
	}
}

func TestSetProfileNoneHasNoEscapes(t *testing.T) {
	SetProfile("none")
	for i, s := range allStyles(t) {
		if got := s.Render("x"); strings.Contains(got, "\x1b") {
			t.Errorf("style %d rendered escape with profile none: %q", i, got)
		}
	}
	// "auto" keeps the profile already in force.
	SetProfile("auto")
	if got := Dark().Crit.Render("x"); strings.Contains(got, "\x1b") {
		t.Errorf("SetProfile(auto) changed the profile: %q", got)
	}
}

func TestSetProfileNamed(t *testing.T) {
	SetProfile("16")
	if got := Dark().OK.Render("x"); !strings.Contains(got, "\x1b[92m") {
		t.Errorf("16-colour OK = %q, want the bright ANSI green 92", got)
	}
	SetProfile("256")
	if got := Dark().OK.Render("x"); !strings.Contains(got, "38;5;42") {
		t.Errorf("256-colour OK = %q, want 38;5;42", got)
	}
	// An unknown name leaves the current profile alone.
	SetProfile("bogus")
	if got := Dark().OK.Render("x"); !strings.Contains(got, "38;5;42") {
		t.Errorf("unknown profile changed the profile: %q", got)
	}
}
