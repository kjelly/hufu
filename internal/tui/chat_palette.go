package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// paletteState is the Ctrl+P command palette: a filter input over the
// command table. Choosing an entry only inserts it into the composer; it
// never runs anything by itself.
type paletteState struct {
	filter textinput.Model
	index  int
}

func (m ChatModel) openPalette() (ChatModel, tea.Cmd) {
	filter := textinput.New()
	filter.Prompt = "/"
	filter.Placeholder = "filter commands"
	filter.SetWidth(40)
	if m.dash.displayPreset == DisplayEpaper {
		staticCursor(&filter)
	}
	cmd := filter.Focus()
	if m.dash.displayPreset == DisplayEpaper {
		cmd = nil
	}
	m.palette = paletteState{filter: filter}
	m.overlay = overlayPalette
	m.view = viewChat
	return m, cmd
}

func (m ChatModel) paletteMatches() []chatCommandSpec {
	query := strings.ToLower(strings.TrimSpace(m.palette.filter.Value()))
	var matches []chatCommandSpec
	for _, spec := range chatCommands {
		if query == "" || strings.Contains(spec.name, query) || strings.Contains(strings.ToLower(spec.summary), query) {
			matches = append(matches, spec)
		}
	}
	return matches
}

func (m ChatModel) updatePaletteKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	matches := m.paletteMatches()
	switch msg.String() {
	case "esc", "ctrl+c":
		m.overlay = overlayNone
		return m, nil
	case "up", "ctrl+k":
		m.palette.index = max(m.palette.index-1, 0)
		return m, nil
	case "down", "ctrl+j":
		m.palette.index = min(m.palette.index+1, max(len(matches)-1, 0))
		return m, nil
	case "enter":
		m.overlay = overlayNone
		if m.palette.index < len(matches) {
			m.composer.SetValue("/" + matches[m.palette.index].name + " ")
			m.rerender()
		}
		return m.focusComposer()
	}
	var cmd tea.Cmd
	m.palette.filter, cmd = m.palette.filter.Update(msg)
	m.palette.index = 0
	return m, cmd
}

func (m ChatModel) paletteView() string {
	s := m.dash.styles
	width := max(min(m.width-8, 72), 20)
	lines := []string{s.header.Render("Commands"), "", m.palette.filter.View(), ""}
	matches := m.paletteMatches()
	if len(matches) == 0 {
		lines = append(lines, s.dim.Render("no matching command"))
	}
	visible := max(m.height-12, 3)
	start := max(m.palette.index-visible+1, 0)
	for i := start; i < len(matches) && i < start+visible; i++ {
		line := truncateCells(padRight(matches[i].usage, 30)+" "+matches[i].summary, width)
		if i == m.palette.index {
			line = s.selectedFG.Render("› " + line)
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", s.dim.Render("↑/↓ select · enter insert · esc close"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, s.infoBox.Render(strings.Join(lines, "\n")))
}
