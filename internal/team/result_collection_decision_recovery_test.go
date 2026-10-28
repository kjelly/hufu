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
	sidecarbackend "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/tools"
)

// Seals a real collection occurrence around a settled decision. The unrelated
// source gap is part of the accepted source payload and remains byte-identical.
func decisionRecoveryFixture(t *testing.T, mutations ...func(map[string]any)) (*Coordinator, string, *ResultCollectionCheckpoint, *atomic.Int32) {
	t.Helper()
	return decisionRecoveryFixtureAtEndpoint(t, "http://127.0.0.1:1/v1", mutations...)
}

func decisionRecoveryFixtureAtEndpoint(t *testing.T, endpoint string, mutations ...func(map[string]any)) (*Coordinator, string, *ResultCollectionCheckpoint, *atomic.Int32) {
	t.Helper()
	f := newBoundDecisionFixture(t, 1, func(root map[string]any) {
		item := root["properties"].(map[string]any)["decisions"].(map[string]any)["items"].(map[string]any)
		item["properties"].(map[string]any)["reason"] = map[string]any{"type": "string"}
		item["allOf"] = []any{map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "reject"}}}, "then": map[string]any{"properties": map[string]any{"reason": map[string]any{"const": ""}}}}}
	})
	f.sources[0].ID = "accepted-source"
	f.sources[0].TypedResult.TaskID = f.sources[0].ID
	calls := new(atomic.Int32)
	c, item, ctx, receipt := resultCollectionTaskFixture(t, endpoint, true, func(spec *TodoSpec) {
		ref := f.c.session.ResultContracts["decisions.json"].ref(true)
		spec.ResultContract = &ref
		spec.EvidenceFrom = []string{f.sources[0].ID}
	}, func(c *Coordinator) {
		c.sidecarModel = "test-sidecar"
		c.session.ResultContracts = f.c.session.ResultContracts
		c.session.Config.DecisionPrimitives = f.c.session.Config.DecisionPrimitives
		c.session.Agents["worker"].Tools = "view,decision_primitive"
		var err error
		c.decisionPrimitives, err = catalog.New(c.session.Config.DecisionPrimitives, catalog.WithSidecarGenerator(func(string) (sidecarbackend.Generator, error) { return countingGenerator{calls: calls}, nil }))
		if err != nil {
			t.Fatal(err)
		}
		c.decisionPrimitiveGate = make(chan struct{}, 1)
		c.taskTracker.TodoList().AddReserved(f.sources)
	})
	// Persist the accepted source in this lineage as well as the in-memory view.
	for _, eventType := range []EventType{EventTaskCreated, EventTaskCompleted} {
		source := cloneTodoItem(f.sources[0])
		if eventType == EventTaskCreated {
			source.Status = TaskPending
		}
		body, err := json.Marshal(source, collectionDurationFormat)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.EventJournal().Append(ctx, RunEvent{Type: string(eventType), RunID: receipt.RunID, BranchID: "main", TaskID: source.ID, Actor: source.Agent, Payload: body}); err != nil {
			t.Fatal(err)
		}
	}
	artifacts, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := artifacts.Put(ctx, PutArtifactRequest{Kind: "test", Path: "source.txt", Content: []byte("original source has a documented local limitation"), TaskID: f.sources[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	scope := &ArtifactAccessScope{TaskID: item.ID, Attempt: 1, AuthorizedRefs: []ArtifactRef{artifact.ArtifactRef}}
	ctx = context.WithValue(ctx, artifactAccessScopeKey, scope)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: receipt.RunID, TaskID: item.ID, AgentName: item.Agent, Attempt: 1, ModelExecutionID: receipt.ModelExecutionID})
	ctx = tools.SetToolsAllowed(ctx, []string{"view", decisionPrimitiveToolName})
	view := tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))
	raw, _ := json.Marshal(map[string]any{"artifact_ref": artifact.ID, "byte_limit": 1024})
	call := fantasy.ToolCall{ID: "source-read", Name: "view", Input: string(raw)}
	response, err := view.Run(ctx, call)
	if err != nil || response.IsError {
		t.Fatalf("view: %v %s", err, response.Content)
	}
	if err = c.captureDecisionArtifactRead(ctx, view, call, &response); err != nil {
		t.Fatal(err)
	}
	plans, err := c.fixedCollectionPlans(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]any{"collection": "/decisions", "row_key": plans[0].rows[0].Key, "group": "assessment"})
	response, err = (&decisionPrimitiveTool{coordinator: c}).Run(ctx, fantasy.ToolCall{ID: "original-decision", Name: decisionPrimitiveToolName, Input: string(raw)})
	if err != nil || response.IsError {
		t.Fatalf("decision: %v %s", err, response.Content)
	}
	receipt.ArtifactScope = scope
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "Original local limitation belongs in summary/source; settled decision is reject.")
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.reserveResultCollection(ctx, cp, "old-collector", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{"status": "success", "summary": "Inherited source limitation retained here", "structured_payload": map[string]any{"decisions": map[string]any{plans[0].rows[0].Key: map[string]any{"reason": "inherited source limitation"}}}}
	for _, mutate := range mutations {
		mutate(claims)
	}
	raw, _ = json.Marshal(claims)
	cp, err = c.recordCollectionResponse(ctx, cp, raw)
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.prepareCollectionClaims(ctx, cp)
	if err != nil {
		t.Fatal(err)
	}
	// Save the historical misclassification, without invoking today's admission.
	cp, err = c.blockResultCollection(ctx, cp, RepairFailureSource)
	if err != nil {
		t.Fatal(err)
	}
	c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureSource, Cause: errors.New("historical conditional-schema rejection")})
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "historical conditional-schema rejection", NeedsHuman, FailureProtocol, TaskBlocked, "")
	return c, item.ID, cp, calls
}

func TestResultCollectionDecisionRecoveryRetainsSourcesAndVerdict(t *testing.T) {
	for _, corruption := range []string{"none", "source", "prepared", "model_override"} {
		t.Run(corruption, func(t *testing.T) {
			c, id, cp, calls := decisionRecoveryFixture(t)
			before := c.dependencyResultsForTask(id)[0].StructuredPayload.clone()
			if corruption == "source" {
				source := c.todoItemByID(c.todoItemByID(id).EvidenceFrom[0])
				source.TypedResult.StructuredPayload.SHA256 = strings.Repeat("a", 64)
				if err := c.taskTracker.TodoList().TryApplyProjectedItem(source); err != nil {
					t.Fatal(err)
				}
			}
			if corruption == "prepared" {
				item := c.todoItemByID(id)
				item.ExecutionReceipt.ResultCollection.Prepared.SHA256 = strings.Repeat("b", 64)
				if err := c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
					t.Fatal(err)
				}
			}
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "source" || corruption == "prepared" {
				if a.Disposition != RecoveryBlocked {
					t.Fatalf("invalid source classified as %s", a.Disposition)
				}
				return
			}
			if a.Disposition != RecoveryRepairClaims || a.AdditionalTurns != 2 {
				t.Fatalf("assessment=%+v", a)
			}
			c.executionRunID = "repair-conditional-claims"
			ctx := WithResultOnlyRepair(t.Context(), id)
			if corruption == "model_override" {
				if err := c.authorizeOperatorResultRepair(WithResultRepairModel(ctx, "ollama/test")); err == nil {
					t.Fatal("model override accepted")
				}
				return
			}
			if err := c.authorizeOperatorResultRepair(ctx); err != nil {
				t.Fatal(err)
			}
			granted := c.todoItemByID(id).ExecutionReceipt.ResultCollection
			if granted.State != ResultCollectionReceived || granted.Prepared != nil || granted.TurnLimit != cp.TurnsUsed+2 {
				t.Fatalf("illegal prepared recovery: %+v", granted)
			}
			grant := c.todoItemByID(id).ExecutionReceipt.RepairProvenance.OperatorGrants[0]
			if grant.Recovery.Checkpoint.Prepared == nil || *grant.Recovery.Checkpoint.Prepared != *cp.Prepared {
				t.Fatal("lost old prepared evidence")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := ReplayTodoList(events)
			if err != nil {
				t.Fatal(err)
			}
			c.taskTracker = NewTaskTracker()
			c.taskTracker.TodoList().AddReserved(tasks)
			c.revokeTaskOccurrence(id)
			ctx, err = c.restoreCollectionContext(WithSessionResume(t.Context()), granted)
			if err != nil {
				t.Fatal(err)
			}
			// A corrected collector response uses the same row and settled decision.
			plans, err := c.fixedCollectionPlans(id)
			if err != nil {
				t.Fatal(err)
			}
			raw := fmt.Sprintf(`{"status":"success","summary":"Inherited source limitation retained here","structured_payload":{"decisions":{%q:{"reason":""}}}}`, plans[0].rows[0].Key)
			granted, err = c.reserveResultCollection(ctx, granted, "repair-collector", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			granted, err = c.recordCollectionResponse(ctx, granted, []byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			granted, err = c.prepareCollectionClaims(ctx, granted)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.commitPreparedCollection(ctx, granted); err != nil {
				t.Fatal(err)
			}
			result := c.GetTaskResult(id)
			if result == nil || !strings.Contains(string(result.StructuredPayload.Value), `"state":"reject"`) || !strings.Contains(result.Summary, "limitation") || calls.Load() != 1 {
				t.Fatal("repair changed settled decision, lost gap, or repeated inference")
			}
			after := c.dependencyResultsForTask(id)[0].StructuredPayload
			if before.SHA256 != after.SHA256 || string(before.Value) != string(after.Value) {
				t.Fatal("repair rewrote accepted source")
			}
		})
	}
}

func TestResultCollectionRecoverySourceFailurePrecedesSchemaFailure(t *testing.T) {
	c, id, _, _ := decisionRecoveryFixture(t, func(claims map[string]any) { claims["receipt_ids"] = []string{"invented-receipt"} })
	a, err := c.AssessResultCollectionRecovery(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Disposition != RecoveryBlocked {
		t.Fatalf("mixed failure became repairable: %+v", a)
	}
	found := false
	for _, check := range a.Checks {
		if check.Code == "receipt_claims" && !check.Passed {
			found = true
		}
	}
	if !found {
		t.Fatalf("receipt failure was hidden by conditional schema: %+v", a.Checks)
	}
}

func TestResultCollectionRecoveryRunsOnlyCollector(t *testing.T) {
	var calls atomic.Int32
	var claims string
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
		if exposed, _ := request["tools"].([]any); len(exposed) != 0 {
			t.Error("recovery collector exposed tools")
		}
		prompt := fmt.Sprint(request["messages"])
		if !strings.Contains(prompt, "format_diagnostic") || !strings.Contains(prompt, "previous_response") || !strings.Contains(prompt, "canonical_item_schema") {
			t.Error("recovery collector lost original claims, diagnostic, or settled contract")
		}
		if calls.Add(1) > 1 {
			t.Error("unexpected second collector call")
			http.Error(w, "extra call", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"recovery\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", claims)
	}))
	t.Cleanup(server.Close)
	c, id, cp, decisions := decisionRecoveryFixtureAtEndpoint(t, server.URL+"/v1")
	plans, err := c.fixedCollectionPlans(id)
	if err != nil {
		t.Fatal(err)
	}
	claims = fmt.Sprintf(`{"status":"success","summary":"Inherited limitation retained here","structured_payload":{"decisions":{%q:{"reason":""}}}}`, plans[0].rows[0].Key)
	c = reopenRecoveryCoordinator(t, c)
	c.executionRunID = "collector-recovery"
	worker := scriptWorker(textTurn("must not execute"))
	c.workerAgentOverride = worker
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
		t.Fatal(err)
	}
	// Reopen after the grant as well; no generator or occurrence survives.
	c = reopenRecoveryCoordinator(t, c)
	c.executionRunID = "collector-recovery"
	c.workerAgentOverride = worker
	item := c.todoItemByID(id)
	if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(item), id); err != nil {
		t.Fatal(err)
	}
	current := c.todoItemByID(id)
	if current.Status != TaskDone || current.ExecutionReceipt.ResultCollection.TurnsUsed != cp.TurnsUsed+1 || calls.Load() != 1 || decisions.Load() != 1 || worker.Calls() != 0 {
		t.Fatalf("status=%s collector=%d decisions=%d worker=%d", current.Status, calls.Load(), decisions.Load(), worker.Calls())
	}
}
