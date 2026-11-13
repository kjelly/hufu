package team

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/cost"
)

func TestStructuredStepTargetUsesBoundProviderInsteadOfNamedDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(server.Close)
	providers := map[string]config.ProviderConfig{"remote": {ProviderURL: server.URL + "/remote/v1"}}
	c := newTestCoordinator(t)
	c.session.Config.DefaultLLMBackend, c.session.Config.Providers = "remote", providers
	var err error
	c.providerManager, err = agent.NewProviderManager(server.URL+"/v1", "", providers)
	if err != nil {
		t.Fatal(err)
	}
	c.initEventStore()
	catalog, err := cost.NewCatalog(map[string]cost.PriceConfig{
		"ollama/gpt": {BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "2"},
		"remote/gpt": {BillingMode: cost.BillingMetered, InputUSDPerMillion: "7", OutputUSDPerMillion: "11"},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := cost.ResolveRunPolicy(&cost.PolicyConfig{UnknownPricePolicy: cost.UnknownPriceDeny})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := cost.NewPolicySnapshot(policy, catalog.Snapshots())
	if err != nil {
		t.Fatal(err)
	}
	c.costManager = newCostManager(snapshot)
	bound := NewModelProfileRuntime(c.providerManager, true).AdmissionContext(t.Context(), "gpt", 8192, 64, 32)
	if bound.ProviderIdentity != "local" {
		t.Fatalf("fixture bound provider=%q", bound.ProviderIdentity)
	}
	inner := &costAdmissionLanguageModel{usage: fantasy.Usage{InputTokens: 17, OutputTokens: 3, TotalTokens: 20}}
	model := agent.NewAdmittedLanguageModelWithContext("gpt", inner, c.providerAdmission(), bound)
	contract := ExecutionContract{Steps: []ExecutionStep{{ID: "generate", Tool: "generator", Effect: ExecutionEffectRead}}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "generate", Execution: contract}})[0]
	_, err = c.RunStructuredTask(t.Context(), item.ID, 1, StructuredStepRunnerFunc(func(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, AgentName: "worker", Purpose: "task_execution"})
		_, err := model.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("generate")}, MaxOutputTokens: new(int64(32))})
		return ExecutionStepResult{}, err
	}))
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("actual calls=%d", inner.calls)
	}
	events, err := c.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, events)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Invocations) != 1 || rows[0].Invocations[0].ExecutionTarget != "ollama/gpt" {
		t.Fatalf("bound provider=%s, observed attribution=%+v", bound.ProviderIdentity, rows)
	}
	view, err := ProjectCostView(events, c.executionRunID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.UsageDerivedMicros == nil || *view.UsageDerivedMicros != 23 {
		t.Fatalf("bound-provider cost=%+v", view)
	}
}
