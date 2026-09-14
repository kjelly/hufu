package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kjelly/hufu/internal/utils"
)

func (m Model) footer() string {
	if !m.owner && !m.inDetail {
		return m.styles.footer.Render("read-only view · q/esc close · Enter detail · / search · T theme")
	}
	if m.inDetail {
		if m.inVisual {
			start := min(m.visualStart, m.visualEnd)
			end := max(m.visualStart, m.visualEnd)
			lineCount := end - start + 1
			return m.styles.visualLabel.Render("-- VISUAL --") + " " +
				m.styles.footer.Render(fmt.Sprintf("%d line%s selected · ", lineCount, pluralS(lineCount))) +
				m.styles.bold.Render("y") + m.styles.footer.Render(" copy · ") +
				m.styles.bold.Render("v/esc") + m.styles.footer.Render(" cancel · ") +
				m.styles.bold.Render("j/k") + m.styles.footer.Render(" extend")
		}
		return m.styles.footer.Render("J/K/ctrl+d/u ↑↓ scroll · t attach PTY · v visual · T theme · esc back")
	}
	if m.inInfo {
		return m.styles.footer.Render("i/esc close · ↑↓ scroll")
	}
	if m.inHelp {
		return m.styles.footer.Render("?/esc close")
	}
	if m.inSearch {
		return m.styles.footer.Render("enter search · esc cancel")
	}
	if len(m.searchResults) > 0 {
		return m.styles.footer.Render(fmt.Sprintf("n/N next/prev match (%d/%d) · / search · i info · esc clear · g/G top/bot · J/K/ctrl+d/u scroll · ↑↓ j/k · enter detail · q quit", m.searchIdx+1, len(m.searchResults)))
	}
	if m.finished {
		if m.IsChat {
			return m.styles.footer.Render("g/G top/bot · J/K/ctrl+d/u scroll · / search · r report · i info · c chat · ↑↓ j/k · enter detail · q quit")
		}
		return m.styles.footer.Render("g/G top/bot · J/K/ctrl+d/u scroll · / search · r report · i info · ↑↓ j/k · enter detail · q quit")
	}
	return m.styles.footer.Render("owner view · esc quit options · ctrl+c finish tasks/force · / search · i info · L learning/evidence · c prompt · T theme · ? help · enter detail")
}

func (m Model) helpView() string {
	box := m.styles.infoBox.Render(`hufu TUI — keyboard reference

Columns (dashboard)
  h / l  ←/→        switch column
  tab               cycle through all 6 columns
  j / k  ↑/↓        move cursor in column
  g / G             first / last item
  ctrl+d / ctrl+u   half-page down / up
  enter             open detail view for the focused task

Search
  /                 open search dialog
  n / N             next / previous match

Actions
  c                 inject prompt / chat
  i                 show team info panel
  a                 full-screen activity log
  M                 memory (STM/LTM) view
  L                 evidence/context/learning/promotion panel
  m                 toggle mouse support
  T                 cycle auto/light/dark/mono theme
  r                 generate report (only when finished)
  ?  or  F1         this help screen
  q                 close a finished or read-only view
  esc               return / owner quit options
  ctrl+c            owner: request wrap-up (1st) / force quit (2nd)

Detail view
  j/k ↑/↓           scroll one line
  g / G             top / bottom
  v                 enter VISUAL mode
  y                 yank selection to clipboard (VISUAL only)
  t                 attach to the task PTY (Ctrl-] returns control)
  esc / backspace   return to columns
`)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func (m Model) confirmView() string {
	noLabel := " No "
	yesLabel := " Yes "
	forceLabel := " Force "
	noStyled := m.styles.confirmNormal.Render(noLabel)
	yesStyled := m.styles.confirmNormal.Render(yesLabel)
	forceStyled := m.styles.confirmNormal.Render(forceLabel)
	switch m.confirmChoice {
	case 0:
		noStyled = m.styles.confirmHighlight.Render(noLabel)
	case 1:
		yesStyled = m.styles.confirmHighlight.Render(yesLabel)
	case 2:
		forceStyled = m.styles.confirmHighlight.Render(forceLabel)
	}
	buttons := noStyled + "  " + yesStyled + "  " + forceStyled
	dialog := m.styles.confirmBox.Render("  Quit hufu?" + "\n\n" + buttons + "\n")
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

func (m Model) promptInputView() string {
	m.promptInput.Width = max(m.width-8, 20)
	content := m.styles.prompt.Render("─── Additional Prompt ───") + "\n\n" +
		m.promptInput.View() + "\n\n" +
		m.styles.dim.Render("enter submit · esc cancel")
	dialog := m.styles.promptInputBox.Render(content)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

func (m Model) searchView() string {
	m.searchInput.Width = max(m.width-8, 20)
	content := m.styles.prompt.Render("─── Search ───") + "\n\n" +
		m.searchInput.View() + "\n\n" +
		m.styles.dim.Render("enter search · esc cancel")
	dialog := m.styles.searchBox.Render(content)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

func (m Model) infoPanelView() string {
	info := m.teamInfo
	var b strings.Builder
	b.WriteString(m.styles.header.Render("─── Info ───"))
	b.WriteString("\n\n")

	if len(info.AvailableTeams) > 0 {
		b.WriteString(m.styles.bold.Render("Teams: "))
		b.WriteString(strings.Join(info.AvailableTeams, ", "))
		b.WriteString("\n")
	}

	if info.TeamName != "" {
		b.WriteString(m.styles.bold.Render("Team:  "))
		b.WriteString(m.styles.team.Render(info.TeamName))
		b.WriteString("\n")
	}

	if len(info.Agents) > 0 {
		b.WriteString(m.styles.bold.Render("Agents: "))
		var agentDisplay []string
		for _, a := range info.Agents {
			agentDisplay = append(agentDisplay, fmt.Sprintf("%s (%s)", m.styles.agent.Render(a.Name), m.styles.dim.Render(a.Role)))
		}
		b.WriteString(strings.Join(agentDisplay, ", "))
		b.WriteString("\n")
	}

	if info.MemoryEnabled {
		b.WriteString(m.styles.done.Render("✓") + " " + m.styles.bold.Render("Memory: "))
		b.WriteString("enabled")
		if info.MemoryModel != "" {
			fmt.Fprintf(&b, " (model: %s)", info.MemoryModel)
		}
		b.WriteString("\n")
	}

	if len(info.Skills) > 0 {
		b.WriteString(m.styles.bold.Render("Skills: "))
		b.WriteString(strings.Join(info.Skills, ", "))
		b.WriteString("\n")
	}

	if info.SidecarModel != "" {
		b.WriteString(m.styles.bold.Render("Sidecar: "))
		b.WriteString(info.SidecarModel)
		b.WriteString("\n")
	}

	if info.GuardModel != "" {
		b.WriteString(m.styles.bold.Render("Guard:   "))
		b.WriteString(info.GuardModel)
		b.WriteString("\n")
	}

	if len(info.Decisions) > 0 {
		b.WriteString(m.styles.bold.Render("Decisions: "))
		var decisionDisplay []string
		for _, rawDecision := range info.Decisions {
			decision := rawDecision.Redacted()
			status := "active"
			if decision.Stale {
				status = "stale"
			} else if decision.Outcome != nil {
				status = decision.Outcome.ResolvedOutcome
			}
			if decision.FinalizationStale {
				status += "; finalization stale"
			}
			finalizer := strings.Trim(strings.Join([]string{
				decision.FinalizationMode,
				decision.FinalizationIdentity,
				decision.FinalizationOutcome,
			}, "/"), "/")
			if finalizer != "" {
				status += "; " + finalizer
			}
			if len(decision.FinalizationWarnings) > 0 {
				status += fmt.Sprintf("; %d finalization warning(s)", len(decision.FinalizationWarnings))
			}
			recordRef := decision.EffectiveRecordRef()
			if recordRef.ID != "" {
				status += "; record_ref=" + utils.RedactSecrets(recordRef.ID)
			}
			decisionDisplay = append(decisionDisplay, fmt.Sprintf("%s (%s)", utils.RedactSecrets(decision.DecisionID), status))
		}
		b.WriteString(strings.Join(decisionDisplay, ", "))
		b.WriteString("\n")
	}

	if info.SSHSessions > 0 {
		b.WriteString(m.styles.bold.Render("SSH:     "))
		fmt.Fprintf(&b, "%d active session(s)\n", info.SSHSessions)
	}

	b.WriteString("\n" + m.styles.dim.Render("esc close"))

	content := utils.RedactSecrets(b.String())
	dialog := m.styles.infoBox.Render(content)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

func (m *Model) loadMemoryContent() {
	var b strings.Builder
	b.WriteString(m.styles.header.Render("─── Memory ───"))
	b.WriteString("\n\n")

	ws := m.teamInfo.Workspace

	if ws != "" {
		data, err := os.ReadFile(filepath.Join(ws, "stm.md"))
		if err == nil && len(data) > 0 {
			content := strings.TrimSpace(string(data))
			if content != "" {
				b.WriteString(m.styles.bold.Render("Short-term Memory (stm.md)"))
				b.WriteString("\n\n")
				b.WriteString(m.renderMemory(content))
				b.WriteString("\n\n")
			} else {
				b.WriteString(m.styles.dim.Render("Short-term memory is empty."))
				b.WriteString("\n\n")
			}
		} else {
			b.WriteString(m.styles.dim.Render("Short-term memory is empty."))
			b.WriteString("\n\n")
		}
	}

	if ws != "" && m.teamInfo.TeamName != "" {
		data, err := os.ReadFile(filepath.Join(ws, fmt.Sprintf("ltm-%s.md", m.teamInfo.TeamName)))
		if err == nil && len(data) > 0 {
			content := strings.TrimSpace(string(data))
			if content != "" {
				b.WriteString(m.styles.bold.Render("Long-term Memory (ltm.md)"))
				b.WriteString("\n\n")
				b.WriteString(m.renderMemory(content))
				b.WriteString("\n\n")
			} else {
				b.WriteString(m.styles.dim.Render("Long-term memory is empty."))
				b.WriteString("\n\n")
			}
		} else {
			b.WriteString(m.styles.dim.Render("Long-term memory is empty."))
			b.WriteString("\n\n")
		}
	}

	b.WriteString(m.styles.dim.Render("esc back"))

	w := m.width - 4
	if w < 10 {
		w = 10
	}
	h := m.height - 2
	if h < 3 {
		h = 3
	}
	m.memoryVP = viewport.New(w, h)
	m.memoryVP.SetContent(b.String())
	m.memoryVP.GotoTop()
	m.memoryReady = true
}

func (m Model) renderMemory(content string) string {
	lines := strings.Split(content, "\n")
	var b strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			b.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(trimmed, "# ") {
			b.WriteString(m.styles.bold.Render(trimmed))
		} else if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			b.WriteString("  " + m.styles.dim.Render(trimmed))
		} else {
			runes := []rune(trimmed)
			if len(runes) > 80 {
				trimmed = string(runes[:80]) + "..."
			}
			b.WriteString(m.styles.dim.Render(trimmed))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (m Model) memoryView() string {
	if !m.memoryReady {
		return ""
	}
	footer := m.styles.footer.Render("j/k ↑↓ scroll · esc back")
	return m.memoryVP.View() + "\n" + footer
}

func (m Model) updateMemory(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.inMemory = false
		m.memoryReady = false
		return m, nil
	case "ctrl+c":
		return m.handleCtrlC()
	case "q":
		if m.finished {
			return m, tea.Quit
		}
	case "g":
		if m.memoryReady {
			m.memoryVP.GotoTop()
		}
		return m, nil
	case "G":
		if m.memoryReady {
			m.memoryVP.GotoBottom()
		}
		return m, nil
	}
	if m.memoryReady {
		var cmd tea.Cmd
		m.memoryVP, cmd = m.memoryVP.Update(msg)
		return m, cmd
	}
	return m, nil
}

// ── Detail view ───────────────────────────────────────────────────────────────
