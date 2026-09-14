package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// sanitizeTerminalText removes ANSI, OSC, DCS and other control sequences
// from untrusted runtime text while preserving readable newlines and tabs.
// OSC52 is emitted only by the explicit clipboard command in detail_view.go.
func sanitizeTerminalText(value string) string {
	var out strings.Builder
	for index := 0; index < len(value); {
		if value[index] == 0x1b {
			index = skipEscapeSequence(value, index+1)
			continue
		}
		if value[index] < 0x20 || value[index] == 0x7f {
			if value[index] == '\n' || value[index] == '\t' {
				out.WriteByte(value[index])
			}
			index++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[index:])
		if r == utf8.RuneError && size == 1 {
			index++
			continue
		}
		out.WriteRune(r)
		index += size
	}
	return out.String()
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

func skipEscapeSequence(value string, index int) int {
	if index >= len(value) {
		return index
	}
	switch value[index] {
	case ']': // OSC: BEL or ST terminated.
		index++
		for index < len(value) {
			if value[index] == 0x07 {
				return index + 1
			}
			if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
				return index + 2
			}
			index++
		}
		return index
	case 'P', '_', '^', 'X': // DCS/APC/PM/SOS: ST terminated.
		index++
		for index+1 < len(value) {
			if value[index] == 0x1b && value[index+1] == '\\' {
				return index + 2
			}
			index++
		}
		return len(value)
	case '[': // CSI: final byte is in 0x40..0x7e.
		index++
		for index < len(value) {
			if value[index] >= 0x40 && value[index] <= 0x7e {
				return index + 1
			}
			index++
		}
		return index
	default:
		return index + 1
	}
}
