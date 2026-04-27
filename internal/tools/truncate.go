package tools

import (
	"fmt"
	"strings"
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
		if len(line) > defaultMaxLineLen {
			lines[i] = line[:defaultMaxLineLen] + "..."
		}
	}

	if total > maxLines {
		lines = lines[total-maxLines:]
	}

	result := strings.Join(lines, "\n")
	if len(result) > maxBytes {
		result = result[len(result)-maxBytes:]
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
		if len(line) > defaultMaxLineLen {
			lines[i] = line[:defaultMaxLineLen] + "..."
		}
	}

	if total > maxLines {
		lines = lines[:maxLines]
	}

	result := strings.Join(lines, "\n")
	if len(result) > maxBytes {
		result = result[:maxBytes]
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
	if len(line) <= maxLen {
		return line
	}
	return line[:maxLen] + "..."
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
