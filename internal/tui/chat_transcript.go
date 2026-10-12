package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
	"github.com/charmbracelet/x/ansi"
)

type chatItemKind int

const (
	itemUser chatItemKind = iota
	itemAssistant
	itemProgress
	itemNotice
	itemError
)

// chatItem is one transcript entry. Rendered lines are cached per width,
// theme, and content version, and rebuilt only when one of those changes.
type chatItem struct {
	kind     chatItemKind
	turnID   string
	title    string
	subtitle string
	body     string
	tag      string
	restored bool
	card     *progressCard
	version  int

	cacheWidth   int
	cacheTheme   string
	cacheVersion int
	lines        []string
}

// chatTranscript holds the conversation. Scrolling is delegated to a
// bubbles viewport; the transcript only keeps per-item render caches and
// whether new content arrived while the user was scrolled up.
type chatTranscript struct {
	items  []chatItem
	vp     viewport.Model
	unseen bool
}

func newChatTranscript() chatTranscript {
	vp := viewport.New()
	vp.FillHeight = true
	return chatTranscript{vp: vp}
}

func (t *chatTranscript) add(item chatItem) {
	t.items = append(t.items, item)
}

// findTurnItem returns the index of the newest item of kind for turnID.
func (t *chatTranscript) findTurnItem(kind chatItemKind, turnID string) int {
	for i := len(t.items) - 1; i >= 0; i-- {
		if t.items[i].kind == kind && t.items[i].turnID == turnID {
			return i
		}
	}
	return -1
}

// rerender refreshes stale item caches and the viewport content. The view
// keeps following new content only while it is at the bottom.
func (t *chatTranscript) rerender(width, height int, theme string, styles styleSet) {
	if width <= 0 {
		return
	}
	resized := t.vp.Width() != width || t.vp.Height() != height
	changed := false
	for i := range t.items {
		item := &t.items[i]
		if item.cacheWidth == width && item.cacheTheme == theme && item.cacheVersion == item.version && item.lines != nil {
			continue
		}
		item.lines = renderChatItem(*item, width, styles)
		item.cacheWidth, item.cacheTheme, item.cacheVersion = width, theme, item.version
		changed = true
	}
	if !changed && !resized {
		return
	}
	following := t.vp.AtBottom()
	before := t.vp.TotalLineCount()
	t.vp.SetWidth(width)
	t.vp.SetHeight(max(height, 0))
	var lines []string
	for i := range t.items {
		lines = append(lines, t.items[i].lines...)
	}
	t.vp.SetContentLines(lines)
	switch {
	case following:
		t.vp.GotoBottom()
		t.unseen = false
	case t.vp.TotalLineCount() > before:
		t.unseen = true
	}
}

// followIfAtBottom clears the new-content marker once the user is back at
// the bottom.
func (t *chatTranscript) followIfAtBottom() {
	if t.vp.AtBottom() {
		t.unseen = false
	}
}

func renderChatItem(item chatItem, width int, s styleSet) []string {
	var lines []string
	switch item.kind {
	case itemUser:
		lines = append(lines, truncateCells(s.agent.Render("You")+tagSuffix(item, s), width))
		lines = append(lines, wrapChatText(item.body, width)...)
	case itemAssistant:
		lines = append(lines, truncateCells(s.resultLabel.Render(item.title)+tagSuffix(item, s), width))
		if item.subtitle != "" {
			for _, l := range wrapChatText(item.subtitle, width) {
				lines = append(lines, s.dim.Render(l))
			}
		}
		lines = append(lines, wrapChatText(item.body, width)...)
	case itemProgress:
		lines = append(lines, renderProgressCard(item.card, width, s)...)
	case itemNotice:
		for _, l := range wrapChatText("· "+item.body, width) {
			lines = append(lines, s.dim.Render(l))
		}
	case itemError:
		for _, l := range wrapChatText("✗ "+item.body, width) {
			lines = append(lines, s.errorIcon.Render(l))
		}
	}
	return append(lines, "")
}

func tagSuffix(item chatItem, s styleSet) string {
	var tags []string
	if item.restored {
		tags = append(tags, "restored")
	}
	if item.tag != "" {
		tags = append(tags, item.tag)
	}
	if len(tags) == 0 {
		return ""
	}
	return s.dim.Render("  (" + strings.Join(tags, ", ") + ")")
}

// wrapChatText renders untrusted plain text: control sequences are removed,
// prose is word-wrapped, and fenced code is only hard-wrapped so its
// indentation survives.
func wrapChatText(text string, width int) []string {
	if width <= 0 {
		return nil
	}
	clean := strings.ReplaceAll(sanitizeTerminalText(text), "\t", "    ")
	clean = strings.TrimRight(clean, "\n")
	if clean == "" {
		return nil
	}
	var out []string
	inFence := false
	for _, line := range strings.Split(clean, "\n") {
		fence := strings.HasPrefix(strings.TrimSpace(line), "```")
		var wrapped string
		if inFence || fence {
			wrapped = ansi.Hardwrap(line, width, true)
		} else {
			wrapped = ansi.Wrap(line, width, "")
		}
		for _, w := range strings.Split(wrapped, "\n") {
			out = append(out, ansi.Truncate(w, width, ""))
		}
		if fence {
			inFence = !inFence
		}
	}
	return out
}
