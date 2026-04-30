package memory

import (
	"context"
	"fmt"
	"strings"
)

const defaultAutoQueryResults = 5

func AutoQuery(ctx context.Context, store *MemoryStore, prompt string) (string, error) {
	if store == nil {
		return "", nil
	}

	if prompt == "" {
		return "", nil
	}

	results, err := store.Query(ctx, prompt, defaultAutoQueryResults, nil)
	if err != nil {
		return "", fmt.Errorf("memory auto-query failed: %w", err)
	}

	if len(results) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## Relevant Memory\n\n")
	b.WriteString("The following memories from past sessions may be relevant to this task:\n\n")
	for _, r := range results {
		cat := ""
		if r.Metadata != nil && r.Metadata["category"] != "" {
			cat = fmt.Sprintf(" [%s]", r.Metadata["category"])
		}
		b.WriteString(fmt.Sprintf("- [%.2f]%s %s\n", r.Similarity, cat, r.Content))
	}
	b.WriteString("\nUse `memory_query` to search for more memories if needed.\n")

	return b.String(), nil
}
