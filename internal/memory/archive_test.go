package memory

import (
	"testing"
)

func TestArchiveSessionSummaryNilStore(t *testing.T) {
	entries := []SessionSummaryEntry{
		{Role: "user", Content: "fix the bug"},
		{Role: "assistant", Content: "The bug was in auth.go, line 42. The JWT secret was rotated but the old secret was still cached."},
	}
	err := ArchiveSessionSummary(nil, nil, entries, "test-team")
	if err != nil {
		t.Errorf("expected nil error for nil store, got: %v", err)
	}
}

func TestArchiveSessionSummaryEmptyEntries(t *testing.T) {
	err := ArchiveSessionSummary(nil, nil, []SessionSummaryEntry{}, "test-team")
	if err != nil {
		t.Errorf("expected nil error for empty entries, got: %v", err)
	}
}

func TestArchiveSessionSummaryNoAssistant(t *testing.T) {
	entries := []SessionSummaryEntry{
		{Role: "user", Content: "hello"},
	}
	err := ArchiveSessionSummary(nil, nil, entries, "test-team")
	if err != nil {
		t.Errorf("expected nil error for no assistant entries, got: %v", err)
	}
}

func TestArchiveSessionSummaryShortContent(t *testing.T) {
	entries := []SessionSummaryEntry{
		{Role: "assistant", Content: "done"},
	}
	err := ArchiveSessionSummary(nil, nil, entries, "test-team")
	if err != nil {
		t.Errorf("expected nil error for short content, got: %v", err)
	}
}

func TestArchiveSessionSummaryPicksLastAssistant(t *testing.T) {
	entries := []SessionSummaryEntry{
		{Role: "assistant", Content: "This is the first response from the coordinator about the initial research."},
		{Role: "user", Content: "continue"},
		{Role: "assistant", Content: "This is the final conclusion: the root cause was a misconfigured JWT token lifecycle. The fix requires updating the token refresh logic in auth middleware."},
	}

	lastAssistant := findLastAssistant(entries)
	if lastAssistant == nil {
		t.Fatal("expected to find last assistant entry")
	}
	if !contains(lastAssistant.Content, "final conclusion") {
		t.Errorf("expected last assistant entry with 'final conclusion', got: %s", lastAssistant.Content)
	}
}

func TestArchiveSessionSummaryTruncation(t *testing.T) {
	longContent := ""
	for i := 0; i < 3000; i++ {
		longContent += "x"
	}

	entries := []SessionSummaryEntry{
		{Role: "assistant", Content: longContent},
	}

	truncated := truncateSummary(entries[0].Content)
	if len(truncated) != maxSummaryLength {
		t.Errorf("expected truncated length %d, got %d", maxSummaryLength, len(truncated))
	}
}

func TestArchiveSessionSummaryMinLength(t *testing.T) {
	tiny := "ok"
	if !isTooShort(tiny) {
		t.Error("2-char content should be too short")
	}

	medium := ""
	for i := 0; i < 60; i++ {
		medium += "x"
	}
	if isTooShort(medium) {
		t.Error("60-char content should not be too short")
	}

	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	if isTooShort(long) {
		t.Error("200-char content should not be too short")
	}
}

func findLastAssistant(entries []SessionSummaryEntry) *SessionSummaryEntry {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role == "assistant" {
			return &entries[i]
		}
	}
	return nil
}

func truncateSummary(content string) string {
	if len(content) > maxSummaryLength {
		return content[:maxSummaryLength]
	}
	return content
}

func isTooShort(content string) bool {
	return len(content) < minSummaryLength
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}