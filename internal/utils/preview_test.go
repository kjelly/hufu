package utils

import (
	"testing"
)

func TestTruncatePreview(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxLen   int
		expected string
	}{
		{
			name:     "empty string",
			text:     "",
			maxLen:   10,
			expected: "",
		},
		{
			name:     "string shorter than maxLen",
			text:     "Hi",
			maxLen:   10,
			expected: "Hi",
		},
		{
			name:     "string exactly maxLen",
			text:     "Hello",
			maxLen:   5,
			expected: "He...",
		},
		{
			name:     "string longer than maxLen",
			text:     "Hello, World!",
			maxLen:   10,
			expected: "Hello, ...",
		},
		{
			name:     "zero maxLen",
			text:     "Hello",
			maxLen:   0,
			expected: "",
		},
		{
			name:     "negative maxLen",
			text:     "Hello",
			maxLen:   -1,
			expected: "",
		},
		{
			name:     "maxLen 1",
			text:     "Hello",
			maxLen:   1,
			expected: "H",
		},
		{
			name:     "maxLen 2",
			text:     "Hello",
			maxLen:   2,
			expected: "He",
		},
		{
			name:     "maxLen 3",
			text:     "Hello",
			maxLen:   3,
			expected: "Hel",
		},
		{
			name:     "multi-byte characters",
			text:     "你好世界",
			maxLen:   5,
			expected: "你好...",
		},
		{
			name:     "multi-byte characters exact fit",
			text:     "你好",
			maxLen:   5,
			expected: "你好",
		},
		{
			name:     "emoji handling",
			text:     "Hello 👋 World",
			maxLen:   10,
			expected: "Hello 👋...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TruncatePreview(tt.text, tt.maxLen)
			if result != tt.expected {
				t.Errorf("TruncatePreview(%q, %d) = %q, want %q", tt.text, tt.maxLen, result, tt.expected)
			}
		})
	}
}

func TestTruncateLine(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxLen   int
		expected string
	}{
		{
			name:     "single line short",
			text:     "Hello",
			maxLen:   10,
			expected: "Hello",
		},
		{
			name:     "single line truncated",
			text:     "Hello, World!",
			maxLen:   10,
			expected: "Hello, ...",
		},
		{
			name:     "multi-line keeps first line",
			text:     "Hello\nWorld",
			maxLen:   10,
			expected: "Hello",
		},
		{
			name:     "multi-line first line too long",
			text:     "Hello, World!\nSecond line",
			maxLen:   10,
			expected: "Hello, ...",
		},
		{
			name:     "newline at start",
			text:     "\nAfter newline",
			maxLen:   10,
			expected: "",
		},
		{
			name:     "empty string",
			text:     "",
			maxLen:   10,
			expected: "",
		},
		{
			name:     "multi-byte with newline",
			text:     "你好世界\n第二行",
			maxLen:   5,
			expected: "你好...",
		},
		{
			name:     "CR+LF newline",
			text:     "Hello\r\nWorld",
			maxLen:   10,
			expected: "Hello\r",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TruncateLine(tt.text, tt.maxLen)
			if result != tt.expected {
				t.Errorf("TruncateLine(%q, %d) = %q, want %q", tt.text, tt.maxLen, result, tt.expected)
			}
		})
	}
}

func TestWrapLine(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		maxLen    int
		maxLines  int
		wantLines []string
		wantTrunc bool
	}{
		{
			name:      "short text no wrap",
			text:      "Hello",
			maxLen:    20,
			maxLines:  3,
			wantLines: []string{"Hello"},
			wantTrunc: false,
		},
		{
			name:      "empty text",
			text:      "",
			maxLen:    20,
			maxLines:  3,
			wantLines: nil,
			wantTrunc: false,
		},
		{
			name:      "exact fit",
			text:      "Hello",
			maxLen:    5,
			maxLines:  3,
			wantLines: []string{"Hello"},
			wantTrunc: false,
		},
		{
			name:      "word wrap to 2 lines",
			text:      "Hello World",
			maxLen:    11,
			maxLines:  3,
			wantLines: []string{"Hello World"},
			wantTrunc: false,
		},
		{
			name:      "word wrap forces continuation",
			text:      "Hello World",
			maxLen:    7,
			maxLines:  3,
			wantLines: []string{"Hello", "  - Wor", "  - ld"},
			wantTrunc: false,
		},
		{
			name:      "word wrap truncated to maxLines",
			text:      "One Two Three Four Five Six",
			maxLen:    10,
			maxLines:  2,
			wantLines: []string{"One Two", "  - Thr..."},
			wantTrunc: true,
		},
		{
			name:      "multi-line source text",
			text:      "First line\nSecond line",
			maxLen:    20,
			maxLines:  3,
			wantLines: []string{"First line", "  - Second line"},
			wantTrunc: false,
		},
		{
			name:      "multi-line source truncated",
			text:      "First line\nSecond line\nThird line that is quite long",
			maxLen:    20,
			maxLines:  2,
			wantLines: []string{"First line", "  - Second line"},
			wantTrunc: true,
		},
		{
			name:      "long word split across lines",
			text:      "ABCDEFGHIJKLMNOP",
			maxLen:    8,
			maxLines:  5,
			wantLines: []string{"ABCDEFGH", "  - IJKLMNOP"},
			wantTrunc: false,
		},
		{
			name:      "long word exceeds maxLines",
			text:      "ABCDEFGHIJKLMNOP",
			maxLen:    8,
			maxLines:  2,
			wantLines: []string{"ABCDEFGH", "  - IJKLMNOP"},
			wantTrunc: false,
		},
		{
			name:      "single line truncated",
			text:      "Hello, World!",
			maxLen:    14,
			maxLines:  3,
			wantLines: []string{"Hello, World!"},
			wantTrunc: false,
		},
		{
			name:      "single very long word truncated on first line",
			text:      "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			maxLen:    10,
			maxLines:  1,
			wantLines: []string{"ABCDEFG..."},
			wantTrunc: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := WrapLine(tt.text, tt.maxLen, tt.maxLines)
			if result.Truncated != tt.wantTrunc {
				t.Errorf("WrapLine(%q, %d, %d).Truncated = %v, want %v", tt.text, tt.maxLen, tt.maxLines, result.Truncated, tt.wantTrunc)
			}
			if len(result.Lines) != len(tt.wantLines) {
				t.Errorf("WrapLine(%q, %d, %d).Lines = %v (len=%d), want %v (len=%d)", tt.text, tt.maxLen, tt.maxLines, result.Lines, len(result.Lines), tt.wantLines, len(tt.wantLines))
				return
			}
			for i, got := range result.Lines {
				want := tt.wantLines[i]
				if got != want {
					t.Errorf("WrapLine(%q, %d, %d).Lines[%d] = %q, want %q", tt.text, tt.maxLen, tt.maxLines, i, got, want)
				}
			}
		})
	}
}
