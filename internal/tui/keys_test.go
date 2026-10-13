package tui

import (
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

var namedTestKeys = map[string]rune{
	"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab,
	"space": tea.KeySpace, "backspace": tea.KeyBackspace,
	"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
	"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
	"F1": tea.KeyF1,
}

// keyPress builds a v2 key press from a key name ("enter", "ctrl+c",
// "shift+tab", "alt+enter") or from literal typed text ("q", "fix it").
func keyPress(s string) tea.KeyPressMsg {
	var mod tea.KeyMod
	name := s
	for _, prefix := range []struct {
		text string
		mod  tea.KeyMod
	}{{"ctrl+", tea.ModCtrl}, {"alt+", tea.ModAlt}, {"shift+", tea.ModShift}} {
		if rest, ok := strings.CutPrefix(name, prefix.text); ok && rest != "" {
			mod |= prefix.mod
			name = rest
		}
	}
	if code, ok := namedTestKeys[name]; ok {
		msg := tea.KeyPressMsg{Code: code, Mod: mod}
		if code == tea.KeySpace {
			msg.Text = " "
		}
		return msg
	}
	if mod != 0 {
		r, _ := utf8.DecodeRuneInString(name)
		return tea.KeyPressMsg{Code: r, Mod: mod}
	}
	if utf8.RuneCountInString(s) == 1 {
		r, _ := utf8.DecodeRuneInString(s)
		return tea.KeyPressMsg{Code: r, Text: s}
	}
	return tea.KeyPressMsg{Code: tea.KeyExtended, Text: s}
}
