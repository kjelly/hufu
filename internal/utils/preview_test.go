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
