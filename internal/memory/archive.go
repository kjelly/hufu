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

func ArchiveSessionSummary(ctx context.Context, store *MemoryStore, entries []SessionSummaryEntry, teamName string) error {
	if store == nil || len(entries) == 0 {
		return nil
	}

	var lastAssistant *SessionSummaryEntry
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role == "assistant" {
			e := entries[i]
			lastAssistant = &e
			break
		}
	}

	if lastAssistant == nil {
		return nil
	}

	content := strings.TrimSpace(lastAssistant.Content)
	if len(content) < minSummaryLength {
		return nil
	}
	if len(content) > maxSummaryLength {
		content = content[:maxSummaryLength]
	}

	id := fmt.Sprintf("session-%d", time.Now().UnixNano())
	metadata := map[string]string{
		"category": "session-summary",
		"team":     teamName,
	}
	if lastAssistant.Timestamp != "" {
		metadata["session_date"] = lastAssistant.Timestamp
	}

	return store.Save(ctx, id, content, metadata)
}