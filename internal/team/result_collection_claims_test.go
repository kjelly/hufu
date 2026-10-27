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

const collectedFindingSchema = `{"type":"object","additionalProperties":false,"required":["findings"],"properties":{"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["summary"],"properties":{"summary":{"type":"string"}}}}}}`

func TestResultCollectionRepairsClaimsWithoutRepeatingWork(t *testing.T) {
	valid := `{"status":"success","summary":"review complete","findings":[{"summary":"observed finding"}],"structured_payload":{"findings":[{"summary":"observed finding"}]}}`
	mismatch := strings.Replace(valid, `"findings":[{"summary":"observed finding"}]`, `"findings":[{"summary":"different wording"}]`, 1)
	for _, tc := range []struct {
		name            string
		first           string
		second          string
		contractFailure bool
	}{
		{name: "empty_response", second: valid},
		{name: "ordinary_invariant_field", first: strings.Replace(valid, `"status"`, `"invariant_assessments":[],"status"`, 1), second: valid},
		{name: "unknown_field", first: strings.Replace(valid, `"status"`, `"assumptions":[],"status"`, 1), second: valid},
		{name: "projection_mismatch", first: mismatch, second: valid},
		{name: "projection_budget_exhausted", first: mismatch, second: mismatch},
		{name: "acceptance_failure", first: valid, contractFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				if exposed, _ := request["tools"].([]any); len(exposed) != 0 {
					t.Error("collector exposed tools")
				}
				n := calls.Add(1)
				if n > 2 || (tc.contractFailure && n > 1) {
					t.Error("unexpected repair turn")
					http.Error(w, "extra call", 400)
					return
				}
				body := tc.first
				if n == 2 {
					body = tc.second
					if prompt := fmt.Sprint(request["messages"]); !strings.Contains(prompt, "format_diagnostic") || (tc.first != "" && !strings.Contains(prompt, "previous_response")) {
						t.Error("repair lost rejected claims or diagnostic")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"claims\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
			}))
			t.Cleanup(server.Close)
			compiled, err := CompileResultContractDocument("findings.json", []byte(collectedFindingSchema))
			if err != nil {
				t.Fatal(err)
			}
			c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true)}
			})
			worker := scriptWorker(textTurn("Review complete: observed finding."))
			c.workerAgentOverride = worker
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			assertions := []TaskResultAssertion{{Pointer: "/findings", Op: "equals_projection", Value: map[string]any{"pointer": "/structured_payload/value/findings", "fields": []string{"summary"}}}}
			if tc.contractFailure {
				assertions = append(assertions, TaskResultAssertion{Pointer: "/summary", Op: "equals", Value: "required acceptance"})
			}
			_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "review", Agent: "worker", Model: "ollama/test", Goal: "Review findings", SideEffect: SideEffectNone,
				Execution: ExecutionContract{RequiresResult: true, ForbidArtifacts: true}, VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: assertions}}})
			item := c.taskTracker.TodoList().Items()[1]
			wantSuccess := tc.second == valid
			if (err == nil) != wantSuccess {
				t.Fatalf("status=%s detail=%s error=%v", item.Status, item.Detail, err)
			}
			wantCalls := int32(2)
			if tc.contractFailure {
				wantCalls = 1
			}
			checkpoint := currentResultCollection(item)
			if worker.Calls() != 1 || calls.Load() != wantCalls || checkpoint == nil || checkpoint.TurnsUsed != int(wantCalls) {
				t.Fatalf("work=%d collection=%d checkpoint=%+v", worker.Calls(), calls.Load(), checkpoint)
			}
			if wantSuccess && (item.Status != TaskDone || checkpoint.State != ResultCollectionCommitted) {
				t.Fatal("repaired claims did not complete")
			}
			if !wantSuccess && (item.TypedResult != nil || checkpoint.State != ResultCollectionBlocked) {
				t.Fatal("rejected claims published success")
			}
			events, readErr := c.EventJournal().ReadEvents(t.Context())
			if readErr != nil {
				t.Fatal(readErr)
			}
			replayed := ReduceToTodoList(events)[1]
			if replayed.Status != item.Status || currentResultCollection(replayed).TurnsUsed != checkpoint.TurnsUsed {
				t.Fatal("replay changed completion or budget")
			}
		})
	}
}

func TestResultCollectionSchemaProjectsOrdinaryInvariantClaims(t *testing.T) {
	c, item, _, _ := resultCollectionFixture(t)
	if _, exists := collectionSubmissionInfo(c, item.ID).Parameters["invariant_assessments"]; exists {
		t.Fatal("ordinary task advertises forbidden claims")
	}
	// Preserve the tool transport's legacy schema for existing sessions.
	if _, exists := localResultSubmissionInfo(c, item.ID).Parameters["invariant_assessments"]; !exists {
		t.Fatal("legacy schema changed")
	}
	items := c.taskTracker.TodoList().Items()
	items[0].InvariantVerification = InvariantVerificationMode("report")
	c.taskTracker.TodoList().Restore(items)
	info := collectionSubmissionInfo(c, item.ID)
	if _, exists := info.Parameters["invariant_assessments"]; !exists {
		t.Fatal("invariant task lost required claims")
	}
}

func TestResultCollectionResumesProjectionRepairWithOriginalBudget(t *testing.T) {
	for _, state := range []ResultCollectionState{ResultCollectionPrepared, ResultCollectionReceived} {
		t.Run(string(state), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
					return
				}
				calls.Add(1)
				body := `{"status":"success","summary":"review complete","findings":[{"summary":"observed"}],"structured_payload":{"findings":[{"summary":"observed"}]}}`
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
			}))
			t.Cleanup(server.Close)
			compiled, err := CompileResultContractDocument("findings.json", []byte(collectedFindingSchema))
			if err != nil {
				t.Fatal(err)
			}
			c, item, ctx, receipt := resultCollectionTaskFixture(t, server.URL+"/v1", true, func(spec *TodoSpec) {
				spec.ResultContract = new(compiled.ref(true))
				spec.VerifySpec = &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{Pointer: "/findings", Op: "equals_projection", Value: map[string]any{"pointer": "/structured_payload/value/findings", "fields": []string{"summary"}}}}}
			}, func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			})
			checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "observed finding")
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.reserveResultCollection(ctx, checkpoint, "first-collector", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, []byte(`{"status":"success","summary":"review complete","findings":[{"summary":"different"}],"structured_payload":{"findings":[{"summary":"observed"}]}}`))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.prepareCollectionClaims(ctx, checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.commitPreparedCollection(ctx, checkpoint); err == nil || localResultRejectionReason(err) != RepairFailureInvalidSchema {
				t.Fatalf("unexpected admission error: %v", err)
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
			_, err = c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(restored), item.ID)
			if err != nil {
				t.Fatal(err)
			}
			current := c.todoItemByID(item.ID)
			if worker.Calls() != 0 || calls.Load() != 1 || current.Status != TaskDone || currentResultCollection(current).TurnsUsed != 2 || currentResultCollection(current).Work != checkpoint.Work {
				t.Fatal("resume replayed work, reset budget, or lost completion")
			}
		})
	}
}

func TestCollectionProjectionFormatFailureKeepsContractBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pointer      string
		source       string
		artifact     bool
		otherFailure bool
		want         bool
	}{
		{name: "findings", pointer: "/findings", source: "/structured_payload/value/findings", want: true},
		{name: "invariants", pointer: "/invariant_verification/assessments", source: "/structured_payload/value/findings", want: true},
		{name: "runtime_output", pointer: "/outputs", source: "/structured_payload/value/findings"},
		{name: "receipt_source", pointer: "/findings", source: "/outputs/findings"},
		{name: "materialized_artifact", pointer: "/findings", source: "/structured_payload/value/findings", artifact: true},
		{name: "other_contract_failure", pointer: "/findings", source: "/structured_payload/value/findings", otherFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := &TaskResult{Status: TaskResultStatusSuccess, Summary: "reviewed", Findings: []Finding{{Summary: "outer"}}, InvariantVerification: &InvariantVerificationResult{Assessments: []InvariantAssessment{{Summary: "outer"}}}, StructuredPayload: &ResultPayload{Value: []byte(`{"findings":[{"summary":"inner"}]}`)}}
			if tc.artifact {
				result.Artifacts = []ArtifactRef{{ID: "materialized"}}
			}
			assertions := []TaskResultAssertion{{Pointer: tc.pointer, Op: "equals_projection", Value: map[string]any{"pointer": tc.source, "fields": []string{"summary"}}}}
			if tc.otherFailure {
				assertions = append(assertions, TaskResultAssertion{Pointer: "/summary", Op: "equals", Value: "required acceptance"})
			}
			contract := taskResultSubmissionContract{SubmissionTaskResultAssertions: assertions}
			if got := collectionProjectionFormatFailure(contract, result); got != tc.want {
				t.Fatalf("repairable=%t, want %t", got, tc.want)
			}
		})
	}
}
