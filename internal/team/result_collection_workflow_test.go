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
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

func TestResultCollectionRepairsRuntimeWorksetFieldsWithoutRepeatingWork(t *testing.T) {
	var calls atomic.Int32
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
		assertCollectorResponseChannel(t, request)
		n := calls.Add(1)
		body := `{"status":"success","summary":"decision from original work","structured_payload":{"record":{"decision":true}}}`
		if n == 1 {
			body = strings.Replace(body, `"decision":true`, `"decision":true,"identity":"model-reconstructed"`, 1)
		} else if n != 2 {
			t.Error("collector exceeded its repair budget")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"claims\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
	}))
	t.Cleanup(server.Close)
	compiled, err := CompileResultContractDocument("assigned.json", []byte(runtimeWorksetInputSchema))
	if err != nil {
		t.Fatal(err)
	}
	c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true)}
	})
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	c.delegatedTasks = make(map[string]int)
	testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
	worker := scriptWorker(textTurn("Decision true from the completed work."))
	c.workerAgentOverride = worker
	_, manifest := runtimeWorksetProducerFixture(t, c, []byte(`{"schema_version":1,"items":[{"key":"assigned-child","bindings":{"identity":"frozen-value"}}]}`))
	_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "evaluate", Agent: "worker", Model: "ollama/test", SideEffect: SideEffectNone,
		Execution: ExecutionContract{RequiresResult: true},
		FanOut:    &FanOutSpec{SourceArtifact: FactRef{TaskID: "seed", Artifact: manifest.ID}, GoalTemplate: "Evaluate {identity}"}}})
	if err != nil {
		t.Fatal(err)
	}
	items := c.taskTracker.TodoList().Items()
	item := items[len(items)-1]
	cp := currentResultCollection(item)
	if worker.Calls() != 1 || calls.Load() != 2 || cp == nil || cp.State != ResultCollectionCommitted || cp.TurnsUsed != 2 || item.Status != TaskDone {
		t.Fatalf("work=%d collections=%d state=%s checkpoint=%+v", worker.Calls(), calls.Load(), item.Status, cp)
	}
	if item.TypedResult == nil || !strings.Contains(string(item.TypedResult.StructuredPayload.Value), `"identity":"frozen-value"`) || !strings.Contains(string(item.TypedResult.StructuredPayload.Value), `"decision":true`) {
		t.Fatal("repair lost the work decision or reconstructed runtime identity")
	}
}

func TestResultCollectionRepairsWorksetPathReferences(t *testing.T) {
	t.Run("direct payload", func(t *testing.T) { testResultCollectionRepairsWorksetPathReferences(t, false) })
	t.Run("sealed normalizer output", func(t *testing.T) { testResultCollectionRepairsWorksetPathReferences(t, true) })
}

func testResultCollectionRepairsWorksetPathReferences(t *testing.T, normalized bool) {
	t.Helper()
	var calls atomic.Int32
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
		assertCollectorResponseChannel(t, request)
		prompt := fmt.Sprint(request["messages"])
		path := "dir/short.go"
		n := calls.Add(1)
		if n == 2 {
			path = "dir/source.go"
			for _, pointer := range []string{"/findings/0/location", "/findings/1/location", "/findings/2/location"} {
				if !strings.Contains(prompt, pointer) {
					t.Errorf("repair prompt did not identify %s", pointer)
				}
			}
		} else if n != 1 {
			t.Error("path repair exceeded its bounded calls")
		}
		matches := regexp.MustCompile(`"(path_[0-9a-f]{12})":"` + regexp.QuoteMeta(path) + `"`).FindStringSubmatch(prompt)
		if len(matches) != 2 || !strings.Contains(prompt, "Frozen source line spans by path") {
			t.Error("collector was not given frozen path keys and source spans")
			http.Error(w, "missing choices", http.StatusBadRequest)
			return
		}
		var findings []map[string]any
		for index, line := range []int{67, 81, 399} {
			location := any(fmt.Sprintf("%s:%d", matches[1], line))
			if n == 1 && index == 0 {
				location = line
			}
			findings = append(findings, map[string]any{"location": location, "summary": fmt.Sprintf("original issue %d retained", line)})
		}
		body, err := json.Marshal(map[string]any{"status": "success", "summary": "original issues retained", "structured_payload": map[string]any{"findings": findings}})
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"claims\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", string(body))
	}))
	t.Cleanup(server.Close)
	compiled, err := CompileResultContractDocument("paths.json", []byte(withWorksetLineSpans(worksetPathResultSchema)))
	if err != nil {
		t.Fatal(err)
	}
	var normalizations atomic.Int32
	c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		ref := compiled.ref(true)
		if normalized {
			bindTestNormalizer(t, c, &TodoItem{ResultContract: &ref}, &mockNormalizerProvider{fn: func(_ context.Context, input []byte) ([]byte, error) {
				normalizations.Add(1)
				return input, nil
			}})
		}
		c.session.AgentResultContracts = map[string]ResultContractRef{"worker": ref}
	})
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	c.delegatedTasks = make(map[string]int)
	testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
	worker := scriptWorker(textTurn("Original issues at dir/source.go lines 67, 81, and 399."))
	c.workerAgentOverride = worker
	if err := os.MkdirAll(filepath.Join(c.projectDir, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.projectDir, "dir", "source.go"), []byte("package fixture\n\n\n\n\n\n// original issue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, manifest := runtimeWorksetProducerFixture(t, c, []byte(`{"schema_version":1,"items":[{"key":"assigned-child","bindings":{"identity":"frozen-value"},"touched_paths":["dir/source.go","dir/short.go"]}]}`), PutArtifactRequest{Kind: "source", Path: "line-inventory.json", Content: []byte(worksetLineInventory)})
	_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "evaluate", Agent: "worker", Model: "ollama/test", SideEffect: SideEffectNone,
		Execution: ExecutionContract{RequiresResult: true},
		FanOut:    &FanOutSpec{SourceArtifact: FactRef{TaskID: "seed", Artifact: manifest.ID}, GoalTemplate: "Evaluate {identity}"}}})
	if err != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			t.Logf("task=%s status=%s detail=%s failure=%+v", item.ID, item.Status, item.Detail, item.FailureEvent)
		}
		t.Fatalf("work=%d collection=%d: %v", worker.Calls(), calls.Load(), err)
	}
	items := c.taskTracker.TodoList().Items()
	item := items[len(items)-1]
	cp := currentResultCollection(item)
	if item.Status != TaskDone || cp == nil || cp.State != ResultCollectionCommitted || worker.Calls() != 1 || calls.Load() != 2 || cp.TurnsUsed != 2 {
		t.Fatalf("work=%d collection=%d task=%s checkpoint=%+v", worker.Calls(), calls.Load(), item.Status, cp)
	}
	if normalized && normalizations.Load() != 2 {
		t.Fatalf("expected one normalization per response, got %d", normalizations.Load())
	}
	for _, line := range []int{67, 81, 399} {
		if !strings.Contains(string(item.TypedResult.StructuredPayload.Value), fmt.Sprintf(`"location":"dir/source.go:%d","summary":"original issue %d retained"`, line, line)) {
			t.Fatalf("consumer did not receive the exact frozen path and original claim at line %d", line)
		}
	}
	replayed := ReduceToTodoList(mustReadEvents(t, c.eventStore))
	stored := todoItemByID(replayed, item.ID)
	if stored == nil || stored.Status != TaskDone || !sameCanonicalEvidence(stored.TypedResult, item.TypedResult) || stored.ExecutionReceipt.ResultPayloadSHA256 != item.TypedResult.StructuredPayload.SHA256 {
		t.Fatal("replay lost the canonical selected path or its receipt")
	}
}

func TestResultCollectionCompletesBoundWorksetDiamond(t *testing.T) {
	const leaves = 16
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
			assertCollectorResponseChannel(t, request)
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
	boundary, err := CompileResultContractDocument("boundary.json", []byte(`{"type":"object","required":["rows"],"properties":{"rows":{"type":"array","maxItems":0}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
		c.maxConcurrent = 4
		c.session.Agents["worker"].ResultTransport = ""
		c.session.Agents["worker"].Tools = "view"
		edge := *c.session.Agents["worker"]
		edge.Name = "boundary"
		c.session.Agents[edge.Name] = &edge
		c.coreTools = []fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))}
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled, boundary.ID: boundary}
		c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true), edge.Name: boundary.ref(true)}
	})
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	c.delegatedTasks = make(map[string]int)
	testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
	manifest := WorksetManifest{SchemaVersion: WorksetSchemaVersion}
	for i := range leaves {
		name := fmt.Sprintf("item-%02d", i)
		manifest.Items = append(manifest.Items, WorksetItem{Key: name, Bindings: map[string]string{"name": name, "target_ids": "[]"}})
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeWorksetManifest(t, c.session.Workspace, string(body))
	root := TaskDef{ID: "prepare", Agent: "boundary", Model: "ollama/test", SideEffect: SideEffectNone,
		Goal: "Confirm input scope.", Execution: ExecutionContract{RequiresResult: true}}
	batch := root
	batch.ID, batch.Agent, batch.DependsOn = "reviews", "worker", []int{0}
	batch.FanOut = &FanOutSpec{Source: "manifest.json", GoalTemplate: "Review {name}; there are no assigned rows."}
	final := root
	final.ID, final.Goal, final.DependsOn = "aggregate", "Summarize accepted results.", []int{1}
	_, err = c.ExecuteTasks(t.Context(), []TaskDef{root, batch, final})
	if err != nil {
		for _, item := range c.taskTracker.TodoList().Items()[1:] {
			t.Logf("child %s: status=%s detail=%s output=%s", item.ID, item.Status, item.Detail, item.Output)
		}
		t.Fatal(err)
	}
	diamond := c.taskTracker.TodoList().Items()[1:]
	if len(diamond) != leaves+2 {
		t.Fatalf("diamond tasks=%d, want %d", len(diamond), leaves+2)
	}
	first, items, last := diamond[0], diamond[1:len(diamond)-1], diamond[len(diamond)-1]
	if first.Status != TaskDone || last.Status != TaskDone || len(last.DependsOn) != leaves {
		for _, item := range diamond {
			if item.Status != TaskDone {
				t.Logf("task %s: status=%s detail=%s", item.ID, item.Status, item.Detail)
			}
		}
		t.Fatalf("diamond boundary states=%s/%s, aggregate dependencies=%d", first.Status, last.Status, len(last.DependsOn))
	}
	receipt := items[0].WorksetReceipt
	if receipt == nil {
		t.Fatal("workset expansion receipt is missing")
	}
	for _, item := range items {
		if item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.Source != "collected" || item.WorksetBinding == nil || item.ExecutionReceipt.ResultValidation != ResultValidationValid || receipt.Children[item.WorksetBinding.ItemKey] != item.ID {
			t.Fatalf("bound review incomplete: status=%s detail=%s", item.Status, item.Detail)
		}
		if !slices.Equal(item.DependsOn, []string{first.ID}) || !slices.Contains(last.DependsOn, item.ID) {
			t.Fatalf("leaf %s lost its root or fan-in dependency", item.ID)
		}
	}
	if workCalls.Load() != leaves+2 || collectionCalls.Load() != leaves+2 {
		t.Fatalf("work=%d collection=%d, want one of each per item", workCalls.Load(), collectionCalls.Load())
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayedDiamond := ReduceToTodoList(events)[1:]
	if len(replayedDiamond) != len(diamond) || replayedDiamond[len(replayedDiamond)-1].Status != TaskDone {
		t.Fatal("replay lost the aggregate completion")
	}
	replayed := replayedDiamond[1 : len(replayedDiamond)-1]
	if replayed[0].WorksetReceipt == nil || !equivalentWorksetReceipts(receipt, replayed[0].WorksetReceipt) {
		t.Fatal("replay lost the workset expansion receipt")
	}
	for _, item := range replayed {
		if item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.StructuredPayload == nil || item.WorksetBinding == nil || receipt.Children[item.WorksetBinding.ItemKey] != item.ID || item.ExecutionReceipt.ResultPayloadSHA256 != item.TypedResult.StructuredPayload.SHA256 {
			t.Fatal("replay lost the accepted workset result or receipt")
		}
	}
	if err := os.Remove(filepath.Join(c.session.Workspace, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := c.validateAcceptedDependencyPayloadWithContext(t.Context(), items[0]); err != nil {
		t.Fatalf("deleting the original workspace path invalidated its frozen snapshot: %v", err)
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
				assertCollectorResponseChannel(t, request)
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
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	for _, scenario := range []string{"valid", "escaped_key", "string_refs", "unknown", "corrupt", "runtime_owned", "runtime_full_id", "runtime_full_typo", "runtime_key_typo", "runtime_normalized_key_typo", "runtime_normalized_read_fault", "runtime_repeated_typo", "runtime_typo_corrupt", "runtime_typo_receipt", "runtime_review_typo", "runtime_documentation_typo", "runtime_supplement_typo", "runtime_documentation_empty", "runtime_full_repeated_typo", "runtime_review_invariant_typo", "runtime_review_invariant_foreign_typo"} {
		t.Run(scenario, func(t *testing.T) {
			var workCalls, collectionCalls atomic.Int32
			var normalizations atomic.Int32
			readFault := scenario == "runtime_normalized_read_fault"
			if readFault && os.Geteuid() == 0 {
				t.Skip("permission interruption requires a non-root process")
			}
			normalize := readFault || scenario == "runtime_normalized_key_typo" || scenario == "escaped_key"
			var source ArtifactRef
			var store *FileArtifactStore
			var sourceFile string
			var sourceMode os.FileMode
			var issued string
			reviewSchema := ""
			sourcePath, sourceText := "source.txt", "review source evidence"
			switch scenario {
			case "runtime_review_typo", "runtime_review_invariant_typo", "runtime_review_invariant_foreign_typo":
				reviewSchema = "schemas/review-v2.json"
			case "runtime_documentation_typo", "runtime_documentation_empty":
				reviewSchema = "schemas/documentation-review-v2.json"
			case "runtime_supplement_typo":
				reviewSchema = "schemas/supplement-review-v2.json"
				sourcePath, sourceText = "source.go", "package source\n\nconst Value = 42\n"
			}
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
					turn := collectionCalls.Add(1)
					if issued == "" {
						t.Error("work did not receive an evidence key")
					}
					key := issued
					if scenario == "unknown" {
						key = "ev_000000000000"
					}
					if scenario == "corrupt" || scenario == "runtime_typo_corrupt" {
						if err := os.WriteFile(filepath.Join(store.root, "data", source.ID), []byte("tampered"), 0600); err != nil {
							t.Error(err)
						}
					}
					body = fmt.Sprintf(`{"status":"success","summary":"reviewed","files_read":[{"path":%q}],"structured_payload":{"claims":[{"evidence":[%q]}]}}`, key, key)
					if scenario == "escaped_key" {
						body = strings.ReplaceAll(body, "ev_", `ev\u005f`)
					}
					if strings.HasPrefix(scenario, "runtime_") {
						if scenario == "runtime_full_id" {
							key = source.ID
						}
						if strings.Contains(scenario, "typo") && (turn == 1 || (strings.Contains(scenario, "repeated"))) {
							key = "ev_000000000000"
							if strings.Contains(scenario, "full_") {
								last := "0"
								if strings.HasSuffix(source.ID, last) {
									last = "1"
								}
								key = source.ID[:len(source.ID)-1] + last
							}
						}
						if turn > 1 {
							wantDiagnostic := "enum"
							if normalize {
								wantDiagnostic = "evidence_reference_unknown"
							}
							if !strings.Contains(messages, issued) || !strings.Contains(messages, wantDiagnostic) {
								t.Error("repair request omitted exact observed choices")
							}
						}
						body = fmt.Sprintf(`{"status":"success","summary":"reviewed","structured_payload":{"claims":[{"evidence":[%q],"note":"preserve this judgment"}]}}`, key)
						if reviewSchema != "" {
							findings := []any{}
							if scenario != "runtime_documentation_empty" {
								findings = append(findings, map[string]any{"summary": "original finding", "detail": "preserve this judgment", "severity": "info", "location": sourcePath + ":1", "requires_critique": false, "evidence": []string{key}})
							}
							record := map[string]any{"findings": findings, "gaps": []any{}, "reference_assessments": []any{}, "invariant_assessments": []any{}}
							if strings.Contains(scenario, "review_invariant") {
								finding := findings[0].(map[string]any)
								delete(finding, "severity")
								finding["requires_critique"] = true
								invariantID := "safe"
								if strings.Contains(scenario, "foreign") {
									invariantID = "foreign"
								}
								record["invariant_assessments"] = []any{map[string]any{"invariant_id": invariantID, "status": "violated", "summary": "boundary violated", "finding_index": 0}}
							}
							encoded, err := json.Marshal(map[string]any{"status": "success", "summary": "reviewed", "structured_payload": map[string]any{"record": record}})
							if err != nil {
								t.Error(err)
								return
							}
							body = string(encoded)
						}
						if scenario == "runtime_typo_receipt" {
							body = strings.Replace(body, `"summary":`, `"receipt_ids":["forged"],"summary":`, 1)
						}
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
				if strings.HasPrefix(scenario, "runtime_") && strings.Contains(messages, resultCollectorSystem) {
					assertCollectorResponseChannel(t, request)
					functions, _ := request["tools"].([]any)
					if len(functions) != 1 {
						return
					}
					function := functions[0].(map[string]any)["function"].(map[string]any)
					parameters := function["parameters"].(map[string]any)
					arguments := fmt.Sprintf(`{"result":%s}`, body)
					info := fantasy.ToolInfo{Name: "return_result", Parameters: parameters["properties"].(map[string]any), Required: []string{"result"}}
					invalidSelection := strings.Contains(scenario, "typo") && (collectionCalls.Load() == 1 || (strings.Contains(scenario, "repeated")))
					if violation := validateToolArguments(arguments, info); (violation != nil) != (invalidSelection && !normalize) {
						t.Errorf("serialized response schema did not constrain observed choices: %v", violation)
					}
					chunk, err := json.Marshal(map[string]any{"id": "collected", "object": "chat.completion.chunk", "created": 1, "model": "test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "return", "type": "function", "function": map[string]any{"name": "return_result", "arguments": arguments}}}}, "finish_reason": "tool_calls"}}})
					if err != nil {
						t.Error(err)
						return
					}
					_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
					return
				}
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"review\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
			}))
			t.Cleanup(server.Close)
			schema := artifactKeysTestSchema
			if strings.HasPrefix(scenario, "runtime_") {
				schema = observedFilesTestSchema()
			}
			compiled, err := CompileResultContractDocument("keys.json", []byte(schema))
			if err != nil {
				t.Fatal(err)
			}
			reviewExecution := ExecutionContract{RequiresResult: true, ForbidArtifacts: true}
			var reviewVerification *VerificationSpec
			var invariantMode InvariantVerificationMode
			if reviewSchema != "" {
				loaded := loadHufuCodeReviewTeam(t)
				compiled = loaded.ResultContracts[reviewSchema]
				for _, task := range loaded.ContractTasks {
					if task.ResultContract != nil && task.ResultContract.ID == reviewSchema {
						reviewExecution, reviewVerification, invariantMode = task.Execution, task.VerifySpec, task.InvariantVerification
						break
					}
				}
				if reviewVerification == nil {
					t.Fatal("real review contract lost its objective verification")
				}
				if !runtimeOwnsFilesRead(compiled) {
					t.Fatal("review contract does not validate native evidence before acceptance")
				}
			}
			ref := compiled.ref(true)
			c, seed, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
				c.session.Agents["worker"].Tools = "view"
				c.coreTools = []fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))}
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				c.session.AgentResultContracts = nil
				if normalize {
					ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: func(_ context.Context, draft []byte) ([]byte, error) {
						normalizations.Add(1)
						if readFault {
							if err := os.Chmod(sourceFile, 0); err != nil {
								return nil, err
							}
						}
						return draft, nil
					}})
				}
				if strings.Contains(scenario, "review_invariant") {
					c.session.InvariantCatalog = []InvariantDefinition{{ID: "safe", Statement: "Preserve the declared safety boundary", Severity: InvariantSeverityError, AppliesTo: []string{"*"}}}
				}
			})
			if readFault {
				// Ordinary resume visits all interrupted tasks. The fixture's seed
				// is outside this workset and must already be terminal.
				if err := c.CommitTaskTransition(t.Context(), seed.ID, TaskInProgress, TaskDone, "", "fixture initialized", nil); err != nil {
					t.Fatal(err)
				}
			}
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			store, err = NewFileArtifactStore(c.session.Workspace, c.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			itemKey, lens := "one", "general"
			if reviewSchema != "" {
				itemKey = "unit-0000"
				if strings.Contains(reviewSchema, "documentation") {
					lens = "documentation-risk"
				}
				if strings.Contains(reviewSchema, "supplement") {
					itemKey = "supplement-0000"
				}
			}
			c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
				artifact, err := store.Put(ctx, PutArtifactRequest{Content: []byte(sourceText), Path: sourcePath, Kind: "input", RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, Agent: "worker"})
				if err != nil {
					return ExecutionStepResult{}, err
				}
				source = artifact.ArtifactRef
				manifest, err := json.Marshal(WorksetManifest{SchemaVersion: 1, Items: []WorksetItem{{Key: itemKey, Bindings: map[string]string{"name": "one", "key": itemKey, "review_revision": "frozen-head", "lens": lens, "reference_candidate_ids": "[]"}, Inputs: []ArtifactRef{source}, TouchedPaths: []string{sourcePath}}}})
				if err != nil {
					return ExecutionStepResult{}, err
				}
				manifestRef, err := store.Put(ctx, PutArtifactRequest{Content: manifest, Path: "manifest.json", Kind: "workset_manifest", RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, Agent: "worker"})
				if err != nil {
					return ExecutionStepResult{}, err
				}
				return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"manifest": manifestRef.ArtifactRef, "source": source}}, nil
			}))
			if err := os.WriteFile(filepath.Join(c.projectDir, sourcePath), []byte(sourceText), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "produce", Agent: "worker", Goal: "produce review evidence", Execution: ExecutionContract{Steps: []ExecutionStep{{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "manifest", Kind: ExecutionOutputArtifact}, {Name: "source", Kind: ExecutionOutputArtifact}}}}}}})
			if err != nil {
				t.Fatalf("producer: %v; items=%+v", err, c.taskTracker.TodoList().Items())
			}
			if readFault {
				sourceFile = filepath.Join(store.root, "data", source.ID)
				info, err := os.Stat(sourceFile)
				if err != nil {
					t.Fatal(err)
				}
				sourceMode = info.Mode()
				t.Cleanup(func() { _ = os.Chmod(sourceFile, sourceMode) })
			}
			_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "review", Agent: "worker", Model: "ollama/test", ResultContract: &ref, InvariantVerification: invariantMode, SideEffect: SideEffectNone, Execution: reviewExecution, VerifySpec: reviewVerification, FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "produce", Artifact: "manifest"}, GoalTemplate: "Read and review {name}"}}})
			items := c.taskTracker.TodoList().Items()
			item := items[len(items)-1]
			wantCollections := int32(1)
			if strings.Contains(scenario, "typo") && scenario != "runtime_typo_corrupt" && scenario != "runtime_typo_receipt" && !strings.Contains(scenario, "foreign") {
				wantCollections = 2
			}
			if workCalls.Load() != 2 || collectionCalls.Load() != wantCollections {
				t.Fatalf("work=%d collector=%d status=%s detail=%s err=%v", workCalls.Load(), collectionCalls.Load(), item.Status, item.Detail, err)
			}
			if normalize && normalizations.Load() != wantCollections {
				t.Fatalf("normalizer ran %d times, want %d", normalizations.Load(), wantCollections)
			}
			if readFault {
				checkpoint := currentResultCollection(item)
				if err == nil || item.Status != TaskInProgress || item.TypedResult != nil || checkpoint == nil ||
					checkpoint.State != ResultCollectionPrepared || checkpoint.Prepared == nil {
					t.Fatalf("read interruption lost prepared claims: checkpoint=%+v, err=%v", checkpoint, err)
				}
				if err := os.Chmod(sourceFile, sourceMode); err != nil {
					t.Fatal(err)
				}
				c = reopenRecoveryCoordinator(t, c)
				replayed := currentResultCollection(c.todoItemByID(item.ID))
				if replayed == nil || replayed.State != ResultCollectionPrepared || replayed.Work != checkpoint.Work {
					t.Fatalf("read interruption did not replay as prepared: checkpoint=%+v", replayed)
				}
				c.executionRunID = "evidence-read-recovery"
				worker := scriptWorker(textTurn("must never repeat original work"))
				c.workerAgentOverride = worker
				var recovered int
				recovered, err = c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
				item = c.todoItemByID(item.ID)
				if err != nil || recovered != 1 || worker.Calls() != 0 || workCalls.Load() != 2 || collectionCalls.Load() != 1 || normalizations.Load() != 1 {
					t.Fatalf("local commit recovered %d tasks: %v, work/collection/normalizer calls=%d/%d/%d", recovered, err, workCalls.Load(), collectionCalls.Load(), normalizations.Load())
				}
				after := currentResultCollection(item)
				if after == nil || after.Work != checkpoint.Work || after.Prepared == nil || *after.Prepared != *checkpoint.Prepared {
					t.Fatalf("local commit replaced original evidence: checkpoint=%+v", after)
				}
			}
			if scenario == "unknown" || scenario == "corrupt" || strings.Contains(scenario, "repeated") || scenario == "runtime_typo_corrupt" || scenario == "runtime_typo_receipt" || strings.Contains(scenario, "foreign") {
				checkpoint := currentResultCollection(item)
				wantReason := RepairFailureSource
				if strings.Contains(scenario, "repeated") {
					wantReason = RepairFailureInvalidSchema
				}
				if err == nil || item.TypedResult != nil || checkpoint == nil || checkpoint.FailureReason != wantReason {
					t.Fatalf("invalid source accepted: status=%s err=%v checkpoint=%+v", item.Status, err, checkpoint)
				}
				c = reopenRecoveryCoordinator(t, c)
				assessment, previewErr := c.AssessResultCollectionRecovery(t.Context(), item.ID)
				if !strings.Contains(scenario, "repeated") {
					if previewErr == nil && (assessment.Disposition != RecoveryBlocked || assessment.AdditionalTurns != 0) {
						t.Fatalf("invalid source earned recovery: %+v", assessment)
					}
					return
				}
				if previewErr != nil || assessment.Disposition != RecoveryBlocked || assessment.AdditionalTurns != 0 || collectionCalls.Load() != 2 {
					t.Fatalf("exhausted selection correction preview=%+v error=%v", assessment, previewErr)
				}
				c.executionRunID = "evidence-selection-recovery"
				if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err == nil {
					t.Fatal("explicit recovery approved a third correction turn")
				}
				replayed := currentResultCollection(c.todoItemByID(item.ID))
				if collectionCalls.Load() != 2 || workCalls.Load() != 2 || !sameCanonicalEvidence(replayed, checkpoint) {
					t.Fatal("blocked recovery changed the original work or turn history")
				}
				return
			}
			if err != nil {
				t.Fatalf("status=%s detail=%s: %v", item.Status, item.Detail, err)
			}
			result := item.TypedResult
			if result == nil || result.StructuredPayload == nil || len(result.FilesRead) != 1 || result.FilesRead[0].Path != source.ID || (scenario != "runtime_documentation_empty" && !strings.Contains(string(result.StructuredPayload.Value), source.ID)) || strings.Contains(string(result.StructuredPayload.Value), issued) {
				t.Fatalf("noncanonical result: %+v", result)
			}
			if strings.Contains(scenario, "typo") && !strings.Contains(string(result.StructuredPayload.Value), "preserve this judgment") {
				t.Fatal("repair lost the original judgment")
			}
			if reviewSchema != "" {
				expectedSeverity := "info"
				if strings.Contains(scenario, "review_invariant") {
					expectedSeverity = "error"
					if result.InvariantVerification == nil || len(result.InvariantVerification.Assessments) != 1 || result.InvariantVerification.Assessments[0].Status != InvariantViolated {
						t.Fatal("citation repair lost the invariant violation")
					}
				}
				if scenario == "runtime_documentation_empty" {
					if len(result.Findings) != 0 {
						t.Fatal("empty review gained a finding during collection")
					}
				} else if len(result.Findings) != 1 || result.Findings[0].Summary != "original finding" || result.Findings[0].Detail != "preserve this judgment" || result.Findings[0].Severity != expectedSeverity {
					t.Fatal("citation repair changed the original finding or its severity")
				}
				acceptedHash := result.StructuredPayload.SHA256
				var handoffs atomic.Int32
				c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
					handoffs.Add(1)
					if request.Step.Tool != "inventory" || c.todoItemByID(item.ID).Status != TaskDone || c.todoItemByID(item.ID).TypedResult.StructuredPayload.SHA256 != acceptedHash {
						t.Error("handoff lost its accepted review source")
					}
					return ExecutionStepResult{}, nil
				}))
				_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "inventory", Agent: "worker", Goal: "verify accepted source", EvidenceFrom: []string{item.ID}, SideEffect: SideEffectNone, Execution: ExecutionContract{Steps: []ExecutionStep{{ID: "audit", Tool: "inventory", Effect: ExecutionEffectVerify}}}}})
				if err != nil || handoffs.Load() != 1 || workCalls.Load() != 2 || collectionCalls.Load() != wantCollections {
					t.Fatalf("handoff repeated work or lost repair: calls=%d error=%v", handoffs.Load(), err)
				}
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToTodoList(events)
			restored := todoItemByID(replayed, item.ID)
			if restored.Status != TaskDone || restored.TypedResult.StructuredPayload.SHA256 != result.StructuredPayload.SHA256 || restored.ExecutionReceipt.ResultPayloadSHA256 != result.StructuredPayload.SHA256 {
				t.Fatal("replay lost canonical result or receipt")
			}
		})
	}
}
