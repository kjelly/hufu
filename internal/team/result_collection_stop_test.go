package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

func TestResultCollectionPreservesWorkAfterToolLoop(t *testing.T) {
	for _, scenario := range []string{"collected", "successful_loop", "repair", "invalid", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			var collections atomic.Int32
			var recovering atomic.Bool
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
				if messages := fmt.Sprint(request["messages"]); !strings.Contains(messages, "Work stopped by loop protection") || !strings.Contains(messages, "retained observation") {
					t.Error("collector lost the work stop or original evidence")
				}
				n := collections.Add(1)
				body := `{"status":"completed_with_gaps","summary":"retained observation; repeated source access stopped"}`
				if scenario == "invalid" && !recovering.Load() {
					body = `{"status":`
				} else if scenario == "repair" && n == 1 {
					body = `{"status":"success","summary":"must reject unqualified success"}`
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
			}))
			t.Cleanup(server.Close)
			c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true)
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			worker := scriptWorker(scriptedTurn{stream: func(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				limit := 3
				if scenario == "successful_loop" {
					limit = 4
				}
				for i := range limit {
					id := fmt.Sprintf("call-%d", i)
					if err := call.OnToolCall(fantasy.ToolCallContent{ToolCallID: id, ToolName: "use_dynamic_tool", Input: `{"action":"call","target":"browser_snapshot","arguments":{}}`}); err != nil {
						if scenario == "cancelled" {
							cancel()
						}
						return textAgentResult("retained observation"), err
					}
					var result fantasy.ToolResultOutputContent = fantasy.ToolResultOutputContentError{Error: errors.New("source not readable")}
					if scenario == "successful_loop" {
						result = fantasy.ToolResultOutputContentText{Text: `{"status":"source_unavailable","citable":false,"exhausted":true}`}
					}
					if err := call.OnToolResult(fantasy.ToolResultContent{ToolCallID: id, ToolName: "use_dynamic_tool", Result: result}); err != nil {
						return nil, err
					}
				}
				return nil, errors.New("loop was not stopped")
			}})
			c.workerAgentOverride = worker
			_, runErr := c.ExecuteTasks(withTestAuxiliaryInvocationContext(ctx), []TaskDef{{Agent: "worker", Goal: "Collect research with accurate gaps", SideEffect: SideEffectBrowserInteraction, Recovery: RecoveryManual, Execution: ExecutionContract{RequiresResult: true}}})
			items := c.taskTracker.TodoList().Items()
			item := items[len(items)-1]
			cp := currentResultCollection(item)
			if worker.Calls() != 1 {
				t.Fatalf("work was replayed: %d", worker.Calls())
			}
			if scenario == "cancelled" {
				if runErr == nil || cp != nil || collections.Load() != 0 {
					t.Fatal("cancelled work started collection")
				}
				return
			}
			if cp == nil || item.ExecutionReceipt.WorkStopReason != workStopToolLoop {
				t.Fatalf("stopped work lost its collection checkpoint: err=%v detail=%s", runErr, item.Detail)
			}
			work, err := c.loadCollectionWork(t.Context(), item, cp)
			if err != nil || work.StopReason != workStopToolLoop || work.Receipt.ExitCode == nil || *work.Receipt.ExitCode != 1 {
				t.Fatalf("original work receipt was rewritten: work=%+v err=%v", work, err)
			}
			if scenario == "invalid" {
				if runErr == nil || item.Status != TaskBlocked || cp.TurnsUsed != resultCollectionDefaultTurns {
					t.Fatalf("invalid collection did not retain bounded repair: %v %+v", runErr, cp)
				}
				c = reopenRecoveryCoordinator(t, c)
				a, err := c.AssessResultCollectionRecovery(t.Context(), item.ID)
				if err != nil || a.Disposition == RecoveryBlocked || c.todoItemByID(item.ID).ExecutionReceipt.TranscriptRef != work.Receipt.TranscriptRef {
					t.Fatalf("restart lost result-only recovery: %+v err=%v", a, err)
				}
				recovering.Store(true)
				c.executionRunID = "result-only-recovery"
				if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err != nil {
					t.Fatal(err)
				}
				forbidden := scriptWorker(failTurn(errors.New("worker must not run during result repair")))
				c.workerAgentOverride = forbidden
				if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(item.ID)), item.ID); err != nil {
					t.Fatal(err)
				}
				repaired := c.todoItemByID(item.ID)
				if forbidden.Calls() != 0 || repaired.Status != TaskDone || !repaired.ExecutionReceipt.Succeeded() || currentResultCollection(repaired).Work != cp.Work {
					t.Fatal("result-only repair replayed work or replaced its sealed evidence")
				}
				return
			}
			if runErr != nil || item.Status != TaskDone || !item.ExecutionReceipt.Succeeded() || item.TypedResult.Status != TaskResultStatusCompletedWithGaps {
				t.Fatalf("valid gaps could not finish: %v status=%s", runErr, item.Status)
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			replayed := todoItemByID(ReduceToTodoList(events), item.ID)
			if replayed == nil || replayed.ExecutionReceipt.WorkStopReason != workStopToolLoop || replayed.TypedResult.Status != TaskResultStatusCompletedWithGaps {
				t.Fatal("replay lost the stopped work or accepted gaps")
			}
		})
	}
}

func TestResultCollectionToolLoopEligibilityPreservesHardFailures(t *testing.T) {
	for _, scenario := range []string{"read_only", "browser", "mutation", "sequence", "denial", "cancelled", "protocol", "storage"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			task := TaskDef{SideEffect: SideEffectNone}
			var err error = newToolLoopError("worker", "snapshot", 2)
			dispositions := &attemptToolDispositions{}
			switch scenario {
			case "browser":
				task.SideEffect = SideEffectBrowserInteraction
			case "mutation":
				task.SideEffect = SideEffectExternalWrite
			case "sequence":
				task.Execution.ToolSequence = []string{"snapshot"}
			case "denial":
				dispositions.add(ToolExecutionDisposition{Kind: ToolExecutionPolicyDenied})
			case "cancelled":
				cancel()
			case "protocol":
				err = newToolLoopError("worker", submitResultToolName, 2)
			case "storage":
				err = errors.New("cannot preserve transcript")
			}
			if got, want := collectableToolLoop(ctx, err, task, dispositions), scenario == "read_only" || scenario == "browser"; got != want {
				t.Fatalf("eligibility=%t want %t", got, want)
			}
		})
	}
}
