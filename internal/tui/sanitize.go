package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/kjelly/hufu/internal/utils"
)

// sanitizeTerminalText removes ANSI, OSC, DCS and other control sequences
// from untrusted runtime text while preserving readable newlines and tabs.
// OSC52 is emitted only by the explicit clipboard command in detail_view.go.
func sanitizeTerminalText(value string) string {
	return utils.SanitizeTerminalText(value)
}

type cellWrapResult struct {
	Lines     []string
	Truncated bool
}

func truncateLineCells(value string, width int) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	return truncateCells(value, width)
}

func wrapCells(value string, width, maxLines int) cellWrapResult {
	if width <= 0 || maxLines <= 0 || value == "" {
		return cellWrapResult{}
	}
	wrapped := ansi.Hardwrap(sanitizeTerminalText(value), width, false)
	lines := strings.Split(wrapped, "\n")
	result := cellWrapResult{Lines: lines}
	if len(lines) > maxLines {
		result.Lines = append([]string(nil), lines[:maxLines]...)
		result.Lines[maxLines-1] = ansi.Truncate(result.Lines[maxLines-1], width, "…")
		result.Truncated = true
	}
	return result
}
