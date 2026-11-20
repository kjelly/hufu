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

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

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
		{"replacement fails a different schema check", `"receipt_ids":["fixture-observed-artifact"],`, true, "confidence", 2},
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
