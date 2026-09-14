package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

func (m *Model) loadOperatorContent() {
	width := max(m.width-4, 10)
	height := max(m.height-2, 3)
	m.operatorVP = viewport.New(width, height)
	m.operatorVP.SetContent(m.buildOperatorContent())
	m.operatorVP.GotoTop()
	m.operatorReady = true
}

func (m Model) buildOperatorContent() string {
	var output strings.Builder
	output.WriteString(m.styles.header.Render("─── Evidence · Context · Learning · Promotion ───"))
	output.WriteString("\n\n")
	output.WriteString(m.styles.bold.Render("Evidence"))
	output.WriteByte('\n')
	if !m.hasOperatorSummary || !m.operatorEvidence.Available {
		output.WriteString(m.styles.dim.Render("unavailable: no audit-verified evidence projection"))
		output.WriteByte('\n')
	} else {
		fmt.Fprintf(&output, "manifest=%s verdict=%s acceptance=%s requirements=%d artifacts=%d findings=%d\n",
			unknownIfEmpty(m.operatorEvidence.ManifestStatus), unknownIfEmpty(m.operatorEvidence.Verdict),
			unknownIfEmpty(m.operatorEvidence.Acceptance), m.operatorEvidence.Requirements,
			m.operatorEvidence.Artifacts, m.operatorEvidence.Findings)
		output.WriteString(m.styles.dim.Render("Only audit-verified verdicts are verification; raw claims remain claims."))
		output.WriteByte('\n')
	}

	output.WriteString("\n" + m.styles.bold.Render("Context") + "\n")
	if task := m.focusedTask(); task != nil {
		fmt.Fprintf(&output, "task=%s context_manifests=%d memory_manifests=%d\n", sanitizeTerminalText(task.ID), len(task.ContextManifests), len(task.MemoryManifests))
		for _, manifest := range task.ContextManifests {
			fmt.Fprintf(&output, "  attempt=%d agent=%s items=%d fingerprint=%s\n", manifest.Attempt, sanitizeTerminalText(manifest.Agent), len(manifest.Items), shortOperatorRef(manifest.Fingerprint))
		}
		for _, manifest := range task.MemoryManifests {
			fmt.Fprintf(&output, "  recall attempt=%d agent=%s items=%d policy=%s retrieval=%s\n", manifest.Attempt, sanitizeTerminalText(manifest.Agent), len(manifest.Items), sanitizeTerminalText(manifest.PolicyVersion), shortOperatorRef(manifest.RetrievalID))
		}
		output.WriteString(m.styles.dim.Render("Content stays hidden here; use scoped inspect context with explicit --show-content when authorized."))
		output.WriteByte('\n')
	} else {
		output.WriteString(m.styles.dim.Render("No focused task; select a task on the dashboard before opening this panel."))
		output.WriteByte('\n')
	}

	learning := m.operatorSnapshot.Learning
	output.WriteString("\n" + m.styles.bold.Render("Learning") + "\n")
	fmt.Fprintf(&output, "status=%s requested=%s effective=%s policy=%s\n", unknownIfEmpty(learning.Status), unknownIfEmpty(learning.RequestedMode), unknownIfEmpty(learning.EffectiveMode), unknownIfEmpty(learning.PolicyVersion))
	fmt.Fprintf(&output, "recall: exposed=%s consulted=%s\nusage: applied=%s rejected=%s\noutcome: verified=%s causal_failures=%s\n",
		operatorCount(learning.Exposures), operatorCount(learning.Consulted), operatorCount(learning.Applied), operatorCount(learning.Rejected), operatorCount(learning.VerifiedSupport), operatorCount(learning.CausalFailures))
	if learning.EmptyState != "" {
		fmt.Fprintf(&output, "state=%s\n", sanitizeTerminalText(learning.EmptyState))
	}
	if learning.UnavailableReason != "" {
		fmt.Fprintf(&output, "note=%s\n", sanitizeTerminalText(learning.UnavailableReason))
	}

	output.WriteString("\n" + m.styles.bold.Render("Promotion") + "\n")
	if m.operatorPromotionStatus != "available" {
		output.WriteString(m.styles.dim.Render("Promotion state is unavailable; proposal count is unknown."))
		output.WriteByte('\n')
	} else if len(m.operatorPromotions) == 0 {
		output.WriteString(m.styles.dim.Render("No proposals. Zero is a valid empty state; no draft is created by viewing."))
		output.WriteByte('\n')
	}
	for _, proposal := range m.operatorPromotions {
		fmt.Fprintf(&output, "%s  %s  %s  %s  sources=%d\n", shortOperatorRef(proposal.ID), sanitizeTerminalText(proposal.Status), sanitizeTerminalText(proposal.Type), sanitizeTerminalText(proposal.TargetPath), proposal.SourceCount)
	}
	if m.operatorPromotionLimited {
		fmt.Fprintf(&output, "Showing %d of %d proposals; use the scoped CLI list for the full result.\n", len(m.operatorPromotions), m.operatorPromotionTotal)
	}
	output.WriteString("\n" + m.styles.bold.Render("Next") + "\n")
	if m.operatorScope.WorkspaceExact != "" && m.operatorScope.ProjectID != "" && m.operatorScope.TeamName != "" {
		writeOperatorCommand(&output, []string{"hufu", "context", "learning", "--workspace", m.operatorScope.WorkspaceExact, "--project", m.operatorScope.ProjectID, "--team", m.operatorScope.TeamName})
		writeOperatorCommand(&output, []string{"hufu", "context", "promotion", "review", "--workspace", m.operatorScope.WorkspaceExact, "--project", m.operatorScope.ProjectID, "--team", m.operatorScope.TeamName})
		writeOperatorCommand(&output, []string{"hufu", "skill", "list", "--workspace", m.operatorScope.WorkspaceExact})
		writeOperatorCommand(&output, []string{"hufu", "improve", "--workspace", m.operatorScope.WorkspaceExact, "--team", m.operatorScope.TeamName})
	}
	output.WriteString(m.styles.dim.Render("Skill draft review/promote and context promotion are distinct lifecycles. Promotion review is human-gated: approve does not apply; publication needs a separate exact confirmation."))
	return output.String()
}

func writeOperatorCommand(output *strings.Builder, argv []string) {
	if command, ok := operator.RenderArgv(argv, "sh"); ok {
		output.WriteString(command)
		output.WriteByte('\n')
	}
}

func (m Model) operatorView() string {
	if !m.operatorReady {
		return ""
	}
	return m.operatorVP.View() + "\n" + m.styles.footer.Render("j/k ↑↓ scroll · g/G top/bottom · esc back")
}

func (m Model) updateOperator(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc", "backspace", "q", "L":
		m.inOperator = false
		return m, nil
	case "ctrl+c":
		return m.handleCtrlC()
	case "g":
		m.operatorVP.GotoTop()
		return m, nil
	case "G":
		m.operatorVP.GotoBottom()
		return m, nil
	}
	var command tea.Cmd
	m.operatorVP, command = m.operatorVP.Update(message)
	return m, command
}

func (m Model) focusedTask() *team.TodoItem {
	items := m.colItems(m.col)
	if m.row < 0 || m.row >= len(items) {
		return nil
	}
	return items[m.row]
}

func operatorCount(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *value)
}

func unknownIfEmpty(value string) string {
	if value == "" {
		return "unknown"
	}
	return sanitizeTerminalText(value)
}

func shortOperatorRef(value string) string {
	value = sanitizeTerminalText(value)
	runes := []rune(value)
	if len(runes) <= 20 {
		return value
	}
	return string(runes[:17]) + "…"
}
