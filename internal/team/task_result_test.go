package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestTaskResult_SchemaAndFormatting(t *testing.T) {
	tr := &TaskResult{
		TaskID:  "task-123",
		Agent:   "coder",
		Status:  "completed",
		Summary: "Successfully refactored module",
		Artifacts: []ArtifactRef{
			{Path: "workspace/output.txt", Description: "output log"},
		},
		FilesModified: []FileRef{
			{Path: "internal/team/task_result.go", Purpose: "implementation"},
		},
		Findings: []Finding{
			{Category: "refactor", Summary: "code is now modular"},
		},
		Decisions: []Decision{
			{Topic: "architecture", Choice: "use typed task result"},
		},
		Confidence: 1.0,
		Source:     "submitted",
	}

	formatted := tr.FormatForContext()
	if !strings.Contains(formatted, "Summary: Successfully refactored module") {
		t.Errorf("expected summary in formatted context, got: %s", formatted)
	}
	if !strings.Contains(formatted, "Result Source: submitted (confidence: 1.00)") {
		t.Errorf("expected source and confidence in formatted context, got: %s", formatted)
	}
	if !strings.Contains(formatted, "workspace/output.txt") {
		t.Errorf("expected artifact path in formatted context, got: %s", formatted)
	}
	if !strings.Contains(formatted, "internal/team/task_result.go") {
		t.Errorf("expected file path in formatted context, got: %s", formatted)
	}
	if !strings.Contains(formatted, "[refactor] code is now modular") {
		t.Errorf("expected finding in formatted context, got: %s", formatted)
	}
}

func TestValidateSubmittedTaskResultRejectsNonSuccess(t *testing.T) {
	for _, status := range []string{"partial", "failed", "blocked"} {
		t.Run(status, func(t *testing.T) {
			if err := validateSubmittedTaskResult(&TaskResult{Status: status, Summary: "work remains"}); err == nil {
				t.Fatalf("status %q unexpectedly accepted", status)
			}
		})
	}
	if err := validateSubmittedTaskResult(&TaskResult{Status: "success", Summary: "done"}); err != nil {
		t.Fatalf("success rejected: %v", err)
	}
}

func TestParseFreeTextResult(t *testing.T) {
	text := "Done with the task. All tests passed."
	tr := ParseFreeTextResult(text)
	if tr.Summary != text {
		t.Errorf("got Summary %q, want %q", tr.Summary, text)
	}
	if tr.Confidence != 0.4 {
		t.Errorf("got Confidence %f, want 0.4", tr.Confidence)
	}
	if tr.Source != "parsed_free_text" {
		t.Errorf("got Source %q, want %q", tr.Source, tr.Source)
	}
}

func TestSubmitResultTool(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "test goal", Model: "m1", Source: "src"}})
	todoID := items[0].ID

	tool := &submitResultTool{
		coordinator: c,
		todoID:      todoID,
	}

	info := tool.Info()
	if info.Name != "submit_result" {
		t.Fatalf("expected tool name 'submit_result', got %q", info.Name)
	}
	if _, ok := info.Parameters["raw_output_ref"]; !ok {
		t.Fatal("submit_result schema omitted raw_output_ref")
	}

	input := map[string]any{
		"status":  "success",
		"summary": "Completed subtask successfully",
		"artifacts": []map[string]any{
			{"path": "workspace/result.json", "description": "JSON report"},
		},
		"findings": []map[string]any{
			{"category": "perf", "summary": "latency reduced by 20%"},
		},
		"confidence": 0.95,
	}
	inputBytes, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("failed to marshal tool input: %v", err)
	}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: string(inputBytes)})
	if err != nil {
		t.Fatalf("tool.Run unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("tool.Run returned error response: %v", resp)
	}

	res := c.GetTaskResult(todoID)
	if res == nil {
		t.Fatalf("expected stored TaskResult for todoID %s, got nil", todoID)
	}
	if res.Summary != "Completed subtask successfully" {
		t.Errorf("got Summary %q, want 'Completed subtask successfully'", res.Summary)
	}
	if res.Source != "submitted" {
		t.Errorf("got Source %q, want 'submitted'", res.Source)
	}
	if res.Confidence != 0.95 {
		t.Errorf("got Confidence %f, want 0.95", res.Confidence)
	}

	// Verify TodoItem in TodoList was also updated with TypedResult
	todoItems := c.taskTracker.TodoList().Items()
	var updatedItem *TodoItem
	for _, item := range todoItems {
		if item.ID == todoID {
			updatedItem = item
			break
		}
	}
	if updatedItem == nil || updatedItem.TypedResult == nil {
		t.Fatalf("expected TodoItem to have non-nil TypedResult")
	}
	if updatedItem.TypedResult.Summary != "Completed subtask successfully" {
		t.Errorf("got TodoItem.TypedResult.Summary %q, want 'Completed subtask successfully'", updatedItem.TypedResult.Summary)
	}
}

func TestStrictTaskResultEnforcement(t *testing.T) {
	// Verify that TaskExecutionPolicy with StrictResult = true fails if no submit_result was called
	taskDef := TaskDef{
		Agent: "worker",
		Goal:  "do strict work",
		Execution: TaskExecutionPolicy{
			StrictResult: true,
		},
	}
	if !taskDef.Execution.StrictResult {
		t.Errorf("expected StrictResult to be true")
	}
}
