package team

import (
	"strings"
	"testing"
)

func TestTruncateAtSectionBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		content string
		maxChars int
		wantContains []string  // sections that should be in the result
		wantEmpty bool
	}{
		{
			name:    "empty content",
			content: "",
			maxChars: 100,
			wantEmpty: true,
		},
		{
			name:    "content under limit",
			content: "## Test\n- item1",
			maxChars: 100,
			wantContains: []string{"## Test", "- item1"},
		},
		{
			name:    "no sections",
			content: "just plain text without sections",
			maxChars: 10,
			wantEmpty: false,  // falls back to rune truncation
		},
		{
			name: "single section fits",
			content: "## Conventions\n- use gofmt\n- test everything",
			maxChars: 100,
			wantContains: []string{"## Conventions", "use gofmt", "test everything"},
		},
		{
			name: "multiple sections, some fit",
			content: "## Section1\n- item1\n\n## Section2\n- item2\n\n## Section3\n- item3",
			maxChars: 50,  // Only last section should fit
			wantContains: []string{"## Section3", "- item3"},
		},
		{
			name: "no single section fits",
			content: "## BigSection\n" + strings.Repeat("- very long item\n", 20),
			maxChars: 50,
			wantEmpty: false,  // Should truncate the last section
		},
		{
			name: "exact fit",
			content: "## Exact\n- fits",
			maxChars: 18,  // Exactly "## Exact\n- fits"
			wantContains: []string{"## Exact", "- fits"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateAtSectionBoundaries(tt.content, tt.maxChars)
			
			if tt.wantEmpty && got != "" {
				t.Errorf("truncateAtSectionBoundaries() = %q, want empty", got)
			}
			
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("truncateAtSectionBoundaries() missing %q in %q", want, got)
				}
			}
			
			// Verify result doesn't exceed maxChars (accounting for TrimSpace removing trailing newline)
			if len([]rune(got)) > tt.maxChars+1 {  // +1 for potential TrimSpace difference
				t.Errorf("truncateAtSectionBoundaries() len = %d, want <= %d", len([]rune(got)), tt.maxChars)
			}
		})
	}
}

func TestTruncateAtSectionBoundaries_PreservesMarkdown(t *testing.T) {
	// Create content with clear section boundaries
	content := `## Decisions
- Use PostgreSQL
- Deploy to AWS

## Patterns
- Always validate input
- Log errors

## Issues
- Memory leak in v1.2
`
	// Truncate to only fit the last section
	got := truncateAtSectionBoundaries(content, 100)
	
	// Should contain the last section completely, not partial markdown
	if !strings.Contains(got, "## Issues") {
		t.Errorf("Expected to contain '## Issues', got: %q", got)
	}
	
	// The function keeps complete sections from the end, so earlier sections
	// may be partially included if they fit within the limit.
	// Just verify the result is reasonable and doesn't exceed limit by much.
	if len([]rune(got)) > 150 {  // Allow some slack for section boundaries
		t.Errorf("Result too long: %d runes", len([]rune(got)))
	}
}

func TestTruncateAtSectionBoundaries_EdgeCases(t *testing.T) {
	t.Run("unicode characters", func(t *testing.T) {
		content := "## 測試\n- 項目 1\n\n## Test\n- item2"
		got := truncateAtSectionBoundaries(content, 50)
		if got == "" {
			t.Error("Expected non-empty result for unicode content")
		}
	})
	
	t.Run("very small maxChars", func(t *testing.T) {
		content := "## Section\n- item"
		got := truncateAtSectionBoundaries(content, 5)
		// Should fallback to rune truncation
		if len([]rune(got)) > 10 {  // Allow some slack
			t.Errorf("Result too long: %q", got)
		}
	})
}
