package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anomalyco/hufu/internal/notify"
	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
	tuipkg "github.com/anomalyco/hufu/internal/tui"
	"github.com/anomalyco/hufu/internal/utils"
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
	thinkStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	pendingIcon  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	progressIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	pausedIcon   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	doneIcon     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	errorIcon    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	skipTagStyle = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("3"))
	doneTagStyle = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("2"))
	errTagStyle  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("9"))
	teamStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	wrapUpStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
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

var askUserMu sync.Mutex

func init() {
	tools.SetOnAskUserTUI(func(ctx context.Context, question, qtype string, opts []tools.AskUserTUIOption, allowAny bool) (string, bool) {
		askUserMu.Lock()
		defer askUserMu.Unlock()

		select {
		case <-ctx.Done():
			return "", false
		default:
		}

		p := activeTUIProgram.Load()
		if p == nil {
			return "", false
		}
		replyCh := make(chan string, 1)
		tuiOpts := make([]tuipkg.AskUserOption, len(opts))
		for i, o := range opts {
			tuiOpts[i] = tuipkg.AskUserOption{Label: o.Label, Value: o.Value}
		}
		p.Send(tuipkg.AskUserMsg{
			Question: question,
			Type:     qtype,
			Options:  tuiOpts,
			AllowAny: allowAny,
			ReplyCh:  replyCh,
		})
		select {
		case resp := <-replyCh:
			return resp, true
		case <-ctx.Done():
			p.Send(tuipkg.AskUserCancelMsg{})
			return "", false
		}
	})

	// path_consent still uses ReleaseTerminal/RestoreTerminal (no native TUI dialog).
	tools.SetOnAskUserStart(func() {
		if p := activeTUIProgram.Load(); p != nil {
			if err := p.ReleaseTerminal(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to release terminal: %v\n", err)
			}
		}
	})

	tools.SetOnAskUserDone(func() {
		if p := activeTUIProgram.Load(); p != nil {
			if err := p.RestoreTerminal(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to restore terminal: %v\n", err)
			}
			return
		}
		// Non-TUI mode: flush any buffered display output.
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
		var tag string
		switch t.Status {
		case team.TaskPending:
			icon = pendingIcon.Render("○")
			desc = dimStyle.Render(t.Desc)
		case team.TaskInProgress:
			icon = progressIcon.Render("◑")
			desc = t.Desc
		case team.TaskDone:
			icon = doneIcon.Render("●")
			desc = dimStyle.Render(t.Desc)
			tag = doneTagStyle.Render(" [DONE]")
		case team.TaskError:
			icon = errorIcon.Render("✗")
			if t.Detail != "" {
				desc = errStyle.Render(t.Detail)
			} else {
				desc = dimStyle.Render(t.Desc)
			}
			tag = errTagStyle.Render(" [ERROR]")
		case team.TaskSkipped:
			icon = dimStyle.Render("—")
			desc = dimStyle.Render(t.Desc)
			tag = skipTagStyle.Render(" [SKIP]")
		case team.TaskPaused:
			icon = pausedIcon.Render("◐")
			desc = dimStyle.Render(t.Desc)
			tag = skipTagStyle.Render(" [WAITING]")
		}
		agentLabel := agentStyle.Render(t.Agent)
		if t.Model != "" {
			agentLabel = fmt.Sprintf("%s %s", agentStyle.Render(t.Agent), dimStyle.Render("["+t.Model+"]"))
		}
		if len(t.DependsOn) > 0 && (t.Status == team.TaskPending || t.Status == team.TaskInProgress) {
			tag = dimStyle.Render(" [after:"+strings.Join(t.DependsOn, ",")+"]") + tag
		}
		timeStr := formatTodoItemTime(t)
		b.WriteString(fmt.Sprintf("  %s %s %s %s%s %s\n", icon, dimStyle.Render(t.ID+"."), agentLabel, desc, tag, dimStyle.Render(timeStr)))
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

func setupStatusReporter(w *lineWriter, coordinator *team.Coordinator, taskDisp *taskDisplay, skillDisp *skillDisplay, idleTimer *idleWarningTimer, notifier *notify.Notifier) {
	var mu sync.Mutex
	currentAgent := ""
	textBuf := ""

	coordinator.SetStatusReporter(func(event team.StatusEvent) {
		mu.Lock()
		defer mu.Unlock()
		idleTimer.reset()

		if notifier != nil {
			notifier.Notify(event.Type, event.Agent, event.Message, event.Output)
		}

		if event.Model != "" {
			idleTimer.SetModel(event.Model)
		}

		switch event.Type {
		case "start":
			if currentAgent != "" && textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			currentAgent = event.Agent
			w.write(fmt.Sprintf("\n%s %s %s\n",
				headerStyle.Render("▶"),
				formatAgentLabel(event),
				dimStyle.Render("— "+event.Message),
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
				if strings.Contains(argsPreview, "\n") {
					for _, line := range strings.Split(argsPreview, "\n") {
						w.write(fmt.Sprintf("    %s\n", dimStyle.Render(line)))
					}
				} else {
					w.write(fmt.Sprintf("    %s\n", dimStyle.Render(argsPreview)))
				}
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
				w.write(fmt.Sprintf("  %s %s %s %s\n    %s\n",
					doneStyle.Render("✓"),
					formatAgentLabel(event),
					toolStyle.Render("›"),
					toolStyle.Render(event.ToolName),
					resultStyle.Render(utils.TruncatePreview(resultLine, 200)),
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

		case "wrap_up_phase":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			w.write(fmt.Sprintf("\n%s %s\n\n",
				wrapUpStyle.Render("⏹"),
				boldStyle.Render(strings.ToUpper(event.Message+"...")),
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

		case "plan_approved":
			w.write(fmt.Sprintf("%s %s\n",
				doneStyle.Render("✓"),
				dimStyle.Render(event.Message),
			))

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

		case "cache_hit":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			w.write(fmt.Sprintf("%s %s %s %s\n",
				dimStyle.Render("⟐"),
				agentStyle.Render(event.Agent),
				doneStyle.Render("✓ cached"),
				dimStyle.Render(utils.TruncateLine(event.Message, 60)),
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

		case "think_skills":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 skills"),
				dimStyle.Render(event.Message),
			))

		case "think_skill_detail":
			w.write(fmt.Sprintf("%s   %s — %s\n",
				thinkStyle.Render("💭"),
				agentStyle.Render(event.Agent),
				dimStyle.Render(utils.TruncateLine(event.Message, 80)),
			))

		case "think_agents":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 agents"),
				dimStyle.Render(event.Message),
			))

		case "think_prompt":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 prompt"),
				dimStyle.Render(event.Message),
			))

		case "think_prompt_dump":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 prompt"),
				dimStyle.Render(event.Message),
			))

		case "think_delegation":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 delegate"),
				dimStyle.Render(event.Message),
			))

		case "think_sidecar":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 sidecar"),
				dimStyle.Render(event.Message),
			))

		case "think_text":
			w.write(fmt.Sprintf("%s %s",
				thinkStyle.Render("💭"),
				thinkStyle.Render(event.Message),
			))
		}
	})
}

type fileWriter struct {
	mu   sync.Mutex
	f    *os.File
}

func (w *fileWriter) write(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		fmt.Fprint(w.f, s)
	}
}

func newFileWriter(f *os.File) *fileWriter {
	return &fileWriter{f: f}
}

// makeFileReporter returns a StatusReporter that writes CLI-format log lines
// to the given file. It mirrors the output of setupStatusReporter but without
// idle timer, task panel, or skill panel.
func makeFileReporter(f *os.File) team.StatusReporter {
	var mu sync.Mutex
	w := newFileWriter(f)
	currentAgent := ""
	textBuf := ""

	return func(event team.StatusEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch event.Type {
		case "start":
			if currentAgent != "" && textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			currentAgent = event.Agent
			w.write(fmt.Sprintf("\n%s %s %s\n",
				headerStyle.Render("▶"),
				formatAgentLabel(event),
				dimStyle.Render("— "+event.Message),
			))

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
			w.write(fmt.Sprintf("  %s %s %s %s\n",
				toolStyle.Render("⟹"),
				formatAgentLabel(event),
				toolStyle.Render("›"),
				toolStyle.Render(event.ToolName),
			))
			if argsPreview != "" {
				if strings.Contains(argsPreview, "\n") {
					for _, line := range strings.Split(argsPreview, "\n") {
						w.write(fmt.Sprintf("    %s\n", dimStyle.Render(line)))
					}
				} else {
					w.write(fmt.Sprintf("    %s\n", dimStyle.Render(argsPreview)))
				}
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
				w.write(fmt.Sprintf("  %s %s %s %s\n    %s\n",
					doneStyle.Render("✓"),
					formatAgentLabel(event),
					toolStyle.Render("›"),
					toolStyle.Render(event.ToolName),
					resultStyle.Render(utils.TruncatePreview(resultLine, 200)),
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

		case "wrap_up_phase":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			w.write(fmt.Sprintf("\n%s %s\n\n",
				wrapUpStyle.Render("⏹"),
				boldStyle.Render(strings.ToUpper(event.Message+"...")),
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

		case "plan_approved":
			w.write(fmt.Sprintf("%s %s\n",
				doneStyle.Render("✓"),
				dimStyle.Render(event.Message),
			))

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

		case "cache_hit":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			w.write(fmt.Sprintf("%s %s %s %s\n",
				dimStyle.Render("⟐"),
				agentStyle.Render(event.Agent),
				doneStyle.Render("✓ cached"),
				dimStyle.Render(utils.TruncateLine(event.Message, 60)),
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

		case "think_skills":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 skills"),
				dimStyle.Render(event.Message),
			))
		case "think_skill_detail":
			w.write(fmt.Sprintf("%s   %s — %s\n",
				thinkStyle.Render("💭"),
				agentStyle.Render(event.Agent),
				dimStyle.Render(utils.TruncateLine(event.Message, 80)),
			))
		case "think_agents":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 agents"),
				dimStyle.Render(event.Message),
			))
		case "think_prompt":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 prompt"),
				dimStyle.Render(event.Message),
			))
		case "think_prompt_dump":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 prompt"),
				dimStyle.Render(event.Message),
			))
		case "think_delegation":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 delegate"),
				dimStyle.Render(event.Message),
			))
		case "think_sidecar":
			w.write(fmt.Sprintf("%s %s\n",
				thinkStyle.Render("💭 sidecar"),
				dimStyle.Render(event.Message),
			))
		case "think_text":
			w.write(fmt.Sprintf("%s %s",
				thinkStyle.Render("💭"),
				thinkStyle.Render(event.Message),
			))
		}
	}
}

func flushText(agentName, text string) string {
	if text == "" {
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("  %s %s\n",
		textStyle.Render("💬"),
		textStyle.Render(text),
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
	if t.StartedAt.IsZero() {
		return ""
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
	args = strings.TrimSpace(args)
	if toolName == "bash" {
		return args
	}
	args = strings.ReplaceAll(args, "\n", " ")
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

func padRight(str string, width int) string {
	visibleWidth := lipgloss.Width(str)
	if visibleWidth >= width {
		return str
	}
	return str + strings.Repeat(" ", width-visibleWidth)
}

// ── TUI support ───────────────────────────────────────────────────────────────

// activeTUIProgram is non-nil when --tui mode is active. Set by runWithTUI before
// executeSegments goroutine starts; read by newCoordDisplay inside that goroutine.
var activeTUIProgram atomic.Pointer[tea.Program]

// coordDisplay holds display handles for one coordinator run. In TUI mode both
// idleTimer and taskDisp are nil; stopThinking stops background ticker goroutines.
// logFile receives CLI-format event log when TUI is active.
type coordDisplay struct {
	idleTimer    *idleWarningTimer
	taskDisp     *taskDisplay
	stopThinking func()
	logFile      *os.File
}

func (d *coordDisplay) stopTimer() {
	if d.idleTimer != nil {
		d.idleTimer.stop()
	}
	if d.stopThinking != nil {
		d.stopThinking()
	}
	if d.logFile != nil {
		d.logFile.Close()
		d.logFile = nil
	}
}

func (d *coordDisplay) finalizeTasks() {
	if d.taskDisp != nil {
		d.taskDisp.update()
	}
}

// newCoordDisplay wires up the coordinator's status reporter. In TUI mode it
// attaches the TUI reporter plus a file-based CLI-format log; otherwise it
// creates the normal line-writer chain.
func newCoordDisplay(tc *teamContext) *coordDisplay {
	if p := activeTUIProgram.Load(); p != nil {
		tuiReporter, stopAll := makeTUIReporter(p)

		// Open session log file for post-hoc traceability
		logPath := filepath.Join(tc.session.Workspace, "session.log")
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			log.Printf("warning: cannot create session log %q: %v", logPath, err)
		}
		fileRep := makeFileReporter(logFile)

		compositeReporter := func(event team.StatusEvent) {
			tuiReporter(event)
			fileRep(event)
		}
		if tc.notifier != nil {
			orig := compositeReporter
			compositeReporter = func(event team.StatusEvent) {
				orig(event)
				tc.notifier.Notify(event.Type, event.Agent, event.Message, event.Output)
			}
		}

		tc.coordinator.SetStatusReporter(compositeReporter)
		return &coordDisplay{stopThinking: stopAll, logFile: logFile}
	}
	w := &lineWriter{}
	idleTimer := newIdleWarningTimer(w, 30*time.Second)
	taskDisp := newTaskDisplay(w, tc.coordinator.TaskTracker())
	skillDisp := newSkillDisplay(w)
	setupStatusReporter(w, tc.coordinator, taskDisp, skillDisp, idleTimer, tc.notifier)
	setStatusFlusher(w, taskDisp, skillDisp)
	return &coordDisplay{idleTimer: idleTimer, taskDisp: taskDisp}
}

// thinkingEntry tracks one agent's LLM wait so the TUI status bar keeps
// updating even when the model is silent for >30 s.
type thinkingEntry struct {
	label     string // pre-rendered "agent [model]" string
	startTime time.Time
	cancel    context.CancelFunc
}

// thinkingTracker maintains one active ticker per agent-task. Each ticker fires
// every tickInterval and sends an updated "waiting …Xs" StatusBarMsg to p.
// The most recently started thinking entry is tracked as "latest" so the status
// bar always reflects the freshest activity.
type thinkingTracker struct {
	mu      sync.Mutex
	entries map[string]*thinkingEntry // todoID → entry
	latest  string                    // todoID of the most-recently started entry
	p       *tea.Program
}

const thinkingTickInterval = 5 * time.Second

func newThinkingTracker(p *tea.Program) *thinkingTracker {
	return &thinkingTracker{
		entries: make(map[string]*thinkingEntry),
		p:       p,
	}
}

func (tt *thinkingTracker) start(todoID, agentLabel string) {
	tt.mu.Lock()
	// Cancel any previous ticker for this todoID (e.g. retry).
	if old, ok := tt.entries[todoID]; ok {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &thinkingEntry{label: agentLabel, startTime: time.Now(), cancel: cancel}
	tt.entries[todoID] = e
	tt.latest = todoID
	tt.mu.Unlock()

	go func() {
		ticker := time.NewTicker(thinkingTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tt.mu.Lock()
				cur, ok := tt.entries[todoID]
				isLatest := tt.latest == todoID
				tt.mu.Unlock()
				if !ok {
					return
				}
				elapsed := time.Since(cur.startTime).Truncate(time.Second)
				waitStr := dimStyle.Render(fmt.Sprintf("  waiting for LLM… %v", elapsed))
				if isLatest {
					tt.p.Send(tuipkg.StatusBarMsg{Text: cur.label + waitStr})
				}
			}
		}
	}()
}

func (tt *thinkingTracker) stop(todoID string) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if e, ok := tt.entries[todoID]; ok {
		e.cancel()
		delete(tt.entries, todoID)
	}
	// If this was the latest, promote the next remaining entry (if any).
	if tt.latest == todoID {
		tt.latest = ""
		for id := range tt.entries {
			tt.latest = id
			break
		}
	}
}

func (tt *thinkingTracker) stopAll() {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	for _, e := range tt.entries {
		e.cancel()
	}
	tt.entries = make(map[string]*thinkingEntry)
	tt.latest = ""
}

// tuiTextBufs holds per-todoID text buffers and model mappings with
// concurrency-safe access for use by the multi-model reporter.
type tuiTextBufs struct {
	mu       sync.RWMutex
	texts    map[string]string
	models   map[string]string
}

func newTUIBufs() *tuiTextBufs {
	return &tuiTextBufs{
		texts:  make(map[string]string),
		models: make(map[string]string),
	}
}

func (b *tuiTextBufs) append(todoID, text string) {
	b.mu.Lock()
	b.texts[todoID] += text
	b.mu.Unlock()
}

func (b *tuiTextBufs) setModel(todoID, model string) {
	b.mu.Lock()
	b.models[todoID] = model
	b.mu.Unlock()
}

func (b *tuiTextBufs) flush(todoID string) (text string, model string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	text = strings.TrimSpace(b.texts[todoID])
	model = b.models[todoID]
	delete(b.texts, todoID)
	delete(b.models, todoID)
	return
}

func (b *tuiTextBufs) get(todoID string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.texts[todoID]
}

// makeTUIReporter returns a StatusReporter that forwards relevant events to p,
// and a cleanup func that stops any background thinking-ticker goroutines.
func makeTUIReporter(p *tea.Program) (team.StatusReporter, func()) {
	tb := newTUIBufs()
	coordAdded := false
	tt := newThinkingTracker(p)

	return func(event team.StatusEvent) {
		switch event.Type {
		case "todos_updated":
			if event.Todos != nil {
				p.Send(tuipkg.TasksUpdatedMsg{Items: event.Todos})
			}
			if event.SSHSessions > 0 {
				p.Send(tuipkg.StatusBarMsg{Text: dimStyle.Render(fmt.Sprintf("SSH: %d active", event.SSHSessions))})
				p.Send(tuipkg.SSHSessionsMsg{Count: event.SSHSessions})
			}

		case "plan_approved":
			if event.Message != "" {
				p.Send(tuipkg.StatusBarMsg{Text: doneStyle.Render("✓ " + event.Message)})
			}

		case "wrap_up_phase":
			if event.Todos != nil {
				p.Send(tuipkg.TasksUpdatedMsg{Items: event.Todos})
			}
			if event.Message != "" {
				p.Send(tuipkg.StatusBarMsg{Text: wrapUpStyle.Render("⏹ " + event.Message + " — Ctrl+C to force quit")})
			}

		case "start":
			todoID := event.TodoID
			if todoID == "" {
				return
			}
			if event.Model != "" {
				tb.setModel(todoID, event.Model)
			}
			if todoID == team.CoordTodoID {
				if !coordAdded {
					coordAdded = true
					p.Send(tuipkg.CoordItemMsg{Item: &team.TodoItem{
						ID:     team.CoordTodoID,
						Agent:  event.Agent,
						Desc:   "coordinating",
						Status: team.TaskInProgress,
						Model:  event.Model,
					}})
				} else {
					p.Send(tuipkg.CoordStatusMsg{Status: team.TaskInProgress})
				}
			}
			label := agentStyle.Render(event.Agent)
			if event.Model != "" {
				label += " " + dimStyle.Render("["+event.Model+"]")
			}
			var line string
			if event.Message != "" {
				line = label + "\n" + dimStyle.Render("  Task: ") + event.Message
			} else {
				line = label
			}
			p.Send(tuipkg.TaskLogMsg{TodoID: todoID, Line: line, Model: event.Model})
			p.Send(tuipkg.StatusBarMsg{Text: label + dimStyle.Render("  thinking…")})
			tt.start(todoID, label)

		case "step":
			if event.TodoID == "" {
				return
			}
			tt.stop(event.TodoID)
			if event.Step > 0 {
				p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: tuipkg.RenderStep(event.Step), Model: event.Model})
				label := agentStyle.Render(event.Agent)
				p.Send(tuipkg.StatusBarMsg{Text: label + dimStyle.Render(fmt.Sprintf("  step %d", event.Step))})
			} else if event.Message != "" {
				p.Send(tuipkg.StatusBarMsg{Text: dimStyle.Render(event.Message)})
			}

		case "tool_call":
			if event.TodoID == "" {
				return
			}
			tt.stop(event.TodoID)
			p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: tuipkg.RenderToolCall(event.ToolName, event.ToolArgs), Model: event.Model})
			argsPreview := event.ToolArgs
			if event.ToolName != "bash" {
				argsPreview = utils.TruncateLine(argsPreview, 120)
			}
			toolLabel := toolStyle.Render("⟹ " + event.ToolName)
			agentLabel := agentStyle.Render(event.Agent)
			p.Send(tuipkg.StatusBarMsg{Text: agentLabel + "  " + toolLabel + dimStyle.Render("  "+argsPreview)})

		case "tool_result":
			if event.TodoID == "" {
				return
			}
			p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: tuipkg.RenderToolResult(event.ToolName, event.ToolResult), Model: event.Model})
			agentLabel := agentStyle.Render(event.Agent)
			p.Send(tuipkg.StatusBarMsg{Text: agentLabel + "  " + doneStyle.Render("✓ "+event.ToolName)})
			label := agentStyle.Render(event.Agent)
			if event.Model != "" {
				label += " " + dimStyle.Render("["+event.Model+"]")
			}
			tt.start(event.TodoID, label)

		case "cache_hit":
			if event.TodoID == "" {
				return
			}
			tt.stop(event.TodoID)
			p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: doneStyle.Render("✓ cached"), Model: event.Model})
			p.Send(tuipkg.StatusBarMsg{Text: agentStyle.Render(event.Agent) + "  " + doneStyle.Render("✓ cached") + "  " + dimStyle.Render(utils.TruncateLine(event.Message, 50))})

		case "text":
			if event.TodoID == "" {
				return
			}
			tb.append(event.TodoID, event.Message)

		case "done":
			if event.TodoID == "" {
				return
			}
			tt.stop(event.TodoID)
			hadText := strings.TrimSpace(tb.get(event.TodoID)) != ""
			if text, model := tb.flush(event.TodoID); true {
				if text != "" {
					p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: tuipkg.RenderText(text), Model: model})
				} else if !hadText {
					if out := strings.TrimSpace(event.Output); out != "" {
						p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: tuipkg.RenderText(out), Model: event.Model})
					}
				}
				_ = model
			}
			p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: doneStyle.Render("✓ done"), Model: event.Model})
			p.Send(tuipkg.StatusBarMsg{Text: agentStyle.Render(event.Agent) + "  " + doneStyle.Render("✓ done")})
			if event.TodoID == team.CoordTodoID {
				p.Send(tuipkg.CoordStatusMsg{Status: team.TaskDone})
			}

		case "error":
			if event.TodoID == "" {
				return
			}
			tt.stop(event.TodoID)
			if text, model := tb.flush(event.TodoID); text != "" {
				p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: tuipkg.RenderText(text), Model: model})
			}
			p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: errStyle.Render("✗ " + event.Message), Model: event.Model})
			p.Send(tuipkg.StatusBarMsg{Text: agentStyle.Render(event.Agent) + "  " + errStyle.Render("✗ "+utils.TruncateLine(event.Message, 60))})

		case "think_text":
			if event.TodoID == "" {
				return
			}
			tb.append(event.TodoID, event.Message)
			cur := tb.get(event.TodoID)
			if utf8.RuneCountInString(cur) > 10000 {
				if trimmed := strings.TrimSpace(cur); trimmed != "" {
					p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: thinkStyle.Render("💭 " + trimmed)})
				}
				tb.flush(event.TodoID) // clear buffer (ignore return values)
				return
			}
			for {
				cur = tb.get(event.TodoID)
				idx := strings.Index(cur, "\n")
				if idx == -1 {
					break
				}
				line := cur[:idx]
				rest := cur[idx+1:]
				tb.mu.Lock()
				tb.texts[event.TodoID] = rest
				tb.mu.Unlock()
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: thinkStyle.Render("💭 " + trimmed)})
				}
			}

		case "think_skills", "think_agents", "think_delegation", "think_sidecar":
			if event.TodoID == "" {
				return
			}
			p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: thinkStyle.Render("💭 "+event.Message)})

		case "think_skill_detail":
			if event.TodoID == "" {
				return
			}
			p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: thinkStyle.Render("💭 "+event.Agent+": "+event.Message)})

		case "think_prompt":
			if event.TodoID == "" {
				return
			}
			p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: thinkStyle.Render("💭 prompt: "+event.Message)})

		case "think_prompt_dump":
			if event.TodoID == "" {
				return
			}
			p.Send(tuipkg.TaskLogMsg{TodoID: event.TodoID, Line: thinkStyle.Render("💭 prompt: " + event.Message)})
		}
	}, tt.stopAll
}

// stderrLog writes to stderr only when the TUI is not active (avoids garbling
// the altscreen with progress lines while the TUI is running).
func stderrLog(format string, args ...any) {
	if activeTUIProgram.Load() == nil {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

// runWithTUI starts executeSegments in a goroutine and blocks on the Bubble Tea
// program in the main goroutine. Returns when the user quits or the work is done.
func runWithTUI(ctx context.Context, cancel context.CancelFunc, prompt string, segments []team.PromptSegment, registry *team.TeamRegistry, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator, pathConsent *tools.PathConsent, vars map[string]string, teamInfo tuipkg.TeamInfo) (string, error) {
	model := tuipkg.New(prompt, teamInfo)
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithoutSignalHandler())

	activeTUIProgram.Store(p)
	defer activeTUIProgram.Store(nil)

	if injector != nil {
		promptCh := model.PromptInjectCh
		go func() {
			for prompt := range promptCh {
				injector.enqueue(prompt)
			}
		}()
		defer close(promptCh)
	}

	var wrapUpCount int
	wrapUpCh := model.WrapUpCh
	go func() {
		for range wrapUpCh {
			wrapUpCount++
			if wrapUpCount == 1 {
				if c := activeCoord.Get(); c != nil {
					c.SetWrapUp()
				}
				if injector != nil {
					injector.injectWrapUp()
				}
			} else {
				cancel()
			}
		}
	}()
	defer close(wrapUpCh)

	reportCh := model.ReportCh
	var execResult string
	go func() {
		for range reportCh {
			generateReport(loadedTeams, execResult)
			p.Send(tuipkg.StatusBarMsg{Text: doneStyle.Render("✓ Report saved to workspace")})
		}
	}()
	defer close(reportCh)

	var execErr error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		execResult, execErr = executeSegments(ctx, segments, registry, providerURL, loadedTeams, injector, activeCoord, pathConsent, vars)
		if execResult != "" {
			p.Send(tuipkg.ResultMsg{Text: execResult})
		}
		p.Send(tuipkg.FinishedMsg{})
	}()

	if _, err := p.Run(); err != nil {
		cancel()
		waitWithTimeout(finished)
		return "", fmt.Errorf("TUI error: %w", err)
	}

	select {
	case <-finished:
	default:
		cancel()
		waitWithTimeout(finished)
	}

	if execErr != nil && ctx.Err() == context.Canceled {
		return "", errInterrupted{}
	}
	return execResult, execErr
}

func waitWithTimeout(finished chan struct{}) {
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		os.Exit(130)
	}
}

func renderDryRun(result *team.DryRunResult) {
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── DRY RUN ───"))
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("  %s %s\n", boldStyle.Render("Team:"), teamStyle.Render(result.TeamName)))
	b.WriteString(fmt.Sprintf("  %s %s\n", boldStyle.Render("Model:"), result.Model))
	if result.SidecarModel != "" {
		b.WriteString(fmt.Sprintf("  %s %s\n", boldStyle.Render("Sidecar:"), result.SidecarModel))
	}

	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── Agents ───"))
	b.WriteString("\n")
	for _, a := range result.Agents {
		roleLabel := dimStyle.Render("(" + a.Role + ")")
		modelLabel := ""
		if a.Model != "" {
			modelLabel = dimStyle.Render("[" + a.Model + "]")
		}
		nameStr := agentStyle.Render(a.Name)
		b.WriteString(fmt.Sprintf("  %s %s %s", padRight(nameStr, 20), padRight(roleLabel, 14), modelLabel))
		if len(a.Tools) > 0 {
			toolStr := strings.Join(a.Tools, ",")
			b.WriteString(fmt.Sprintf("  %s", dimStyle.Render("tools: "+toolStr)))
		}
		if len(a.Skills) > 0 {
			skillStr := strings.Join(a.Skills, ",")
			b.WriteString(fmt.Sprintf("  %s", dimStyle.Render("skills: "+skillStr)))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── Skills ───"))
	b.WriteString("\n")
	if len(result.AllSkills) == 0 {
		b.WriteString("  " + dimStyle.Render("No skills available") + "\n")
	} else {
		for _, s := range result.AllSkills {
			desc := utils.TruncateLine(s.Description, 60)
			b.WriteString(fmt.Sprintf("  %s %s\n", padRight(doneStyle.Render(s.Name), 20), dimStyle.Render(desc)))
		}
	}

	if len(result.MatchedSkillNames) > 0 {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("─── Sidecar Skill Matching ───"))
		b.WriteString("\n")
		matchedDisplay := make([]string, len(result.MatchedSkillNames))
		for i, n := range result.MatchedSkillNames {
			matchedDisplay[i] = doneStyle.Render(n)
		}
		b.WriteString(fmt.Sprintf("  Matched: %s\n", strings.Join(matchedDisplay, ", ")))
	}

	if len(result.FirstRoundTasks) > 0 {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("─── Coordinator Plan ───"))
		b.WriteString("\n")
		b.WriteString("  " + boldStyle.Render("Round 1:") + "\n")
		for _, t := range result.FirstRoundTasks {
			modelLabel := ""
			if t.Model != "" {
				modelLabel = " " + dimStyle.Render("["+t.Model+"]")
			}
			b.WriteString(fmt.Sprintf("    %s → %s%s\n", agentStyle.Render(t.Agent), t.Goal, modelLabel))
		}
	} else {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("─── Coordinator Plan ───"))
		b.WriteString("\n")
		b.WriteString("  " + dimStyle.Render("No tasks delegated (coordinator did not call the agent tool)") + "\n")
	}

	if result.Error != "" {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("─── Warning ───"))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("  %s\n", errStyle.Render(result.Error)))
	}

	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── DRY RUN COMPLETE (no tasks were executed) ───"))
	b.WriteString("\n")

	fmt.Fprint(os.Stderr, b.String())
}
