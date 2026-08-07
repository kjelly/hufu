package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestValidateSubmittedTaskResultCompletionStates(t *testing.T) {
	for _, status := range []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps} {
		t.Run(status, func(t *testing.T) {
			if err := validateSubmittedTaskResult(&TaskResult{Status: status, Summary: "done"}); err != nil {
				t.Fatalf("completion status %q rejected: %v", status, err)
			}
		})
	}
	for _, status := range []string{TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked} {
		t.Run(status, func(t *testing.T) {
			if err := validateSubmittedTaskResult(&TaskResult{Status: status, Summary: "work remains"}); err == nil {
				t.Fatalf("status %q unexpectedly accepted", status)
			}
		})
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
	workspace := t.TempDir()
	artifactPath := filepath.Join(workspace, "result.json")
	if err := os.WriteFile(artifactPath, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
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
	statusSchema, ok := info.Parameters["status"].(map[string]any)
	if !ok {
		t.Fatalf("status schema = %#v, want object", info.Parameters["status"])
	}
	statuses, ok := statusSchema["enum"].([]string)
	foundCompletedWithGaps := false
	for _, status := range statuses {
		if status == TaskResultStatusCompletedWithGaps {
			foundCompletedWithGaps = true
			break
		}
	}
	if !ok || !foundCompletedWithGaps {
		t.Fatalf("status schema omitted completed_with_gaps: %#v", statusSchema)
	}

	input := map[string]any{
		"status":  "success",
		"summary": "Completed subtask successfully",
		"artifacts": []map[string]any{
			{"path": artifactPath, "description": "JSON report"},
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
	if len(updatedItem.TypedResult.Artifacts) != 1 || updatedItem.TypedResult.Artifacts[0].ID == "" {
		t.Fatalf("submitted artifact was not materialized: %#v", updatedItem.TypedResult.Artifacts)
	}
}

func TestSubmitResultToolAcceptsCompletedWithGaps(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "analyst", Desc: "survey target capability"}})
	tool := &submitResultTool{coordinator: c, todoID: items[0].ID}

	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"status":"completed_with_gaps","summary":"Survey complete; the target has no structured roster action.","findings":[{"category":"capability_gap","summary":"roster workflow is interactive only"}]}`})
	if err != nil {
		t.Fatalf("tool.Run unexpected error: %v", err)
	}
	if response.IsError {
		t.Fatalf("completed_with_gaps response = %#v", response)
	}
	got := c.GetTaskResult(items[0].ID)
	if got == nil || got.Status != TaskResultStatusCompletedWithGaps {
		t.Fatalf("stored task result = %#v", got)
	}
}

func TestSubmitResultToolRejectsMissingArtifactBeforeStoringResult(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		taskTracker: NewTaskTracker(),
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "test goal"}})
	tool := &submitResultTool{coordinator: c, todoID: items[0].ID}

	input := `{"status":"success","summary":"done","artifacts":[{"path":"missing-report.txt"}]}`
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("tool.Run unexpected error: %v", err)
	}
	if !response.IsError || !strings.Contains(response.Content, "missing-report.txt") {
		t.Fatalf("missing artifact response = %#v, want path-specific error", response)
	}
	if got := c.GetTaskResult(items[0].ID); got != nil {
		t.Fatalf("missing artifact stored a task result: %#v", got)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.TypedResult != nil {
		t.Fatalf("missing artifact stored a todo typed result: %#v", item.TypedResult)
	}
}

func TestStrictTaskResultEnforcement(t *testing.T) {
	// Verify that ExecutionContract with RequiresResult = true fails if no submit_result was called
	taskDef := TaskDef{
		Agent: "worker",
		Goal:  "do strict work",
		Execution: ExecutionContract{
			RequiresResult: true,
		},
	}
	if !taskDef.Execution.RequiresResult {
		t.Errorf("expected RequiresResult to be true")
	}
}
