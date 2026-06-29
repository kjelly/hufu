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
	if maxLen <= 3 {
		runes := []rune(text)
		if len(runes) == 0 {
			return ""
		}
		if len(runes) <= maxLen {
			return string(runes[:])
		}
		return string(runes[:maxLen])
	}

	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
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
	hitLimit := false

	emitLine := func(content string) bool {
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
				if emitLine(cur.String()) {
					hitLimit = true
					goto done
				}
				cur.Reset()
				curLen = 0
				width = contWidth
			}
			if emitLine("") {
				hitLimit = true
				goto done
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
				if emitLine(cur.String()) {
					hitLimit = true
					goto done
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
					if emitLine(cur.String()) {
						hitLimit = true
						goto done
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
		emitLine(cur.String())
	}

done:
	if len(allLines) == 0 {
		return WrapLineResult{}
	}

	// Check if there's remaining content that was cut off
	// (this covers the case where we hit maxLines via goto)
	// If we have exactly maxLines lines and hit the limit, content was dropped.
	if hitLimit {
		// Truncate the last line and add "..." to indicate more content
		lastLine := allLines[len(allLines)-1]
		if strings.HasPrefix(lastLine, contPrefix) {
			content := strings.TrimPrefix(lastLine, contPrefix)
			runes := []rune(content)
			if len(runes) > contWidth-3 {
				content = string(runes[:contWidth-3])
			}
			allLines[len(allLines)-1] = contPrefix + content + "..."
		} else {
			runes := []rune(lastLine)
			if len(runes) > maxLen-3 {
				lastLine = string(runes[:maxLen-3])
			}
			allLines[len(allLines)-1] = lastLine + "..."
		}
		return WrapLineResult{Lines: allLines, Truncated: true}
	}

	truncated := false
	if len(allLines) > maxLines {
		allLines = allLines[:maxLines]
		lastLine := allLines[maxLines-1]
		if strings.HasPrefix(lastLine, contPrefix) {
			content := strings.TrimPrefix(lastLine, contPrefix)
			runes := []rune(content)
			if len(runes) > contWidth-3 {
				content = string(runes[:contWidth-3])
			}
			allLines[maxLines-1] = contPrefix + content + "..."
		} else {
			runes := []rune(lastLine)
			if len(runes) > maxLen-3 {
				lastLine = string(runes[:maxLen-3])
			}
			allLines[maxLines-1] = lastLine + "..."
		}
		truncated = true
	}

	return WrapLineResult{Lines: allLines, Truncated: truncated}
}

// TruncateString returns s shortened to max characters with an ellipsis.
// If max <= 0, returns "…". If len(s) <= max, returns s unchanged.
// This is a byte-based truncation (not rune-safe); use TruncateRunes for CJK text.
func TruncateString(s string, max int) string {
	if max <= 0 {
		return "…"
	}
	if len(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return s[:max-1] + "…"
}

// TruncateRunes truncates text to at most maxRunes runes, appending "..."
// when truncation occurs. Safe for multi-byte characters such as CJK text.
func TruncateRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) == 0 {
		return ""
	}
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "..."
}
