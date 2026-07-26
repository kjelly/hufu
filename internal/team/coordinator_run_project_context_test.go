package team

import (
	"context"
	"strings"
	"testing"
)

type projectContextTextCompacter struct {
	calls       int
	text        string
	instruction string
	result      string
}

func (c *projectContextTextCompacter) Compact(_ context.Context, text, instruction string) (string, error) {
	c.calls++
	c.text, c.instruction = text, instruction
	return c.result, nil
}

func TestCompactLegacyProjectContextUsesTextCompactionForLargeAgentsMD(t *testing.T) {
	agentsMD := "# Project Instructions\n" + strings.Repeat("Keep this convention. ", 250)
	compacter := &projectContextTextCompacter{result: "# Project Instructions\n\nCondensed convention."}

	got := compactLegacyProjectContext(context.Background(), compacter, agentsMD)
	if got != compacter.result {
		t.Fatalf("legacy project context = %q, want plain-text compacted result %q", got, compacter.result)
	}
	if compacter.calls != 1 || compacter.text != agentsMD {
		t.Fatalf("Compact calls/text = %d/%q, want 1/original project context", compacter.calls, compacter.text)
	}
	if !strings.Contains(compacter.instruction, "project context") || !strings.Contains(compacter.instruction, "all key facts") {
		t.Fatalf("unexpected legacy compact instruction: %q", compacter.instruction)
	}
	if strings.Contains(got, `"goal"`) || strings.Contains(got, `"completed_tasks"`) {
		t.Fatalf("legacy project context must not be a structured conversation summary: %q", got)
	}
}
