package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/scripted"
)

func TestResponsesWorkerCollectionRepairAndReplay(t *testing.T) {
	const schema = `{"type":"object","additionalProperties":false,"required":["marker"],"properties":{"marker":{"type":"string","minLength":1}}}`
	const valid = `{"status":"success","summary":"reviewed","structured_payload":{"marker":"verified"}}`
	for _, tc := range []struct {
		name      string
		transport agent.ResultTransport
		first     string
		wantCalls int32
		blocked   bool
	}{
		{"runtime", agent.ResultTransportRuntime, valid, 2, false},
		{"tool", agent.ResultTransportTool, valid, 1, false},
		{"runtime repair", agent.ResultTransportRuntime, `{"status":"success","summary":"reviewed","structured_payload":{"marker":""}}`, 3, false},
		{"forged receipt", agent.ResultTransportRuntime, `{"status":"success","summary":"reviewed","receipt_ids":["forged"],"structured_payload":{"marker":"verified"}}`, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := &scripted.Usage{InputTokens: 10, OutputTokens: 2}
			steps := []scripted.Step{{Content: "complete original review", Usage: usage},
				{ToolCall: &scripted.ToolCall{Name: agent.StructuredResponseFunctionName, Arguments: `{"result":` + tc.first + `}`}, Usage: usage}}
			if tc.transport == agent.ResultTransportTool {
				steps = []scripted.Step{{ToolCall: &scripted.ToolCall{Name: submitResultToolName, Arguments: valid}, Usage: usage}}
			} else if tc.wantCalls == 3 {
				steps = append(steps, scripted.Step{ToolCall: &scripted.ToolCall{Name: agent.StructuredResponseFunctionName, Arguments: `{"result":` + valid + `}`}, Usage: usage})
			}
			fixture := &responsesWorkflowFixture{t: t, provider: scripted.NewProvider("test", steps), transport: tc.transport}
			var c *Coordinator
			server := httptest.NewServer(fixture)
			t.Cleanup(server.Close)
			compiled, err := CompileResultContractDocument("response-result.json", []byte(schema))
			if err != nil {
				t.Fatal(err)
			}
			c, _, _, _ = resultCollectionProviderFixture(t, server.URL+"/v1", tc.transport == agent.ResultTransportRuntime, func(c *Coordinator) {
				pm, err := agent.NewProviderManager(server.URL+"/v1", "", map[string]config.ProviderConfig{"local": {APIProtocol: "responses"}})
				if err != nil {
					t.Fatal(err)
				}
				c.providerManager = pm
				c.modelProfileRuntime = NewModelProfileRuntimeWithCatalog(pm, true, nil)
				c.session.Config.Generation.ContextWindow = 131072
				c.session.Config.ResultTransport = tc.transport
				c.session.Agents["worker"].ResultTransport = tc.transport
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true)}
				c.session.Config.CostPolicy = cost.RunPolicy{Configured: true}
				catalog, err := cost.NewCatalog(map[string]cost.PriceConfig{"ollama/test": {BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "2"}})
				if err != nil {
					t.Fatal(err)
				}
				c.session.CostCatalog = catalog
			})
			fixture.coordinator.Store(c)
			c.costManager = newCostManager(c.executionPolicy.snapshot.Cost)
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "review", Agent: "worker", Model: "ollama/test", Goal: "Review the supplied text", SideEffect: SideEffectNone, Execution: ExecutionContract{RequiresResult: true}}})
			item := c.taskTracker.TodoList().Items()[1]
			if (err != nil) != tc.blocked {
				t.Fatalf("status=%s detail=%s error=%v", item.Status, item.Detail, err)
			}
			if fixture.calls.Load() != tc.wantCalls {
				t.Fatalf("generation calls=%d want=%d", fixture.calls.Load(), tc.wantCalls)
			}
			if !tc.blocked && (item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.StructuredPayload == nil || item.ExecutionReceipt.ResultPayloadSHA256 != item.TypedResult.StructuredPayload.SHA256) {
				t.Fatalf("result/receipt did not complete: %+v", item)
			}
			if tc.blocked && item.TypedResult != nil {
				t.Fatal("forged receipt produced an accepted result")
			}
			events, err := c.EventStore().ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := ReplayTodoList(events)
			if err != nil {
				t.Fatal(err)
			}
			restored := replayed[1]
			if restored.Status != item.Status || (!tc.blocked && (restored.TypedResult == nil || restored.TypedResult.StructuredPayload.SHA256 != item.TypedResult.StructuredPayload.SHA256)) {
				t.Fatal("replay changed accepted result or status")
			}
			ledger := newCostManager(c.executionPolicy.snapshot.Cost)
			if err := ledger.Rehydrate(events); err != nil {
				t.Fatal(err)
			}
			projection := ledger.Projection()
			if len(projection.Invocations) != int(tc.wantCalls) {
				t.Fatal("ledger lost or duplicated an invocation")
			}
			for _, invocation := range projection.Invocations {
				settlement := invocation.Settlement
				if settlement == nil || settlement.Usage == nil || settlement.Usage.InputTokens != 10 || settlement.Usage.OutputTokens != 2 || settlement.Usage.TotalTokens != 12 {
					t.Fatal("wire usage did not settle exactly once through replay")
				}
			}
		})
	}
}

type responsesWorkflowFixture struct {
	t           *testing.T
	provider    *scripted.Provider
	transport   agent.ResultTransport
	calls       atomic.Int32
	coordinator atomic.Pointer[Coordinator]
}

func (f *responsesWorkflowFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
		f.t.Error("Responses silently fell back to Chat")
		http.Error(w, "wrong protocol", 400)
		return
	}
	if r.URL.Path == "/v1/responses" {
		n := f.calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Error(err)
			http.Error(w, "read failed", 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Model string `json:"model"`
			Tools []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Model != "test" {
			f.t.Errorf("wire model=%s error=%v", body.Model, err)
		}
		if n > 1 && f.transport == agent.ResultTransportRuntime && (len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Name != agent.StructuredResponseFunctionName) {
			f.t.Error("collector exposed an executable work tool")
		}
		events, err := f.coordinator.Load().EventStore().ReadEvents()
		if err != nil {
			f.t.Error(err)
			http.Error(w, "events unavailable", 500)
			return
		}
		var profiles int32
		for _, event := range events {
			if event.Type != string(EventModelProfileResolved) {
				continue
			}
			var profile modelprofile.TelemetryProjection
			if err := json.Unmarshal(event.Payload, &profile); err != nil || profile.SchemaVersion != 2 || profile.APIProtocol.Effective != "responses" {
				f.t.Error("generation was sent without its durable protocol binding")
			}
			profiles++
		}
		if profiles != n {
			f.t.Errorf("profiles before generation=%d calls=%d", profiles, n)
		}
	}
	f.provider.ServeHTTP(w, r)
}
