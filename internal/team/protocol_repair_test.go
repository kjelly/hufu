package team

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func readStoredArtifact(t *testing.T, workspace, id string) []byte {
	t.Helper()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	reader, err := store.Open(t.Context(), id)
	if err != nil {
		t.Fatalf("open artifact %q: %v", id, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read artifact %q: %v", id, err)
	}
	return data
}

func TestClassifyRepairFailure_SubReasons(t *testing.T) {
	validProgress := &TaskResult{Status: "partial", Source: "submitted", Summary: "still working"}

	tests := []struct {
		name              string
		steps             []fantasy.StepResult
		result            *TaskResult
		wantReason        RepairFailureReason
		wantExecutionFail bool
	}{
		{
			name:              "no tool call",
			wantReason:        RepairFailureNoToolCall,
			wantExecutionFail: false,
		},
		{
			name:              "invalid schema",
			steps:             invalidSchemaRepairSteps(),
			wantReason:        RepairFailureInvalidSchema,
			wantExecutionFail: false,
		},
		{
			name:              "progress is not final",
			result:            validProgress,
			wantReason:        RepairFailureProgressNotFinal,
			wantExecutionFail: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotReason, gotExecutionFail := classifyRepairFailure(tt.steps, tt.result)
			if gotReason != tt.wantReason || gotExecutionFail != tt.wantExecutionFail {
				t.Fatalf("classifyRepairFailure() = (%q, %t), want (%q, %t)", gotReason, gotExecutionFail, tt.wantReason, tt.wantExecutionFail)
			}
		})
	}

	if RepairFailureProgressNotFinal.IsProtocolRepairFailure() {
		t.Fatal("progress_not_final must not be counted as a protocol repair failure")
	}
	if !RepairFailureInvalidSchema.IsProtocolRepairFailure() {
		t.Fatal("invalid_schema must be counted as a protocol repair failure")
	}
}

func TestClassifyTaskFailure_ExplicitOverrideBeatsProtocolText(t *testing.T) {
	err := withFailureClassOverride(errors.New("execution failure (reclassified from protocol repair)"), FailureExecution)
	if got := ClassifyTaskFailureStructured(FailureClassificationInput{Err: err}); got != FailureExecution {
		t.Fatalf("explicit failure class = %q, want %q", got, FailureExecution)
	}
}

func TestProtocolAttemptWasReadOnly(t *testing.T) {
	readOnly := []fantasy.StepResult{
		{Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.ToolCallContent{ToolName: "view", Input: `{"file_path":"internal/team/coordinator.go"}`},
			fantasy.ToolCallContent{ToolName: "bash", Input: `{"command":"cd internal/team && git diff --stat"}`},
		}}},
	}
	if !protocolAttemptWasReadOnly(readOnly) {
		t.Fatal("read-only tool sequence must be eligible for the bounded capability fallback")
	}

	mutating := []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolName: "write", Input: `{"file_path":"changed.go"}`},
	}}}}
	if protocolAttemptWasReadOnly(mutating) {
		t.Fatal("mutating tool sequence must never be eligible for worker replay")
	}
	if protocolAttemptWasReadOnly(nil) {
		t.Fatal("an attempt with no recorded tool calls is not sufficient proof for worker replay")
	}
}

func TestProtocolRepairEvidenceSummaryDoesNotReplayToolMessages(t *testing.T) {
	summary := protocolRepairEvidenceSummary([]fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolName: "view", Input: `{"file_path":"internal/team/agent.go"}`},
	}}}}, &ArtifactRef{ID: "transcript-1"})
	if !strings.Contains(summary, "view") || !strings.Contains(summary, "transcript-1") || !strings.Contains(summary, "Only submit_result") {
		t.Fatalf("evidence summary = %q, want bounded repair guidance", summary)
	}
	if strings.Contains(summary, "file_path") {
		t.Fatalf("evidence summary leaked executable tool input: %q", summary)
	}
}

func TestProtocolRepairMetricsExcludeOnlyProgressTurn(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "mixed repair sequence"}})[0]
	item.ExecutionReceipts = []ExecutionReceipt{{RepairProvenance: &RepairProvenance{
		Attempted:      true,
		RepairAttempts: 2,
		History: []RepairAttemptProvenance{
			{Attempt: 1, FailureReason: RepairFailureInvalidSchema},
			{Attempt: 2, FailureReason: RepairFailureProgressNotFinal},
		},
		FailureReason: RepairFailureProgressNotFinal,
	}}}
	metrics := (&Coordinator{taskTracker: tracker}).Metrics()
	if metrics.ProtocolRepairsAttempted != 1 {
		t.Fatalf("mixed repair protocol attempts = %d, want 1 (invalid_schema only)", metrics.ProtocolRepairsAttempted)
	}
}

func TestProtocolRepair_ProgressNotFinalIsExecutionFailure(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "progress-repair", Timeout: 30, MaxRetries: 2},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", MaxRetries: 2, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-progress-repair",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "incomplete execution"}})[0]

	c.workerAgentOverride = &countingTextAgent{calls: &workerCalls, text: "execution produced progress"}
	c.repairAgentOverride = &mockRepairAgent{onSubmit: func() {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "worker", Status: "partial",
			Summary: "work remains", Source: "submitted",
		})
	}}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "incomplete execution", Recovery: RecoveryRetry,
		SideEffect: SideEffectExternalWrite,
		Execution:  ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err == nil {
		t.Fatal("expected execution failure after non-final repair result")
	}
	if !strings.Contains(err.Error(), "reclassified from protocol repair") {
		t.Fatalf("error = %q, want explicit execution reclassification", err)
	}

	got := c.taskTracker.TodoList().Items()[0]
	if got.ExecutionReceipt == nil || got.ExecutionReceipt.RepairProvenance == nil {
		t.Fatal("expected repair provenance on the execution receipt")
	}
	if got.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureProgressNotFinal {
		t.Fatalf("failure reason = %q, want %q", got.ExecutionReceipt.RepairProvenance.FailureReason, RepairFailureProgressNotFinal)
	}
	metrics := c.Metrics()
	if metrics.ProtocolRepairsAttempted != 0 || metrics.ProtocolRepairsSucceeded != 0 {
		t.Fatalf("progress_not_final polluted protocol repair metrics: attempted=%d succeeded=%d", metrics.ProtocolRepairsAttempted, metrics.ProtocolRepairsSucceeded)
	}
	if workerCalls != 1 {
		t.Fatalf("worker calls = %d, want 1 (this test uses incomplete evidence and should not replay)", workerCalls)
	}
}

func TestProtocolRepair_ProgressNotFinalRetriesAndClearsAttemptResult(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0
	resultClearedBeforeRetry := false

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "progress-retry", Timeout: 30, MaxRetries: 2},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", MaxRetries: 2, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-progress-retry",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "replay progress result"}})[0]

	c.workerAgentOverride = &replayableWorkerAgent{
		calls: &workerCalls,
		onSecond: func() {
			resultClearedBeforeRetry = c.GetTaskResult(item.ID) == nil
			c.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: "worker", Status: "success",
				Summary: "new attempt completed", Source: "submitted",
			})
		},
	}
	c.repairAgentOverride = &mockRepairAgent{onSubmit: func() {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "worker", Status: "partial",
			Summary: "first attempt still in progress", Source: "submitted",
		})
	}}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "replay progress result", Recovery: RecoveryRetry,
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err != nil {
		t.Fatalf("progress result should retry and complete, got %v", err)
	}
	if workerCalls != 2 {
		t.Fatalf("worker invocation count = %d, want 2 (RetryWorker)", workerCalls)
	}
	if !resultClearedBeforeRetry {
		t.Fatal("prior partial result remained visible at the start of the replayed worker attempt")
	}

	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskDone || got.TypedResult == nil || got.TypedResult.Status != "success" {
		t.Fatalf("final task state = status %s, typed result %#v; want done/success", got.Status, got.TypedResult)
	}
	if len(got.ExecutionReceipts) != 2 {
		t.Fatalf("execution receipt history length = %d, want 2", len(got.ExecutionReceipts))
	}
	if got.ExecutionReceipts[0].RepairProvenance == nil || got.ExecutionReceipts[0].RepairProvenance.FailureReason != RepairFailureProgressNotFinal {
		t.Fatalf("first attempt provenance = %#v, want progress_not_final", got.ExecutionReceipts[0].RepairProvenance)
	}
	metrics := c.Metrics()
	if metrics.RetriesByFailureClass[FailureExecution] != 1 {
		t.Fatalf("execution retries = %d, want 1", metrics.RetriesByFailureClass[FailureExecution])
	}
	if metrics.RetriesByFailureClass[FailureProtocol] != 0 {
		t.Fatalf("protocol retries = %d, want 0", metrics.RetriesByFailureClass[FailureProtocol])
	}
	if metrics.ProtocolRepairsAttempted != 0 {
		t.Fatalf("progress_not_final counted as protocol repair = %d, want 0", metrics.ProtocolRepairsAttempted)
	}
}

func TestProtocolRepair_InvalidSchemaGetsOneSchemaOnlyRetry(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0
	repairCalls := 0
	var prompts []string

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "schema-repair", Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-schema-repair-success",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "schema repair task"}})[0]
	c.workerAgentOverride = &countingTextAgent{calls: &workerCalls, text: "execution output"}
	c.repairAgentOverride = &scriptedRepairAgent{
		calls:   &repairCalls,
		prompts: &prompts,
		steps: func(call int) []fantasy.StepResult {
			if call == 1 {
				return invalidSchemaRepairSteps()
			}
			return nil
		},
		onCall: func(call int) {
			if call == 2 {
				c.storeSubmittedTaskResult(item.ID, &TaskResult{
					TaskID: item.ID, Agent: "worker", Status: "success",
					Summary: "schema-only repair succeeded", Source: "submitted",
				})
			}
		},
	}

	out, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "schema repair task",
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err != nil {
		t.Fatalf("expected schema-only repair to succeed, got %v", err)
	}
	if out == "" {
		t.Fatal("expected task output")
	}
	if workerCalls != 1 {
		t.Fatalf("worker calls = %d, want 1; schema repair must not replay execution", workerCalls)
	}
	if repairCalls != 2 {
		t.Fatalf("repair calls = %d, want exactly 2 (one schema-only retry)", repairCalls)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[1], "Schema-only repair") || !strings.Contains(prompts[1], "Do NOT execute work") || !strings.Contains(prompts[1], "`status`") || !strings.Contains(prompts[1], "non-empty `summary`") {
		t.Fatalf("second repair prompt was not schema-only: %#v", prompts)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskDone {
		t.Fatalf("task status = %s, want done", got.Status)
	}
	prov := got.ExecutionReceipt.RepairProvenance
	if prov == nil || !prov.Success || prov.RepairAttempts != 2 {
		t.Fatalf("unexpected schema repair provenance: %#v", prov)
	}
	if len(prov.History) != 2 || prov.History[0].FailureReason != RepairFailureInvalidSchema || !prov.History[1].Success {
		t.Fatalf("schema repair history = %#v, want invalid_schema then success", prov.History)
	}
	if got := c.Metrics().ProtocolRepairsAttempted; got != 2 {
		t.Fatalf("protocol repair attempts metric = %d, want 2 repair turns", got)
	}
}

func TestProtocolRepair_TwoInvalidSchemasRecoverProvisionallyAndBlock(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0
	repairCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "schema-repair-failure", Timeout: 30, MaxRetries: 2},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", MaxRetries: 2, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-schema-repair-failure",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "unrecoverable schema task"}})[0]
	c.workerAgentOverride = &countingTextAgent{calls: &workerCalls, text: "original execution evidence"}
	c.repairAgentOverride = &scriptedRepairAgent{
		calls: &repairCalls,
		steps: func(int) []fantasy.StepResult { return invalidSchemaRepairSteps() },
	}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "unrecoverable schema task",
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err == nil {
		t.Fatal("expected final protocol repair failure")
	}
	if workerCalls != 1 || repairCalls != 2 {
		t.Fatalf("worker/repair calls = %d/%d, want 1/2", workerCalls, repairCalls)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskBlocked {
		t.Fatalf("task status = %s, detail=%q, want blocked for reconcile_only", got.Status, got.Detail)
	}
	prov := got.ExecutionReceipt.RepairProvenance
	if prov == nil || prov.Success || prov.FailureReason != RepairFailureInvalidSchema || prov.RepairAttempts != 2 {
		t.Fatalf("unexpected final repair provenance: %#v", prov)
	}
	if len(prov.History) != 2 || prov.History[0].FailureReason != RepairFailureInvalidSchema || prov.History[1].FailureReason != RepairFailureInvalidSchema {
		t.Fatalf("repair history = %#v, want two invalid_schema failures", prov.History)
	}
	if got.TypedResult == nil || got.TypedResult.Source != "recovered_protocol" || got.TypedResult.Confidence <= 0 || got.TypedResult.RawOutputRef == nil {
		t.Fatalf("missing evidence-backed recovered_protocol result: %#v", got.TypedResult)
	}
	if !strings.Contains(got.TypedResult.Summary, "original execution evidence") {
		t.Fatalf("provisional result lost worker evidence: %q", got.TypedResult.Summary)
	}
	if gotMetrics := c.Metrics().ProtocolRepairsAttempted; gotMetrics != 2 {
		t.Fatalf("protocol repair attempts metric = %d, want 2 repair turns", gotMetrics)
	}
}

type mockWorkerTextAgent struct {
	text string
}

type countingTextAgent struct {
	calls *int
	text  string
}

// resultContractWorker lets protocol-repair tests model an agent that did
// useful read-only work but omitted submit_result. Its recorded messages make
// it possible to prove that a repair/fallback does not replay old tool context.
type resultContractWorker struct {
	calls        *int
	callMessages *[][]fantasy.Message
	onSecond     func()
}

func (m *resultContractWorker) response(call fantasy.AgentCall) *fantasy.AgentResult {
	(*m.calls)++
	if m.callMessages != nil {
		*m.callMessages = append(*m.callMessages, append([]fantasy.Message(nil), call.Messages...))
	}
	if *m.calls == 2 && m.onSecond != nil {
		m.onSecond()
	}
	return &fantasy.AgentResult{
		Steps: []fantasy.StepResult{{
			Messages: []fantasy.Message{fantasy.NewUserMessage("old worker tool history")},
			Response: fantasy.Response{Content: fantasy.ResponseContent{
				fantasy.ToolCallContent{ToolName: "view", Input: `{"file_path":"internal/team/coordinator.go"}`},
			}},
		}},
		Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "read-only inspection completed"},
		}},
	}
}

func (m *resultContractWorker) Generate(_ context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return m.response(call), nil
}

func (m *resultContractWorker) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.response(fantasy.AgentCall{Prompt: call.Prompt, Messages: call.Messages}), nil
}

// exhaustedWorkerAgent simulates a worker that has consumed the final allowed
// execution step without a final response. It lets the test verify that a
// requires-result task bypasses prose rescue and enters the result-only path.
type exhaustedWorkerAgent struct {
	calls *int
}

func (m *exhaustedWorkerAgent) response() *fantasy.AgentResult {
	(*m.calls)++
	return &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "read-only evidence from the final work step"},
	}}}}}
}

func (m *exhaustedWorkerAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return m.response(), nil
}

func (m *exhaustedWorkerAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.response(), nil
}

func (m *countingTextAgent) response() *fantasy.AgentResult {
	(*m.calls)++
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: m.text},
	}}}
}

func (m *countingTextAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return m.response(), nil
}

func (m *countingTextAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.response(), nil
}

func (m *mockWorkerTextAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: m.text},
			},
		},
	}, nil
}

func (m *mockWorkerTextAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: m.text},
			},
		},
	}, nil
}

type mockRepairAgent struct {
	onSubmit func()
}

type scriptedRepairAgent struct {
	calls    *int
	prompts  *[]string
	messages *[][]fantasy.Message
	onCall   func(int)
	steps    func(int) []fantasy.StepResult
}

func invalidSchemaRepairSteps() []fantasy.StepResult {
	return []fantasy.StepResult{{Response: fantasy.Response{
		Content: fantasy.ResponseContent{
			fantasy.ToolCallContent{ToolName: submitResultToolName, Input: "{bad json"},
			fantasy.ToolResultContent{
				ToolName: submitResultToolName,
				Result:   fantasy.ToolResultOutputContentError{Error: errors.New("invalid result schema")},
			},
		},
	}}}
}

func (m *scriptedRepairAgent) result(call fantasy.AgentCall) *fantasy.AgentResult {
	(*m.calls)++
	if m.prompts != nil {
		*m.prompts = append(*m.prompts, call.Prompt)
	}
	if m.messages != nil {
		*m.messages = append(*m.messages, append([]fantasy.Message(nil), call.Messages...))
	}
	if m.onCall != nil {
		m.onCall(*m.calls)
	}
	steps := []fantasy.StepResult(nil)
	if m.steps != nil {
		steps = m.steps(*m.calls)
	}
	return &fantasy.AgentResult{Steps: steps, Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "scripted repair response"},
	}}}
}

func (m *scriptedRepairAgent) Generate(_ context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return m.result(call), nil
}

func (m *scriptedRepairAgent) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.result(fantasy.AgentCall{Prompt: call.Prompt, Messages: call.Messages}), nil
}

type replayableWorkerAgent struct {
	calls    *int
	onSecond func()
}

func (m *replayableWorkerAgent) response() *fantasy.AgentResult {
	(*m.calls)++
	text := "attempt-one-original"
	if *m.calls == 2 {
		text = "attempt-two-original"
		if m.onSecond != nil {
			m.onSecond()
		}
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: text},
	}}}
}

func (m *replayableWorkerAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return m.response(), nil
}

func (m *replayableWorkerAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.response(), nil
}

func (m *mockRepairAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	if m.onSubmit != nil {
		m.onSubmit()
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "Repaired typed result submitted."},
			},
		},
	}, nil
}

func (m *mockRepairAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if m.onSubmit != nil {
		m.onSubmit()
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "Repaired typed result submitted."},
			},
		},
	}, nil
}

func TestProtocolRepair_SuccessAndReceipt(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "protocol-repair", Timeout: 30, MaxRetries: 3},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-101",
	}

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker",
		Desc:  "data processing task",
	}})
	todoID := items[0].ID

	// First agent run produces output but omits submit_result.
	c.workerAgentOverride = &mockWorkerTextAgent{text: "Processed input data successfully."}

	// We inject a mock repair agent that calls submit_result when invoked during repair.
	c.repairAgentOverride = &mockRepairAgent{
		onSubmit: func() {
			c.storeSubmittedTaskResult(todoID, &TaskResult{
				TaskID:   todoID,
				Agent:    "worker",
				Status:   "success",
				Summary:  "repaired structured result",
				Source:   "submitted",
				Evidence: nil,
			})
		},
	}

	out, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker",
		Goal:  "data processing task",
		Execution: ExecutionContract{
			RequiresResult: true,
		},
	}, todoID)

	if err != nil {
		t.Fatalf("expected protocol repair to succeed, got error: %v", err)
	}
	if out == "" {
		t.Error("expected non-empty task output")
	}
	if strings.Contains(out, "VERBATIM TRANSCRIPT CAPTURED") {
		t.Fatalf("summary-mode task must preserve worker output, got verbatim manifest: %q", out)
	}
	if !strings.Contains(out, "Processed input data successfully") {
		t.Fatalf("summary-mode task output lost worker response: %q", out)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskDone {
		t.Fatalf("task status = %s, want done", item.Status)
	}

	// Verify ExecutionReceipt is preserved on TodoItem
	if item.ExecutionReceipt == nil {
		t.Fatal("expected ExecutionReceipt to be preserved on TodoItem")
	}
	if item.ExecutionReceipt.RunID != "run-protocol-101" {
		t.Errorf("Receipt RunID = %q, want run-protocol-101", item.ExecutionReceipt.RunID)
	}
	if item.ExecutionReceipt.TaskID != todoID {
		t.Errorf("Receipt TaskID = %q, want %s", item.ExecutionReceipt.TaskID, todoID)
	}
	if item.ExecutionReceipt.Attempt != 1 {
		t.Errorf("Receipt Attempt = %d, want 1", item.ExecutionReceipt.Attempt)
	}
	if item.ExecutionReceipt.ProducerID != "worker" {
		t.Errorf("Receipt ProducerID = %q, want worker", item.ExecutionReceipt.ProducerID)
	}
	if item.ExecutionReceipt.TranscriptRef == "" {
		t.Error("expected ExecutionReceipt.TranscriptRef to be populated in production execution path")
	}
	transcript := readStoredArtifact(t, c.session.Workspace, item.ExecutionReceipt.TranscriptRef)
	if !strings.Contains(string(transcript), `"event":"assistant_output"`) {
		t.Fatalf("original transcript does not contain worker output: %s", transcript)
	}
	if strings.Contains(string(transcript), "Repaired typed result submitted") {
		t.Fatal("repair output must not be appended to the original transcript")
	}
	if item.ExecutionReceipt.RepairProvenance == nil || !item.ExecutionReceipt.RepairProvenance.Success {
		t.Error("expected repair provenance to record success=true")
	}
	if len(item.ExecutionReceipts) != 1 {
		t.Fatalf("execution receipt history length = %d, want one receipt for attempt 1", len(item.ExecutionReceipts))
	}
}

func TestProtocolRepair_UsesCleanContextInsteadOfOriginalToolHistory(t *testing.T) {
	workspace := t.TempDir()
	workerCalls := 0
	repairCalls := 0
	var repairMessages [][]fantasy.Message
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "clean-protocol-repair", Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-clean-protocol-repair",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect runtime"}})[0]
	c.workerAgentOverride = &resultContractWorker{calls: &workerCalls}
	c.repairAgentOverride = &scriptedRepairAgent{
		calls:    &repairCalls,
		messages: &repairMessages,
		onCall: func(int) {
			c.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: "worker", Status: "success", Summary: "inspection finalized", Source: "submitted",
			})
		},
	}

	if _, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "inspect runtime", Execution: ExecutionContract{RequiresResult: true},
	}, item.ID); err != nil {
		t.Fatalf("executeTask returned error: %v", err)
	}
	if workerCalls != 1 {
		t.Fatalf("worker calls = %d, want 1", workerCalls)
	}
	if len(repairMessages) != 1 || len(repairMessages[0]) != 0 {
		t.Fatalf("repair inherited old tool history: %#v", repairMessages)
	}
}

func TestProtocolRepair_ReadOnlyResultContractFailureRetriesOnceCleanly(t *testing.T) {
	workspace := t.TempDir()
	workerCalls := 0
	repairCalls := 0
	var workerMessages [][]fantasy.Message
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "capability-fallback", Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"worker": {
					Name: "worker", Role: "worker", MaxRetries: 1,
					SideEffect: string(SideEffectWorkspaceWrite), Recovery: string(RecoveryManual),
					Generation: agent.GenerationParams{Model: "test"},
				},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-capability-fallback",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect before implementation"}})[0]
	c.workerAgentOverride = &resultContractWorker{
		calls:        &workerCalls,
		callMessages: &workerMessages,
		onSecond: func() {
			c.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: "worker", Status: "success", Summary: "second attempt honoured contract", Source: "submitted",
			})
		},
	}
	// This repair agent deliberately produces no submit_result call. The first
	// worker attempt used only view, so the runtime may make one fresh attempt.
	c.repairAgentOverride = &scriptedRepairAgent{calls: &repairCalls}

	if _, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "inspect before implementation", Execution: ExecutionContract{RequiresResult: true},
	}, item.ID); err != nil {
		t.Fatalf("read-only result-contract fallback should complete: %v", err)
	}
	if workerCalls != 2 {
		t.Fatalf("worker calls = %d, want exactly 2 (one bounded fallback)", workerCalls)
	}
	if len(workerMessages) != 2 || len(workerMessages[1]) != 0 {
		t.Fatalf("fallback worker attempt inherited old tool history: %#v", workerMessages)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskDone || got.TypedResult == nil || got.TypedResult.Status != "success" {
		t.Fatalf("fallback final state = status %s result %#v, want done/success", got.Status, got.TypedResult)
	}
}

func TestReadOnlyFreeTextWorkerCapturesTranscriptEvidence(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name:                 "read-only-free-text",
				Timeout:              30,
				MaxRetries:           1,
				AllowFreeTextResults: true,
			},
			Agents: map[string]*agent.AgentDef{
				"reviewer": {Name: "reviewer", Role: "worker", SideEffect: string(SideEffectNone), Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-read-only-free-text",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "review changes"}})[0]
	c.workerAgentOverride = &mockWorkerTextAgent{text: "[WARNING] internal/team/example.go:1 — review finding"}

	output, err := c.executeTask(context.Background(), TaskDef{Agent: "reviewer", Goal: "review changes"}, item.ID)
	if err != nil {
		t.Fatalf("executeTask returned error: %v", err)
	}
	if !strings.Contains(output, "review finding") {
		t.Fatalf("output = %q, want original free-text result", output)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.ExecutionReceipt == nil || got.ExecutionReceipt.TranscriptRef == "" {
		t.Fatalf("read-only free-text completion must retain transcript evidence: %#v", got.ExecutionReceipt)
	}
}

func TestProtocolRepair_StepBudgetExhaustionUsesResultOnlyFinalization(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0
	repairCalls := 0
	var repairPrompts []string

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "budget-finalization", Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", MaxSteps: 1, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-budget-finalization",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "long read-only task"}})[0]
	c.workerAgentOverride = &exhaustedWorkerAgent{calls: &workerCalls}
	c.repairAgentOverride = &scriptedRepairAgent{
		calls:   &repairCalls,
		prompts: &repairPrompts,
		onCall: func(int) {
			c.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: "worker", Status: TaskResultStatusSuccess,
				Summary: "finalized from read-only evidence", Source: "submitted",
			})
		},
	}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "long read-only task", Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err != nil {
		t.Fatalf("step-budget finalization should submit a result, got %v", err)
	}
	if workerCalls != 1 {
		t.Fatalf("worker calls = %d, want 1; requires-result exhaustion must not invoke prose rescue", workerCalls)
	}
	if repairCalls != 1 || len(repairPrompts) != 1 || !strings.Contains(repairPrompts[0], "Finalization Instructions") {
		t.Fatalf("repair calls/prompts = %d/%q, want one result-only budget finalization", repairCalls, repairPrompts)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskDone || got.TypedResult == nil || got.TypedResult.Status != TaskResultStatusSuccess {
		t.Fatalf("finalized task = status %s result %#v, want done/success", got.Status, got.TypedResult)
	}
	if got.ExecutionReceipt == nil || got.ExecutionReceipt.StepBudget == nil || !got.ExecutionReceipt.StepBudget.Exhausted {
		t.Fatalf("step budget receipt = %#v, want exhausted receipt", got.ExecutionReceipt)
	}
}

func TestProtocolRepair_ReplayableTaskBlocksAfterRepairFailure(t *testing.T) {
	// §5/§6.1: protocol failures allow result-only repair only; worker tools
	// must not be replayed. After repair fails on a replayable task, the task
	// is blocked for reconciliation instead of retrying the worker.
	// This replaces the old replayable-retry behaviour (WP-08).
	//
	// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §6.1, WP-08
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "protocol-retry", Timeout: 30, MaxRetries: 2},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", MaxRetries: 2, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-retry",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "replayable protocol task"}})[0]

	c.workerAgentOverride = &replayableWorkerAgent{
		calls: &workerCalls,
		onSecond: func() {
			c.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: "worker", Status: "success",
				Summary: "second attempt submitted result", Source: "submitted",
			})
		},
	}
	// Attempt 1 repair fails. Per §6.1 the worker must NOT be replayed.
	c.repairAgentOverride = &mockWorkerTextAgent{text: "repair-output-must-not-be-in-attempt-1"}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "replayable protocol task",
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err == nil {
		t.Fatal("expected protocol failure to block, got nil error")
	}

	// Worker must only be invoked once — no replay (§6.1).
	if workerCalls != 1 {
		t.Fatalf("worker invocation count = %d, want 1 (protocol failure must not replay worker, §6.1)", workerCalls)
	}

	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskBlocked {
		t.Fatalf("task status = %s, want blocked (protocol failure must block for reconciliation, §5/§6.1)", got.Status)
	}
	if len(got.ExecutionReceipts) != 1 {
		t.Fatalf("receipt history length = %d, want 1 (single attempt, no replay)", len(got.ExecutionReceipts))
	}
	first := got.ExecutionReceipts[0]
	if first.Attempt != 1 {
		t.Fatalf("receipt attempt = %d, want 1", first.Attempt)
	}
	if first.RepairProvenance == nil || first.RepairProvenance.Success {
		t.Fatalf("attempt 1 should retain failed repair provenance: %#v", first.RepairProvenance)
	}
	if first.RepairProvenance.FailureReason != RepairFailureNoToolCall {
		t.Fatalf("failure reason = %q, want %q", first.RepairProvenance.FailureReason, RepairFailureNoToolCall)
	}
	if got.TypedResult == nil || got.TypedResult.Source != "recovered_protocol" || got.TypedResult.Confidence <= 0 {
		t.Fatalf("missing recovered_protocol provisional result: %#v", got.TypedResult)
	}
	if !strings.Contains(got.TypedResult.Summary, "attempt-one-original") || got.TypedResult.RawOutputRef == nil {
		t.Fatalf("provisional result did not preserve execution evidence: %#v", got.TypedResult)
	}

	// The transcript must preserve the original worker output and must not
	// contain the repair agent's output (evidence isolation, §7).
	if first.TranscriptRef == "" {
		t.Fatal("expected non-empty transcript ref for attempt 1")
	}
	firstData := readStoredArtifact(t, c.session.Workspace, first.TranscriptRef)
	if !strings.Contains(string(firstData), "attempt-one-original") {
		t.Fatalf("attempt 1 transcript missing original worker output: %s", firstData)
	}
	if strings.Contains(string(firstData), "repair-output-must-not") {
		t.Fatalf("attempt 1 transcript must not contain repair agent output: %s", firstData)
	}
}

func TestProtocolRepair_NonReplayableTaskBlocksOnRepairFailure(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "protocol-block", Timeout: 30, MaxRetries: 3},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-102",
	}

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:      "worker",
		Desc:       "mutate external system",
		SideEffect: SideEffectExternalWrite,
	}})
	todoID := items[0].ID

	// Counting agent that omits submit_result in both initial run and repair step
	c.workerAgentOverride = &countingEmptyAgent{calls: &workerCalls}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "mutate external system",
		SideEffect: SideEffectExternalWrite,
		Execution: ExecutionContract{
			RequiresResult: true,
		},
	}, todoID)

	if err == nil {
		t.Fatal("expected protocol failure when repair fails")
	}

	// Worker tools should only be called ONCE (no retries for non-replayable protocol failure)
	if workerCalls != 1 {
		t.Fatalf("worker invocation count = %d, want exactly 1 (worker tools run once, no replay)", workerCalls)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", item.Status)
	}
	if !strings.Contains(item.Detail, "protocol") {
		t.Fatalf("blocked detail should contain protocol failure explanation, got: %q", item.Detail)
	}
	if item.ExecutionReceipt == nil {
		t.Error("ExecutionReceipt should be preserved even when task blocks")
	}
	if item.ExecutionReceipt != nil {
		transcript := readStoredArtifact(t, c.session.Workspace, item.ExecutionReceipt.TranscriptRef)
		if !strings.Contains(string(transcript), "Processed") && !strings.Contains(string(transcript), "assistant_output") {
			t.Fatalf("failed-task transcript does not preserve original execution: %s", transcript)
		}
		if strings.Contains(string(transcript), "Repaired typed result submitted") {
			t.Fatal("repair output must not be appended to failed-task original transcript")
		}
	}
}

func TestProtocolRepair_FreeTextOutputCannotBypassRepairFailure(t *testing.T) {
	// Finding 1 test: Non-empty worker output + repair fails + successful verifier ("true") MUST NOT turn task to done.
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "protocol-bypass", Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-bypass",
	}

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:      "worker",
		Desc:       "critical task requiring result",
		SideEffect: SideEffectExternalWrite,
	}})
	todoID := items[0].ID

	// Worker returns non-empty output text but omits submit_result
	c.workerAgentOverride = &mockWorkerTextAgent{text: "completed work text without submit_result"}
	// Repair agent runs but fails to call submit_result
	c.repairAgentOverride = &mockWorkerTextAgent{text: "repair failed to submit"}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "critical task requiring result",
		Verify:     "true",
		SideEffect: SideEffectExternalWrite,
		Execution: ExecutionContract{
			RequiresResult: true,
		},
	}, todoID)

	if err == nil {
		t.Fatal("expected execution error when protocol repair fails to produce submit_result")
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status == TaskDone {
		t.Fatal("task MUST NOT be marked done when protocol repair fails, even if free-text output exists and verifier passes")
	}
	if item.Status != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", item.Status)
	}
}

func TestProtocolRepair_AllowsReplayFalseBlocksWorkerReplay(t *testing.T) {
	// Finding 2 test: SideEffectNone + explicit AllowsReplay = false MUST block retries and run worker exactly once.
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "replay-false", Timeout: 30, MaxRetries: 3},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-replay-false",
	}

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:      "worker",
		Desc:       "inline non-replayable task",
		SideEffect: SideEffectNone,
	}})
	todoID := items[0].ID

	c.workerAgentOverride = &countingEmptyAgent{calls: &workerCalls}
	noReplay := false

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "inline non-replayable task",
		SideEffect: SideEffectNone,
		Execution: ExecutionContract{
			RequiresResult: true,
			AllowsReplay:   &noReplay,
		},
	}, todoID)

	if err == nil {
		t.Fatal("expected protocol failure when repair fails")
	}

	if workerCalls != 1 {
		t.Fatalf("worker invocation count = %d, want exactly 1 (AllowsReplay=false prohibits re-running worker)", workerCalls)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", item.Status)
	}
}

func TestProtocolRepair_RecoveryPolicyBlocksWorkerReplay(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "recovery-policy", Timeout: 30, MaxRetries: 3},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-policy",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "reconcile before retry"}})[0]
	c.workerAgentOverride = &countingEmptyAgent{calls: &workerCalls}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "reconcile before retry",
		Recovery:   RecoveryReconcile,
		SideEffect: SideEffectNone,
		Execution:  ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err == nil {
		t.Fatal("expected protocol failure when recovery policy disallows replay")
	}
	if workerCalls != 1 {
		t.Fatalf("worker invocation count = %d, want exactly 1", workerCalls)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", got)
	}
}

func TestExecutionReceipt_ProvenanceAndEventStoreReplay(t *testing.T) {
	// Finding 3 test: Multi-attempt receipts and repair provenance survive event store reduction.
	receipt1 := ExecutionReceipt{
		RunID:         "run-1",
		TaskID:        "todo-1",
		Attempt:       1,
		StartedAt:     time.Now().Add(-10 * time.Second),
		FinishedAt:    time.Now().Add(-5 * time.Second),
		ProducerID:    "worker",
		TranscriptRef: "task-log-1",
		RepairProvenance: &RepairProvenance{
			Attempted:     true,
			Success:       false,
			Error:         "missing submit_result",
			FailureReason: RepairFailureNoToolCall,
		},
	}
	receipt2 := ExecutionReceipt{
		RunID:         "run-1",
		TaskID:        "todo-1",
		Attempt:       2,
		StartedAt:     time.Now().Add(-4 * time.Second),
		FinishedAt:    time.Now(),
		ProducerID:    "worker",
		TranscriptRef: "task-log-2",
		RepairProvenance: &RepairProvenance{
			Attempted: true,
			Success:   true,
			SubmittedResult: &TaskResult{
				TaskID:  "todo-1",
				Agent:   "worker",
				Status:  "success",
				Summary: "repaired typed result",
				Source:  "submitted",
			},
		},
	}

	payloadData, _ := json.Marshal(map[string]interface{}{
		"id":                 "todo-1",
		"desc":               "test receipt provenance",
		"status":             "done",
		"agent":              "worker",
		"execution_receipt":  receipt2,
		"execution_receipts": []ExecutionReceipt{receipt1, receipt2},
	})

	events := []RunEvent{
		{Type: "task_created", TaskID: "todo-1", Payload: payloadData},
		{Type: "task_completed", TaskID: "todo-1", Payload: payloadData},
	}

	reduced := ReduceToTodoList(events)
	if len(reduced) != 1 {
		t.Fatalf("reduced items count = %d, want 1", len(reduced))
	}
	item := reduced[0]
	if len(item.ExecutionReceipts) != 2 {
		t.Fatalf("reduced ExecutionReceipts length = %d, want 2", len(item.ExecutionReceipts))
	}
	if item.ExecutionReceipts[0].RepairProvenance == nil || item.ExecutionReceipts[0].RepairProvenance.Success {
		t.Errorf("attempt 1 repair provenance should show success=false")
	}
	if got := item.ExecutionReceipts[0].RepairProvenance.FailureReason; got != RepairFailureNoToolCall {
		t.Errorf("attempt 1 failure reason = %q, want %q", got, RepairFailureNoToolCall)
	}
	if item.ExecutionReceipts[1].RepairProvenance == nil || !item.ExecutionReceipts[1].RepairProvenance.Success {
		t.Errorf("attempt 2 repair provenance should show success=true")
	}
}

func TestProtocolRepair_AllFailureReasonsSurviveEventStoreReduction(t *testing.T) {
	reasons := []RepairFailureReason{
		RepairFailureNoToolCall,
		RepairFailureInvalidSchema,
		RepairFailureProgressNotFinal,
	}
	receipts := make([]ExecutionReceipt, 0, len(reasons))
	for i, reason := range reasons {
		receipts = append(receipts, ExecutionReceipt{
			RunID:   "run-reasons",
			TaskID:  "todo-reasons",
			Attempt: i + 1,
			RepairProvenance: &RepairProvenance{
				Attempted:      true,
				Success:        false,
				FailureReason:  reason,
				RepairAttempts: 1,
				History: []RepairAttemptProvenance{{
					Attempt:       1,
					FailureReason: reason,
				}},
				SubmittedResult: func() *TaskResult {
					if reason != RepairFailureNoToolCall {
						return nil
					}
					return &TaskResult{Source: "recovered_protocol", Summary: "original evidence"}
				}(),
			},
		})
	}
	payloadData, err := json.Marshal(map[string]interface{}{
		"id":                 "todo-reasons",
		"desc":               "all repair reasons",
		"status":             "blocked",
		"agent":              "worker",
		"typed_result":       &TaskResult{Source: "recovered_protocol", Summary: "original evidence"},
		"execution_receipt":  receipts[0],
		"execution_receipts": receipts,
	})
	if err != nil {
		t.Fatalf("marshal receipts: %v", err)
	}
	reduced := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: "todo-reasons", Payload: payloadData}})
	if len(reduced) != 1 || len(reduced[0].ExecutionReceipts) != len(reasons) {
		t.Fatalf("reduced receipts = %#v, want %d receipts", reduced, len(reasons))
	}
	for i, want := range reasons {
		prov := reduced[0].ExecutionReceipts[i].RepairProvenance
		if prov == nil || prov.FailureReason != want || len(prov.History) != 1 || prov.History[0].FailureReason != want {
			t.Fatalf("receipt %d provenance = %#v, want reason %q in current and history", i, prov, want)
		}
	}
	if got := reduced[0].ExecutionReceipts[0].RepairProvenance.SubmittedResult.Source; got != "recovered_protocol" {
		t.Fatalf("recovered provisional source after reduction = %q, want recovered_protocol", got)
	}
	if reduced[0].TypedResult == nil || reduced[0].TypedResult.Source != "recovered_protocol" {
		t.Fatalf("typed recovered provisional result after reduction = %#v, want recovered_protocol", reduced[0].TypedResult)
	}
}

func TestProtocolIncomplete_ConvergenceAndFinalization(t *testing.T) {
	// Finding 4 test: TaskProtocolIncomplete convergence on unexpected termination and interrupted status check.
	c := &Coordinator{
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker",
		Desc:  "incomplete protocol task",
	}})
	items := c.taskTracker.TodoList().Items()
	_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskProtocolIncomplete, "waiting repair", "")

	if !isInterruptedStatus(TaskProtocolIncomplete) {
		t.Error("isInterruptedStatus(TaskProtocolIncomplete) should return true")
	}

	c.finalizeRemainingTasks()

	itemsAfter := c.taskTracker.TodoList().Items()
	if itemsAfter[0].Status != TaskError {
		t.Fatalf("finalizeRemainingTasks should transition TaskProtocolIncomplete to TaskError, got: %s", itemsAfter[0].Status)
	}
}
