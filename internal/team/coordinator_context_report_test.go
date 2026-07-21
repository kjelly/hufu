package team

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestRecordContextBreakdownAndReport(t *testing.T) {
	tracker := NewTaskTracker()
	added := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do thing"}})
	if len(added) == 0 {
		t.Fatal("expected at least one todo item")
	}
	// Items() returns copies; mutate the internal item directly (same package).
	tracker.todo.items[0].Status = TaskDone
	tracker.todo.items[0].Output = "Completed the thing successfully."

	c := &Coordinator{
		session:     &TeamSession{},
		taskTracker: tracker,
		conversationHistory: []fantasy.Message{
			fantasy.NewUserMessage("Please implement feature X"),
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "On it, starting now."}}},
		},
		lastCompactionSummary: &StructuredSummary{
			Goal:           "implement feature X",
			CompletedTasks: []string{"task-a"},
		},
	}

	c.recordContextBreakdown(context.Background(), "qwen3:8b",
		"You are a coordinator. Delegate tasks.",       // coreText
		"# AGENTS.md\nProject conventions go here.",    // projectText
		"STM: prior finding Y\nLTM: relevant memory Z", // memoryText
	)

	budget, usage, modelID, ready := c.ContextUsageReport()
	if !ready {
		t.Fatal("expected ContextUsageReport ready=true after recordContextBreakdown")
	}
	if modelID != "qwen3:8b" {
		t.Errorf("modelID = %q, want qwen3:8b", modelID)
	}
	if budget.Window <= 0 {
		t.Errorf("budget.Window = %d, want > 0", budget.Window)
	}
	if usage.SystemInstructions <= 0 {
		t.Errorf("SystemInstructions = %d, want > 0", usage.SystemInstructions)
	}
	if usage.ProjectContext <= 0 {
		t.Errorf("ProjectContext = %d, want > 0", usage.ProjectContext)
	}
	if usage.StmLtmRag <= 0 {
		t.Errorf("StmLtmRag = %d, want > 0", usage.StmLtmRag)
	}
	if usage.RecentConversation <= 0 {
		t.Errorf("RecentConversation = %d, want > 0", usage.RecentConversation)
	}
	if usage.CompactedHistory <= 0 {
		t.Errorf("CompactedHistory = %d, want > 0", usage.CompactedHistory)
	}
	if usage.TaskDependencyResults <= 0 {
		t.Errorf("TaskDependencyResults = %d, want > 0 (completed task output)", usage.TaskDependencyResults)
	}
	if usage.ReplyReserve <= 0 {
		t.Errorf("ReplyReserve = %d, want > 0", usage.ReplyReserve)
	}

	section := c.RenderContextUsageSection()
	if section == "" {
		t.Fatal("expected non-empty context usage section")
	}
	if !strings.Contains(section, "Context usage:") {
		t.Errorf("section missing 'Context usage:' header:\n%s", section)
	}
	if !strings.Contains(section, "System instructions") {
		t.Errorf("section missing System instructions row:\n%s", section)
	}
}

func TestRenderContextUsageSectionEmptyWhenNotReady(t *testing.T) {
	c := &Coordinator{session: &TeamSession{}}
	if got := c.RenderContextUsageSection(); got != "" {
		t.Errorf("expected empty section before any recording, got %q", got)
	}
}

func TestEstimatedContextModelFallback(t *testing.T) {
	c := &Coordinator{session: &TeamSession{}}
	c.recordContextBreakdown(context.Background(), "totally-unknown-model-xyz", "core", "project", "memory")
	if !c.EstimatedContextModel() {
		t.Error("expected EstimatedContextModel=true for unknown model")
	}
	section := c.RenderContextUsageSection()
	if !strings.Contains(section, "estimated") {
		t.Errorf("expected section to annotate estimated counts:\n%s", section)
	}
}
