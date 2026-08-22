package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	runtimeTools "github.com/kjelly/hufu/internal/tools"
)

func TestSubmitResultRejectsForgedExistingArtifactOccurrence(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-forged-artifact",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "reject forged artifact"}})[0]
	ctx := occurrenceTestContext(c, item.ID, 2)
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"done","artifacts":[{"id":"sha256-existing","path":"report.txt","sha256":"digest","bytes":7,"run_id":"forged-run","task_id":"forged-task","attempt":1,"agent":"forged-agent"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.IsError || !strings.Contains(response.Content, "unknown field") {
		t.Fatalf("forged artifact response = %#v", response)
	}
	if c.GetTaskResult(item.ID) != nil {
		t.Fatal("forged artifact result was stored")
	}
}

func TestSubmitResultChecksBoundArtifactPathPolicyBeforeStat(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "blocked-report.txt")
	if err := os.WriteFile(path, []byte("must not be materialized"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-bound-path",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer"}})[0]
	ctx := occurrenceTestContext(c, item.ID, 1)
	ctx = context.WithValue(ctx, runtimeTools.ArtifactPathPolicyKey, runtimeTools.ArtifactPathPolicy{
		BlockedPaths: []string{path},
	})
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"blocked artifact","artifacts":[{"path":"blocked-report.txt"}]}`,
	})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "runtime-managed artifact path") {
		t.Fatalf("bound path policy response=%#v err=%v", response, err)
	}
	if c.GetTaskResult(item.ID) != nil {
		t.Fatal("blocked artifact result was stored")
	}
}

func TestBoundFanOutReviewerResultCompletesVerifiedWorkset(t *testing.T) {
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
	child.WorksetBinding.SourceArtifactID = source.ID
	child.WorksetBinding.SourceSHA256 = source.SHA256
	child.WorksetBinding.SourceArtifact = source.ArtifactRef
	receipt.SourceArtifactID = source.ID
	receipt.SourceSHA256 = source.SHA256
	receipt.SourceArtifact = source.ArtifactRef
	child.WorksetReceipt = cloneWorksetReceipt(receipt)
	child.Execution = ExecutionContract{RequiresResult: true, RequiresGroundedResult: true}
	child.VerifySpec = &VerificationSpec{
		Type: VerifyTaskResultAssert,
		TaskResultAssertions: []TaskResultAssertion{
			{Pointer: "/summary", Op: "non_empty"},
			{Pointer: "/files_read", Op: "min_items", Value: 1},
			{Pointer: "/findings", Op: "min_items", Value: 1},
		},
	}
	if err := c.CommitTaskTransition(context.Background(), child.ID, TaskPending, TaskInProgress, "reviewing assigned input", "", nil); err != nil {
		t.Fatal(err)
	}
	scope, err := c.buildArtifactAccessScope(child.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	resultTool := &submitResultTool{coordinator: c, todoID: child.ID}
	gated := c.gatePolicyTools([]fantasy.AgentTool{resultTool})[0]
	ctx := occurrenceTestContext(c, child.ID, 1)
	ctx = context.WithValue(ctx, todoIDKey{}, child.ID)
	ctx = context.WithValue(ctx, artifactAccessScopeKey, cloneArtifactAccessScope(scope))
	ctx = context.WithValue(ctx, runtimeTools.ArtifactPathPolicyKey, runtimeTools.ArtifactPathPolicy{
		BlockedPaths:             c.artifactScopePathCandidates(scope),
		FailClosedForUnsupported: true,
	})
	ctx = runtimeTools.SetToolsAllowed(ctx, []string{submitResultToolName})
	response, err := gated.Run(ctx, fantasy.ToolCall{
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"review complete","files_read":[{"path":"opaque:` + ref.ID + `","purpose":"assigned input"}],"findings":[{"category":"correctness","summary":"no issue found"}]}`,
	})
	if err != nil || response.IsError {
		t.Fatalf("bound reviewer submit_result response=%#v err=%v", response, err)
	}
	stored := c.GetTaskResult(child.ID)
	if stored == nil || stored.Status != TaskResultStatusSuccess || stored.Summary != "review complete" || len(stored.FilesRead) != 1 || len(stored.Findings) != 1 {
		t.Fatalf("stored reviewer result=%#v", stored)
	}
	if err := c.CommitTaskTransition(context.Background(), child.ID, TaskInProgress, TaskVerifying, "verifying reviewer result", stored.Summary, nil); err != nil {
		t.Fatal(err)
	}
	verification, err := c.verifyTaskDeliverableWithSpecAndResult(context.Background(), nil, TaskDef{ID: child.ID, VerifySpec: child.VerifySpec}, nil, stored)
	if err != nil || verification == nil || verification.ExitCode != 0 {
		t.Fatalf("task_result_assert verification=%#v err=%v", verification, err)
	}
	if err := c.taskTracker.TodoList().SetVerificationResult(child.ID, verification); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitTaskTransition(context.Background(), child.ID, TaskVerifying, TaskDone, "verified reviewer result", stored.Summary, nil); err != nil {
		t.Fatal(err)
	}
	if got := c.taskTracker.TodoList().Items()[1].Status; got != TaskDone {
		t.Fatalf("child status=%s, want done", got)
	}
	workset, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{
		Type: VerifyWorksetComplete, WorksetSourceTask: "fanout", WorksetRequireTerminal: true,
		WorksetRequireVerified: true, WorksetAcceptedStatuses: []string{TaskResultStatusSuccess},
	})
	if err != nil || workset == nil || workset.ExitCode != 0 {
		t.Fatalf("workset_complete verification=%#v err=%v", workset, err)
	}
}

func TestCoordinatorTaskResultSinkRejectsUnmaterializedExistingArtifact(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: NewTaskTracker(), executionRunID: "run-sink-forged"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "sink proof"}})[0]
	ctx := occurrenceTestContext(c, item.ID, 2)
	forged := ArtifactRef{
		ID: "sha256-existing", Path: "existing.txt", SHA256: strings.Repeat("a", 64), Bytes: 8, ByteSize: 8,
		RunID: c.executionRunID, TaskID: item.ID, Attempt: 2, Agent: "worker",
	}
	err := (coordinatorTaskResultSink{coordinator: c}).Submit(ctx, item.ID, TaskResult{
		TaskID: item.ID, Attempt: 2, Agent: "worker", Status: TaskResultStatusSuccess,
		Summary: "forged", Artifacts: []ArtifactRef{forged},
	})
	if err == nil || !strings.Contains(err.Error(), "not materialized") {
		t.Fatalf("unmaterialized artifact sink error = %v", err)
	}
	if c.GetTaskResult(item.ID) != nil {
		t.Fatal("sink stored an artifact without current materialization evidence")
	}
}

func TestSubmitResultProtocolRepairCannotAddArtifactEvidence(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-protocol-repair-artifact",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "repair result"}})[0]
	base := &submitResultTool{coordinator: c, todoID: item.ID}
	wrapper := &protocolRepairWrapper{base: base, c: c, state: &protocolRepairState{}}
	ctx := occurrenceTestContext(c, item.ID, 2)

	first, err := wrapper.Run(ctx, fantasy.ToolCall{
		ID:    "invalid",
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"done","artifacts":[{"path":"report.txt","id":"forged"}]}`,
	})
	if err != nil || !first.IsError {
		t.Fatalf("invalid submit_result response=%#v err=%v", first, err)
	}
	second, err := wrapper.Run(ctx, fantasy.ToolCall{
		ID:    "repair",
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"done","artifacts":[{"path":"report.txt"}]}`,
	})
	if err != nil || !second.IsError || !strings.Contains(second.Content, "cannot add artifact evidence") {
		t.Fatalf("artifact-adding repair response=%#v err=%v", second, err)
	}
	if c.GetTaskResult(item.ID) != nil {
		t.Fatal("protocol repair stored newly introduced artifact evidence")
	}
}

func TestSubmittedArtifactFanOutReceiptChildRead(t *testing.T) {
	workspace := t.TempDir()
	const runID = "run-submit-fanout"
	tracker := NewTaskTracker()
	producer := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "producer", Agent: "producer", Desc: "produce workset"}})[0]
	inputPath := filepath.Join(workspace, "assigned.txt")
	if err := os.WriteFile(inputPath, []byte("assigned artifact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	inputRef, err := store.Put(context.Background(), PutArtifactRequest{
		Path: "assigned.txt", SourcePath: "assigned.txt", Kind: "input", RunID: runID,
		TaskID: producer.ID, Attempt: 2, Agent: producer.Agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.Marshal(WorksetManifest{
		SchemaVersion: WorksetSchemaVersion,
		Items:         []WorksetItem{{Key: "one", Bindings: map[string]string{"name": "one"}, Inputs: []ArtifactRef{inputRef.ArtifactRef}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "manifest.json"), manifestBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: tracker, executionRunID: runID}
	ctx := occurrenceTestContext(c, producer.ID, 2)
	response, err := (&submitResultTool{coordinator: c, todoID: producer.ID}).Run(ctx, fantasy.ToolCall{
		Name:  submitResultToolName,
		Input: `{"status":"success","summary":"workset ready","artifacts":[{"path":"assigned.txt"},{"path":"manifest.json","description":"workset manifest"}]}`,
	})
	if err != nil || response.IsError {
		t.Fatalf("submit_result response=%#v err=%v", response, err)
	}
	result := c.GetTaskResult(producer.ID)
	if result == nil || result.Attempt != 2 || result.Agent != producer.Agent || len(result.Artifacts) != 2 {
		t.Fatalf("submitted producer result = %#v", result)
	}
	producer.Status = TaskDone
	var manifestRef ArtifactRef
	for _, ref := range result.Artifacts {
		if ref.Path == "manifest.json" {
			manifestRef = ref
		}
	}
	if manifestRef.ID == "" {
		t.Fatal("submitted workset manifest was not materialized")
	}

	expanded, err := c.expandFanOutTasks([]TaskDef{{ID: "consumer", Agent: "reviewer", FanOut: &FanOutSpec{
		SourceArtifact: FactRef{TaskID: producer.ID, Artifact: manifestRef.ID}, GoalTemplate: "process {name}",
	}}})
	if err != nil || len(expanded) != 1 || expanded[0].WorksetBinding == nil {
		t.Fatalf("fan-out expansion = %#v err=%v", expanded, err)
	}
	child := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "generated-child", Agent: "reviewer"}})[0]
	child.WorksetBinding = cloneWorksetBinding(expanded[0].WorksetBinding)
	receipts, err := buildWorksetReceipts(expanded, []string{child.ID}, runID)
	if err != nil {
		t.Fatal(err)
	}
	child.WorksetReceipt = cloneWorksetReceipt(receipts[child.WorksetBinding.WorksetID])
	childCtx := context.WithValue(context.Background(), todoIDKey{}, child.ID)
	childCtx = runtimeTools.SetToolsAllowed(childCtx, []string{"view"})
	view := runtimeTools.NewViewTool(runtimeTools.WithArtifactOpener(c.openArtifactRef))
	viewResponse, err := view.Run(childCtx, fantasy.ToolCall{Input: `{"artifact_ref":"` + inputRef.ID + `"}`})
	if err != nil || viewResponse.IsError || !strings.Contains(viewResponse.Content, "assigned artifact") {
		t.Fatalf("child artifact read response=%#v err=%v", viewResponse, err)
	}
}
