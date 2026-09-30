package team

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func submitResultCall(id, input string) fantasy.ToolCallContent {
	return fantasy.ToolCallContent{ToolCallID: id, ToolName: submitResultToolName, Input: input}
}

func submitResultOutcome(id string, rejection string) fantasy.ToolResultContent {
	if rejection == "" {
		return fantasy.ToolResultContent{ToolCallID: id, ToolName: submitResultToolName, Result: fantasy.ToolResultOutputContentText{Text: "result recorded"}}
	}
	return fantasy.ToolResultContent{ToolCallID: id, ToolName: submitResultToolName, Result: fantasy.ToolResultOutputContentError{Error: errors.New(rejection)}}
}

func TestProtocolRepairRejectedSubmission(t *testing.T) {
	const findings = `{"status":"success","summary":"No findings; approve"}`
	steps := func(parts ...fantasy.Content) []fantasy.StepResult {
		return []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent(parts)}}}
	}
	cases := []struct {
		name     string
		evidence *toolCallEvidence
		steps    []fantasy.StepResult
		want     []string
		empty    bool
	}{
		{name: "no submission", steps: steps(fantasy.TextContent{Text: "done"}), empty: true},
		{name: "rejected in steps", steps: steps(submitResultCall("1", findings), submitResultOutcome("1", "preserved claim cannot include finding_index")),
			want: []string{"No findings; approve", "preserved claim cannot include finding_index"}},
		{name: "accepted after rejection", steps: steps(
			submitResultCall("1", findings), submitResultOutcome("1", "bad"),
			submitResultCall("2", findings), submitResultOutcome("2", ""),
		), empty: true},
		{name: "pointer content parts", steps: steps(func() fantasy.Content { c := submitResultCall("1", findings); return &c }(), func() fantasy.Content {
			r := submitResultOutcome("1", "bad field")
			return &r
		}()), want: []string{"bad field"}},
		{name: "stream evidence without steps", evidence: &toolCallEvidence{rejectedSubmitInput: findings, rejectedSubmitError: "invariant_id is required"},
			want: []string{"No findings; approve", "invariant_id is required"}},
		{name: "other tool rejection", steps: steps(
			fantasy.ToolCallContent{ToolCallID: "1", ToolName: "view", Input: `{}`},
			fantasy.ToolResultContent{ToolCallID: "1", ToolName: "view", Result: fantasy.ToolResultOutputContentError{Error: errors.New("not authorized")}},
		), empty: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := protocolRepairRejectedSubmission(tc.evidence, tc.steps)
			if tc.empty {
				if got != "" {
					t.Fatalf("got %q, want no rejected submission", got)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("rendered submission lacks %q:\n%s", want, got)
				}
			}
			if omitted := omitRejectedSubmission("prefix"+got+"suffix", got); strings.Contains(omitted, "No findings") || !strings.HasPrefix(omitted, "prefix") {
				t.Fatalf("omitRejectedSubmission kept the arguments: %q", omitted)
			}
		})
	}
}

// rejectedOnceWorkerAgent reproduces the 2026-09-30 reviewer: one rejected
// submit_result carrying the whole review, then an empty final message.
type rejectedOnceWorkerAgent struct{ calls *int }

func (a *rejectedOnceWorkerAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, errors.New("rejectedOnceWorkerAgent requires streaming callbacks")
}

func (a *rejectedOnceWorkerAgent) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	*a.calls++
	input := `{"status":"success","summary":"No findings; approve the security-tool unit","details":"reviewed unit-0013 diff"}`
	if err := call.OnToolCall(submitResultCall("review-1", input)); err != nil {
		return nil, err
	}
	if err := call.OnToolResult(submitResultOutcome("review-1", "invariant_assessments[0] preserved claim cannot include finding_index")); err != nil {
		return nil, err
	}
	return &fantasy.AgentResult{}, nil
}

func newEvidenceRepairCoordinator(t *testing.T, name string) (*Coordinator, *TodoItem) {
	t.Helper()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: name, Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"reviewer": {Name: "reviewer", Role: "worker", SideEffect: string(SideEffectNone), MaxRetries: 1, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:    time.Now(),
		taskTracker:    NewTaskTracker(),
		reportStatus:   func(StatusEvent) {},
		taskCache:      newDefaultTaskCache(taskCacheDependencies{}),
		executionRunID: "run-" + name,
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "review unit-0013"}})[0]
	return c, item
}

func TestProtocolRepairRestatesTheRejectedSubmission(t *testing.T) {
	c, item := newEvidenceRepairCoordinator(t, "rejected-submission-repair")
	workerCalls, repairCalls := 0, 0
	var prompts []string
	c.workerAgentOverride = &rejectedOnceWorkerAgent{calls: &workerCalls}
	c.repairAgentOverride = &scriptedRepairAgent{calls: &repairCalls, prompts: &prompts, onCall: func(int) {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "reviewer", Status: TaskResultStatusSuccess,
			Summary: "No findings; approve the security-tool unit", Source: "submitted",
		})
	}}

	if _, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), TaskDef{
		Agent: "reviewer", Goal: "review unit-0013", SideEffect: SideEffectNone,
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if workerCalls != 1 || repairCalls != 1 || len(prompts) != 1 {
		t.Fatalf("worker/repair calls = %d/%d, want 1/1", workerCalls, repairCalls)
	}
	for _, want := range []string{"No findings; approve the security-tool unit", "preserved claim cannot include finding_index"} {
		if !strings.Contains(prompts[0], want) {
			t.Fatalf("repair prompt lacks %q:\n%s", want, prompts[0])
		}
	}
}

func TestProtocolRepairWithoutEvidenceDoesNotAcceptCompletedWithGaps(t *testing.T) {
	c, item := newEvidenceRepairCoordinator(t, "evidence-less-repair")
	workerCalls, repairCalls := 0, 0
	c.workerAgentOverride = &countingTextAgent{calls: &workerCalls, text: ""}
	c.repairAgentOverride = &scriptedRepairAgent{calls: &repairCalls, onCall: func(int) {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "reviewer", Status: TaskResultStatusCompletedWithGaps,
			Summary: "sealed transcript and diff artifact not re-supplied; no findings", Source: "submitted", Confidence: 0.2,
		})
	}}

	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), TaskDef{
		Agent: "reviewer", Goal: "review unit-0013", SideEffect: SideEffectNone,
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err == nil || !strings.Contains(err.Error(), "reports missing evidence") {
		t.Fatalf("err = %v, want the placeholder result rejected", err)
	}
	got := c.todoItemByID(item.ID)
	if got.Status == TaskDone {
		t.Fatalf("task accepted an evidence-less completed_with_gaps result: %#v", got.TypedResult)
	}
	if got.ExecutionReceipt == nil || got.ExecutionReceipt.RepairProvenance == nil || got.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureNoEvidence {
		t.Fatalf("repair provenance = %#v, want failure reason %q", got.ExecutionReceipt, RepairFailureNoEvidence)
	}
	if metrics := c.Metrics(); metrics.ProtocolRepairsSucceeded != 0 {
		t.Fatalf("protocol repairs succeeded = %d, want 0", metrics.ProtocolRepairsSucceeded)
	}
}
