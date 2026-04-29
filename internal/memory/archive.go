package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const maxSummaryLength = 2000
const minSummaryLength = 50

type SessionSummaryEntry struct {
	Role      string
	Content   string
	Timestamp string
}

// extractSummaryContent finds the last assistant entry and returns its (possibly
// truncated) content and timestamp. Returns empty strings when the content is
// absent or too short to be worth archiving.
func extractSummaryContent(entries []SessionSummaryEntry) (content, timestamp string) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role == "assistant" {
			content = strings.TrimSpace(entries[i].Content)
			timestamp = entries[i].Timestamp
			break
		}
	}
	if len(content) < minSummaryLength {
		return "", ""
	}
	if len(content) > maxSummaryLength {
		content = content[:maxSummaryLength]
	}
	return content, timestamp
}

func ArchiveSessionSummary(ctx context.Context, store *MemoryStore, entries []SessionSummaryEntry, teamName string) error {
	if store == nil || len(entries) == 0 {
		return nil
	}
	content, timestamp := extractSummaryContent(entries)
	if content == "" {
		return nil
	}
	id := fmt.Sprintf("session-%d", time.Now().UnixNano())
	metadata := map[string]string{
		"category": "session-summary",
		"team":     teamName,
	}
	if timestamp != "" {
		metadata["session_date"] = timestamp
	}
	return store.Save(ctx, id, content, metadata)
}