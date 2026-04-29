package memory

import (
	"strings"
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

func TestExtractSummaryPicksLastAssistant(t *testing.T) {
	entries := []SessionSummaryEntry{
		{Role: "assistant", Content: "This is the first response from the coordinator about the initial research."},
		{Role: "user", Content: "continue"},
		{Role: "assistant", Content: "This is the final conclusion: the root cause was a misconfigured JWT token lifecycle. The fix requires updating the token refresh logic in auth middleware."},
	}
	content, _ := extractSummaryContent(entries)
	if content == "" {
		t.Fatal("expected non-empty content from last assistant entry")
	}
	if !strings.Contains(content, "final conclusion") {
		t.Errorf("expected content with 'final conclusion', got: %s", content)
	}
}

func TestExtractSummaryTruncation(t *testing.T) {
	entries := []SessionSummaryEntry{
		{Role: "assistant", Content: strings.Repeat("x", 3000)},
	}
	content, _ := extractSummaryContent(entries)
	if len(content) != maxSummaryLength {
		t.Errorf("expected truncated length %d, got %d", maxSummaryLength, len(content))
	}
}

func TestExtractSummaryMinLength(t *testing.T) {
	tiny := []SessionSummaryEntry{{Role: "assistant", Content: "ok"}}
	content, _ := extractSummaryContent(tiny)
	if content != "" {
		t.Error("2-char content should be filtered out as too short")
	}

	medium := []SessionSummaryEntry{{Role: "assistant", Content: strings.Repeat("x", 60)}}
	content, _ = extractSummaryContent(medium)
	if content == "" {
		t.Error("60-char content should not be filtered out")
	}
}

func TestExtractSummaryTimestamp(t *testing.T) {
	entries := []SessionSummaryEntry{
		{Role: "assistant", Content: strings.Repeat("x", 100), Timestamp: "2024-01-01T00:00:00Z"},
	}
	_, ts := extractSummaryContent(entries)
	if ts != "2024-01-01T00:00:00Z" {
		t.Errorf("expected timestamp '2024-01-01T00:00:00Z', got %q", ts)
	}
}

func TestExtractSummaryNoTimestamp(t *testing.T) {
	entries := []SessionSummaryEntry{
		{Role: "assistant", Content: strings.Repeat("x", 100)},
	}
	_, ts := extractSummaryContent(entries)
	if ts != "" {
		t.Errorf("expected empty timestamp, got %q", ts)
	}
}
