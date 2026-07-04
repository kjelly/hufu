package utils

import "strings"

// TruncatePreview truncates text to a maximum length, appending "..." for preview purposes.
// It handles multi-byte characters correctly by working with runes.
//
// Behavior:
//   - If text is empty, returns empty string
//   - If text length <= maxLen, returns text unchanged
//   - If text length > maxLen, truncates to maxLen-3 characters and appends "..."
//
// Example:
//
//	TruncatePreview("Hello, World!", 10) // returns "Hello, Wor..."
//	TruncatePreview("Hi", 10)           // returns "Hi"
//	TruncatePreview("", 10)             // returns ""
func TruncatePreview(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

// TruncateLine takes the first line of text (up to \n) and truncates it to maxLen runes.
// If the text contains newlines, everything after the first \n is discarded.
// The resulting single line is then truncated with TruncatePreview.
func TruncateLine(text string, maxLen int) string {
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	return TruncatePreview(text, maxLen)
}

// WrapLineResult holds the output of WrapLine.
type WrapLineResult struct {
	Lines     []string
	Truncated bool
}

// truncateAndReturn is a helper that truncates the last line of the given list
// of lines to fit the maximum lengths and returns a WrapLineResult with Truncated=true.
func truncateAndReturn(allLines []string, maxLen, contWidth int, contPrefix string) WrapLineResult {
	if len(allLines) == 0 {
		return WrapLineResult{}
	}
	lastIdx := len(allLines) - 1
	lastLine := allLines[lastIdx]
	if strings.HasPrefix(lastLine, contPrefix) {
		content := strings.TrimPrefix(lastLine, contPrefix)
		runes := []rune(content)
		if len(runes) > contWidth-3 {
			content = string(runes[:contWidth-3])
		}
		allLines[lastIdx] = contPrefix + content + "..."
	} else {
		runes := []rune(lastLine)
		if len(runes) > maxLen-3 {
			lastLine = string(runes[:maxLen-3])
		}
		allLines[lastIdx] = lastLine + "..."
	}
	return WrapLineResult{Lines: allLines, Truncated: true}
}

// WrapLine wraps text to fit within maxLen rune width per line.
//
// If the text contains newlines, each source line is wrapped independently.
// Continuation lines are prefixed with "  - " (4 visible chars).
// At most maxLines lines are produced. If text overflows, the last line
// is truncated with "..." and Truncated is set to true.
//
// The first line uses the full maxLen width; continuation lines use
// maxLen-4 (reduced by the prefix length).
func WrapLine(text string, maxLen int, maxLines int) WrapLineResult {
	if maxLen <= 0 || maxLines <= 0 {
		return WrapLineResult{}
	}
	if text == "" {
		return WrapLineResult{}
	}

	contPrefix := "  - "
	prefixLen := len([]rune(contPrefix))
	contWidth := maxLen - prefixLen
	if contWidth < 1 {
		contWidth = 1
	}

	var allLines []string
	isFirst := true

	emitAndCheckLimit := func(content string) bool {
		if isFirst {
			allLines = append(allLines, content)
			isFirst = false
		} else {
			allLines = append(allLines, contPrefix+content)
		}
		return len(allLines) >= maxLines
	}

	sourceLines := strings.Split(text, "\n")
	var cur strings.Builder
	curLen := 0
	width := maxLen

	for _, srcLine := range sourceLines {
		words := strings.Fields(srcLine)
		if srcLine == "" && len(words) == 0 {
			if curLen > 0 {
				if emitAndCheckLimit(cur.String()) {
					return truncateAndReturn(allLines, maxLen, contWidth, contPrefix)
				}
				cur.Reset()
				curLen = 0
				width = contWidth
			}
			if emitAndCheckLimit("") {
				return truncateAndReturn(allLines, maxLen, contWidth, contPrefix)
			}
			width = contWidth
			continue
		}

		for _, word := range words {
			wordRunes := len([]rune(word))
			if curLen > 0 && curLen+1+wordRunes <= width {
				cur.WriteByte(' ')
				curLen++
				cur.WriteString(word)
				curLen += wordRunes
				continue
			}
			// Word doesn't fit on current line — flush current line
			if curLen > 0 {
				if emitAndCheckLimit(cur.String()) {
					return truncateAndReturn(allLines, maxLen, contWidth, contPrefix)
				}
				cur.Reset()
				curLen = 0
				width = contWidth
			}
			// Start new line with this word
			if wordRunes <= width {
				cur.WriteString(word)
				curLen = wordRunes
				continue
			}
			// Word exceeds width — split it
			wRunes := []rune(word)
			for len(wRunes) > 0 {
				chunk := width
				if chunk > len(wRunes) {
					chunk = len(wRunes)
				}
				if curLen > 0 {
					if emitAndCheckLimit(cur.String()) {
						return truncateAndReturn(allLines, maxLen, contWidth, contPrefix)
					}
					cur.Reset()
					curLen = 0
					width = contWidth
				}
				cur.WriteString(string(wRunes[:chunk]))
				curLen = chunk
				wRunes = wRunes[chunk:]
			}
		}
		width = contWidth
	}

	// Flush remaining content
	if curLen > 0 {
		emitAndCheckLimit(cur.String())
	}

	if len(allLines) == 0 {
		return WrapLineResult{}
	}

	if len(allLines) > maxLines {
		allLines = allLines[:maxLines]
		return truncateAndReturn(allLines, maxLen, contWidth, contPrefix)
	}

	return WrapLineResult{Lines: allLines, Truncated: false}
}

// TruncateString returns s shortened to max characters with an ellipsis.
// If max <= 0, returns "…". If len(s) <= max, returns s unchanged.
// This is a byte-based truncation (not rune-safe); use TruncateRunes for CJK text.
func TruncateString(s string, max int) string {
	if max <= 0 || max == 1 {
		return "…"
	}
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// TruncateRunes truncates text to at most maxRunes runes, appending "..."
// when truncation occurs. Safe for multi-byte characters such as CJK text.
func TruncateRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return "..."
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return ""
	}
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "..."
}
