package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"charm.land/fantasy"
	"fmt"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/tools"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestResultCollectionFailureExplainsPossibleModelCause(t *testing.T) {
	for _, reason := range []RepairFailureReason{RepairFailureInvalidSchema, RepairFailureSource, RepairFailureCommit, repairFailureCollectionUnavailable, repairFailureCollectionBudget} {
		t.Run(string(reason), func(t *testing.T) {
			cause := errors.New("original diagnostic")
			failure := &resultCollectionFailure{Reason: reason, Cause: cause}
			message := failure.Error()
			hasModelHint := strings.Contains(message, "model or an upstream provider/adapter")
			if hasModelHint != (reason == RepairFailureInvalidSchema) {
				t.Fatalf("inappropriate cause hint for %s: %s", reason, message)
			}
			if !strings.Contains(message, cause.Error()) || !errors.Is(failure, cause) || failure.Reason != reason {
				t.Fatal("diagnostic hint changed the failure reason or cause")
			}
			if hasModelHint && !strings.Contains(message, "does not establish a Hufu runtime defect") {
				t.Fatal("diagnostic asserted an unverified cause")
			}
		})
	}
}

func TestResultCollectionKeepsRedactedEvidenceJSONValid(t *testing.T) {
	requests := make(chan string, 2)
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
		for _, raw := range request["messages"].([]any) {
			message := raw.(map[string]any)
			if message["role"] == "user" {
				requests <- message["content"].(string)
				break
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"response\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n", agent.StructuredResponseFunctionName, `{"result":{"status":"success","summary":"saved observations retained","facts":{"worker_tokens":{"input":5,"output":3},"scope":"fixture"}}}`)
	}))
	t.Cleanup(server.Close)
	c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true)
	const secret = "collector-fixture-secret-3d29"
	report, err := json.Marshal(map[string]any{"source": `const password = "` + secret + `"`, "observations": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, string(report))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.collectWorkResult(ctx, c.session.Agents["worker"], checkpoint); err != nil {
		t.Fatal(err)
	}
	var prompt string
	select {
	case prompt = <-requests:
	default:
		t.Fatal("collection did not send its frozen evidence")
	}
	start := strings.Index(prompt, `{"claims_schema":`)
	if start < 0 {
		t.Fatal("collector evidence block is missing")
	}
	data, err := jsontext.NewDecoder(strings.NewReader(prompt[start:])).ReadValue()
	if err != nil {
		t.Fatalf("collector wire evidence is not valid JSON: %v", err)
	}
	var input struct {
		Report string `json:"work_report"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Source       string `json:"source"`
		Observations []any  `json:"observations"`
	}
	if err := json.Unmarshal([]byte(input.Report), &saved); err != nil || saved.Observations == nil {
		t.Fatalf("encoded work report was corrupted: %v", err)
	}
	if strings.Contains(prompt, secret) || !strings.Contains(saved.Source, "[REDACTED]") {
		t.Fatal("credential example was not safely redacted")
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)
	if len(replayed) != 1 {
		t.Fatal("collection replay lost the task")
	}
	replayedCollection := currentResultCollection(replayed[0])
	if replayedCollection == nil || replayedCollection.State != ResultCollectionCommitted || replayedCollection.Work != checkpoint.Work {
		t.Fatal("collection replay changed the sealed work binding")
	}
	accepted, err := c.acceptedCollectionResult(ctx, replayed[0], replayedCollection)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Facts["worker_tokens"] != "[REDACTED]" || accepted.Facts["scope"] != "fixture" {
		t.Fatal("accepted artifact was saved before JSON-aware redaction")
	}
	var eventResult *TaskResult
	for _, event := range events {
		if event.Type != string(EventTaskResultCollectionCheckpointed) {
			continue
		}
		var payload resultCollectionEvent
		if err := json.Unmarshal(event.Payload, &payload, collectionDurationFormat); err != nil {
			t.Fatal(err)
		}
		if payload.Result != nil {
			eventResult = payload.Result
		}
	}
	if !reflect.DeepEqual(accepted, eventResult) {
		t.Fatal("durable event and accepted artifact disagree after redaction")
	}
	work, err := c.loadCollectionWork(t.Context(), replayed[0], replayedCollection)
	if err != nil || !reflect.DeepEqual(work.Receipt, receipt) {
		t.Fatalf("collection changed the original work receipt: %v", err)
	}
}

func TestResultCollectorAttributesProviderFailureToFrozenTarget(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		status int
	}{
		{name: "http_error", status: http.StatusInternalServerError},
		{name: "stream_error"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const response = `{"error":{"message":"Internal Server Error","type":"api_error","param":null,"code":null}}`
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
					return
				}
				calls.Add(1)
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				if request["model"] != "test" {
					t.Errorf("wire model = %v, want frozen worker model", request["model"])
				}
				assertCollectorResponseChannel(t, request)
				if scenario.status != 0 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(scenario.status)
					_, _ = fmt.Fprint(w, response)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", response)
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true)
			ctx = context.WithValue(ctx, modelKey{}, "ollama/coordinator")
			checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "complete work report")
			if err != nil {
				t.Fatal(err)
			}
			err = c.collectWorkResult(ctx, c.session.Agents["worker"], checkpoint)
			failure, ok := errors.AsType[*resultCollectionFailure](err)
			if !ok || failure.Reason != repairFailureCollectionUnavailable {
				t.Fatalf("expected unavailable collection, got %v", err)
			}
			providerFailure := providerModelFailureFrom(err)
			if providerFailure == nil {
				t.Fatalf("missing provider failure: %v", err)
			}
			if providerFailure.model != checkpoint.ExecutionTarget.String() || providerFailure.provider != "ollama" {
				t.Errorf("failure attributed to %q/%q, want frozen target %s", providerFailure.provider, providerFailure.model, checkpoint.ExecutionTarget)
			}
			if providerFailure.statusCode != scenario.status || !strings.HasSuffix(providerFailure.response, response) {
				t.Errorf("provider evidence changed: status=%d response=%s", providerFailure.statusCode, providerFailure.response)
			}
			blocked := failure.Checkpoint
			if blocked.State != ResultCollectionBlocked || blocked.TurnsUsed != 1 || blocked.Response != nil {
				t.Fatalf("unexpected failure checkpoint: %+v", blocked)
			}
			if blocked.Work != checkpoint.Work || blocked.ModelExecutionID != checkpoint.ModelExecutionID || calls.Load() != agent.ProviderTransientRetryLimit+1 {
				t.Fatal("provider retry changed saved work or exceeded the request retry limit")
			}
			if ctx.Value(modelKey{}) != "ollama/coordinator" {
				t.Fatal("collector changed the caller's model identity")
			}
		})
	}
}

func TestResultCollectionRetriesTransientRequestWithoutRepeatingWork(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			var calls atomic.Int32
			var mu sync.Mutex
			var requests []string
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
				encoded, err := json.Marshal(request, json.Deterministic(true))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				requests = append(requests, string(encoded))
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) <= agent.ProviderTransientRetryLimit {
					if partial {
						_, _ = fmt.Fprintf(w, "data: {\"id\":\"partial\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"prepare claims\",\"tool_calls\":[{\"index\":0,\"id\":\"partial\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", agent.StructuredResponseFunctionName, `{"result":{"status":`)
					}
					_, _ = fmt.Fprint(w, "data: {\"error\":{\"message\":\"Internal Server Error\",\"type\":\"api_error\"}}\n\ndata: [DONE]\n\n")
					return
				}
				arguments := `{"result":{"status":"success","summary":"review preserved","details":"original evidence","facts":{"proof":"unchanged"}}}`
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"result\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"result\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", agent.StructuredResponseFunctionName, arguments)
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true)
			checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "complete work report")
			if err != nil {
				t.Fatal(err)
			}
			if err := c.collectWorkResult(ctx, c.session.Agents["worker"], checkpoint); err != nil {
				t.Fatalf("provider calls=%d: %v", calls.Load(), err)
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToTodoList(events)
			if len(replayed) != 1 {
				t.Fatal("collection lost its original task")
			}
			committed := currentResultCollection(replayed[0])
			if committed == nil || committed.State != ResultCollectionCommitted || committed.TurnsUsed != 1 || committed.Work != checkpoint.Work || calls.Load() != agent.ProviderTransientRetryLimit+1 {
				t.Fatalf("resends repeated work or consumed format turns: %+v calls=%d", committed, calls.Load())
			}
			accepted, err := c.acceptedCollectionResult(ctx, replayed[0], committed)
			if err != nil || accepted == nil || accepted.Status != TaskResultStatusSuccess || accepted.Facts["proof"] != "unchanged" {
				t.Fatalf("accepted=%+v err=%v", accepted, err)
			}
			work, err := c.loadCollectionWork(ctx, replayed[0], committed)
			if err != nil || !reflect.DeepEqual(work.Receipt, receipt) {
				t.Fatalf("original receipt changed: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, request := range requests[1:] {
				if request != requests[0] {
					t.Fatal("resend changed collector input or result schema")
				}
			}
		})
	}
}

func TestResultCollectorUsesDataOnlyRequestsAndBoundedFormatRepair(t *testing.T) {
	for _, scenario := range []struct {
		replies []string
		worker  bool
		entry   string
		fanout  bool
	}{
		{replies: []string{`{"status":"success","summary":"analysis complete"}`}},
		{entry: "native_response", replies: []string{`{"status":"success","summary":"analysis complete"}`}},
		{entry: "native_response", replies: []string{`{"status":"success","summary":"analysis complete","assumption_checks":[],"commands":[],"files_modified":[],"memory_uses":[],"receipt_ids":[],"suggested_next_tasks":[],"verification":[]}`}},
		{entry: "native_response", replies: []string{`{"status":"success","summary":""}`, `{"status":"success","summary":"analysis complete"}`}},
		{replies: []string{`invalid JSON`, `{"status":"success","summary":"analysis complete"}`}},
		{replies: []string{`{"status":"success","summary":""}`, `{"status":"success","summary":"analysis complete"}`}},
		{replies: []string{`invalid JSON`, `still invalid JSON`}},
		{entry: "collector_tool", replies: []string{"call_tool", `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, replies: []string{`complete work report`, `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, entry: "unattended", replies: []string{`complete work report`, `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, entry: "approved_plan", replies: []string{`complete work report`, `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, replies: []string{`complete work report`, `invalid JSON`, `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, replies: []string{`complete work report`, `invalid JSON`, `still invalid JSON`}},
		{worker: true, entry: "direct", replies: []string{`complete work report`, `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, entry: "subagent", replies: []string{`complete work report`, `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, entry: "fanout", fanout: true, replies: []string{`complete work report`, `{"status":"success","summary":"analysis complete"}`, `complete work report`, `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, entry: "script", replies: []string{`invalid JSON`, `still invalid JSON`}},
		{worker: true, entry: "script", replies: []string{`{"status":"success","summary":"analysis complete"}`}},
		{worker: true, entry: "deprecated", replies: []string{`{"status":"success","summary":"analysis complete"}`}},
		{worker: true, entry: "transcript_closed", replies: []string{"unused model response"}},
		{worker: true, entry: "verbatim", replies: []string{`complete work report`, `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, entry: "isolated", replies: []string{`complete work report`, `{"status":"success","summary":"analysis complete"}`}},
	} {
		replies := scenario.replies
		t.Run(fmt.Sprintf("worker_%t_%s_turns_%d_%s", scenario.worker, scenario.entry, len(replies), replies[0][:min(6, len(replies[0]))]), func(t *testing.T) {
			var mu sync.Mutex
			var requests []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/show":
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
				case "/api/ps":
					_, _ = fmt.Fprint(w, `{"models":[{"name":"test","context_length":131072}]}`)
				case "/v1/chat/completions":
					var request map[string]any
					if err := json.UnmarshalRead(r.Body, &request); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					mu.Lock()
					index := len(requests)
					requests = append(requests, request)
					mu.Unlock()
					if index >= len(replies) {
						http.Error(w, "unexpected extra model call", http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if scenario.entry == "native_response" {
						_, _ = fmt.Fprintf(w, "data: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Let me analyze this task carefully.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"response\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\ndata: [DONE]\n\n", agent.StructuredResponseFunctionName, `{"result":`+replies[index]+`}`)
						return
					}
					if replies[index] == "call_tool" {
						_, _ = fmt.Fprint(w, "data: {\"id\":\"forbidden-tool\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"forbidden-call\",\"type\":\"function\",\"function\":{\"name\":\"write\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"forbidden-tool\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
						return
					}

					_, _ = fmt.Fprintf(w, "data: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", func() string {
						if scenario.fanout {
							if strings.Contains(fmt.Sprint(request["messages"]), resultCollectorSystem) {
								return `{"status":"success","summary":"analysis complete"}`
							}
							return "complete work report"
						}
						return replies[index]
					}())
					_, _ = fmt.Fprint(w, "data: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\ndata: [DONE]\n\n")
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
				// Exercise the admitted new-session default through real HTTP,
				// without an explicit per-agent transport override.
				c.session.Agents["worker"].ResultTransport = ""
				c.unattended = scenario.entry == "unattended"
				if scenario.entry == "approved_plan" {
					c.pendingPlans = map[string]*PlanEntry{"approved-1": {TodoID: "approved-1", PlanText: "analyze the evidence", Status: "approved"}}
				}
				if scenario.entry == "isolated" {
					c.session.Config.WorkerWorkspace = isolatedSpec()
					c.session.Scope.Managed = true
					c.session.Scope.ProjectID = "collector-isolated-fixture"
				}
				if scenario.fanout {
					c.session.Agents["worker"].ExtraModels = []string{"ollama/test2"}
				}
			})
			var scripted *scriptedWorker
			var workTool fantasy.AgentTool
			countEffects := func() int { data, _ := os.ReadFile(filepath.Join(c.projectDir, "work-count")); return len(data) }
			if scenario.entry == "script" || scenario.entry == "deprecated" || scenario.entry == "transcript_closed" {
				workTool = tools.NewBashTool(tools.WithWorkDir(c.projectDir))
				c.coreTools = []fantasy.AgentTool{workTool}
				c.session.Agents["worker"].Tools = "bash"
				c.session.Agents["worker"].SideEffect = string(SideEffectWorkspaceWrite)
				scripted = scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
					name := "bash"
					input := `{"command":"printf x >> work-count"}`
					if scenario.entry == "deprecated" {
						name = submitResultToolName
					}
					if err := call.OnToolCall(fantasy.ToolCallContent{ToolCallID: "work-once", ToolName: name, Input: input}); err != nil {
						return nil, err
					}
					response, err := c.gatePolicyTools([]fantasy.AgentTool{workTool})[0].Run(ctx, fantasy.ToolCall{ID: "work-once", Name: name, Input: input})
					if err != nil || response.IsError {
						return nil, fmt.Errorf("work tool failed: %v %s", err, response.Content)
					}
					if err := call.OnToolResult(fantasy.ToolResultContent{ToolCallID: "work-once", ToolName: name, Result: fantasy.ToolResultOutputContentText{Text: response.Content}}); err != nil {
						return nil, err
					}
					if scenario.entry == "transcript_closed" {
						transcript, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript)
						if transcript == nil {
							return nil, fmt.Errorf("missing native recorder")
						}
						if err := transcript.Close(); err != nil {
							return nil, err
						}
					}
					return textAgentResult("complete work report"), nil
				}})
				c.workerAgentOverride = scripted
			}
			var err error
			if scenario.worker {
				c.delegatedTasks = make(map[string]int)
				c.providerBoundaryStart = func(context.Context, string) error { return nil }
				testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
				switch scenario.entry {
				case "direct":
					_, err = c.RunDirectAgent(t.Context(), "worker", "prepare a complete analysis report for this invocation")
				case "subagent":
					response, callErr := (&requestAgentTool{coordinator: c}).Run(ctx, fantasy.ToolCall{Name: "request_agent", Input: `{"goal":"prepare a complete delegated analysis report","agent":"worker"}`})
					err = callErr
					if response.IsError {
						err = fmt.Errorf("%s", response.Content)
					}

				default:
					_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "analysis", Agent: "worker", Model: "ollama/test", Goal: "prepare a complete analysis report for this invocation", PlanFirst: scenario.entry == "approved_plan", PlanID: func() string {
						if scenario.entry == "approved_plan" {
							return "approved-1"
						}
						return ""
					}(), OutputMode: func() string {
						if scenario.entry == "verbatim" {
							return TaskOutputModeVerbatim
						}
						return ""
					}(), Execution: ExecutionContract{RequiresResult: true}}})
				}

				items := c.taskTracker.TodoList().Items()
				item = items[len(items)-1]
			} else {
				err = c.collectReturnedWork(ctx, c.session.Agents["worker"], receipt, "complete work report", "analyze the supplied evidence")
			}
			wantSuccess := replies[len(replies)-1] != "still invalid JSON" && scenario.entry != "transcript_closed"
			if !scenario.worker && (err == nil) != wantSuccess || scenario.worker && wantSuccess && err != nil {
				t.Fatalf("collection success=%v want=%v: %v; task status=%s detail=%s", err == nil, wantSuccess, err, item.Status, item.Detail)
			}
			if scripted != nil {
				if scripted.Calls() != 1 {
					t.Fatalf("work replayed %d times", scripted.Calls())
				}
				wantCalls := 1
				if scenario.entry == "deprecated" {
					wantCalls = 0
				}
				if countEffects() != wantCalls {
					t.Fatalf("side-effect count=%d want %d", countEffects(), wantCalls)
				}
				if !wantSuccess {
					_, _ = c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(item), item.ID)
					if scripted.Calls() != 1 || countEffects() != wantCalls {
						t.Fatal("blocked resume replayed completed work")
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if scenario.entry == "transcript_closed" {
				if len(requests) != 0 || c.todoItemByID(item.ID).Status != TaskBlocked || countEffects() != 1 {
					t.Fatal("sealing failure replayed completed work or dispatched a collector")
				}
				return
			}
			if len(requests) != len(replies) {
				t.Fatalf("model calls=%d want %d", len(requests), len(replies))
			}
			for _, request := range requests {
				if strings.Contains(fmt.Sprint(request["messages"]), resultCollectorSystem) {
					assertCollectorResponseChannel(t, request)
					continue
				}
				if tools, _ := request["tools"].([]any); len(tools) != 0 {
					t.Fatalf("collector exposed tools: %v", tools)
				}
				if choice := request["tool_choice"]; choice != nil && choice != "none" && choice != "auto" {
					t.Fatalf("collector required a tool: %v", choice)
				}
			}
			current := todoByID(t, c, item.ID)
			checkpoint := current.ExecutionReceipt.ResultCollection
			turns := len(replies)
			if scenario.worker && scripted == nil {
				turns--
			}
			if checkpoint == nil {
				t.Fatalf("missing collection checkpoint: task=%+v error=%v", current, err)
			}
			if scenario.fanout {
				if current.Status != TaskDone || len(current.ExecutionReceipts) != 2 {
					t.Fatalf("fanout did not complete: status=%s receipts=%d detail=%s", current.Status, len(current.ExecutionReceipts), current.Detail)
				}
				for _, leaf := range current.ExecutionReceipts {
					if cp := leaf.ResultCollection; cp == nil || cp.State != ResultCollectionCommitted || cp.TurnsUsed != 1 {
						t.Fatalf("invalid fanout checkpoint: %+v", cp)
					}
				}
				return
			}
			if scenario.entry != "collector_tool" {
				usage := checkpoint.ObservedUsage
				if usage == nil || usage.TotalTokens != 15*turns || usage.CacheReadTokens != 4*turns || usage.PromptTokens() != 10*turns {
					t.Fatalf("collection lost complete native usage: %+v", usage)
				}
			}
			if scenario.worker && !wantSuccess && scripted == nil {
				expected := checkpoint.ObservedUsage.TotalTokens
				if usage := current.ExecutionReceipt.Usage; usage != nil {
					if usage.ProgressTokens > 0 {
						expected += usage.ProgressTokens
					} else {
						expected += usage.TotalTokens
					}
				}
				c.metricsMu.Lock()
				observed := c.tokensSinceCriterionProgress
				c.metricsMu.Unlock()
				if observed != int64(expected) {
					t.Fatalf("work/collection accounting duplicated or lost: got %d want %d", observed, expected)
				}
			}
			if checkpoint.TurnsUsed != turns || checkpoint.SourceRunID != current.ExecutionReceipt.RunID || checkpoint.ModelExecutionID != current.ExecutionReceipt.ModelExecutionID {
				t.Fatalf("collection changed work identity or lost its consumed turns: used=%d want=%d model=%s/%s source_run=%s/%s; detail=%s", checkpoint.TurnsUsed, turns, checkpoint.ModelExecutionID, current.ExecutionReceipt.ModelExecutionID, checkpoint.SourceRunID, receipt.RunID, current.Detail)
			}
			wantStatus := TaskInProgress
			if scenario.worker {
				if wantSuccess {
					wantStatus = TaskDone
				} else {
					wantStatus = TaskBlocked
				}
			}
			if current.Status != wantStatus {
				t.Fatalf("status=%s want %s: %v", current.Status, wantStatus, err)
			}
			if wantSuccess && (current.TypedResult == nil || current.TypedResult.Source != "collected" || checkpoint.State != ResultCollectionCommitted) {
				t.Fatalf("result not committed: %+v", current)
			}
			if scenario.entry == "native_response" {
				raw, readErr := c.readCollectionBlob(t.Context(), checkpoint, "response", checkpoint.Response)
				if readErr != nil || string(raw) != replies[len(replies)-1] {
					t.Fatalf("native claim bytes changed or included reasoning: %s %v", raw, readErr)
				}
				requestRaw, readErr := c.readCollectionBlob(t.Context(), checkpoint, "request", checkpoint.Request)
				frozen, decodeErr := decodeFrozenCollectionRequest(requestRaw, checkpoint)
				if readErr != nil || decodeErr != nil || frozen.ClaimsSchema == nil || len(frozen.Tools) != 0 {
					t.Fatalf("response schema was not frozen separately from work tools: %v %v", readErr, decodeErr)
				}
			}
			if scenario.entry == "verbatim" {
				accepted, readErr := c.acceptedCollectionResult(t.Context(), current, checkpoint)
				before, _ := json.Marshal(accepted, json.Deterministic(true))
				after, _ := json.Marshal(current.TypedResult, json.Deterministic(true))
				if readErr != nil || accepted.RawOutputRef == nil || string(before) != string(after) {
					t.Fatalf("verbatim canonical bytes changed after collection: %v\nbefore=%s\nafter=%s", readErr, before, after)
				}
			}
			if !wantSuccess && (current.TypedResult != nil || checkpoint.State != ResultCollectionBlocked) {
				t.Fatal("invalid responses produced an accepted result")
			}
		})
	}
}

func TestResultCollectionTextRequestCompatibility(t *testing.T) {
	for _, scenario := range []string{"historical request", "text-only model"} {
		t.Run(scenario, func(t *testing.T) {
			const claims = `{"status":"success","summary":"preserved legacy work"}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					capabilities := `["completion","tools"]`
					if scenario == "text-only model" {
						capabilities = `["completion"]`
					}
					_, _ = fmt.Fprintf(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":%s}`, capabilities)
					return
				}
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				if exposed, _ := request["tools"].([]any); len(exposed) != 0 {
					t.Error("a historical frozen text request gained a response function")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"legacy\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", claims)
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true)
			checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "complete original work")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "text-only model" {
				var bound providerBoundInvocationContext
				ctx, bound, err = c.resolveProviderBoundInvocationContext(ctx, c.executionModelIDForTarget(checkpoint.ExecutionTarget, receipt.Model), c.session.Agents["worker"])
				if err != nil {
					t.Fatal(err)
				}
				// The original invocation's immutable capability decision wins
				// even if later metadata is unknown or reports tool support.
				bound.SupportsTools = modelprofile.CapabilityNo
				ctx = withProviderBoundInvocationContext(ctx, bound)
			}
			invocation, data, err := c.prepareCollectionInvocation(ctx, c.session.Agents["worker"], checkpoint, "result-collection-legacy", "")
			if err != nil {
				t.Fatal(err)
			}
			var frozen frozenCollectionRequest
			if err := json.Unmarshal(data, &frozen); err != nil {
				t.Fatal(err)
			}
			if scenario == "text-only model" && frozen.ResponseChannel != agent.CollectorResponseTextJSON {
				t.Fatal("text-only model did not retain the text channel")
			}
			if scenario == "historical request" {
				// Historical requests predate both the wire schema and explicit
				// channel metadata. New text requests retain the claims schema.
				frozen.ClaimsSchema = nil
				frozen.ResponseChannel = ""
			}
			data, err = json.Marshal(frozen)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.reserveResultCollection(ctx, checkpoint, frozen.InvocationID, data)
			if err != nil {
				t.Fatal(err)
			}
			actual, _, err := c.runCollectionInvocation(invocation, c.session.Agents["worker"], checkpoint)
			if err != nil || actual != claims {
				t.Fatalf("legacy output changed: %q %v", actual, err)
			}
		})
	}
}
