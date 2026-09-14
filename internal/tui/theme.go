package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ThemeMode selects semantic colors without changing runtime behavior.
type ThemeMode string

const (
	ThemeAuto  ThemeMode = "auto"
	ThemeLight ThemeMode = "light"
	ThemeDark  ThemeMode = "dark"
	ThemeMono  ThemeMode = "mono"
)

// DisplayPreset controls rendering frequency and decorative motion.
type DisplayPreset string

const (
	DisplayDefault DisplayPreset = "default"
	DisplayEpaper  DisplayPreset = "epaper"
)

// Options contains presentation-only, instance-scoped TUI preferences.
type Options struct {
	Theme         ThemeMode
	DisplayPreset DisplayPreset
	NoColor       bool
	Spinner       bool
	Compact       bool
	Owner         bool
	Context       context.Context
	ThemeDetector func(context.Context) ThemeMode
	ThemePoll     time.Duration
}

func (o Options) normalized() Options {
	if o.Theme == "" {
		o.Theme = ThemeAuto
	}
	if o.DisplayPreset == "" {
		o.DisplayPreset = DisplayDefault
	}
	if o.DisplayPreset == DisplayEpaper {
		o.Spinner = false
	}
	return o
}

// ParseThemeMode validates the stable CLI/config theme vocabulary.
func ParseThemeMode(value string) (ThemeMode, error) {
	mode := ThemeMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case ThemeAuto, ThemeLight, ThemeDark, ThemeMono:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid theme %q: use auto, light, dark, or mono", value)
	}
}

// ParseDisplayPreset validates the stable CLI/config display preset vocabulary.
func ParseDisplayPreset(value string) (DisplayPreset, error) {
	preset := DisplayPreset(strings.ToLower(strings.TrimSpace(value)))
	switch preset {
	case DisplayDefault, DisplayEpaper:
		return preset, nil
	default:
		return "", fmt.Errorf("invalid display preset %q: use default or epaper", value)
	}
}

func resolveAutoTheme(colorFGBG string) ThemeMode {
	parts := strings.Split(colorFGBG, ";")
	if len(parts) > 0 {
		last := strings.TrimSpace(parts[len(parts)-1])
		if n, err := strconv.Atoi(last); err == nil {
			if n >= 7 && n <= 15 {
				return ThemeLight
			}
			if n >= 0 && n <= 6 {
				return ThemeDark
			}
		}
	}
	return ThemeDark
}

func effectiveTheme(mode ThemeMode) ThemeMode {
	if mode != ThemeAuto {
		return mode
	}
	if override := strings.ToLower(strings.TrimSpace(os.Getenv("HUFU_THEME"))); override == "light" || override == "dark" {
		return ThemeMode(override)
	}
	if os.Getenv("LC_IS_EINK") == "1" {
		return ThemeLight
	}
	return resolveAutoTheme(os.Getenv("COLORFGBG"))
}

type semanticPalette struct {
	foreground lipgloss.AdaptiveColor
	muted      lipgloss.AdaptiveColor
	focus      lipgloss.AdaptiveColor
	accent     lipgloss.AdaptiveColor
	skill      lipgloss.AdaptiveColor
	success    lipgloss.AdaptiveColor
	warning    lipgloss.AdaptiveColor
	error      lipgloss.AdaptiveColor
	border     lipgloss.AdaptiveColor
	selection  lipgloss.AdaptiveColor
	match      lipgloss.AdaptiveColor
	visual     lipgloss.AdaptiveColor
}

var defaultSemanticPalette = semanticPalette{
	foreground: lipgloss.AdaptiveColor{Light: "0", Dark: "15"},
	muted:      lipgloss.AdaptiveColor{Light: "8", Dark: "8"},
	focus:      lipgloss.AdaptiveColor{Light: "5", Dark: "13"},
	accent:     lipgloss.AdaptiveColor{Light: "4", Dark: "12"},
	skill:      lipgloss.AdaptiveColor{Light: "6", Dark: "6"},
	success:    lipgloss.AdaptiveColor{Light: "2", Dark: "2"},
	warning:    lipgloss.AdaptiveColor{Light: "3", Dark: "11"},
	error:      lipgloss.AdaptiveColor{Light: "1", Dark: "9"},
	border:     lipgloss.AdaptiveColor{Light: "4", Dark: "14"},
	selection:  lipgloss.AdaptiveColor{Light: "254", Dark: "237"},
	match:      lipgloss.AdaptiveColor{Light: "225", Dark: "55"},
	visual:     lipgloss.AdaptiveColor{Light: "252", Dark: "236"},
}

func resolveColor(color lipgloss.AdaptiveColor, theme ThemeMode) lipgloss.TerminalColor {
	if theme == ThemeLight {
		return lipgloss.Color(color.Light)
	}
	return lipgloss.Color(color.Dark)
}

type styleSet struct {
	prompt, promptBox, header, dim, agent, skill, footer lipgloss.Style
	selectedFG, selectedBG                               lipgloss.Style
	pendingIcon, progressIcon, pausedIcon                lipgloss.Style
	doneIcon, errorIcon, skippedIcon                     lipgloss.Style
	match, wrapUp, visual, visualLabel, cursor           lipgloss.Style
	toolCall, toolResult, stepHeader, textLog            lipgloss.Style
	confirmBox, confirmHighlight, confirmNormal          lipgloss.Style
	resultBox, resultLabel, bold, done, team, infoBox    lipgloss.Style
	promptInputBox, searchBox                            lipgloss.Style
	askBox, askQuestion, askCursor, askActive            lipgloss.Style
	askCheckOn, askCheckOff, askCustom, askHint          lipgloss.Style
}

type themeDetectedMsg struct {
	mode       ThemeMode
	generation uint64
}

type themeProbeMsg struct {
	mode       ThemeMode
	generation uint64
}

// CLIStyleSet exposes the same semantic resolver to the non-TUI renderer.
// It is constructed once per invocation; unlike TUI styles it is not switched
// while a run is active.
type CLIStyleSet struct {
	Bold, Dim, Agent, Tool, Result, Error, Step, Done, Text, Think lipgloss.Style
	Header, PendingIcon, ProgressIcon, PausedIcon                  lipgloss.Style
	DoneIcon, ErrorIcon, SkipTag, DoneTag, ErrorTag                lipgloss.Style
	Team, WrapUp                                                   lipgloss.Style
}

// NewCLIStyleSet resolves CLI styles from the same semantic tokens as the TUI.
func NewCLIStyleSet(mode ThemeMode, noColor bool) CLIStyleSet {
	s := newStyleSet(mode, noColor)
	return CLIStyleSet{
		Bold: s.bold, Dim: s.dim, Agent: s.agent, Tool: s.toolCall,
		Result: s.toolResult, Error: s.errorIcon.Bold(true), Step: s.stepHeader,
		Done: s.done, Text: s.textLog, Think: s.agent, Header: s.header,
		PendingIcon: s.pendingIcon, ProgressIcon: s.progressIcon, PausedIcon: s.pausedIcon,
		DoneIcon: s.doneIcon, ErrorIcon: s.errorIcon, SkipTag: s.skippedIcon,
		DoneTag: s.doneIcon.Faint(true), ErrorTag: s.errorIcon.Faint(true),
		Team: s.team, WrapUp: s.wrapUp,
	}
}

func (s styleSet) warningBadge(text string) string {
	return s.wrapUp.Render(text)
}

func newStyleSet(mode ThemeMode, noColor bool) styleSet {
	effective := effectiveTheme(mode)
	if effective == ThemeMono || noColor {
		return monoStyleSet()
	}
	p := defaultSemanticPalette
	fg := resolveColor(p.foreground, effective)
	focus := resolveColor(p.focus, effective)
	accent := resolveColor(p.accent, effective)
	skill := resolveColor(p.skill, effective)
	success := resolveColor(p.success, effective)
	warning := resolveColor(p.warning, effective)
	errColor := resolveColor(p.error, effective)
	border := resolveColor(p.border, effective)
	selection := resolveColor(p.selection, effective)
	match := resolveColor(p.match, effective)
	visual := resolveColor(p.visual, effective)

	return styleSet{
		prompt: lipgloss.NewStyle().Bold(true).Foreground(focus),
		promptBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(focus).Padding(0, 1),
		header:       lipgloss.NewStyle().Bold(true).Foreground(border),
		dim:          lipgloss.NewStyle().Faint(true),
		agent:        lipgloss.NewStyle().Bold(true).Foreground(accent),
		skill:        lipgloss.NewStyle().Faint(true).Foreground(skill),
		footer:       lipgloss.NewStyle().Faint(true),
		selectedFG:   lipgloss.NewStyle().Bold(true).Foreground(fg),
		selectedBG:   lipgloss.NewStyle().Background(selection),
		pendingIcon:  lipgloss.NewStyle().Foreground(resolveColor(p.muted, effective)),
		progressIcon: lipgloss.NewStyle().Foreground(warning),
		pausedIcon:   lipgloss.NewStyle().Foreground(skill),
		doneIcon:     lipgloss.NewStyle().Foreground(success),
		errorIcon:    lipgloss.NewStyle().Foreground(errColor),
		skippedIcon:  lipgloss.NewStyle().Faint(true).Foreground(resolveColor(p.muted, effective)),
		match:        lipgloss.NewStyle().Background(match).Foreground(fg),
		wrapUp:       lipgloss.NewStyle().Bold(true).Foreground(warning),
		visual:       lipgloss.NewStyle().Background(visual).Foreground(fg),
		visualLabel:  lipgloss.NewStyle().Bold(true).Foreground(success),
		cursor:       lipgloss.NewStyle().Bold(true).Foreground(focus),
		toolCall:     lipgloss.NewStyle().Foreground(success),
		toolResult:   lipgloss.NewStyle().Faint(true),
		stepHeader:   lipgloss.NewStyle().Faint(true).Foreground(resolveColor(p.muted, effective)),
		textLog:      lipgloss.NewStyle().Faint(true),
		confirmBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(errColor).Padding(1, 3),
		confirmHighlight: lipgloss.NewStyle().Bold(true).Foreground(fg).Background(selection),
		confirmNormal:    lipgloss.NewStyle().Faint(true),
		resultBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(success).Padding(0, 1),
		resultLabel: lipgloss.NewStyle().Bold(true).Foreground(success),
		bold:        lipgloss.NewStyle().Bold(true), done: lipgloss.NewStyle().Bold(true).Foreground(success),
		team: lipgloss.NewStyle().Bold(true).Foreground(focus),
		infoBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(border).Padding(1, 3),
		promptInputBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(accent).Padding(1, 2),
		searchBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(skill).Padding(1, 2),
		askBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(accent).Padding(1, 2),
		askQuestion: lipgloss.NewStyle().Bold(true),
		askCursor:   lipgloss.NewStyle().Foreground(accent).Bold(true),
		askActive:   lipgloss.NewStyle().Foreground(accent),
		askCheckOn:  lipgloss.NewStyle().Foreground(accent),
		askCheckOff: lipgloss.NewStyle().Faint(true),
		askCustom:   lipgloss.NewStyle().Faint(true), askHint: lipgloss.NewStyle().Faint(true),
	}
}

func monoStyleSet() styleSet {
	plain := lipgloss.NewStyle()
	bold := plain.Bold(true)
	faint := plain.Faint(true)
	box := plain.Border(lipgloss.RoundedBorder()).Padding(0, 1)
	dialog := plain.Border(lipgloss.RoundedBorder()).Padding(1, 2)
	return styleSet{
		prompt: bold, promptBox: box, header: bold, dim: faint, agent: bold,
		skill: plain, footer: faint, selectedFG: bold.Underline(true), selectedBG: plain,
		pendingIcon: plain, progressIcon: bold, pausedIcon: bold, doneIcon: bold,
		errorIcon: bold, skippedIcon: faint, match: bold.Underline(true), wrapUp: bold,
		visual: plain.Reverse(true), visualLabel: bold, cursor: bold,
		toolCall: bold, toolResult: plain, stepHeader: faint, textLog: plain,
		confirmBox: dialog, confirmHighlight: bold.Underline(true), confirmNormal: plain,
		resultBox: box, resultLabel: bold, bold: bold, done: bold, team: bold,
		infoBox: dialog, promptInputBox: dialog, searchBox: dialog, askBox: dialog,
		askQuestion: bold, askCursor: bold, askActive: bold, askCheckOn: bold,
		askCheckOff: plain, askCustom: plain, askHint: plain,
	}
}

func (m Model) cycleTheme() (Model, string) {
	switch m.themeMode {
	case ThemeAuto:
		m.themeMode = ThemeLight
	case ThemeLight:
		m.themeMode = ThemeDark
	case ThemeDark:
		m.themeMode = ThemeMono
	default:
		m.themeMode = ThemeAuto
	}
	m.effectiveTheme = effectiveTheme(m.themeMode)
	m.styles = newStyleSet(m.themeMode, m.noColor)
	return m, string(m.themeMode)
}

func (m Model) canSwitchTheme() bool {
	switch m.currentOverlay() {
	case OverlayAskUser, OverlaySearch, OverlayPromptInput, OverlayConfirm:
		return false
	default:
		return true
	}
}

func detectRuntimeTheme(ctx context.Context) ThemeMode {
	if override := strings.ToLower(strings.TrimSpace(os.Getenv("HUFU_THEME"))); override == "light" || override == "dark" {
		return ThemeMode(override)
	}
	if os.Getenv("LC_IS_EINK") == "1" {
		return ThemeLight
	}
	if os.Getenv("TMUX") == "" {
		return resolveAutoTheme(os.Getenv("COLORFGBG"))
	}
	for _, name := range []string{"HUFU_THEME", "LC_IS_EINK", "COLORFGBG"} {
		value := tmuxEnvironment(ctx, name)
		switch name {
		case "HUFU_THEME":
			if value == "light" || value == "dark" {
				return ThemeMode(value)
			}
		case "LC_IS_EINK":
			if value == "1" {
				return ThemeLight
			}
		case "COLORFGBG":
			if value != "" {
				return resolveAutoTheme(value)
			}
		}
	}
	if name := tmuxOutput(ctx, "display-message", "-p", "#{session_name}"); strings.HasSuffix(name, "-light") || strings.HasSuffix(name, "-eink") {
		return ThemeLight
	} else if strings.HasSuffix(name, "-dark") {
		return ThemeDark
	}
	if style := tmuxOutput(ctx, "show-window-options", "-v", "window-style"); strings.Contains(style, "bg=white") || strings.Contains(style, "bg=colour15") {
		return ThemeLight
	}
	return resolveAutoTheme(os.Getenv("COLORFGBG"))
}

func tmuxEnvironment(ctx context.Context, name string) string {
	line := tmuxOutput(ctx, "show-environment", "-g", name)
	_, value, found := strings.Cut(line, "=")
	if !found {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func tmuxOutput(ctx context.Context, args ...string) string {
	probeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, "tmux", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (m Model) watchThemeCmd() tea.Cmd {
	if m.themeMode != ThemeAuto || m.themeContext == nil {
		return nil
	}
	detector := m.themeDetector
	if detector == nil {
		detector = detectRuntimeTheme
	}
	poll := m.themePoll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	ctx := m.themeContext
	current := m.effectiveTheme
	generation := m.themeGeneration
	return func() tea.Msg {
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				if detected := detector(ctx); detected != current {
					return themeDetectedMsg{mode: detected, generation: generation}
				}
			}
		}
	}
}

func (m Model) probeThemeCmd() tea.Cmd {
	if m.themeMode != ThemeAuto || m.themeContext == nil {
		return nil
	}
	detector := m.themeDetector
	if detector == nil {
		detector = detectRuntimeTheme
	}
	ctx := m.themeContext
	generation := m.themeGeneration
	return func() tea.Msg {
		return themeProbeMsg{mode: detector(ctx), generation: generation}
	}
}

func (m Model) blinkCmd() tea.Cmd {
	if m.displayPreset == DisplayEpaper {
		return nil
	}
	return textinput.Blink
}
