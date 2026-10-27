package team

import (
	"context"
	"encoding/json/v2"

	"charm.land/fantasy"
	"fmt"
	"github.com/kjelly/hufu/internal/tools"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestResultCollectorUsesToolFreeRequestsAndBoundedFormatRepair(t *testing.T) {
	for _, scenario := range []struct {
		replies []string
		worker  bool
		entry   string
		fanout  bool
	}{
		{replies: []string{`{"status":"success","summary":"analysis complete"}`}},
		{replies: []string{`invalid JSON`, `{"status":"success","summary":"analysis complete"}`}},
		{replies: []string{`{"status":"success","summary":""}`, `{"status":"success","summary":"analysis complete"}`}},
		{replies: []string{`invalid JSON`, `still invalid JSON`}},
		{entry: "collector_tool", replies: []string{"call_tool", `{"status":"success","summary":"analysis complete"}`}},
		{worker: true, replies: []string{`complete work report`, `{"status":"success","summary":"analysis complete"}`}},
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
					_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "analysis", Agent: "worker", Model: "ollama/test", Goal: "prepare a complete analysis report for this invocation", OutputMode: func() string {
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
