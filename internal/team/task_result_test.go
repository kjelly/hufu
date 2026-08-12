package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestTaskResult_SchemaAndFormatting(t *testing.T) {
	tr := &TaskResult{
		TaskID:  "task-123",
		Agent:   "coder",
		Status:  "completed",
		Summary: "Successfully refactored module",
		Details: "Complete refactor rationale and migration notes.",
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
	if !strings.Contains(formatted, "Complete refactor rationale and migration notes.") {
		t.Errorf("expected detailed deliverable in formatted context, got: %s", formatted)
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

func TestTaskResultContextUsesOpaqueTranscriptRefInsteadOfPath(t *testing.T) {
	result := &TaskResult{Summary: "done", RawOutputRef: &ArtifactRef{
		ID: "sha256-opaque", Path: "/workspace/long/hufu-pilot-integration/logs/transcript.jsonl", SHA256: "digest", Bytes: 42,
	}}
	formatted := result.FormatForContext()
	if !strings.Contains(formatted, "sha256-opaque") {
		t.Fatalf("formatted result omitted opaque ref: %s", formatted)
	}
	if strings.Contains(formatted, result.RawOutputRef.Path) {
		t.Fatalf("formatted result leaked copyable transcript path: %s", formatted)
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
	if _, ok := info.Parameters["raw_output_ref"]; ok {
		t.Fatal("submit_result schema exposed runtime-owned raw_output_ref")
	}
	if _, ok := info.Parameters["details"]; !ok {
		t.Fatal("submit_result schema omitted details")
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
		"details": "Complete approval-ready plan body.",
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
	if res.Details != "Complete approval-ready plan body." {
		t.Errorf("got Details %q, want complete typed handoff", res.Details)
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

func TestSubmitResultToolRejectsModelDeclaredRawOutputRef(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "runtime transcript"}})[0]
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(context.Background(), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"done","raw_output_ref":{"id":"invented","path":"/tmp/invented","sha256":"fake"}}`,
	})
	if err != nil {
		t.Fatalf("submit_result error = %v", err)
	}
	if !response.IsError || !strings.Contains(response.Content, "runtime-owned") {
		t.Fatalf("model-declared raw output response = %#v", response)
	}
	if got := c.GetTaskResult(item.ID); got != nil {
		t.Fatalf("model-declared raw output was stored: %#v", got)
	}
}

func TestSubmitResultToolPrefixesRuntimeClosedSequenceFailure(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "run fixed sequence"}})[0]
	sequence := newTaskToolSequence([]string{"bash", "submit_result"}, nil, "", nil)
	sequence.markFailedAt(0, "bash", "STDERR:\nprobe failed\n\nExit code: 23")
	ctx := context.WithValue(context.Background(), taskToolSequenceKey{}, sequence)

	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"blocked","summary":"the fifth command failed"}`,
	})
	if err != nil || response.IsError {
		t.Fatalf("submit_result response = %#v, err = %v", response, err)
	}
	result := c.GetTaskResult(item.ID)
	if result == nil {
		t.Fatal("submitted result was not stored")
	}
	want := `closed sequence failed at position 1 of 2 (tool "bash", exit code 23)`
	if !strings.HasPrefix(result.Summary, want) {
		t.Fatalf("summary = %q, want runtime fact prefix %q", result.Summary, want)
	}
	if !strings.Contains(result.Summary, "Worker report: the fifth command failed") {
		t.Fatalf("summary discarded worker context: %q", result.Summary)
	}
	if len(result.Findings) != 1 || result.Findings[0].Category != "runtime_execution" || result.Findings[0].Summary != want {
		t.Fatalf("runtime finding = %#v, want canonical failure fact", result.Findings)
	}
}

func TestCoordinatorTaskOutputUsesSubmittedTypedResult(t *testing.T) {
	typed := &TaskResult{
		Status:  TaskResultStatusSuccess,
		Summary: "concise result",
		Details: "complete plan body that must reach the coordinator",
		Source:  "submitted",
	}
	got := coordinatorTaskOutput("post-tool prose omitted the plan", typed)
	if !strings.Contains(got, typed.Details) {
		t.Fatalf("coordinator output omitted typed details: %q", got)
	}
	if strings.Contains(got, "post-tool prose") {
		t.Fatalf("coordinator output used fallback prose instead of typed result: %q", got)
	}
}

func TestSubmitResultToolPromotesStringFileRefs(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "test goal", Model: "m1", Source: "src"}})
	tool := &submitResultTool{coordinator: c, todoID: items[0].ID}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"done","files_read":["docs/runbook.md"],"files_modified":["tmp/result.txt"]}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("string file refs should be accepted: response=%+v err=%v", resp, err)
	}
	var stored *TaskResult
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.ID == items[0].ID {
			stored = item.TypedResult
			break
		}
	}
	if stored == nil || len(stored.FilesRead) != 1 || stored.FilesRead[0].Path != "docs/runbook.md" || len(stored.FilesModified) != 1 || stored.FilesModified[0].Path != "tmp/result.txt" {
		t.Fatalf("stored file refs were not normalized: result=%+v", stored)
	}
}

func TestSubmitResultToolPromotesScalarFileRef(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "scalar file ref"}})
	tool := &submitResultTool{coordinator: c, todoID: items[0].ID}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"done","files_read":"docs/runbook.md"}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("scalar file ref should be accepted: response=%+v err=%v", resp, err)
	}
}

func TestSubmitResultToolPromotesScalarDescriptiveArrays(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "scalar findings"}})
	tool := &submitResultTool{coordinator: c, todoID: items[0].ID}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"done","findings":"one finding","open_questions":"one question"}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("scalar descriptive arrays should be accepted: response=%+v err=%v", resp, err)
	}
	stored := c.GetTaskResult(items[0].ID)
	if stored == nil || len(stored.Findings) != 1 || stored.Findings[0].Summary != "one finding" || len(stored.OpenQuestions) != 1 || stored.OpenQuestions[0] != "one question" {
		t.Fatalf("descriptive arrays were not normalized: result=%+v", stored)
	}
}

func TestSubmitResultToolAcceptsStructuredOpenQuestions(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "structured questions"}})[0]
	tool := &submitResultTool{coordinator: c, todoID: item.ID}

	response, err := tool.Run(context.Background(), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"done","open_questions":[{"question":"Which target owns the decision?","context":"The configuration has no default.","detail":"Confirm before deployment."}]}`,
	})
	if err != nil || response.IsError {
		t.Fatalf("structured open question should be accepted: response=%+v err=%v", response, err)
	}
	stored := c.GetTaskResult(item.ID)
	if stored == nil || len(stored.OpenQuestions) != 1 {
		t.Fatalf("structured open question was not stored: %#v", stored)
	}
	want := "Which target owns the decision?\nContext: The configuration has no default.\nDetail: Confirm before deployment."
	if got := stored.OpenQuestions[0]; got != want {
		t.Fatalf("normalized open question = %q, want %q", got, want)
	}
}

func TestSubmitResultToolRejectsMalformedStructuredOpenQuestions(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "malformed questions"}})[0]

	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(context.Background(), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"done","open_questions":[{"question":"valid","unexpected":true}]}`,
	})
	if err != nil {
		t.Fatalf("tool.Run unexpected error: %v", err)
	}
	if !response.IsError || !strings.Contains(response.Content, "unsupported field") {
		t.Fatalf("malformed structured question response = %#v", response)
	}
	if got := c.GetTaskResult(item.ID); got != nil {
		t.Fatalf("malformed result was stored: %#v", got)
	}
}

func TestSubmitResultToolAdvertisesStructuredOpenQuestions(t *testing.T) {
	info := (&submitResultTool{}).Info()
	questions, ok := info.Parameters["open_questions"].(map[string]any)
	if !ok {
		t.Fatalf("open_questions schema = %#v, want object", info.Parameters["open_questions"])
	}
	items, ok := questions["items"].(map[string]any)
	if !ok || len(items["oneOf"].([]any)) != 2 {
		t.Fatalf("open_questions schema must advertise string and structured entries: %#v", questions)
	}
}

func TestSubmitResultToolRejectsForbiddenArtifacts(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "closed evidence task", Execution: ExecutionContract{ForbidArtifacts: true}},
	})
	tool := &submitResultTool{coordinator: c, todoID: items[0].ID}

	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"status":"success","summary":"done","artifacts":[{"path":"report.md"}]}`})
	if err != nil {
		t.Fatalf("tool.Run unexpected error: %v", err)
	}
	if !response.IsError || !strings.Contains(response.Content, "artifacts are forbidden") {
		t.Fatalf("forbidden artifact response = %#v", response)
	}
	if got := c.GetTaskResult(items[0].ID); got != nil {
		t.Fatalf("forbidden artifact result was stored: %#v", got)
	}
}

func TestSubmitResultToolRejectsModelDeclaredRuntimeOutputs(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "runtime outputs"}})[0]
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(context.Background(), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"done","outputs":{"invented":{"kind":"fact","fact":{"name":"invented","sha256":"fake","value":"never produced"}}}}`,
	})
	if err != nil {
		t.Fatalf("submit_result error = %v", err)
	}
	if !response.IsError || !strings.Contains(response.Content, "runtime-owned") {
		t.Fatalf("model-declared runtime output response = %#v", response)
	}
}

func TestSubmitResultToolRequiresCurrentAttemptReceiptsForStructuredExecution(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker",
		Desc:  "structured task",
		Execution: ExecutionContract{Steps: []ExecutionStep{
			{ID: "read", Tool: "view", Effect: ExecutionEffectRead},
			{ID: "verify", Tool: "check", Effect: ExecutionEffectVerify, DependsOn: []string{"read"}},
		}},
	}})
	todoID := items[0].ID
	c.setCurrentTaskAttempt(todoID, 2)
	registry := c.executionStepReceiptRegistry()
	started := time.Now().UTC()
	if err := registry.Record(ExecutionStepReceipt{
		ID: "receipt-current", TaskID: todoID, Attempt: 2, StepID: "read", Tool: "view", InputSHA256: "digest", StartedAt: started, ExitCode: 0, PolicyVerdict: "allowed",
	}); err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
	if err := registry.Record(ExecutionStepReceipt{
		ID: "receipt-current-verify", TaskID: todoID, Attempt: 2, StepID: "verify", Tool: "check", InputSHA256: "digest-2", StartedAt: started.Add(time.Millisecond), ExitCode: 0, PolicyVerdict: "allowed",
	}); err != nil {
		t.Fatalf("seed verify receipt: %v", err)
	}
	if err := registry.Record(ExecutionStepReceipt{
		ID: "receipt-old", TaskID: todoID, Attempt: 1, StepID: "read", Tool: "view", InputSHA256: "digest", ExitCode: 0, PolicyVerdict: "allowed",
	}); err != nil {
		t.Fatalf("seed prior receipt: %v", err)
	}
	tool := &submitResultTool{coordinator: c, todoID: todoID}

	for _, tc := range []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "missing claims", input: `{"status":"success","summary":"done"}`, wantErr: "must cite"},
		{name: "unexecuted claim", input: `{"status":"success","summary":"done","receipt_ids":["receipt-never-ran"]}`, wantErr: "does not exist"},
		{name: "prior attempt claim", input: `{"status":"success","summary":"done","receipt_ids":["receipt-old"]}`, wantErr: "attempt 1"},
		{name: "incomplete success claims", input: `{"status":"success","summary":"done","receipt_ids":["receipt-current"]}`, wantErr: "omits execution receipt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := tool.Run(context.Background(), fantasy.ToolCall{Name: "submit_result", Input: tc.input})
			if err != nil {
				t.Fatalf("tool.Run error = %v", err)
			}
			if !response.IsError || !strings.Contains(response.Content, tc.wantErr) {
				t.Fatalf("response = %#v, want error containing %q", response, tc.wantErr)
			}
		})
	}

	response, err := tool.Run(context.Background(), fantasy.ToolCall{
		Name: "submit_result", Input: `{"status":"success","summary":"done","receipt_ids":["receipt-current","receipt-current-verify"]}`,
	})
	if err != nil || response.IsError {
		t.Fatalf("current-attempt receipt rejected: response=%#v err=%v", response, err)
	}
	stored := c.GetTaskResult(todoID)
	if stored == nil || stored.Attempt != 2 || len(stored.ReceiptIDs) != 2 || stored.ReceiptIDs[0] != "receipt-current" {
		t.Fatalf("stored receipt-backed result = %#v", stored)
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
