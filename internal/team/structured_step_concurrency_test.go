package team

import (
	"context"
	"encoding/json/v2"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
)

// Protect only the existing fake transport's counter. Runtime admission,
// journal writes and settlement remain concurrent in the test below.
type synchronizedStepTransport struct {
	fantasy.LanguageModel
	mu sync.Mutex
}

func (m *synchronizedStepTransport) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.LanguageModel.Generate(ctx, call)
}

func TestStructuredStepTracksConcurrentProviderInvocations(t *testing.T) {
	t.Parallel()
	c := newTestCoordinator(t)
	c.initEventStore()
	inner := &costAdmissionLanguageModel{}
	transport := &synchronizedStepTransport{LanguageModel: inner}
	model := agent.NewAdmittedLanguageModelWithContext("ollama/gpt", transport, c.providerAdmission(), providerAdmissionTestContext(t, "ollama/gpt"))
	contract := ExecutionContract{Steps: []ExecutionStep{{ID: "generate", Tool: "generator", Effect: ExecutionEffectRead}}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "generate", Execution: contract}})[0]
	const count = 6
	result, err := c.RunStructuredTask(t.Context(), item.ID, 1, StructuredStepRunnerFunc(func(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, AgentName: "worker", Purpose: "task_execution"})
		errors := make(chan error, count)
		var workers sync.WaitGroup
		for range count {
			workers.Go(func() {
				_, err := model.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("generate")}, MaxOutputTokens: new(int64(32))})
				errors <- err
			})
		}
		workers.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				return ExecutionStepResult{}, err
			}
		}
		return ExecutionStepResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != count || len(result.Receipts) != 1 || len(result.Receipts[0].ModelExecutionIDs) != count {
		t.Fatalf("calls=%d result=%+v", inner.calls, result)
	}
	events, err := c.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, events)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Invocations) != count {
		t.Fatalf("concurrent attribution=%+v", rows)
	}
}

func TestProviderProfileInputCannotOverrideRuntimeTarget(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"ollama/gpt", "other/gpt"} {
		t.Run(target, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.initEventStore()
			bound := providerAdmissionTestContext(t, "ollama/gpt")
			var projection modelprofile.TelemetryProjection
			if err := json.Unmarshal([]byte(bound.ProfileTelemetryJSON), &projection); err != nil {
				t.Fatal(err)
			}
			projection.ExecutionTarget = target
			raw, err := json.Marshal(projection)
			if err != nil {
				t.Fatal(err)
			}
			bound.ProfileTelemetryJSON = string(raw)
			inner := &costAdmissionLanguageModel{}
			model := agent.NewAdmittedLanguageModelWithContext("ollama/gpt", inner, c.providerAdmission(), bound)
			if _, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("generate")}}); err == nil || inner.calls != 0 {
				t.Fatalf("target override reached transport: calls=%d err=%v", inner.calls, err)
			}
		})
	}
}
