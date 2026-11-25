package team

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func collectionFixtureReceiptSteps() []ExecutionStep {
	return []ExecutionStep{
		{ID: "inspect", Tool: "view", Effect: ExecutionEffectRead},
		{ID: "finish", Tool: "ls", Effect: ExecutionEffectRead, DependsOn: []string{"inspect"}},
	}
}

func bindCollectionFixtureMemory(t *testing.T, c *Coordinator, item *TodoItem, receipt ExecutionReceipt) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(c.session.Workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	record := contextstore.ContextItem{ID: "observed-item", Kind: contextstore.ContextPattern, Content: "observed guidance", Scope: contextstore.Scope{ProjectID: c.contextScopeProjectID()}, Lifecycle: contextstore.LifecycleConfirmed}
	if err := repo.Append(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	records, err := repo.GetMany(t.Context(), []string{record.ID})
	if err != nil || len(records) != 1 {
		t.Fatalf("original context record was not saved: %v", err)
	}
	if err := c.persistMemoryManifest(&MemoryInjectionManifest{
		TaskID: item.ID, RunID: receipt.RunID, Attempt: receipt.Attempt, ModelExecutionID: receipt.ModelExecutionID, Agent: item.Agent,
		RetrievalID: "fixture-retrieval", PolicyVersion: "v1", Items: []MemoryInjectionItem{{ContextItemID: record.ID, ContentHash: records[0].ContentHash}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResultCollectionRecoveryRepairsMemoryFormatForVerifiedSources(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	for _, tc := range []struct {
		name, item, disposition string
		want                    ResultCollectionRecoveryDisposition
	}{
		{"wrong type", "observed-item", `false`, RecoveryRepairClaims},
		{"unknown enum", "observed-item", `"unsupported"`, RecoveryRepairClaims},
		{"forged item", "invented", `false`, RecoveryBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			valid := researchCollectionResponse(t, 2)
			answer := strings.Replace(valid, "{", `{"memory_uses":[{"context_item_id":"observed-item","disposition":"consulted","confidence":0.8}],`, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 262144","capabilities":["completion","tools"]}`)
					return
				}
				calls.Add(1)
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				assertCollectorResponseChannel(t, request)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"memory-repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", `{"result":`+answer+`}`)
			}))
			t.Cleanup(server.Close)
			fields := fmt.Sprintf(`{"memory_uses":[{"context_item_id":%q,"disposition":%s,"confidence":0.8}],`, tc.item, tc.disposition)
			body := strings.Replace(valid, "{", fields, 1)
			c, source, id := contractCollectionFixture(t, body, SideEffectNone, contractCollectionFixtureOptions{AttributableMemory: true, PreserveContract: true, Endpoint: server.URL + "/v1"})
			c = reopenContractCollection(t, c, source)
			cp := currentResultCollection(c.todoItemByID(id))
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil || a.Disposition != tc.want || a.AdditionalTurns != 0 || calls.Load() != 0 || isCollectionSourceFieldRecollection(a) {
				t.Fatalf("unexpected cold preview: %+v, err=%v", a, err)
			}
			ctx := WithResultOnlyRepair(t.Context(), id)
			err = c.authorizeOperatorResultRepair(ctx)
			if tc.want == RecoveryBlocked {
				if err == nil || calls.Load() != 0 {
					t.Fatal("unverified attribution received a correction turn")
				}
				return
			}
			if err != nil || a.RemainingTurns != 1 {
				t.Fatalf("format repair requires one original turn: %+v, %v", a, err)
			}
			c = reopenRecoveryCoordinator(t, c)
			c.executionRunID = "after-memory-format-grant"
			worker := scriptWorker(textTurn("original worker must not rerun"))
			c.workerAgentOverride = worker
			_, err = c.executeTask(ctx, taskDefFromTodoItem(c.todoItemByID(id)), id)
			item := c.todoItemByID(id)
			if err != nil || calls.Load() != 1 || worker.Calls() != 0 || item.Status != TaskDone || currentResultCollection(item).Work != cp.Work || item.TypedResult == nil {
				t.Fatalf("repair did not reuse original work: status=%s collector=%d worker=%d err=%v", item.Status, calls.Load(), worker.Calls(), err)
			}
			if want := (MemoryUseRef{ContextItemID: "observed-item", RetrievalID: "fixture-retrieval", Disposition: MemoryUseConsulted, Confidence: 0.8}); !slices.Equal(item.TypedResult.MemoryUses, []MemoryUseRef{want}) {
				t.Fatalf("result lost original memory attribution: %+v", item.TypedResult.MemoryUses)
			}
			saved, err := c.readCollectionBlob(t.Context(), cp, "response", cp.Response)
			if err != nil || string(saved) != body {
				t.Fatalf("original response changed after repair: %v", err)
			}
		})
	}
}

func TestResultAdmissionRequiresSuccessReceiptsForMalformedStatus(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		wantDirect   RepairFailureReason
		wantCold     ResultCollectionRecoveryDisposition
	}{
		{"unknown", `"unknown"`, RepairFailureSource, RecoveryBlocked},
		{"wrong type", `false`, RepairFailureSource, RecoveryBlocked},
		{"missing", ``, RepairFailureSource, RecoveryBlocked},
		{"duplicate", `"unknown","status":"failed"`, RepairFailureSource, RecoveryBlocked},
		{"uppercase", `"FAILED"`, RepairFailureInvalidSchema, RecoveryBlocked},
		{"surrounding spaces", `" failed "`, RepairFailureInvalidSchema, RecoveryBlocked},
		{"partial", `"partial"`, RepairFailureInvalidSchema, RecoveryRepairClaims},
		{"failed", `"failed"`, RepairFailureInvalidSchema, RecoveryRepairClaims},
		{"blocked", `"blocked"`, RepairFailureInvalidSchema, RecoveryRepairClaims},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := `"summary":["incorrect summary type"]`
			if tc.status != "" {
				fields = `"status":` + tc.status + "," + fields
			}
			t.Run("direct", func(t *testing.T) {
				c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
					spec.Execution.Steps = collectionFixtureReceiptSteps()
				})
				if err := c.recordActualToolReceipt(ctx, item.ID, receipt.Attempt, "first-read", "view", "{}", "observed", false, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				tool := &submitResultTool{coordinator: c, todoID: item.ID}
				response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"receipt_ids":["first-read"],` + fields + `}`})
				if err != nil || !response.IsError || submissionFailureMetadata(response.Metadata) != tc.wantDirect {
					t.Fatalf("direct admission reason=%v, err=%v", submissionFailureMetadata(response.Metadata), err)
				}
			})
			t.Run("cold recovery", func(t *testing.T) {
				body := `{` + fields + `,"structured_payload":{"findings":["a","b"]}}`
				c, source, id := contractCollectionFixture(t, body, SideEffectNone, contractCollectionFixtureOptions{PreserveContract: true, IncompleteReceipts: true})
				c = reopenContractCollection(t, c, source)
				a, err := c.AssessResultCollectionRecovery(t.Context(), id)
				if err != nil || a.Disposition != tc.wantCold || a.AdditionalTurns != 0 {
					t.Fatalf("unexpected cold receipt check: %+v, err=%v", a, err)
				}
				if tc.wantCold == RecoveryBlocked && !slices.ContainsFunc(a.Checks, func(check ResultCollectionRecoveryCheck) bool {
					return !check.Passed && strings.Contains(check.Diagnostic, "finish")
				}) {
					t.Fatalf("missing required step was not the cause: %+v", a.Checks)
				}
			})
		})
	}
}

func TestResultCollectionBindsDurableStepReceipts(t *testing.T) {
	c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
		spec.Execution.Steps = []ExecutionStep{{ID: "inspect", Tool: "view", Effect: ExecutionEffectRead}}
	})
	if err := c.recordActualToolReceipt(ctx, item.ID, receipt.Attempt, "issued-receipt", "view", "{}", "observed", false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "complete original work")
	if err == nil {
		cp, err = c.reserveResultCollection(ctx, cp, "collector", []byte(`{"bound_source_fields":true}`))
	}
	if err == nil {
		cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":"success","summary":"observed"}`))
	}
	if err == nil {
		cp, err = c.prepareCollectionClaims(ctx, cp)
	}
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := c.decodeCollectionClaims(item.ID, []byte(`{"status":"success","summary":"observed","receipt_ids":["issued-receipt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.bindCollectionReceiptFields(ctx, cp, &claimed); localResultRejectionReason(err) != RepairFailureSource {
		t.Fatalf("model override was not rejected: %v", err)
	}
	c = reopenRecoveryCoordinator(t, c)
	if err := c.commitPreparedCollection(WithSessionResume(t.Context()), cp); err != nil {
		t.Fatal(err)
	}
	result := c.GetTaskResult(item.ID)
	if result == nil || !slices.Equal(result.ReceiptIDs, []string{"issued-receipt"}) {
		t.Fatalf("canonical result lost its durable step receipt: %+v", result)
	}
}

func TestResultCollectionSourceFieldsFollowOriginalWork(t *testing.T) {
	c, item, _, cp := beginCollectionFixture(t)
	check := func(wantIDs []string) {
		t.Helper()
		info := collectionSubmissionInfo(c, item.ID)
		if err := c.constrainCollectionSourceFields(cp, info.Parameters); err != nil {
			t.Fatal(err)
		}
		if _, exists := info.Parameters["receipt_ids"]; exists {
			t.Fatal("collector can supply execution receipts")
		}
		memory, offered := info.Parameters["memory_uses"].(map[string]any)
		if offered != (len(wantIDs) != 0) {
			t.Fatalf("memory attribution offered=%t, IDs=%v", offered, wantIDs)
		}
		if offered {
			props := memory["items"].(map[string]any)["properties"].(map[string]any)
			ids := props["context_item_id"].(map[string]any)["enum"].([]string)
			if !slices.Equal(ids, wantIDs) || props["retrieval_id"] != nil {
				t.Fatalf("noncanonical memory schema: %v", props)
			}
		}
	}
	check(nil)
	c.session.Config.MemoryLearning.Mode = agent.MemoryLearningObserve
	for _, source := range []struct{ run, model, id string }{
		{"another-run", cp.ModelExecutionID, "unrelated-run-item"},
		{cp.SourceRunID, "another-model-execution", "unrelated-model-item"},
		{cp.SourceRunID, cp.ModelExecutionID, "observed-item"},
	} {
		err := c.taskTracker.TodoList().SetMemoryManifest(item.ID, &MemoryInjectionManifest{
			TaskID: item.ID, RunID: source.run, Attempt: cp.Attempt, ModelExecutionID: source.model, Agent: item.Agent,
			RetrievalID: "retrieval-" + source.id, Items: []MemoryInjectionItem{{ContextItemID: source.id}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check([]string{"observed-item"})
	repo, err := contextstore.OpenSQLite(filepath.Join(c.session.Workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Append(t.Context(), contextstore.ContextItem{ID: "observed-item", Kind: contextstore.ContextPattern, Content: "observed guidance", Scope: contextstore.Scope{ProjectID: c.contextScopeProjectID()}, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"observed-item", "unrelated-run-item", "unrelated-model-item"} {
		result := TaskResult{MemoryUses: []MemoryUseRef{{ContextItemID: id, Disposition: MemoryUseConsulted, Confidence: 0.8}}}
		err := c.validateCollectedMemoryInput(t.Context(), cp, SubmitResultInput{memoryUsesSupplied: true}, &result)
		if id == "observed-item" {
			if err != nil || result.MemoryUses[0].RetrievalID != "retrieval-observed-item" {
				t.Fatalf("read-only assessment did not bind original memory: %+v, %v", result.MemoryUses, err)
			}
		} else if localResultRejectionReason(err) != RepairFailureSource {
			t.Fatalf("memory from another occurrence was allowed: %v", err)
		}
	}
}

func TestResultCollectionRecollectsInapplicableSourceFields(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	for _, tc := range []struct {
		name, fields    string
		admitted        bool
		badReplacement  string
		collectionCalls int32
	}{
		{"observed artifact in receipt field", `"receipt_ids":["fixture-observed-artifact"],`, true, "", 1},
		{"foreign receipt", `"receipt_ids":["unissued-receipt"],`, false, "", 0},
		{"memory unavailable", `"memory_uses":[{"context_item_id":"current_task","disposition":"applied"}],`, true, "", 1},
		{"memory cannot conceal foreign receipt", `"memory_uses":[{"context_item_id":"current_task","disposition":"applied"}],"receipt_ids":["unissued-receipt"],`, false, "", 0},
		{"repeated inapplicable field", `"memory_uses":[{"context_item_id":"current_task","disposition":"applied","confidence":0.9}],`, true, "memory_uses", 1},
		{"replacement fails a different schema check", `"receipt_ids":["fixture-observed-artifact"],`, true, "confidence", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var answer atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 262144","capabilities":["completion","tools"]}`)
					return
				}
				calls.Add(1)
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				assertCollectorResponseChannel(t, request)
				function := request["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
				claims := function["parameters"].(map[string]any)["properties"].(map[string]any)["result"].(map[string]any)["properties"].(map[string]any)
				if claims["receipt_ids"] != nil || claims["memory_uses"] != nil {
					t.Error("replacement schema offers unavailable sources")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"source-repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", `{"result":`+answer.Load().(string)+`}`)
			}))
			t.Cleanup(server.Close)
			body := strings.Replace(researchCollectionResponse(t, 2), "{", "{"+tc.fields, 1)
			c, source, id := contractCollectionFixture(t, body, SideEffectNone, contractCollectionFixtureOptions{ArtifactKeys: true, Endpoint: server.URL + "/v1", PreserveContract: true})
			cp := currentResultCollection(todoItemByID(source.Session.Tasks, id))
			original, err := c.readCollectionBlob(t.Context(), cp, "response", cp.Response)
			if err != nil {
				t.Fatal(err)
			}
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil || isCollectionSourceFieldRecollection(a) != tc.admitted || calls.Load() != 0 {
				t.Fatalf("preview eligibility=%+v, err=%v", a, err)
			}
			if !tc.admitted {
				if a.Disposition != RecoveryBlocked || a.AdditionalTurns != 0 {
					t.Fatalf("unknown source earned model turns: %+v", a)
				}
				return
			}
			var replacement map[string]jsontext.Value
			if err := json.Unmarshal(original, &replacement); err != nil {
				t.Fatal(err)
			}
			if tc.badReplacement != "memory_uses" {
				delete(replacement, "memory_uses")
			}
			delete(replacement, "receipt_ids")
			if tc.badReplacement == "confidence" {
				replacement["confidence"] = jsontext.Value(`"uncertain"`)
			}
			raw, err := json.Marshal(replacement)
			if err != nil {
				t.Fatal(err)
			}
			answer.Store(string(raw))
			c.executionRunID = "source-field-repair"
			ctx := WithResultOnlyRepair(t.Context(), id)
			if err := c.authorizeOperatorResultRepair(ctx); err != nil {
				t.Fatal(err)
			}
			c = reopenRecoveryCoordinator(t, c)
			c.executionRunID = "after-source-field-grant"
			worker := scriptWorker(textTurn("must not replay saved work"))
			c.workerAgentOverride = worker
			_, err = c.executeTask(ctx, taskDefFromTodoItem(c.todoItemByID(id)), id)
			item := c.todoItemByID(id)
			if calls.Load() != tc.collectionCalls || worker.Calls() != 0 || (err != nil) != (tc.badReplacement != "") || currentResultCollection(item).Work != cp.Work {
				t.Fatalf("retry changed work or exceeded its budget: calls=%d worker=%d status=%s, err=%v", calls.Load(), worker.Calls(), item.Status, err)
			}
			saved, err := c.readCollectionBlob(t.Context(), cp, "response", cp.Response)
			if err != nil || !reflect.DeepEqual(saved, original) {
				t.Fatalf("original response was lost or changed: %v", err)
			}
			if tc.badReplacement != "" {
				again, err := c.AssessResultCollectionRecovery(t.Context(), id)
				if err != nil || again.Disposition != RecoveryBlocked || again.AdditionalTurns != 0 {
					t.Fatalf("second source-field cycle granted: %+v, err %v", again, err)
				}
			} else if item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.StructuredPayload == nil {
				t.Fatalf("replacement did not pass task verification: %s", item.Status)
			}
		})
	}
}
