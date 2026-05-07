package tui

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anomalyco/hufu/internal/utils"
)

// ── Public types ──────────────────────────────────────────────────────────────

// AskUserOption is one selectable item in an ask_user dialog.
type AskUserOption struct {
	Label string
	Value string
}

// AskUserMsg is sent to the TUI when an agent calls the ask_user tool.
// The agent goroutine blocks on ReplyCh; the TUI sends the JSON response there.
type AskUserMsg struct {
	Question string
	Type     string // "free_text" | "single_choice" | "multiple_choice" | "mixed"
	Options  []AskUserOption
	AllowAny bool
	ReplyCh  chan<- string
}

// ── State (embedded in Model) ─────────────────────────────────────────────────

type askState struct {
	req      *AskUserMsg
	ti       textinput.Model
	cursor   int
	selected []bool // toggled entries for multiple_choice
	freeMode bool   // switched to free-text within a choice dialog
}

func (s *askState) isFreeText() bool {
	return s.freeMode || (s.req != nil && s.req.Type == "free_text")
}

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	askBoxStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("4")).Padding(1, 2)
	askQuestionStyle = lipgloss.NewStyle().Bold(true)
	askCursorStr     = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true).Render(">")
	askActiveStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	askCheckOn       = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Render("[✓]")
	askCheckOff      = lipgloss.NewStyle().Faint(true).Render("[ ]")
	askCustomStyle   = lipgloss.NewStyle().Faint(true)
	askHintStyle     = lipgloss.NewStyle().Faint(true)
)

// ── Init ──────────────────────────────────────────────────────────────────────

func initAskUser(msg AskUserMsg, screenW int) (askState, tea.Cmd) {
	ti := textinput.New()
	ti.Placeholder = "type your answer…"
	ti.Width = askTIWidth(screenW)
	st := askState{
		req:      &msg,
		ti:       ti,
		selected: make([]bool, len(msg.Options)),
	}
	if msg.Type == "free_text" {
		return st, st.ti.Focus()
	}
	return st, nil
}

// askTIWidth returns the textinput width for a given terminal width.
func askTIWidth(screenW int) int {
	dialogW := screenW - 8
	if dialogW < 44 {
		dialogW = 44
	}
	if dialogW > 82 {
		dialogW = 82
	}
	return dialogW - 8 // innerW(dialogW-6) minus "> "(2)
}

// maybeQuitAfterAsk checks if we should quit after dismissing an ask_user dialog.
// This handles the case where wrap-up was requested while the dialog was active.
func (m Model) maybeQuitAfterAsk() (tea.Model, tea.Cmd) {
	if m.finished && m.wrapUpRequested {
		return m, tea.Quit
	}
	return m, nil
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m Model) updateAskUser(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	st := &m.ask

	if st.isFreeText() {
		switch msg.String() {
		case "enter":
			st.req.ReplyCh <- marshalAskResp(nil, strings.TrimSpace(st.ti.Value()))
			m.inAskUser = false
			return m.maybeQuitAfterAsk()
		case "ctrl+c":
			st.req.ReplyCh <- marshalAskResp(nil, "")
			m.inAskUser = false
			return m.handleCtrlC()
		}
		var cmd tea.Cmd
		m.ask.ti, cmd = st.ti.Update(msg)
		return m, cmd
	}

	req := st.req
	opts := req.Options
	hasCustom := req.AllowAny || req.Type == "mixed"
	total := len(opts)
	if hasCustom {
		total++
	}

	switch msg.String() {
	case "ctrl+c":
		st.req.ReplyCh <- marshalAskResp(nil, "")
		m.inAskUser = false
		return m.handleCtrlC()
	case "up", "k":
		if st.cursor > 0 {
			st.cursor--
		}
	case "down", "j", "tab":
		if st.cursor < total-1 {
			st.cursor++
		}
	case " ":
		if req.Type == "multiple_choice" && st.cursor < len(opts) {
			st.selected[st.cursor] = !st.selected[st.cursor]
		}
	case "enter":
		if hasCustom && st.cursor == total-1 {
			st.freeMode = true
			return m, m.ask.ti.Focus()
		}
		if req.Type == "multiple_choice" {
			var answers []string
			for i, sel := range st.selected {
				if sel {
					answers = append(answers, optValue(opts[i]))
				}
			}
			req.ReplyCh <- marshalAskResp(answers, "")
		} else {
			// Bounds check: ensure cursor is within valid option range before sending response
			// For single_choice, cursor may point to custom option (index >= len(opts))
			if st.cursor < len(opts) {
				req.ReplyCh <- marshalAskResp([]string{optValue(opts[st.cursor])}, "")
			} else {
				req.ReplyCh <- marshalAskResp(nil, "")
			}
		}
		m.inAskUser = false
		return m.maybeQuitAfterAsk()
	}
	return m, nil
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) askUserView() string {
	st := &m.ask
	req := st.req
	if req == nil {
		return ""
	}

	// Note: m.inAskUser must be set to false before tea.Quit to ensure
	// ReplyCh is sent before termination. This prevents the agent goroutine
	// from deadlocking on the ReplyCh send, as the TUI must clean up the
	// ask_user state before the model exits.
	dialogW := m.width - 8
	if dialogW < 44 {
		dialogW = 44
	}
	if dialogW > 82 {
		dialogW = 82
	}
	innerW := dialogW - 6 // border(1) + padding(2) each side

	var sb strings.Builder
	sb.WriteString(askQuestionStyle.Render(wordWrap(req.Question, innerW)))
	sb.WriteString("\n")

	if st.isFreeText() {
		sb.WriteString("\n")
		sb.WriteString(askActiveStyle.Render("> "))
		sb.WriteString(st.ti.View())
		sb.WriteString("\n\n")
		sb.WriteString(askHintStyle.Render("enter  submit  ctrl+c cancel"))
	} else {
		opts := req.Options
		hasCustom := req.AllowAny || req.Type == "mixed"
		total := len(opts)
		if hasCustom {
			total++
		}

		sb.WriteString("\n")
		for i, opt := range opts {
			sel := st.cursor == i
			label := utils.TruncateLine(opt.Label, innerW-8)
			var line string
			if req.Type == "multiple_choice" {
				check := askCheckOff
				if st.selected[i] {
					check = askCheckOn
				}
				if sel {
					line = askCursorStr + " " + check + " " + askActiveStyle.Render(label)
				} else {
					line = "  " + check + " " + label
				}
			} else {
				if sel {
					line = askCursorStr + " " + askActiveStyle.Render(label)
				} else {
					line = "  " + label
				}
			}
			sb.WriteString(line + "\n")
		}
		if hasCustom {
			custom := utils.TruncatePreview("Type your own answer…", innerW-4)
			if st.cursor == total-1 {
				sb.WriteString(askCursorStr + " " + askActiveStyle.Render(custom) + "\n")
			} else {
				sb.WriteString("  " + askCustomStyle.Render(custom) + "\n")
			}
		}

		sb.WriteString("\n")
		hint := "↑↓/tab navigate  enter select  ctrl+c cancel"
		if req.Type == "multiple_choice" {
			hint = "↑↓/tab navigate  space toggle  enter confirm  ctrl+c cancel"
		}
		sb.WriteString(askHintStyle.Render(hint))
	}

	box := askBoxStyle.Width(innerW).Render(sb.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

type askRespJSON struct {
	Answers []string `json:"answers,omitempty"`
	Free    string   `json:"free_text,omitempty"`
}

func marshalAskResp(answers []string, free string) string {
	data, _ := json.Marshal(askRespJSON{Answers: answers, Free: free})
	return string(data)
}

func optValue(o AskUserOption) string {
	if o.Value != "" {
		return o.Value
	}
	return o.Label
}

func wordWrap(text string, width int) string {
	if width <= 0 || len([]rune(text)) <= width {
		return text
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}
	var sb strings.Builder
	lineLen := 0
	for i, w := range words {
		wLen := len([]rune(w))
		if i == 0 {
			sb.WriteString(w)
			lineLen = wLen
		} else if lineLen+1+wLen > width {
			sb.WriteString("\n")
			sb.WriteString(w)
			lineLen = wLen
		} else {
			sb.WriteString(" ")
			sb.WriteString(w)
			lineLen += 1 + wLen
		}
	}
	return sb.String()
}
