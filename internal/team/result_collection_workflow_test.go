package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

func TestResultCollectionHandsValidatedPayloadsToDependentWorkers(t *testing.T) {
	for _, names := range [][]string{{"coder", "reviewer", "verifier"}, {"researcher", "evidence_consumer"}} {
		t.Run(strings.Join(names, "_"), func(t *testing.T) {
			var collections atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
					return
				}
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", 400)
					return
				}
				if tools, _ := request["tools"].([]any); len(tools) != 0 {
					t.Errorf("collector tools=%v", tools)
				}
				collections.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				body := `{"status":"success","summary":"accepted evidence","structured_payload":{"marker":"validated-handoff"}}`
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\ndata: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
			}))
			t.Cleanup(server.Close)
			compiled, err := CompileResultContractDocument("handoff.json", []byte(`{"type":"object","additionalProperties":false,"required":["marker"],"properties":{"marker":{"type":"string","enum":["validated-handoff"]}}}`))
			if err != nil {
				t.Fatal(err)
			}
			c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
				c.session.Agents["worker"].ResultTransport = ""
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				c.session.AgentResultContracts = make(map[string]ResultContractRef)
				for _, name := range names {
					def := *c.session.Agents["worker"]
					def.Name = name
					c.session.Agents[name] = &def
					c.session.AgentResultContracts[name] = compiled.ref(true)
				}
			})
			var turns []scriptedTurn
			for i := range names {
				turns = append(turns, scriptedTurn{stream: func(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
					if i > 0 && !strings.Contains(fmt.Sprint(call.Prompt), "validated-handoff") {
						return nil, fmt.Errorf("dependent worker did not receive the complete accepted payload")
					}
					return textAgentResult("complete work report"), nil
				}})
			}
			worker := scriptWorker(turns...)
			c.workerAgentOverride = worker
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			var priorID string
			for _, name := range names {
				task := TaskDef{ID: name, Agent: name, Model: "ollama/test", Goal: "perform the declared workflow step", SideEffect: SideEffectNone, Execution: ExecutionContract{RequiresResult: true}}
				if priorID != "" {
					task.EvidenceFrom = []string{priorID}
					task.Execution.RequiresEvidence = true
				}
				if _, err := c.ExecuteTasks(t.Context(), []TaskDef{task}); err != nil {
					t.Fatal(err)
				}
				items := c.taskTracker.TodoList().Items()
				priorID = items[len(items)-1].ID
			}
			for _, item := range c.taskTracker.TodoList().Items()[1:] {
				if item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.Source != "collected" || item.TypedResult.StructuredPayload == nil || item.ExecutionReceipt.ResultValidation != ResultValidationValid {
					t.Fatalf("workflow handoff incomplete: task=%s status=%s validation=%s", item.ID, item.Status, item.ExecutionReceipt.ResultValidation)
				}
				if item.ExecutionReceipt.ResultPayloadSHA256 != item.TypedResult.StructuredPayload.SHA256 {
					t.Fatal("result hash missing from receipt")
				}
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range ReduceToTodoList(events)[1:] {
				if item.TypedResult == nil || item.TypedResult.StructuredPayload == nil || item.ExecutionReceipt.ResultValidation != ResultValidationValid || item.ExecutionReceipt.ResultPayloadSHA256 != item.TypedResult.StructuredPayload.SHA256 {
					t.Fatal("replay lost the validated result receipt binding")
				}
			}
			if worker.Calls() != len(names) || collections.Load() != int32(len(names)) {
				t.Fatalf("work=%d collections=%d", worker.Calls(), collections.Load())
			}
		})
	}
}

func TestResultCollectionDefaultDryRunMakesNoProviderOrWorkerCalls(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "dry run called provider", 500)
	}))
	t.Cleanup(server.Close)
	c, item, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) { c.session.Agents["worker"].ResultTransport = "" })
	worker := scriptWorker(textTurn("must not execute"))
	c.workerAgentOverride = worker
	if _, err := c.DryRun(t.Context(), "preview this task"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || worker.Calls() != 0 || c.todoItemByID(item.ID).ExecutionReceipt != nil {
		t.Fatal("dry run dispatched work or collection")
	}
}
