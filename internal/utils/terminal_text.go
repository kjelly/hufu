package utils

import (
	"strings"
	"unicode/utf8"
)

// SanitizeTerminalText removes ANSI, OSC, DCS and other control sequences
// from untrusted presentation text while preserving readable newlines and
// tabs. Callers may emit terminal controls only through a separate, explicit
// UI action such as clipboard copy.
func SanitizeTerminalText(value string) string {
	var output strings.Builder
	for index := 0; index < len(value); {
		if value[index] == 0x1b {
			index = skipTerminalEscape(value, index+1)
			continue
		}
		if value[index] < 0x20 || value[index] == 0x7f {
			if value[index] == '\n' || value[index] == '\t' {
				output.WriteByte(value[index])
			}
			index++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[index:])
		if r == utf8.RuneError && size == 1 {
			index++
			continue
		}
		output.WriteRune(r)
		index += size
	}
	return output.String()
}

func skipTerminalEscape(value string, index int) int {
	if index >= len(value) {
		return index
	}
	switch value[index] {
	case ']':
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
	case 'P', '_', '^', 'X':
		index++
		for index+1 < len(value) {
			if value[index] == 0x1b && value[index+1] == '\\' {
				return index + 2
			}
			index++
		}
		return len(value)
	case '[':
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
