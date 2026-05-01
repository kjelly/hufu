package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
)

var (
	boldStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle     = lipgloss.NewStyle().Faint(true)
	agentStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	toolStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	resultStyle  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("7"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	stepStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))
	doneStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	textStyle    = lipgloss.NewStyle().Faint(true)
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	pendingIcon  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	progressIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	doneIcon     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	errorIcon    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	teamStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
)

var activeStatusFlusher struct {
	mu        sync.Mutex
	w         *lineWriter
	taskDisp  *taskDisplay
	skillDisp *skillDisplay
}

func setStatusFlusher(w *lineWriter, taskDisp *taskDisplay, skillDisp *skillDisplay) {
	activeStatusFlusher.mu.Lock()
	defer activeStatusFlusher.mu.Unlock()
	activeStatusFlusher.w = w
	activeStatusFlusher.taskDisp = taskDisp
	activeStatusFlusher.skillDisp = skillDisp
}

func init() {
	tools.SetOnAskUserDone(func() {
		activeStatusFlusher.mu.Lock()
		w := activeStatusFlusher.w
		td := activeStatusFlusher.taskDisp
		sd := activeStatusFlusher.skillDisp
		activeStatusFlusher.mu.Unlock()
		if w != nil {
			w.flush()
		}
		if td != nil {
			td.refreshIfDirty()
		}
		if sd != nil {
			sd.refreshIfDirty()
		}
	})
}

type lineWriter struct {
	mu  sync.Mutex
	buf []string
}

func (w *lineWriter) write(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if tools.IsAskUserActive() {
		w.buf = append(w.buf, s)
		return
	}
	if len(w.buf) > 0 {
		for _, b := range w.buf {
			fmt.Fprint(os.Stderr, b)
		}
		w.buf = w.buf[:0]
	}
	fmt.Fprint(os.Stderr, s)
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.buf {
		fmt.Fprint(os.Stderr, b)
	}
	w.buf = w.buf[:0]
}

type taskDisplay struct {
	mu      sync.Mutex
	w       *lineWriter
	tracker *team.TaskTracker
	lines   int
	dirty   bool
}

func newTaskDisplay(w *lineWriter, tracker *team.TaskTracker) *taskDisplay {
	return &taskDisplay{w: w, tracker: tracker}
}

func (d *taskDisplay) render() {
	d.mu.Lock()
	defer d.mu.Unlock()

	todoItems := d.tracker.TodoList().Items()
	if len(todoItems) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── TODO ───"))
	b.WriteString("\n")

	for _, t := range todoItems {
		var icon string
		var desc string
		switch t.Status {
		case team.TaskPending:
			icon = pendingIcon.Render("○")
			desc = dimStyle.Render(t.Desc)
		case team.TaskInProgress:
			icon = progressIcon.Render("◑")
			taskPreview := t.Desc
			if len(taskPreview) > 60 {
				taskPreview = taskPreview[:60] + "..."
			}
			desc = taskPreview
		case team.TaskDone:
			icon = doneIcon.Render("●")
			desc = dimStyle.Render(t.Desc)
		case team.TaskError:
			icon = errorIcon.Render("✗")
			if t.Detail != "" {
				desc = errStyle.Render(t.Detail)
			} else {
				desc = dimStyle.Render(t.Desc)
			}
		}
		agentLabel := agentStyle.Render(t.Agent)
		if t.Model != "" {
			agentLabel = fmt.Sprintf("%s %s", agentStyle.Render(t.Agent), dimStyle.Render("["+t.Model+"]"))
		}
		timeStr := formatTodoItemTime(t)
		b.WriteString(fmt.Sprintf("  %s %s %s %s %s\n", icon, dimStyle.Render(t.ID+"."), agentLabel, desc, dimStyle.Render(timeStr)))
	}

	d.w.write(b.String())
	d.lines = len(todoItems) + 2
}

func (d *taskDisplay) clear() {
	if d.lines > 0 {
		d.w.write(fmt.Sprintf("\033[%dA\033[J", d.lines))
		d.lines = 0
	}
}

func (d *taskDisplay) update() {
	if tools.IsAskUserActive() {
		d.mu.Lock()
		d.dirty = true
		d.mu.Unlock()
		return
	}
	d.clear()
	d.render()
}

func (d *taskDisplay) refreshIfDirty() {
	d.mu.Lock()
	dirty := d.dirty
	d.dirty = false
	d.mu.Unlock()
	if dirty {
		d.clear()
		d.render()
	}
}

func formatAgentLabel(event team.StatusEvent) string {
	agent := ""
	if event.TeamName != "" && event.Agent != "" {
		agent = fmt.Sprintf("%s/%s", teamStyle.Render(event.TeamName), agentStyle.Render(event.Agent))
	} else if event.Agent != "" {
		agent = agentStyle.Render(event.Agent)
	}
	if event.Model != "" && agent != "" {
		return fmt.Sprintf("%s %s", agent, dimStyle.Render("["+event.Model+"]"))
	}
	return agent
}

func setupStatusReporter(w *lineWriter, coordinator *team.Coordinator, taskDisp *taskDisplay, skillDisp *skillDisplay, idleTimer *idleWarningTimer) {
	currentAgent := ""
	textBuf := ""

	coordinator.SetStatusReporter(func(event team.StatusEvent) {
		idleTimer.reset()

		switch event.Type {
		case "start":
			if currentAgent != "" && textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			currentAgent = event.Agent
			desc := event.Message
			if len(desc) > 120 {
				desc = desc[:120] + "..."
			}
			w.write(fmt.Sprintf("\n%s %s %s\n",
				headerStyle.Render("▶"),
				formatAgentLabel(event),
				dimStyle.Render("— "+desc),
			))
			taskDisp.update()

		case "step":
			label := event.Message
			if event.Step > 0 {
				label = fmt.Sprintf("step %d", event.Step)
			}
			w.write(fmt.Sprintf("  %s %s\n",
				stepStyle.Render("│"),
				stepStyle.Render(label),
			))

		case "tool_call":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			argsPreview := formatToolArgs(event.ToolName, event.ToolArgs)
			icon := "⟹"
			w.write(fmt.Sprintf("  %s %s %s %s\n",
				toolStyle.Render(icon),
				formatAgentLabel(event),
				toolStyle.Render("›"),
				toolStyle.Render(event.ToolName),
			))
			if argsPreview != "" {
				w.write(fmt.Sprintf("    %s\n", dimStyle.Render(argsPreview)))
			}

		case "tool_result":
			resultLine := ""
			if event.ToolResult != "" {
				lines := strings.Split(event.ToolResult, "\n")
				maxLines := 3
				if len(lines) > maxLines {
					resultLine = strings.Join(lines[:maxLines], "\n    ") + fmt.Sprintf("  ... (%d more lines)", len(lines)-maxLines)
				} else {
					resultLine = strings.Join(lines, "\n    ")
				}
				maxChars := 200
				if len(resultLine) > maxChars {
					resultLine = resultLine[:maxChars] + "..."
				}
				w.write(fmt.Sprintf("  %s %s %s %s\n    %s\n",
					doneStyle.Render("✓"),
					formatAgentLabel(event),
					toolStyle.Render("›"),
					toolStyle.Render(event.ToolName),
					resultStyle.Render(resultLine),
				))
			} else {
				w.write(fmt.Sprintf("  %s %s %s %s\n",
					doneStyle.Render("✓"),
					formatAgentLabel(event),
					toolStyle.Render("›"),
					toolStyle.Render(event.ToolName),
				))
			}

		case "text":
			textBuf += event.Message

		case "done":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			timingStr := ""
			if event.Duration > 0 {
				timingStr = formatTimingBreakdown(event.Duration, event.ModelTime, event.ToolTime)
			}
			if timingStr != "" {
				w.write(fmt.Sprintf("%s %s %s %s\n",
					doneStyle.Render("✓"),
					formatAgentLabel(event),
					doneStyle.Render("done"),
					dimStyle.Render(timingStr),
				))
			} else {
				w.write(fmt.Sprintf("%s %s %s\n",
					doneStyle.Render("✓"),
					formatAgentLabel(event),
					doneStyle.Render("done"),
				))
			}
			currentAgent = ""
			taskDisp.update()

		case "wrap_up":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			w.write(fmt.Sprintf("\n%s\n  %s\n  %s\n",
				boldStyle.Render("─── WRAP UP ───"),
				dimStyle.Render("No new tasks will be started."),
				dimStyle.Render("Running agents will finish their current work..."),
			))

		case "error":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			timingStr := ""
			if event.Duration > 0 {
				timingStr = formatTimingBreakdown(event.Duration, event.ModelTime, event.ToolTime)
			}
			if timingStr != "" {
				w.write(fmt.Sprintf("%s %s: %s %s\n",
					errStyle.Render("✗"),
					formatAgentLabel(event),
					errStyle.Render(event.Message),
					dimStyle.Render(timingStr),
				))
			} else {
				w.write(fmt.Sprintf("%s %s: %s\n",
					errStyle.Render("✗"),
					formatAgentLabel(event),
					errStyle.Render(event.Message),
				))
			}
			taskDisp.update()

		case "todos_updated":
			taskDisp.update()

		case "skill_used":
			if skillDisp != nil {
				skillDisp.record(event.SkillName, event.Agent)
				skillDisp.update()
			}

		case "loop_warning":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			w.write(fmt.Sprintf("%s %s\n",
				errStyle.Render("⚠ LOOP"),
				errStyle.Render(event.Message),
			))

		case "sidecar_call":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			label := "sidecar"
			if event.Agent != "" {
				label = fmt.Sprintf("sidecar/%s", event.Agent)
			}
			msg := event.Message
			if msg == "summarize" {
				msg = "summarizing output"
			}
			w.write(fmt.Sprintf("%s %s %s\n",
				dimStyle.Render("⟐"),
				dimStyle.Render(label),
				dimStyle.Render(msg),
			))

		case "skill_auto_loaded":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			w.write(fmt.Sprintf("%s %s auto-loaded: %s\n",
				dimStyle.Render("⟐"),
				agentStyle.Render(event.Agent),
				doneStyle.Render(event.SkillName),
			))
		}
	})
}

func flushText(agentName, text string) string {
	if text == "" {
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	maxLen := 300
	display := text
	if len(display) > maxLen {
		display = display[:maxLen] + "..."
	}
	return fmt.Sprintf("  %s %s\n",
		textStyle.Render("💬"),
		textStyle.Render(display),
	)
}

func formatTimingBreakdown(total, modelTime, toolTime time.Duration) string {
	if total == 0 {
		return ""
	}
	totalStr := formatDuration(total)
	if toolTime > 0 && modelTime > 0 {
		return fmt.Sprintf("(%s: %s model + %s tools)", totalStr, formatDuration(modelTime), formatDuration(toolTime))
	}
	return fmt.Sprintf("(%s)", totalStr)
}

func formatTodoItemTime(t *team.TodoItem) string {
	if t.EndedAt.IsZero() {
		if t.StartedAt.IsZero() {
			return ""
		}
		return fmt.Sprintf("(%s)", formatDuration(time.Since(t.StartedAt)))
	}
	if t.ModelTime > 0 || t.ToolTime > 0 {
		return formatTimingBreakdown(t.EndedAt.Sub(t.StartedAt), t.ModelTime, t.ToolTime)
	}
	return fmt.Sprintf("(%s)", formatDuration(t.EndedAt.Sub(t.StartedAt)))
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%ds", m, s)
}

func formatToolArgs(toolName, args string) string {
	if args == "" || args == "{}" {
		return ""
	}
	args = strings.ReplaceAll(args, "\n", " ")
	args = strings.TrimSpace(args)
	maxLen := 80
	if toolName == "agent" {
		maxLen = 200
	}
	if toolName == "finish" {
		maxLen = 120
	}
	if len(args) > maxLen {
		return args[:maxLen] + "..."
	}
	return args
}

type skillEntry struct {
	name   string
	count  int
	agents []string
}

type skillDisplay struct {
	mu     sync.Mutex
	w      *lineWriter
	skills map[string]*skillEntry
	lines  int
	dirty  bool
}

func newSkillDisplay(w *lineWriter) *skillDisplay {
	return &skillDisplay{
		w:      w,
		skills: make(map[string]*skillEntry),
	}
}

func (d *skillDisplay) record(name, agent string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := strings.ToLower(name)
	entry, ok := d.skills[key]
	if !ok {
		entry = &skillEntry{name: name}
		d.skills[key] = entry
	}
	entry.count++
	seen := false
	for _, a := range entry.agents {
		if a == agent {
			seen = true
			break
		}
	}
	if !seen {
		entry.agents = append(entry.agents, agent)
	}
	d.dirty = true
}

func (d *skillDisplay) render() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.skills) == 0 {
		return
	}

	ordered := make([]*skillEntry, 0, len(d.skills))
	for _, entry := range d.skills {
		ordered = append(ordered, entry)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].name < ordered[j].name
	})

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── SKILLS ───"))
	b.WriteString("\n")

	for _, entry := range ordered {
		agentList := strings.Join(entry.agents, ", ")
		b.WriteString(fmt.Sprintf("  %s %-20s ×%-2d %s\n",
			doneStyle.Render("✓"),
			entry.name,
			entry.count,
			dimStyle.Render(agentList),
		))
	}

	d.w.write(b.String())
	d.lines = len(ordered) + 2
}

func (d *skillDisplay) clear() {
	if d.lines > 0 {
		d.w.write(fmt.Sprintf("\033[%dA\033[J", d.lines))
		d.lines = 0
	}
}

func (d *skillDisplay) update() {
	if tools.IsAskUserActive() {
		return
	}
	d.clear()
	d.render()
}

func (d *skillDisplay) refreshIfDirty() {
	d.mu.Lock()
	dirty := d.dirty
	d.dirty = false
	d.mu.Unlock()
	if dirty {
		d.clear()
		d.render()
	}
}

func renderSkillSummary(entries []team.SkillUsageEntry) {
	if len(entries) == 0 {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── SKILLS ───"))
	b.WriteString("\n")
	for _, entry := range entries {
		agentList := strings.Join(entry.Agents, ", ")
		b.WriteString(fmt.Sprintf("  %s %-20s ×%-2d %s\n",
			doneStyle.Render("✓"),
			entry.Name,
			entry.Count,
			dimStyle.Render(agentList),
		))
	}
	fmt.Fprint(os.Stderr, b.String())
}
