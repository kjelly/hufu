package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

func TestResultCollectionCompletesBoundWorksetWithResultContract(t *testing.T) {
	var workCalls, collectionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
			return
		}
		var request map[string]any
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		body := "Review complete; there are no assigned rows."
		if strings.Contains(fmt.Sprint(request["messages"]), resultCollectorSystem) {
			collectionCalls.Add(1)
			if exposed, _ := request["tools"].([]any); len(exposed) != 0 {
				t.Error("collector exposed execution tools")
			}
			body = `{"status":"success","summary":"review complete","structured_payload":{"rows":[]}}`
		} else {
			workCalls.Add(1)
			if exposed, _ := request["tools"].([]any); len(exposed) != 1 || !strings.Contains(fmt.Sprint(exposed), "view") || strings.Contains(fmt.Sprint(exposed), submitResultToolName) {
				t.Errorf("bound work tools=%v, want only view", exposed)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"review\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
	}))
	t.Cleanup(server.Close)
	compiled, err := CompileResultContractDocument("rows.json", []byte(worksetRowsSchema))
	if err != nil {
		t.Fatal(err)
	}
	c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
		c.session.Agents["worker"].ResultTransport = ""
		c.session.Agents["worker"].Tools = "view"
		c.coreTools = []fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))}
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true)}
	})
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	c.delegatedTasks = make(map[string]int)
	testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
	writeWorksetManifest(t, c.session.Workspace, `{"schema_version":1,"items":[{"key":"one","bindings":{"name":"one","target_ids":"[]"}},{"key":"two","bindings":{"name":"two","target_ids":"[]"}}]}`)
	_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "reviews", Agent: "worker", Model: "ollama/test", SideEffect: SideEffectNone,
		Execution: ExecutionContract{RequiresResult: true},
		FanOut:    &FanOutSpec{Source: "manifest.json", GoalTemplate: "Review {name}; there are no assigned rows."}}})
	if err != nil {
		for _, item := range c.taskTracker.TodoList().Items()[1:] {
			t.Logf("child %s: status=%s detail=%s output=%s", item.ID, item.Status, item.Detail, item.Output)
		}
		t.Fatal(err)
	}
	items := c.taskTracker.TodoList().Items()[1:]
	if len(items) != 2 {
		t.Fatalf("workset children=%d, want 2", len(items))
	}
	receipt := items[0].WorksetReceipt
	if receipt == nil {
		t.Fatal("workset expansion receipt is missing")
	}
	for _, item := range items {
		if item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.Source != "collected" || item.WorksetBinding == nil || item.ExecutionReceipt.ResultValidation != ResultValidationValid || receipt.Children[item.WorksetBinding.ItemKey] != item.ID {
			t.Fatalf("bound review incomplete: status=%s detail=%s", item.Status, item.Detail)
		}
	}
	if workCalls.Load() != 2 || collectionCalls.Load() != 2 {
		t.Fatalf("work=%d collection=%d, want one of each per item", workCalls.Load(), collectionCalls.Load())
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)[1:]
	if replayed[0].WorksetReceipt == nil || !equivalentWorksetReceipts(receipt, replayed[0].WorksetReceipt) {
		t.Fatal("replay lost the workset expansion receipt")
	}
	for _, item := range replayed {
		if item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.StructuredPayload == nil || item.WorksetBinding == nil || receipt.Children[item.WorksetBinding.ItemKey] != item.ID || item.ExecutionReceipt.ResultPayloadSHA256 != item.TypedResult.StructuredPayload.SHA256 {
			t.Fatal("replay lost the accepted workset result or receipt")
		}
	}
}

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

func TestResultCollectionResolvesViewedEvidenceKeys(t *testing.T) {
	for _, scenario := range []string{"valid", "string_refs", "unknown", "corrupt", "runtime_owned", "runtime_full_id"} {
		t.Run(scenario, func(t *testing.T) {
			var workCalls, collectionCalls atomic.Int32
			var source ArtifactRef
			var store *FileArtifactStore
			var issued string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
					return
				}
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				messages := fmt.Sprint(request["messages"])
				w.Header().Set("Content-Type", "text/event-stream")
				body := ""
				if strings.Contains(messages, resultCollectorSystem) {
					collectionCalls.Add(1)
					if issued == "" {
						t.Error("work did not receive an evidence key")
					}
					key := issued
					if scenario == "unknown" {
						key = "ev_000000000000"
					}
					if scenario == "corrupt" {
						if err := os.WriteFile(filepath.Join(store.root, "data", source.ID), []byte("tampered"), 0600); err != nil {
							t.Error(err)
						}
					}
					body = fmt.Sprintf(`{"status":"success","summary":"reviewed","files_read":[{"path":%q}],"structured_payload":{"claims":[{"evidence":[%q]}]}}`, key, key)
					if scenario == "runtime_owned" || scenario == "runtime_full_id" {
						if scenario == "runtime_full_id" {
							key = source.ID
						}
						body = fmt.Sprintf(`{"status":"success","summary":"reviewed","structured_payload":{"claims":[{"evidence":[%q]}]}}`, key)
					}
					if scenario == "string_refs" {
						body = fmt.Sprintf(`{"status":"success","summary":"reviewed","files_read":[%q],"structured_payload":{"claims":[{"evidence":[%q]}]}}`, key, key)
					}
				} else if workCalls.Add(1) == 1 {
					args, _ := json.Marshal(map[string]any{"artifact_ref": source.ID, "byte_limit": 128})
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"read\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"read-source\",\"type\":\"function\",\"function\":{\"name\":\"view\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"read\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", string(args))
					return
				} else {
					issued = regexp.MustCompile(`ev_[0-9a-f]{12}`).FindString(messages)
					body = "Reviewed source using evidence " + issued
				}
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"review\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
			}))
			t.Cleanup(server.Close)
			schema := artifactKeysTestSchema
			if scenario == "runtime_owned" || scenario == "runtime_full_id" {
				schema = observedFilesTestSchema()
			}
			compiled, err := CompileResultContractDocument("keys.json", []byte(schema))
			if err != nil {
				t.Fatal(err)
			}
			c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
				c.session.Agents["worker"].Tools = "view"
				c.coreTools = []fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))}
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true)}
			})
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			store, err = NewFileArtifactStore(c.session.Workspace, c.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
				artifact, err := store.Put(ctx, PutArtifactRequest{Content: []byte("review source evidence"), Path: "source.txt", Kind: "input", RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, Agent: "worker"})
				if err != nil {
					return ExecutionStepResult{}, err
				}
				source = artifact.ArtifactRef
				manifest, err := json.Marshal(WorksetManifest{SchemaVersion: 1, Items: []WorksetItem{{Key: "one", Bindings: map[string]string{"name": "one"}, Inputs: []ArtifactRef{source}}}})
				if err != nil {
					return ExecutionStepResult{}, err
				}
				manifestRef, err := store.Put(ctx, PutArtifactRequest{Content: manifest, Path: "manifest.json", Kind: "workset_manifest", RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, Agent: "worker"})
				if err != nil {
					return ExecutionStepResult{}, err
				}
				return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"manifest": manifestRef.ArtifactRef, "source": source}}, nil
			}))
			_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "produce", Agent: "worker", Goal: "produce review evidence", Execution: ExecutionContract{Steps: []ExecutionStep{{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "manifest", Kind: ExecutionOutputArtifact}, {Name: "source", Kind: ExecutionOutputArtifact}}}}}}})
			if err != nil {
				t.Fatalf("producer: %v; items=%+v", err, c.taskTracker.TodoList().Items())
			}
			_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "review", Agent: "worker", Model: "ollama/test", SideEffect: SideEffectNone, Execution: ExecutionContract{RequiresResult: true, ForbidArtifacts: true}, FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "produce", Artifact: "manifest"}, GoalTemplate: "Read and review {name}"}}})
			items := c.taskTracker.TodoList().Items()
			item := items[len(items)-1]
			if workCalls.Load() != 2 || collectionCalls.Load() != 1 {
				t.Fatalf("work=%d collector=%d status=%s detail=%s err=%v", workCalls.Load(), collectionCalls.Load(), item.Status, item.Detail, err)
			}
			if scenario == "unknown" || scenario == "corrupt" {
				checkpoint := currentResultCollection(item)
				if err == nil || item.TypedResult != nil || checkpoint == nil || checkpoint.FailureReason != RepairFailureSource {
					t.Fatalf("invalid source accepted: status=%s err=%v checkpoint=%+v", item.Status, err, checkpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("status=%s detail=%s: %v", item.Status, item.Detail, err)
			}
			result := item.TypedResult
			if result == nil || result.StructuredPayload == nil || len(result.FilesRead) != 1 || result.FilesRead[0].Path != source.ID || !strings.Contains(string(result.StructuredPayload.Value), source.ID) || strings.Contains(string(result.StructuredPayload.Value), issued) {
				t.Fatalf("noncanonical result: %+v", result)
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToTodoList(events)
			restored := replayed[len(replayed)-1]
			if restored.Status != TaskDone || restored.TypedResult.StructuredPayload.SHA256 != result.StructuredPayload.SHA256 || restored.ExecutionReceipt.ResultPayloadSHA256 != result.StructuredPayload.SHA256 {
				t.Fatal("replay lost canonical result or receipt")
			}
		})
	}
}
