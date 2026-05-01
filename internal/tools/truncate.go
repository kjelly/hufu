package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	defaultMaxLines   = 2000
	defaultMaxBytes   = 50 * 1024
	defaultMaxLineLen = 2000
	grepMaxLineLen    = 500
)

type TruncationResult struct {
	Content   string
	Truncated bool
	TruncBy   string
	Total     int
	Kept      int
}

func TruncateTail(content string, maxLines, maxBytes int) TruncationResult {
	if maxLines <= 0 {
		maxLines = defaultMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	lines := strings.Split(content, "\n")
	total := len(lines)

	for i, line := range lines {
		if utf8.RuneCountInString(line) > defaultMaxLineLen {
			lines[i] = safeTruncateString(line, defaultMaxLineLen) + "..."
		}
	}

	if total > maxLines {
		lines = lines[total-maxLines:]
	}

	result := strings.Join(lines, "\n")
	if len(result) > maxBytes {
		result = safeTruncateTailBytes(result, maxBytes)
	}

	kept := len(lines)
	return TruncationResult{
		Content:   result,
		Truncated: total > maxLines || len(content) > maxBytes,
		TruncBy:   truncBy(total, maxLines, len(content), maxBytes),
		Total:     total,
		Kept:      kept,
	}
}

func truncateHead(content string, maxLines, maxBytes int) TruncationResult {
	if maxLines <= 0 {
		maxLines = defaultMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	lines := strings.Split(content, "\n")
	total := len(lines)

	for i, line := range lines {
		if utf8.RuneCountInString(line) > defaultMaxLineLen {
			lines[i] = safeTruncateString(line, defaultMaxLineLen) + "..."
		}
	}

	if total > maxLines {
		lines = lines[:maxLines]
	}

	result := strings.Join(lines, "\n")
	if len(result) > maxBytes {
		result = safeTruncateHeadBytes(result, maxBytes)
	}

	kept := len(lines)
	return TruncationResult{
		Content:   result,
		Truncated: total > maxLines || len(content) > maxBytes,
		TruncBy:   truncBy(total, maxLines, len(content), maxBytes),
		Total:     total,
		Kept:      kept,
	}
}

func truncateLine(line string, maxLen int) string {
	if utf8.RuneCountInString(line) <= maxLen {
		return line
	}
	return safeTruncateString(line, maxLen) + "..."
}

func truncBy(totalLines, maxLines, totalBytes, maxBytes int) string {
	if totalLines > maxLines && totalBytes > maxBytes {
		return "lines+bytes"
	}
	if totalLines > maxLines {
		return "lines"
	}
	if totalBytes > maxBytes {
		return "bytes"
	}
	return ""
}

func formatTruncationNotice(tr TruncationResult) string {
	if !tr.Truncated {
		return ""
	}
	return fmt.Sprintf("\n[output truncated: showing %d of %d lines]", tr.Kept, tr.Total)
}

func safeTruncateString(s string, maxChars int) string {
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxChars])
}

func safeTruncateHeadBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	for maxBytes > 0 {
		if utf8.RuneStart(s[maxBytes]) {
			break
		}
		maxBytes--
	}
	return s[:maxBytes]
}

func safeTruncateTailBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	start := len(s) - maxBytes
	for start < len(s) {
		if utf8.RuneStart(s[start]) {
			break
		}
		start++
	}
	if start >= len(s) {
		// No rune start found in the window; backtrack to include
		// the last complete rune before the window.
		start = len(s) - maxBytes
		for start > 0 && !utf8.RuneStart(s[start]) {
			start--
		}
	}
	return s[start:]
}
