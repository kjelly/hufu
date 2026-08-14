package team

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type fakeSubagentProvider struct{ name string }

func (p fakeSubagentProvider) Name() string                       { return p.name }
func (p fakeSubagentProvider) Capabilities() SubagentCapabilities { return SubagentCapabilities{} }
func (p fakeSubagentProvider) RunAttempt(context.Context, AttemptRequest) (AttemptResult, error) {
	return AttemptResult{}, nil
}

func TestSubagentRegistryFailsClosedForUnknownProvider(t *testing.T) {
	registry := NewSubagentRegistry(fakeSubagentProvider{name: localSubagentProviderName})
	if _, err := registry.Resolve("untrusted-command"); err == nil || !strings.Contains(err.Error(), "unknown subagent provider") {
		t.Fatalf("unknown provider did not fail closed: %v", err)
	}
	if _, err := registry.Resolve(""); err != nil {
		t.Fatalf("default local provider was unavailable: %v", err)
	}
}

func TestSubagentProviderCannotDeclareAcceptance(t *testing.T) {
	// The public attempt DTO contains only execution evidence. Acceptance and
	// verification are deliberately absent and therefore remain coordinator
	// responsibilities even when a provider returns a successful attempt.
	result := AttemptResult{Output: "provider says success"}
	if result.TypedResult != nil || result.TranscriptRef != "" {
		t.Fatalf("unexpected provider-owned terminal state: %#v", result)
	}
}

type recordingTaskResultSink struct {
	taskID string
	result TaskResult
}

func (s *recordingTaskResultSink) Submit(_ context.Context, taskID string, result TaskResult) error {
	s.taskID, s.result = taskID, result
	return nil
}

func TestSubmitResultToolUsesTaskScopedSink(t *testing.T) {
	sink := &recordingTaskResultSink{}
	tool := &submitResultTool{todoID: "task-1", sink: sink}
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"done"}`})
	if err != nil || response.IsError {
		t.Fatalf("submit result failed: response=%#v err=%v", response, err)
	}
	if sink.taskID != "task-1" || sink.result.Status != TaskResultStatusSuccess || sink.result.Summary != "done" {
		t.Fatalf("task result was not passed to the scoped sink: %#v", sink)
	}
}
