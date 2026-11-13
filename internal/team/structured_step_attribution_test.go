package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"slices"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

func TestStructuredStepDistinguishesProposalFromExecution(t *testing.T) {
	for _, tc := range []struct {
		name, wantKind string
		declared       bool
		generations    int
	}{
		{name: "declared tool", wantKind: "tool_call", declared: true},
		{name: "unobserved custom runner", wantKind: "unknown"},
		{name: "observed generation", wantKind: "model_generation", generations: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.session.Config.DefaultLLMBackend = "ollama"
			c.session.Agents["worker"].Tools = "inspector"
			c.modelList = []config.ModelEntry{{ID: "proposed-only"}}
			c.initEventStore()
			if c.EventStore() == nil {
				t.Fatal("fixture event store is unavailable")
			}
			manager, err := agent.NewProviderManager("http://localhost:11434/v1", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			c.providerManager = manager
			inner := &costAdmissionLanguageModel{}
			model := agent.NewAdmittedLanguageModelWithContext("ollama/gpt", inner, c.providerAdmission(), providerAdmissionTestContext(t, "ollama/gpt"))
			toolCalls := 0
			c.coreTools = []fantasy.AgentTool{&artifactAwareStructuredTestTool{structuredTestTool: &structuredTestTool{name: "inspector", run: func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				toolCalls++
				return fantasy.NewTextResponse("inspected"), nil
			}}}}
			contract := ExecutionContract{Steps: []ExecutionStep{{ID: "inspect", Tool: "inspector", Effect: ExecutionEffectRead}}}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect", Execution: contract}})[0]
			var runner StructuredStepRunner = StructuredStepRunnerFunc(func(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
				ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, AgentName: "worker", Purpose: "task_execution"})
				for range tc.generations {
					if _, err := model.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("inspect")}, MaxOutputTokens: new(int64(32))}); err != nil {
						return ExecutionStepResult{}, err
					}
				}
				return ExecutionStepResult{}, nil
			})
			if tc.declared {
				runner = &coordinatorDeclaredToolRunner{c: c}
			}
			result, err := c.RunStructuredTask(t.Context(), item.ID, 1, runner)
			if err != nil {
				t.Fatalf("execute structured task: %v; result=%+v", err, result)
			}
			if len(result.Receipts) != 1 || inner.calls != tc.generations {
				t.Fatalf("receipts=%d actual generations=%d", len(result.Receipts), inner.calls)
			}
			if tc.declared && toolCalls != 1 {
				t.Fatalf("declared tool calls=%d", toolCalls)
			}
			encoded, err := json.Marshal(result.Receipts[0])
			if err != nil {
				t.Fatal(err)
			}
			var observed struct {
				Kind     string   `json:"execution_kind"`
				Proposal string   `json:"proposed_model"`
				IDs      []string `json:"model_execution_ids"`
			}
			if err := json.Unmarshal(encoded, &observed); err != nil {
				t.Fatal(err)
			}
			if observed.Kind != tc.wantKind || observed.Proposal != "proposed-only" || len(observed.IDs) != tc.generations {
				t.Fatalf("attribution=%+v, want kind=%s proposal=proposed-only invocations=%d", observed, tc.wantKind, tc.generations)
			}
			profiles, err := LoadModelProfileTelemetry(c.session.Workspace, c.executionRunID)
			if err != nil {
				t.Fatal(err)
			}
			if len(profiles) != tc.generations {
				t.Fatalf("actual profile count=%d", len(profiles))
			}
			for _, profile := range profiles {
				if profile.ModelID != "ollama/gpt" || !slices.Contains(observed.IDs, profile.InvocationID) {
					t.Fatalf("actual invocation is not linked: %+v", profile)
				}
			}
			events, err := c.EventStore().ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			rows, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, events)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || len(rows[0].Invocations) != tc.generations {
				t.Fatalf("durable attribution=%+v", rows)
			}
			durable, err := json.Marshal(rows[0].Receipt)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, durable) {
				t.Fatalf("receipt changed through persistence: %s", durable)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			restored := newTestCoordinator(t)
			restored.session = cloneSession(c.session, c.session.Workspace)
			restored.initEventStore()
			receipt, exists := restored.executionStepReceiptRegistry().Get(result.Receipts[0].ID)
			if !exists {
				t.Fatal("receipt was not restored in a new store handle")
			}
			replayed, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, replayed) {
				t.Fatalf("receipt changed through replay: %s", replayed)
			}

		})
	}
}
