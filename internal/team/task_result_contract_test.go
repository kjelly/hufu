package team

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func filesReadAssertionTask() TaskDef {
	return TaskDef{
		Execution: ExecutionContract{RequiresResult: true},
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
			{Pointer: "/summary", Op: "non_empty"},
			{Pointer: "/files_read", Op: "min_items", Value: 1},
		}},
	}
}

func TestSubmitResultRequiresFilesReadBeforeCommit(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), executionRunID: "run-contract"}
	task := filesReadAssertionTask()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "reviewer", Desc: "review workset", Execution: task.Execution, VerifySpec: task.VerifySpec},
	})[0]
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name: submitResultToolName, Input: `{"status":"success","summary":"review complete"}`,
	})
	if err != nil {
		t.Fatalf("submit_result returned Go error: %v", err)
	}
	if !response.IsError || !strings.Contains(response.Content, "files_read") {
		t.Fatalf("missing files_read response = %#v", response)
	}
	if got := c.GetTaskResult(item.ID); got != nil || item.TypedResult != nil {
		t.Fatalf("invalid result was persisted: coordinator=%#v item=%#v", got, item.TypedResult)
	}
}

func TestSubmitResultAcceptsValidFilesReadPathEntry(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), executionRunID: "run-contract-valid"}
	task := filesReadAssertionTask()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "reviewer", Desc: "review workset", Execution: task.Execution, VerifySpec: task.VerifySpec},
	})[0]
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"review complete","files_read":[{"path":"sha256-assigned-diff","purpose":"assigned input"}]}`,
	})
	if err != nil || response.IsError {
		t.Fatalf("valid files_read response=%#v err=%v", response, err)
	}
	got := c.GetTaskResult(item.ID)
	if got == nil || len(got.FilesRead) != 1 || got.FilesRead[0].Path != "sha256-assigned-diff" {
		t.Fatalf("stored result=%#v", got)
	}
}

func TestSubmitResultAssertionsEvaluateCanonicalHydratedResult(t *testing.T) {
	workspace := t.TempDir()
	artifactPath := filepath.Join(workspace, "review.md")
	if err := os.WriteFile(artifactPath, []byte("review complete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := TaskDef{
		Execution: ExecutionContract{RequiresResult: true},
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
			{Pointer: "/confidence", Op: "equals", Value: 1},
			{Pointer: "/source", Op: "equals", Value: "submitted"},
			{Pointer: "/attempt", Op: "exists"},
			{Pointer: "/artifacts/0/id", Op: "exists"},
		}},
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: NewTaskTracker(), executionRunID: "run-canonical-assertions"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "submit canonical result", Execution: task.Execution, VerifySpec: task.VerifySpec}})[0]
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"review complete","artifacts":[{"path":"review.md"}]}`,
	})
	if err != nil || response.IsError {
		t.Fatalf("canonical assertion submission response=%#v err=%v", response, err)
	}
	got := c.GetTaskResult(item.ID)
	if got == nil || got.Confidence != 1 || got.Source != "submitted" || got.Attempt != 1 || got.TaskID != item.ID || len(got.Artifacts) != 1 {
		t.Fatalf("canonical result=%#v", got)
	}
	if got.Artifacts[0].ID == "" || got.Artifacts[0].RunID != c.executionRunID || got.Artifacts[0].TaskID != item.ID || got.Artifacts[0].Attempt != 1 || got.Artifacts[0].Agent != item.Agent {
		t.Fatalf("canonical artifact provenance=%#v", got.Artifacts[0])
	}
	verification, err := ExecuteVerificationSpecWithTaskResult(context.Background(), "sh", workspace, *task.VerifySpec, got)
	if err != nil || verification == nil || verification.ExitCode != 0 {
		t.Fatalf("canonical assertions after persistence=%#v err=%v", verification, err)
	}
}

type assertionRecordingSink struct {
	calls  int
	result TaskResult
}

func (s *assertionRecordingSink) Submit(_ context.Context, _ string, result TaskResult) error {
	s.calls++
	s.result = result
	return nil
}

func TestSubmitResultAssertionFailureRollsBackAfterArtifactMaterialization(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "review.md"), []byte("review complete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := TaskDef{
		Execution: ExecutionContract{RequiresResult: true},
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
			{Pointer: "/artifacts/0/id", Op: "equals", Value: "sha256-not-the-materialized-id"},
		}},
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: NewTaskTracker(), executionRunID: "run-assertion-rollback"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "rollback invalid assertion", Execution: task.Execution, VerifySpec: task.VerifySpec}})[0]
	sink := &assertionRecordingSink{}
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID, sink: sink}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"review complete","artifacts":[{"path":"review.md"}]}`,
	})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "task_result_assert admission failed") {
		t.Fatalf("assertion failure response=%#v err=%v", response, err)
	}
	if sink.calls != 0 {
		t.Fatalf("external sink invoked %d time(s) before assertion acceptance", sink.calls)
	}
	if c.GetTaskResult(item.ID) != nil || item.TypedResult != nil {
		t.Fatalf("failed assertion published a result: coordinator=%#v todo=%#v", c.GetTaskResult(item.ID), item.TypedResult)
	}
	controller := c.occurrenceController(item.ID)
	controller.mu.Lock()
	reserved, pending, result := controller.reserved, len(controller.pending), controller.result
	controller.mu.Unlock()
	if reserved || pending != 0 || result != nil {
		t.Fatalf("failed assertion leaked occurrence state: reserved=%t pending=%d result=%#v", reserved, pending, result)
	}
}

func TestVerbatimTranscriptAssertionsRemainForLateFinalization(t *testing.T) {
	task := TaskDef{
		OutputMode: TaskOutputModeVerbatim,
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
			{Pointer: "/raw_output_ref/id", Op: "exists"},
		}},
	}
	contract := taskResultSubmissionContractForTask(task)
	if len(contract.TaskResultAssertions) != 1 || len(contract.SubmissionTaskResultAssertions) != 0 {
		t.Fatalf("verbatim assertion partition=%#v", contract)
	}
	candidate := &TaskResult{Status: TaskResultStatusSuccess, Summary: "verbatim result"}
	if err := contract.validateFinalizableResult(candidate); err != nil {
		t.Fatalf("submit boundary falsely rejected transcript-owned assertion: %v", err)
	}
	workspace := t.TempDir()
	transcript, err := newTaskTranscriptForAttempt(workspace, "task-1", "run-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.RecordAssistantOutput("verbatim worker output"); err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeVerbatimTaskResult(transcript, candidate); err != nil {
		t.Fatal(err)
	}
	verification, err := ExecuteVerificationSpecWithTaskResult(context.Background(), "sh", workspace, *task.VerifySpec, candidate)
	if err != nil || verification == nil || verification.ExitCode != 0 {
		t.Fatalf("late transcript assertion verification=%#v err=%v", verification, err)
	}
}

func TestSubmitResultRejectsReviewerEvidencePathBeforeCommit(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), executionRunID: "run-contract-evidence"}
	task := filesReadAssertionTask()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "reviewer", Desc: "review workset", Execution: task.Execution, VerifySpec: task.VerifySpec},
	})[0]
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"review complete","files_read":[{"path":"sha256-assigned-diff"}],"evidence":[{"path":"sha256-assigned-diff"}]}`,
	})
	if err != nil || !response.IsError {
		t.Fatalf("reviewer evidence response=%#v err=%v", response, err)
	}
	if c.GetTaskResult(item.ID) != nil {
		t.Fatal("reviewer evidence submission was persisted")
	}
}

func TestTaskResultPromptToolDecoderParity(t *testing.T) {
	task := filesReadAssertionTask()
	contract := taskResultSubmissionContractForTask(task)
	info := submitResultToolInfo(contract)
	prompt := resultProtocolInstructions(task, map[string]bool{"submit_result": true})
	for field := range info.Parameters {
		if !strings.Contains(prompt, "`"+field+"`") {
			t.Fatalf("worker prompt advertises no schema-backed mention for field %q", field)
		}
	}
	if _, ok := info.Parameters["evidence"]; ok {
		t.Fatal("review-workset contract advertised evidence")
	}
	filesRead, ok := info.Parameters["files_read"].(map[string]any)
	if !ok || filesRead["minItems"] != 1 {
		t.Fatalf("files_read schema=%#v, want minItems=1", info.Parameters["files_read"])
	}
	if !slices.Contains(info.Required, "files_read") {
		t.Fatalf("required fields=%v, want files_read", info.Required)
	}
	if err := validateToolArguments(`{"status":"success","summary":"done","files_read":[{"path":"sha256-diff","purpose":"input"}]}`, info); err != nil {
		t.Fatalf("schema rejected valid decoder input: %v", err)
	}
	if _, err := decodeSubmitResultInput([]byte(`{"status":"success","summary":"done","files_read":[{"path":"sha256-diff"}]}`), contract); err != nil {
		t.Fatalf("decoder rejected schema-valid input: %v", err)
	}
}

func TestTaskResultContractAdmissionEnforcesAllAssertions(t *testing.T) {
	task := TaskDef{
		Execution: ExecutionContract{RequiresResult: true},
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
			{Pointer: "/status", Op: "equals", Value: TaskResultStatusSuccess},
			{Pointer: "/summary", Op: "non_empty"},
			{Pointer: "/facts/items", Op: "contains_scalar", Value: "observed"},
			{Pointer: "/files_read", Op: "exists"},
		}},
	}
	contract := taskResultSubmissionContractForTask(task)
	valid := &TaskResult{
		Status: TaskResultStatusSuccess, Summary: "review complete",
		Facts:     map[string]any{"items": []any{"observed"}},
		FilesRead: []FileRef{{Path: "sha256-diff"}},
	}
	if err := contract.validate(valid); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}

	for _, tc := range []struct {
		name   string
		result *TaskResult
		want   string
	}{
		{name: "wrong scalar", result: &TaskResult{Status: TaskResultStatusCompletedWithGaps, Summary: "review complete", Facts: map[string]any{"items": []any{"observed"}}, FilesRead: []FileRef{{Path: "sha256-diff"}}}, want: "expected"},
		{name: "missing nested value", result: &TaskResult{Status: TaskResultStatusSuccess, Summary: "review complete", Facts: map[string]any{"items": []any{"other"}}, FilesRead: []FileRef{{Path: "sha256-diff"}}}, want: "does not contain"},
		{name: "missing files_read", result: &TaskResult{Status: TaskResultStatusSuccess, Summary: "review complete", Facts: map[string]any{"items": []any{"observed"}}}, want: "does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := contract.validate(tc.result); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReviewerPromptMatchesEffectiveWorksetResultContract(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	promptBytes, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".agent-teams", "hufu-code-review", "reviewer.md"))
	if err != nil {
		t.Fatalf("read reviewer prompt: %v", err)
	}

	task := TaskDef{
		Execution: ExecutionContract{RequiresResult: true, RequiresGroundedResult: true, ForbidArtifacts: true},
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
			{Pointer: "/summary", Op: "non_empty"},
			{Pointer: "/files_read", Op: "min_items", Value: 1},
		}},
	}
	contract := taskResultSubmissionContractForTask(task)
	info := submitResultToolInfo(contract)
	protocol := resultProtocolInstructions(task, map[string]bool{"submit_result": true})
	staticPrompt := string(promptBytes)

	if strings.Contains(staticPrompt, "The only legal top-level `submit_result` fields are:") {
		t.Fatal("reviewer prompt duplicates the runtime-owned legal field list")
	}
	for _, required := range []string{"runtime-provided `submit_result` schema", "`files_read` is required", "`evidence` and `artifacts` are not legal"} {
		if !strings.Contains(staticPrompt, required) {
			t.Fatalf("reviewer prompt omitted %q", required)
		}
	}
	if _, ok := info.Parameters["evidence"]; ok || contract.AllowEvidence {
		t.Fatalf("workset contract still permits evidence: info=%#v contract=%#v", info, contract)
	}
	if _, ok := info.Parameters["artifacts"]; ok || contract.AllowArtifacts {
		t.Fatalf("workset contract still permits artifacts: info=%#v contract=%#v", info, contract)
	}
	for _, field := range []string{"status", "summary", "files_read"} {
		if !slices.Contains(info.Required, field) || !strings.Contains(protocol, "`"+field+"`") {
			t.Fatalf("field %q not shared by schema/protocol: required=%v protocol=%q", field, info.Required, protocol)
		}
	}
	if !strings.Contains(protocol, "`evidence` is not a legal field") {
		t.Fatalf("protocol omitted the forbidden-evidence rule: %q", protocol)
	}
}

func TestSubmitResultRejectedThenCorrectedSameAttemptUnderFailFast(t *testing.T) {
	c := &Coordinator{
		session:        &TeamSession{Config: agent.TeamConfig{Policies: agent.WorkflowPolicies{FailFast: true}}},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-contract-correction",
	}
	task := filesReadAssertionTask()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "reviewer", Desc: "review workset", Execution: task.Execution, VerifySpec: task.VerifySpec},
	})[0]
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	ctx := occurrenceTestContext(c, item.ID, 1)

	rejected, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"review complete"}`})
	if err != nil || !rejected.IsError {
		t.Fatalf("rejected first submission response=%#v err=%v", rejected, err)
	}
	if got := c.GetTaskResult(item.ID); got != nil || item.TypedResult != nil {
		t.Fatalf("rejected result was persisted: coordinator=%#v item=%#v", got, item.TypedResult)
	}

	accepted, err := tool.Run(ctx, fantasy.ToolCall{
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"review complete","files_read":[{"path":"sha256-assigned-diff","purpose":"assigned input"}]}`,
	})
	if err != nil || accepted.IsError {
		t.Fatalf("corrected submission response=%#v err=%v", accepted, err)
	}
	if got := c.GetTaskResult(item.ID); got == nil || len(got.FilesRead) != 1 || got.FilesRead[0].Path != "sha256-assigned-diff" {
		t.Fatalf("corrected result=%#v", got)
	}
}

func TestWorksetResultCanCorrectWithinSameAttemptAndComplete(t *testing.T) {
	c, producer, child, ref, receipt := newWorksetAuthorizationFixture(t)
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Put(context.Background(), PutArtifactRequest{
		Content: []byte(`{"schema_version":1,"items":[{"key":"one"}]}`), Path: "manifest.json", Kind: "workset_manifest",
		RunID: c.executionRunID, TaskID: producer.ID, Attempt: 1, Agent: producer.Agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	producer.TypedResult.Artifacts = append(producer.TypedResult.Artifacts, source.ArtifactRef)
	c.taskResults[producer.ID] = producer.TypedResult
	receipt.SourceArtifactID = source.ID
	receipt.SourceSHA256 = source.SHA256
	receipt.SourceArtifact = source.ArtifactRef
	child.WorksetBinding.SourceArtifactID = source.ID
	child.WorksetBinding.SourceSHA256 = source.SHA256
	child.WorksetBinding.SourceArtifact = source.ArtifactRef
	child.WorksetReceipt = cloneWorksetReceipt(receipt)
	child.VerifySpec = filesReadAssertionTask().VerifySpec
	child.Execution = ExecutionContract{RequiresResult: true, RequiresGroundedResult: true}
	c.session.Config.Policies = agent.WorkflowPolicies{FailFast: true}
	tool := &submitResultTool{coordinator: c, todoID: child.ID}
	ctx := occurrenceTestContext(c, child.ID, 1)
	first, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"first attempt"}`})
	if err != nil || !first.IsError || c.GetTaskResult(child.ID) != nil {
		t.Fatalf("invalid first submission response=%#v err=%v result=%#v", first, err, c.GetTaskResult(child.ID))
	}
	second, err := tool.Run(ctx, fantasy.ToolCall{
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"review complete","files_read":[{"path":"` + ref.ID + `","purpose":"assigned input"}]}`,
	})
	if err != nil || second.IsError {
		t.Fatalf("corrected same-attempt response=%#v err=%v", second, err)
	}
	child.TypedResult = c.GetTaskResult(child.ID)
	child.Status = TaskDone
	child.VerifyResult = &VerificationResult{ExitCode: 0}
	complete, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{
		Type: VerifyWorksetComplete, WorksetSourceTask: receipt.ParentTaskID, WorksetRequireTerminal: true,
		WorksetRequireVerified: true, WorksetAcceptedStatuses: []string{TaskResultStatusSuccess},
	})
	if err != nil || complete == nil || complete.ExitCode != 0 {
		t.Fatalf("workset_complete result=%#v err=%v", complete, err)
	}
}
