package tui

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"

	"github.com/kjelly/hufu/internal/conversation"
)

// maxComposerLines mirrors textarea's internal line cap; pastes beyond it
// are refused instead of being truncated silently.
const maxComposerLines = 10000

// newChatComposer configures the bubbles textarea: it grows with its
// content (DynamicHeight) up to maxRows, and Enter is left to the chat
// view so it can submit.
func newChatComposer(preset DisplayPreset, maxRows int) textarea.Model {
	ta := textarea.New()
	ta.Prompt = "> "
	ta.Placeholder = "Message the team · #file · /command"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = maxRows
	ta.MaxContentHeight = 1 << 30
	keys := textarea.DefaultKeyMap()
	keys.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter", "shift+enter"))
	// Ctrl+T switches views and Ctrl+P opens the palette; the composer
	// must not consume them.
	keys.TransposeCharacterBackward = key.NewBinding(key.WithDisabled())
	keys.LinePrevious = key.NewBinding(key.WithKeys("up"))
	ta.KeyMap = keys
	if preset == DisplayEpaper {
		styles := ta.Styles()
		styles.Cursor.Blink = false
		ta.SetStyles(styles)
	}
	return ta
}

// applyComposerStyles replaces textarea's fixed colors with the theme's.
func applyComposerStyles(ta *textarea.Model, s styleSet) {
	styles := ta.Styles()
	base := textarea.StyleState{
		Base:        lipgloss.NewStyle(),
		Text:        lipgloss.NewStyle(),
		CursorLine:  lipgloss.NewStyle(),
		EndOfBuffer: lipgloss.NewStyle(),
		Placeholder: s.dim,
		Prompt:      s.prompt,
	}
	styles.Focused = base
	blurred := base
	blurred.Prompt = s.dim
	styles.Blurred = blurred
	ta.SetStyles(styles)
}

type chatInputKind int

const (
	inputPrompt chatInputKind = iota
	inputCommand
	inputUnknownCommand
)

type chatInput struct {
	kind chatInputKind
	name string // command name without the slash
	arg  string
	text string // prompt text for inputPrompt
}

var chatCommandToken = regexp.MustCompile(`^/[a-z][a-z-]*$`)

// parseChatInput recognizes explicit slash-command syntax only. A first
// token shaped like /word is a command; "//text" sends "/text" literally;
// anything else, including paths such as /home/x, is a prompt.
func parseChatInput(text string) chatInput {
	if strings.HasPrefix(text, "//") {
		return chatInput{kind: inputPrompt, text: text[1:]}
	}
	first, rest := text, ""
	if idx := strings.IndexAny(text, " \t\n"); idx >= 0 {
		first, rest = text[:idx], text[idx+1:]
	}
	if !chatCommandToken.MatchString(first) {
		return chatInput{kind: inputPrompt, text: text}
	}
	name := strings.TrimPrefix(first, "/")
	if _, ok := lookupChatCommand(name); !ok {
		return chatInput{kind: inputUnknownCommand, name: name}
	}
	return chatInput{kind: inputCommand, name: name, arg: strings.TrimSpace(rest)}
}

// completeChatToken completes the last token of value against the command
// table (for /) or the team's agents and teams (for @). It returns the new
// value and the candidates that matched.
func completeChatToken(value string, info TeamInfo) (string, []string) {
	if value == "" || strings.HasSuffix(value, " ") || strings.HasSuffix(value, "\n") {
		return value, nil
	}
	start := strings.LastIndexAny(value, " \n\t") + 1
	token := value[start:]
	var pool []string
	switch {
	case strings.HasPrefix(token, "/") && start == 0:
		for _, spec := range chatCommands {
			pool = append(pool, "/"+spec.name)
		}
	case strings.HasPrefix(token, "@"):
		for _, agent := range info.Agents {
			pool = append(pool, "@"+agent.Name)
		}
		for _, name := range info.AvailableTeams {
			pool = append(pool, "@"+name)
		}
	default:
		return value, nil
	}
	var matches []string
	for _, candidate := range pool {
		if strings.HasPrefix(candidate, token) && !slices.Contains(matches, candidate) {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return value, nil
	case 1:
		return value[:start] + matches[0] + " ", matches
	default:
		return value[:start] + commonPrefix(matches), matches
	}
}

func commonPrefix(values []string) string {
	prefix := values[0]
	for _, v := range values[1:] {
		for !strings.HasPrefix(v, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}

func composerSizeNotice(text string) string {
	size := len(text)
	if size < conversation.MaxPromptBytes*9/10 {
		return ""
	}
	return fmt.Sprintf("%d / %d KiB", size/1024, conversation.MaxPromptBytes/1024)
}
