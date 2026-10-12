package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
)

func TestCLIStyleSetFollowsOutputColorProfile(t *testing.T) {
	tests := []struct {
		name      string
		noColor   bool
		profile   colorprofile.Profile
		wantANSI  bool
		want256   bool
		wantColor bool
	}{
		{name: "no tty", profile: colorprofile.NoTTY},
		{name: "ascii", profile: colorprofile.ASCII},
		{name: "no color flag", noColor: true, profile: colorprofile.TrueColor},
		{name: "16 colors", profile: colorprofile.ANSI, wantANSI: true, wantColor: true},
		{name: "256 colors", profile: colorprofile.ANSI256, wantANSI: true, wantColor: true, want256: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			styles := NewCLIStyleSet(ThemeDark, tt.noColor, tt.profile)
			rendered := styles.Error.Render("boom") + styles.Header.Render("head")
			if got := strings.Contains(rendered, "\x1b["); got != tt.wantANSI {
				t.Fatalf("ANSI present = %v, want %v: %q", got, tt.wantANSI, rendered)
			}
			if got := hasColorSGR(rendered); got != tt.wantColor {
				t.Fatalf("color present = %v, want %v: %q", got, tt.wantColor, rendered)
			}
			// The 16-color profile must downsample every color, so no
			// extended (38;5 / 38;2) sequence survives.
			if !tt.want256 && strings.Contains(rendered, "38;5;") {
				t.Fatalf("extended color leaked into a 16-color profile: %q", rendered)
			}
		})
	}
}

func TestDashboardViewDeclaresTerminalModes(t *testing.T) {
	m := New("prompt", TeamInfo{})
	m.width, m.height = 100, 30
	if v := m.View(); !v.AltScreen || v.MouseMode != 0 {
		t.Fatalf("view = alt %v mouse %v, want alt screen without mouse", v.AltScreen, v.MouseMode)
	}
}
