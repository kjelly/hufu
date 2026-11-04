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
)

const collectedNotesSchema = `{"type":"object","additionalProperties":false,"required":["notes"],"properties":{"notes":{"type":"array","items":{"type":"string"}}}}`

func collectedStatusClaims(status, payload string) string {
	return fmt.Sprintf(`{"status":%q,"summary":"review complete","findings":[{"summary":"observed finding","severity":"info"}],"structured_payload":%s}`, status, payload)
}

func TestResultCollectionRepairsStatusFromPayloadItems(t *testing.T) {
	for _, tc := range []struct {
		name, notes, first, second string
		calls                      int
		completed                  bool
	}{
		{"nonempty notes", `["missing source","unexecuted probe"]`, "success", "completed_with_gaps", 2, true},
		{"empty notes", `[]`, "completed_with_gaps", "success", 2, true},
		{"consistent first response", `["missing source"]`, "completed_with_gaps", "", 1, true},
		{"exhausted repair budget", `["missing source"]`, "success", "success", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"notes":` + tc.notes + `}`
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
					return
				}
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				assertCollectorResponseChannel(t, request)
				n := calls.Add(1)
				if n > int32(tc.calls) {
					t.Error("collection exceeded authorized turn budget")
					http.Error(w, "extra turn", http.StatusBadRequest)
					return
				}
				status := tc.first
				if n == 2 {
					status = tc.second
					prompt := fmt.Sprint(request["messages"])
					for _, expected := range []string{"equals_by_items", "configured ", "preserve every array item", "previous_response"} {
						if !strings.Contains(prompt, expected) {
							t.Errorf("repair lost declared consistency diagnostic: %s", expected)
						}
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"status\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", collectedStatusClaims(status, payload))
			}))
			t.Cleanup(server.Close)
			compiled, err := CompileResultContractDocument("notes.json", []byte(collectedNotesSchema))
			if err != nil {
				t.Fatal(err)
			}
			c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true)}
			})
			worker := scriptWorker(textTurn("Observed finding and bounded evidence limitations."))
			c.workerAgentOverride = worker
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "review", Agent: "worker", Model: "ollama/test", Goal: "Review findings", SideEffect: SideEffectNone,
				Execution:  ExecutionContract{RequiresResult: true, ForbidArtifacts: true},
				VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{itemsStatusAssertion("/structured_payload/value/notes")}}}})
			item := c.taskTracker.TodoList().Items()[1]
			if (err == nil) != tc.completed || worker.Calls() != 1 || calls.Load() != int32(tc.calls) || currentResultCollection(item).TurnsUsed != tc.calls {
				t.Fatalf("work=%d collection=%d status=%s err=%v", worker.Calls(), calls.Load(), item.Status, err)
			}
			events, readErr := c.EventJournal().ReadEvents(t.Context())
			if readErr != nil {
				t.Fatal(readErr)
			}
			replayed := ReduceToTodoList(events)[1]
			if !tc.completed {
				if item.Status != TaskBlocked || item.TypedResult != nil || replayed.TypedResult != nil || currentResultCollection(item).State != ResultCollectionBlocked {
					t.Fatal("inconsistent result was accepted after budget exhaustion")
				}
				return
			}
			status := tc.first
			if tc.calls == 2 {
				status = tc.second
			}
			for _, candidate := range []*TodoItem{item, replayed} {
				if candidate.Status != TaskDone || candidate.TypedResult == nil || candidate.TypedResult.Status != status ||
					string(candidate.TypedResult.StructuredPayload.Value) != payload || len(candidate.TypedResult.Findings) != 1 || candidate.TypedResult.Findings[0].Summary != "observed finding" {
					t.Fatal("status repair changed work, findings, payload notes or replay")
				}
			}
		})
	}
}

func TestCollectionItemsProjectionRepairKeepsAuthorityBoundaries(t *testing.T) {
	for _, variant := range []string{"status", "summary", "runtime_source", "failed_outcome", "artifact", "acceptance_failure"} {
		t.Run(variant, func(t *testing.T) {
			assertion := itemsStatusAssertion("/structured_payload/value/notes")
			result := &TaskResult{Status: TaskResultStatusSuccess, Summary: "reviewed", StructuredPayload: &ResultPayload{Value: []byte(`{"notes":["missing source"]}`)}}
			switch variant {
			case "summary":
				assertion.Pointer = "/summary"
			case "runtime_source":
				assertion.Value.(map[string]any)["pointer"] = "/outputs/notes"
			case "failed_outcome":
				assertion.Value.(map[string]any)["non_empty"] = "failed"
			case "artifact":
				result.Artifacts = []ArtifactRef{{ID: "materialized"}}
			}
			assertions := []TaskResultAssertion{assertion}
			if variant == "acceptance_failure" {
				assertions = append(assertions, TaskResultAssertion{Pointer: "/summary", Op: "equals", Value: "required acceptance"})
			}
			if got := collectionProjectionFormatFailure(taskResultSubmissionContract{SubmissionTaskResultAssertions: assertions}, result); got != (variant == "status") {
				t.Fatalf("repair eligibility %s = %t", variant, got)
			}
		})
	}
}

func TestResultCollectionPreservesUnsettledDecisionOutcome(t *testing.T) {
	for _, tc := range []struct {
		name           string
		count, settled int
		fabricate      bool
	}{
		{name: "missing decision", count: 1},
		{name: "partially settled", count: 2, settled: 1},
		{name: "empty collection"},
		{name: "settled decision", count: 1, settled: 1},
		{name: "fabricated success", count: 1, fabricate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsettled := tc.settled < tc.count
			var collectorCalls atomic.Int32
			var response string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
					return
				}
				collectorCalls.Add(1)
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				assertCollectorResponseChannel(t, request)
				prompt := fmt.Sprint(request["messages"])
				if unsettled {
					for _, want := range []string{"Bound decision failure", "decision_missing:", "OMIT structured_payload", `"status":{"enum":["blocked"],"type":"string"}`} {
						if !strings.Contains(prompt, want) {
							t.Errorf("collector lacks runtime outcome guidance: %s", want)
						}
					}
				} else if strings.Contains(prompt, "Bound decision failure") {
					t.Error("settled or empty collection was incorrectly blocked")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"outcome\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", response)
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt, decisionCalls := decisionCollectionFixture(t, server.URL+"/v1", tc.count, tc.settled)
			worker := scriptWorker(textTurn("must never execute"))
			c.workerAgentOverride = worker
			source := c.dependencyResultsForTask(item.ID)[0].StructuredPayload.clone()
			plans, err := c.fixedCollectionPlans(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			report := "All required decisions settled."
			response = `{"status":"success","summary":"settled assessment","structured_payload":` + fixedCollectionSubmission(t, plans[0], func(rows map[string]any) {
				for _, row := range rows {
					delete(row.(map[string]any), "state")
				}
			}) + `}`
			if unsettled {
				report = "Blocked: decision_primitive rejected the arguments; no complete settled assessment exists."
				if !tc.fabricate {
					response = `{"status":"blocked","summary":"Required decision did not settle"}`
				}
			}
			cp, err := c.beginResultCollection(ctx, item.ID, receipt, report)
			if err != nil {
				t.Fatal(err)
			}
			err = c.collectWorkResult(ctx, c.session.Agents["worker"], cp)
			if (err != nil) != tc.fabricate || collectorCalls.Load() != 1 || decisionCalls.Load() != int32(tc.settled) || worker.Calls() != 0 {
				t.Fatalf("collection=%d decisions=%d work=%d err=%v", collectorCalls.Load(), decisionCalls.Load(), worker.Calls(), err)
			}
			current := c.todoItemByID(item.ID)
			if currentResultCollection(current).Work != cp.Work || current.ExecutionReceipt.TranscriptRef != receipt.TranscriptRef || string(c.dependencyResultsForTask(item.ID)[0].StructuredPayload.Value) != string(source.Value) {
				t.Fatal("collection changed the original work, transcript or accepted source")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range []*TodoItem{current, todoItemByID(ReduceToTodoList(events), item.ID)} {
				if tc.fabricate {
					if candidate.TypedResult != nil || currentResultCollection(candidate).State != ResultCollectionBlocked {
						t.Fatal("fabricated success reached canonical state or replay")
					}
					continue
				}
				result := candidate.TypedResult
				if result == nil || (result.Status == TaskResultStatusBlocked) != unsettled || (result.StructuredPayload == nil) != unsettled || currentResultCollection(candidate).State != ResultCollectionCommitted {
					t.Fatal("collection or replay changed the verified decision outcome")
				}
				if !unsettled && len(result.StructuredPayload.CollectionBindings[0].Decisions) != tc.settled {
					t.Fatal("accepted result lost settled decision provenance")
				}
			}
		})
	}
}

func TestResultCollectionResumesStatusRepairWithOriginalBudget(t *testing.T) {
	const payload = `{"notes":["missing source","unexecuted probe"]}`
	for _, state := range []ResultCollectionState{ResultCollectionPrepared, ResultCollectionReceived} {
		t.Run(string(state), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"status\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", collectedStatusClaims("completed_with_gaps", payload))
			}))
			t.Cleanup(server.Close)
			compiled, err := CompileResultContractDocument("notes.json", []byte(collectedNotesSchema))
			if err != nil {
				t.Fatal(err)
			}
			c, item, ctx, receipt := resultCollectionTaskFixture(t, server.URL+"/v1", true, func(spec *TodoSpec) {
				spec.ResultContract = new(compiled.ref(true))
				spec.VerifySpec = &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{itemsStatusAssertion("/structured_payload/value/notes")}}
			}, func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			})
			checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "observed finding and limitations")
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.reserveResultCollection(ctx, checkpoint, "first-collector", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, []byte(collectedStatusClaims("success", payload)))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.prepareCollectionClaims(ctx, checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.commitPreparedCollection(ctx, checkpoint); err == nil || localResultRejectionReason(err) != RepairFailureInvalidSchema || item.TypedResult != nil {
				t.Fatalf("inconsistent result reached publication: %v", err)
			}
			if state == ResultCollectionReceived {
				if _, err = c.recordCollectionFormatFailure(ctx, checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			c.revokeTaskOccurrence(item.ID)
			c.taskTracker = NewTaskTracker()
			c.taskTracker.TodoList().AddReserved(ReduceToTodoList(events))
			worker := scriptWorker(textTurn("must never execute"))
			c.workerAgentOverride = worker
			restored := c.todoItemByID(item.ID)
			if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(restored), item.ID); err != nil {
				t.Fatal(err)
			}
			current := c.todoItemByID(item.ID)
			if worker.Calls() != 0 || calls.Load() != 1 || current.Status != TaskDone || currentResultCollection(current).TurnsUsed != 2 || currentResultCollection(current).Work != checkpoint.Work ||
				current.TypedResult.Status != TaskResultStatusCompletedWithGaps || string(current.TypedResult.StructuredPayload.Value) != payload {
				t.Fatal("resume replayed work, reset budget, or changed frozen evidence")
			}
		})
	}
}
