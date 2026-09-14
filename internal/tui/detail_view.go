package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kjelly/hufu/internal/team"
)

func (m Model) detailView() string {
	item := m.findTask(m.detailID)
	if item == nil {
		return "task not found — press esc"
	}

	header := m.renderDetailHeader(item)
	if !m.vpReady {
		return header
	}
	footer := m.footer()
	return header + "\n" + m.vp.View() + "\n" + footer
}

func (m Model) renderDetailHeader(item *team.TodoItem) string {
	icon, iconSt := m.taskIconStyle(item.Status)

	sep := m.styles.dim.Render(strings.Repeat("─", m.width))
	taskNum := ""
	for i, t := range m.tasks {
		if t.ID == item.ID {
			taskNum = fmt.Sprintf(" #%d ", i+1)
			break
		}
	}
	titleLine := m.styles.header.Render("─── Task" + taskNum + "───")

	agentLine := iconSt.Render(icon) + " " + m.styles.agent.Render(sanitizeTerminalText(item.Agent))
	if item.ID == team.CoordTodoID {
		agentLine += " " + m.styles.dim.Render("(coordinator)")
	}

	modelLine := m.styles.dim.Render("model: ")
	if item.Model != "" {
		modelLine += m.styles.dim.Render(sanitizeTerminalText(item.Model))
	} else {
		modelLine += m.styles.dim.Render("—")
	}

	// providerLine exposes which SubagentProvider ran this task (spec.md
	// §36 Phase 7 PR-16) — only when it is something other than the
	// hufu-local default, to keep the common case's detail view
	// uncluttered. Only the Hufu-assigned provider name is ever shown here,
	// never anything provider-issued (session id, transcript) that could
	// carry a secret.
	var providerLine string
	if provider := strings.TrimSpace(item.SubagentProvider); provider != "" && provider != "hufu-local" {
		providerLine = m.styles.dim.Render("provider: ") + m.styles.dim.Render(sanitizeTerminalText(provider))
	}

	var skillsLine string
	if len(item.Skills) > 0 {
		skillsLine = m.styles.skill.Render("skills: " + sanitizeTerminalText(strings.Join(item.Skills, " · ")))
	}

	var injectedLine string
	if len(item.InjectedSkills) > 0 {
		injectedLine = m.styles.dim.Render("auto: ") + m.styles.skill.Render(sanitizeTerminalText(strings.Join(item.InjectedSkills, " · ")))
	}

	var loadedLine string
	if len(item.LoadedSkills) > 0 {
		loadedLine = m.styles.dim.Render("used: ") + m.styles.done.Render(sanitizeTerminalText(strings.Join(item.LoadedSkills, " · ")))
	}

	descWrapped := wrapCells(item.Desc, m.width-4, 4)
	var descLines []string
	for _, l := range descWrapped.Lines {
		descLines = append(descLines, m.styles.dim.Render("  "+l))
	}
	descBlock := strings.Join(descLines, "\n")

	var parts []string
	parts = append(parts, titleLine)
	agentModelLine := agentLine + "  " + modelLine
	if providerLine != "" {
		agentModelLine += "  " + providerLine
	}
	parts = append(parts, agentModelLine)
	if skillsLine != "" {
		parts = append(parts, skillsLine)
	}
	if injectedLine != "" {
		parts = append(parts, injectedLine)
	}
	if loadedLine != "" {
		parts = append(parts, loadedLine)
	}
	parts = append(parts, descBlock)

	if item.Source != "" && item.Source != team.TaskSourceCoordinator {
		parts = append(parts, m.sourceDetailTag(item.Source))
	}
	if item.ParentID != "" {
		parent := m.findTask(item.ParentID)
		if parent != nil {
			parentDesc := truncateLineCells(parent.Agent+": "+parent.Desc, m.width-20)
			parts = append(parts, m.styles.dim.Render("parent: "+item.ParentID+". "+parentDesc))
		} else {
			parts = append(parts, m.styles.dim.Render("parent: #"+item.ParentID))
		}
	}

	var subtaskLines string
	for _, t := range m.tasks {
		if t.ParentID == item.ID {
			subtaskLines += m.renderSubtaskLine(t, m.width)
		}
	}
	if subtaskLines != "" {
		parts = append(parts, m.styles.dim.Render("─── Subtasks ───"))
		parts = append(parts, subtaskLines)
	}

	parts = append(parts, sep)
	return strings.Join(parts, "\n")
}

func (m Model) sourceDetailTag(source string) string {
	switch source {
	case team.TaskSourceAgent:
		return m.styles.agent.Render("[agent]")
	case team.TaskSourceSubagent:
		return m.styles.agent.Render("[subagent]")
	default:
		return ""
	}
}

func (m Model) renderSubtaskLine(t *team.TodoItem, width int) string {
	icon, _ := m.taskIconStyle(t.Status)
	tag := m.sourceTag(t.Source)
	agentLine := truncateLineCells(t.Agent, width-8)
	descWrapped := wrapCells(t.Desc, width-8, 2)
	parts := []string{m.styles.agent.Render(agentLine) + tag}
	for _, dl := range descWrapped.Lines {
		parts = append(parts, m.styles.dim.Render(dl))
	}
	return fmt.Sprintf("  %s %s\n", icon, strings.Join(parts, " "))
}

// vpHeight is the number of lines available for the detail viewport.
func (m Model) vpHeight() int {
	descLines := (m.width - 4) / 80
	if descLines < 2 {
		descLines = 2
	}
	headerLines := 6 + descLines // title(1) + agent+model(1) + skills(1) + desc(N) + sep(1) + buffer(2)
	if m.detailID != "" {
		if item := m.findTask(m.detailID); item != nil {
			if len(item.InjectedSkills) > 0 {
				headerLines++
			}
			if len(item.LoadedSkills) > 0 {
				headerLines++
			}
		}
	}
	h := m.height - headerLines
	if h < 1 {
		h = 1
	}
	return h
}

func (m Model) buildDetailContent() string {
	lines := m.logs[m.detailID]
	if len(lines) == 0 {
		item := m.findTask(m.detailID)
		if item != nil && item.Status == team.TaskSkipped {
			return m.styles.dim.Render("  (task was not executed)")
		}
		return m.styles.dim.Render("  (no output yet)")
	}
	width := m.width
	if width < 20 {
		width = 20
	}
	if m.inVisual {
		return m.buildVisualContent(lines, width)
	}
	var result strings.Builder
	for i, entry := range lines {
		if i > 0 {
			result.WriteString("\n")
		}
		wrapped := wrapText(m.styleLogEntry(entry), width-2) // leave space for indicator
		if i == m.cursorLine {
			result.WriteString(m.styles.cursor.Render("› ") + wrapped)
		} else {
			result.WriteString("  " + wrapped)
		}
	}
	return result.String()
}

func (m Model) buildVisualContent(lines []string, width int) string {
	start := min(m.visualStart, m.visualEnd)
	end := max(m.visualStart, m.visualEnd)
	if start < 0 {
		start = 0
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}
	var result strings.Builder
	for i, entry := range lines {
		if i > 0 {
			result.WriteString("\n")
		}
		wrapped := wrapText(m.styleLogEntry(entry), width-2)
		var line string
		if i == m.cursorLine {
			line = m.styles.cursor.Render("› ") + wrapped
		} else {
			line = "  " + wrapped
		}

		if i >= start && i <= end {
			result.WriteString(m.styles.visual.Render(line))
		} else {
			result.WriteString(line)
		}
	}
	return result.String()
}

func (m Model) styleLogEntry(entry string) string {
	trimmed := strings.TrimSpace(entry)
	switch {
	case strings.HasPrefix(trimmed, "[step "):
		return m.styles.stepHeader.Render(entry)
	case strings.HasPrefix(trimmed, "⟹"):
		return m.styles.toolCall.Render(entry)
	case strings.HasPrefix(trimmed, "✗"), strings.HasPrefix(trimmed, "⚠"):
		return m.styles.errorIcon.Render(entry)
	case strings.HasPrefix(trimmed, "✓"):
		return m.styles.done.Render(entry)
	case strings.HasPrefix(trimmed, "💭"):
		return m.styles.agent.Render(entry)
	case strings.HasPrefix(trimmed, "💬"):
		return m.styles.textLog.Render(entry)
	default:
		return entry
	}
}

func wrapLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	var result strings.Builder
	line := strings.Builder{}
	pendingANSI := strings.Builder{}
	lineVisible := 0

	flushLine := func() {
		if line.Len() > 0 {
			if pendingANSI.Len() > 0 {
				line.WriteString(pendingANSI.String())
				pendingANSI.Reset()
			}
			result.WriteString(line.String())
			result.WriteByte('\n')
			line.Reset()
			lineVisible = 0
		}
	}

	i := 0
	for i < len(s) {
		// Capture any pending ANSI sequences
		if s[i] == '\x1b' {
			j := i
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				j++
			}
			pendingANSI.WriteString(s[i:j])
			i = j
			continue
		}

		// Collect the next word (non-whitespace sequence)
		wordStart := i
		wordLen := 0
		for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\x1b' {
			_, size := utf8.DecodeRuneInString(s[i:])
			wordLen++
			i += size
		}
		// Count visible characters (runes)
		runeCount := 0
		for j := wordStart; j < i; {
			_, size := utf8.DecodeRuneInString(s[j:])
			runeCount++
			j += size
		}

		// Check if word fits on current line
		if lineVisible > 0 && lineVisible+1+wordLen > width {
			flushLine()
		}

		// Emit word (possibly split if wider than width)
		for runeCount > 0 {
			chunkLen := width - lineVisible
			if chunkLen <= 0 {
				flushLine()
				chunkLen = width
			}
			if chunkLen > runeCount {
				chunkLen = runeCount
			}

			extracted := chunkLen

			// Extract chunk from current position in word
			var chunkBuilder strings.Builder
			pos := wordStart
			for chunkLen > 0 {
				r, size := utf8.DecodeRuneInString(s[pos:])
				chunkBuilder.WriteRune(r)
				pos += size
				chunkLen--
			}

			// Flush pending ANSI before first content on line
			if lineVisible == 0 && pendingANSI.Len() > 0 {
				line.WriteString(pendingANSI.String())
				pendingANSI.Reset()
			}

			line.WriteString(chunkBuilder.String())
			lineVisible += extracted
			runeCount -= extracted
			wordStart = pos
		}

		// Add space after word (if we stopped at whitespace)
		if i < len(s) && s[i] != '\x1b' && (s[i] == ' ' || s[i] == '\t') {
			if line.Len() > 0 {
				line.WriteByte(s[i])
				lineVisible++
			}
			i++
		}
	}

	if line.Len() > 0 {
		result.WriteString(line.String())
	}

	return result.String()
}

func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	parts := strings.Split(s, "\n")
	var result strings.Builder
	for pi, part := range parts {
		if pi > 0 {
			result.WriteString("\n")
		}
		result.WriteString(wrapLine(part, width))
	}
	return result.String()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (m Model) getVisualSelection() string {
	lines := m.logs[m.detailID]
	if len(lines) == 0 {
		return ""
	}
	start := min(m.visualStart, m.visualEnd)
	end := max(m.visualStart, m.visualEnd)
	if start < 0 {
		start = 0
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}
	if start > end {
		start, end = end, start
	}
	selected := lines[start : end+1]
	return strings.Join(selected, "\n")
}

func (m Model) mapRenderedLineToLogIndex(renderedLine int, width int) int {
	lines := m.logs[m.detailID]
	currRenderedLine := 0
	for i, entry := range lines {
		wrapped := wrapText(entry, width-2)
		linesInWrapped := len(strings.Split(wrapped, "\n"))
		if linesInWrapped == 0 {
			linesInWrapped = 1
		}
		if renderedLine >= currRenderedLine && renderedLine < currRenderedLine+linesInWrapped {
			return i
		}
		currRenderedLine += linesInWrapped
	}
	return -1
}

func copyToClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		if text == "" {
			return copySuccessMsg{Lines: 0}
		}
		osc52 := "\x1b]52;c;" + encodeOSC52(text) + "\x1b\\"
		fmt.Fprint(os.Stderr, osc52)
		lines := strings.Count(text, "\n") + 1
		return copySuccessMsg{Lines: lines}
	}
}

func encodeOSC52(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (m Model) colItems(col int) []*team.TodoItem {
	var out []*team.TodoItem
	for _, t := range m.tasks {
		switch {
		case col == 0 && t.Status == team.TaskPending:
			out = append(out, t)
		case col == 1 && t.Status == team.TaskPlanned:
			out = append(out, t)
		case col == 2 && (t.Status == team.TaskInProgress || t.Status == team.TaskPaused || t.Status == team.TaskVerifying || t.Status == team.TaskProtocolIncomplete):
			out = append(out, t)
		case col == 3 && t.Status == team.TaskDone:
			out = append(out, t)
		case col == 4 && t.Status == team.TaskSkipped:
			out = append(out, t)
		case col == 5 && (t.Status == team.TaskError || t.Status == team.TaskBlocked):
			out = append(out, t)
		}
	}
	return out
}

func (m Model) findTask(id string) *team.TodoItem {
	for _, t := range m.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// ── Log-line renderers (called by the status reporter in cmd/hufu) ────────────

// RenderStep returns a formatted step-header log line.
func RenderStep(stepNumber int) string {
	return fmt.Sprintf("[step %d]", stepNumber)
}

// RenderToolCall returns a formatted tool-call log line.
func RenderToolCall(toolName, args string) string {
	if toolName == "bash" {
		args = strings.TrimSpace(args)
		lines := strings.Split(args, "\n")
		indented := make([]string, len(lines))
		for i, l := range lines {
			indented[i] = "  " + l
		}
		return "⟹ " + toolName + "\n" + strings.Join(indented, "\n")
	}
	if len(args) > 4000 {
		r := []rune(args)
		if len(r) > 4000 {
			args = string(r[:4000]) + "…"
		}
	}
	return "⟹ " + toolName + "\n  " + args
}

func RenderToolResult(toolName, result string) string {
	if len(result) > 4000 {
		r := []rune(result)
		if len(r) > 4000 {
			result = string(r[:4000]) + "…"
		}
	}
	return "✓ " + toolName + "\n  " + result
}

// RenderText returns a formatted text-delta log line.
func RenderText(text string) string {
	return "💬 " + strings.TrimSpace(text)
}
