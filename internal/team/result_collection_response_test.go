package team

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

const collectionReferenceSchema = `{
	"type":"object","required":["references"],
	"properties":{"references":{"type":"array","items":{"type":"object","required":["producer","paths"],"properties":{"producer":{"type":"string"},"paths":{"type":"array","items":{"type":"string"}}}}}},
	"x-hufu-evidence-references":{"items_pointer":"/references","task_id_pointer":"/producer","references_pointer":"/paths"}
}`

func TestResultCollectionAuthenticatesPayloadBeforeRepairingEnvelopeFields(t *testing.T) {
	const foreign = `{"producer":"outside","paths":["source.go"]}`
	for _, tc := range []struct {
		name, summary, payload  string
		projected, runtimeBound bool
		want                    RepairFailureReason
		turns                   int32
	}{
		{"malformed DTO and foreign producer", `false`, `{"references":[` + foreign + `]}`, false, false, RepairFailureSource, 1},
		{"empty summary and foreign producer", `""`, `{"references":[` + foreign + `]}`, false, false, RepairFailureSource, 1},
		{"invalid projection and foreign producer", `"saved report"`, `{"references":[` + foreign + `]}`, true, false, RepairFailureSource, 1},
		{"foreign producer after malformed entry", `"saved report"`, `{"references":[false,` + foreign + `]}`, false, false, RepairFailureSource, 1},
		{"runtime input override and foreign producer", `"saved report"`, `{"identity":"forged","references":[` + foreign + `]}`, false, true, RepairFailureSource, 1},
		{"malformed DTO with no references", `false`, `{"references":[]}`, false, false, RepairFailureInvalidSchema, 2},
		{"unreadable payload", `"saved report"`, `false`, false, false, RepairFailureInvalidSchema, 2},
		{"runtime input override with no references", `"saved report"`, `{"identity":"forged","references":[]}`, false, true, RepairFailureInvalidSchema, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const repaired = `{"status":"success","summary":"saved report","structured_payload":{"references":[],"findings":[]}}`
			wire := `{"status":"success","summary":` + tc.summary + `,"structured_payload":` + tc.payload + `}`
			schema := collectionReferenceSchema
			if tc.projected {
				schema = strings.Replace(schema, `"properties":{"references"`, `"properties":{"findings":{"type":"array","maxItems":16,"items":{"type":"object"}},"references"`, 1)
				schema = strings.Replace(schema, `"x-hufu-evidence-references":`, `"x-hufu-result-projections":{"findings":"/findings"},"x-hufu-evidence-references":`, 1)
			}
			if tc.runtimeBound {
				schema = strings.Replace(schema, `"properties":{"references"`, `"properties":{"identity":{"type":"string","const":"verified"},"references"`, 1)
				schema = strings.Replace(schema, `"x-hufu-evidence-references":`, `"x-hufu-runtime-inputs":[{"target":"/identity","source_kind":"const"}],"x-hufu-evidence-references":`, 1)
			}
			compiled := compileFixtureJSONSchema(t, "collection-references", schema)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
					return
				}
				answer := wire
				if calls.Add(1) > 1 {
					answer = repaired
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", `{"result":`+answer+`}`)
			}))
			t.Cleanup(server.Close)
			configure := func(spec *TodoSpec) {
				ref := compiled.ref(true)
				spec.ResultContract = &ref
			}
			install := func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			}
			c, item, ctx, receipt := resultCollectionTaskFixture(t, server.URL+"/v1", true, configure, install)
			err := c.collectReturnedWork(ctx, c.session.Agents[item.Agent], receipt, "saved report", "original task")
			if tc.want == RepairFailureSource {
				if failure, ok := errors.AsType[*resultCollectionFailure](err); !ok || failure.Reason != RepairFailureSource ||
					failure.Checkpoint.TurnsUsed != 1 || !strings.Contains(err.Error(), "evidence_from") {
					t.Fatalf("collector did not reject the unauthorized source before repair: %v", err)
				}
			} else if err != nil || c.GetTaskResult(item.ID) == nil {
				t.Fatalf("ordinary format correction did not complete: %v", err)
			}
			if calls.Load() != tc.turns {
				t.Fatalf("collector used %d calls, want %d", calls.Load(), tc.turns)
			}

			direct, task, ctx, _ := resultCollectionTaskFixture(t, server.URL+"/v1", false, configure, install)
			tool := &submitResultTool{coordinator: direct, todoID: task.ID}
			response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: wire})
			if err != nil || !response.IsError || submissionFailureMetadata(response.Metadata) != tc.want {
				t.Fatalf("direct submission failure = %+v, %v", response, err)
			}
			if tc.want == RepairFailureSource {
				if todoByID(t, direct, task.ID).ResultCandidate != nil {
					t.Fatal("source rejection persisted a direct repair candidate")
				}
				assertDirectPayloadSourceStopsRepair(t, compiled, wire)
			}
		})
	}
}

func assertDirectPayloadSourceStopsRepair(t *testing.T, compiled *CompiledResultContract, wire string) {
	t.Helper()
	c := newTestCoordinator(t)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	ref := compiled.ref(true)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "submit evidence", ResultContract: &ref}})[0]
	worker, model := normalizationStreamedSubmission(c, item.ID, wire, nil)
	repair := scriptWorker(failTurn(errors.New("source rejection must not start repair")))
	c.workerAgentOverride = worker
	c.repairAgentOverride = repair
	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), TaskDef{
		Agent: "worker", Goal: "submit evidence", Recovery: RecoveryManual, SideEffect: SideEffectExternalWrite,
		Execution: ExecutionContract{RequiresResult: true}, ResultContract: &ref,
	}, item.ID)
	got := todoByID(t, c, item.ID)
	if err == nil || model.calls != 1 || repair.Calls() != 0 || got.ResultCandidate != nil ||
		got.ExecutionReceipt == nil || got.ExecutionReceipt.WorkSubmissionFailure != RepairFailureSource {
		t.Fatalf("direct admission did not stop on source rejection: work=%d repair=%d receipt=%+v err=%v", model.calls, repair.Calls(), got.ExecutionReceipt, err)
	}
}

func TestResultCollectionRejectsFramedInvalidClaimsBeforeRepair(t *testing.T) {
	compiled := compileFixtureJSONSchema(t, "framed-collection-references", collectionReferenceSchema)
	c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
		ref := compiled.ref(true)
		spec.ResultContract = &ref
	}, func(c *Coordinator) {
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	})
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "original report")
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.reserveResultCollection(ctx, cp, "framed-claims", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	const wire = "```json\n" + `{"status":"success","summary":"report","structured_payload":{"references":[]},"structured_payload":{"references":[{"producer":"outside","paths":["source.go"]}]}}` + "\n```"
	cp, err = c.recordCollectionResponse(ctx, cp, []byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	err = c.collectWorkResult(ctx, c.session.Agents[item.Agent], cp)
	failure, ok := errors.AsType[*resultCollectionFailure](err)
	if !ok || failure.Reason != RepairFailureSource || failure.Checkpoint.TurnsUsed != 1 || !strings.Contains(err.Error(), "evidence_from") {
		t.Fatalf("framed claims did not reject the unauthorized source before repair: %v", err)
	}
	blob, err := c.readCollectionBlobRecord(ctx, failure.Checkpoint, "response", failure.Checkpoint.Response)
	if err != nil || string(blob.Data) != wire || failure.Checkpoint.Prepared != nil {
		t.Fatalf("source inspection changed the wire or admitted the claims: %v", err)
	}
}

func TestResultCollectionRepairsResponseEnvelopeWithOriginalArguments(t *testing.T) {
	for _, tc := range []struct {
		name, arguments, actual, pointer string
		invalidSource                    string
	}{
		{"encoded claims", `{"result":"{\"status\":\"success\",\"summary\":\"saved report\"}"}`, "string", "/result", ""},
		{"array", `{"result":[]}`, "array", "/result", ""},
		{"null", `{"result":null}`, "null", "/result", ""},
		{"readable result with an extra member", `{"result":{"status":"success","summary":"saved report"},"extra":true}`, "object", "", ""},
		{"artifact in readable result", `{"result":{"status":"success","summary":"saved report","artifacts":[{"path":"missing-artifact"}]},"extra":true}`, "object", "", "missing-artifact"},
		{"receipt in repeated result", `{"result":{"status":"success","summary":"saved report"},"result":{"receipt_ids":["invented-receipt"]}}`, "object", "", "receipt_ids is runtime-owned"},
		{"receipt in encoded result", `{"result":"{\"receipt_ids\":[\"invented-receipt\"]}"}`, "string", "/result", "receipt_ids is runtime-owned"},
		{"receipt before incomplete member", `{"result":{"receipt_ids":["invented-receipt"],"summary":`, "invalid_json", "/result", "receipt_ids is runtime-owned"},
		{"receipt inside incomplete array", `{"result":{"receipt_ids":["invented-receipt",`, "invalid_json", "/result", "receipt_ids is runtime-owned"},
		{"nested forged receipt beside malformed outer field", `{"receipt_ids":[],"result":{"receipt_ids":["invented-receipt"]}}`, "missing", "/result", "receipt_ids is runtime-owned"},
		{"artifact inside incomplete array entry", `{"result":{"artifacts":[{"path":"missing-artifact","description":`, "invalid_json", "/result", "missing-artifact"},
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
				assertCollectorResponseChannel(t, request)
				n := calls.Add(1)
				if n > 2 {
					t.Error("repair exceeded the original collection budget")
					http.Error(w, "extra call", http.StatusBadRequest)
					return
				}
				if n == 1 {
					for _, msg := range request["messages"].([]any) {
						content, _ := msg.(map[string]any)["content"].(string)
						if strings.Contains(content, `"format_diagnostic"`) {
							t.Error("initial collection advertised a validation failure before any response was rejected")
						}
					}
				}
				arguments := tc.arguments
				if n == 2 {
					arguments = `{"result":{"status":"success","summary":"saved report"}}`
					var data struct {
						Previous string                         `json:"previous_response"`
						Issue    *agent.StructuredResponseIssue `json:"response_channel_failure"`
						Feedback *collectionFormatFeedback      `json:"format_feedback"`
					}
					for _, msg := range request["messages"].([]any) {
						content, _ := msg.(map[string]any)["content"].(string)
						_, suffix, found := strings.Cut(content, `{"claims_schema":`)
						if !found {
							continue
						}
						raw, err := jsontext.NewDecoder(strings.NewReader(`{"claims_schema":` + suffix)).ReadValue()
						if err == nil {
							err = json.Unmarshal(raw, &data)
						}
						if err != nil {
							t.Error(err)
						}
					}
					if data.Previous != tc.arguments || data.Issue == nil || data.Issue.Pointer != tc.pointer || data.Issue.Actual != tc.actual || data.Issue.Expected != "object" || data.Feedback == nil || data.Feedback.Stage != "response_envelope" {
						t.Errorf("repair lost raw arguments or envelope-specific feedback: %+v", data)
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"response\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"response\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", arguments)
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true)
			worker := scriptWorker(textTurn("must not repeat work"))
			c.workerAgentOverride = worker
			err := c.collectReturnedWork(ctx, c.session.Agents[item.Agent], receipt, "saved report", "original task")
			if tc.invalidSource != "" {
				assertRejectedResponseSourceSurvivesRestart(t, c, item, tc.arguments, tc.invalidSource, err)
				if calls.Load() != 1 || worker.Calls() != 0 {
					t.Fatal("invalid source consumed a correction turn or replayed work")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || worker.Calls() != 0 {
				t.Fatal("collection repeated worker tools or lost bounded repair")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				if event.Type != string(EventTaskResultCollectionCheckpointed) {
					continue
				}
				var payload resultCollectionEvent
				if err := json.Unmarshal(event.Payload, &payload, collectionDurationFormat); err != nil {
					t.Fatal(err)
				}
				cp := payload.Receipt.ResultCollection
				if cp.State != ResultCollectionReceived || cp.TurnsUsed != 1 {
					continue
				}
				blob, err := c.readCollectionBlobRecord(t.Context(), cp, "response", cp.Response)
				if err != nil || string(blob.Data) != tc.arguments || blob.ResponseIssue == nil || blob.ResponseIssue.Actual != tc.actual {
					t.Fatalf("replayed rejected arguments unavailable: %v", err)
				}
				if cp.Prepared != nil {
					t.Fatal("rejected response arguments acquired a prepared claims reference")
				}
				found = true
			}
			if !found {
				t.Fatal("original rejected response was not retained in the journal")
			}
			items, err := ReplayTodoList(events)
			if err != nil || currentResultCollection(todoItemByID(items, item.ID)).State != ResultCollectionCommitted {
				t.Fatalf("accepted repair did not survive replay: %v", err)
			}
			work, err := c.loadCollectionWork(ctx, c.todoItemByID(item.ID), currentResultCollection(c.todoItemByID(item.ID)))
			if err != nil || work.Receipt.TranscriptRef != receipt.TranscriptRef {
				t.Fatalf("repair changed original work evidence: %v", err)
			}
		})
	}
}

func assertRejectedResponseSourceSurvivesRestart(t *testing.T, c *Coordinator, item *TodoItem, raw, detail string, err error) {
	t.Helper()
	failure, ok := errors.AsType[*resultCollectionFailure](err)
	if !ok || failure.Reason != RepairFailureSource || !strings.Contains(err.Error(), detail) {
		t.Fatalf("readable response source was not blocked before repair: %v", err)
	}
	cp := failure.Checkpoint
	c.recordCollectionStopReceipt(item.ID, failure)
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "response source rejected", NeedsHuman, FailureProtocol, TaskBlocked, "saved report")
	c = reopenRecoveryCoordinator(t, c)
	a, err := c.AssessResultCollectionRecovery(t.Context(), item.ID)
	if err != nil || a.Disposition != RecoveryBlocked || a.AdditionalTurns != 0 || a.TurnsUsed != 1 || a.Checkpoint.Work != cp.Work {
		t.Fatalf("cold assessment granted repair for an invalid response source: %+v %v", a, err)
	}
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err == nil {
		t.Fatal("cold authorization accepted the invalid response source")
	}
	blob, err := c.readCollectionBlobRecord(t.Context(), a.Checkpoint, "response", a.Checkpoint.Response)
	if err != nil || string(blob.Data) != raw || blob.ResponseIssue == nil || *a.Checkpoint.Response != *cp.Response || a.Checkpoint.Prepared != nil {
		t.Fatalf("rejected response lost original wrapper or issue: %v", err)
	}
}

func TestResultCollectionRejectsResponseIssueAsClaims(t *testing.T) {
	c, _, ctx, cp := beginCollectionFixture(t)
	cp, err := c.reserveResultCollection(ctx, cp, "rejected-response", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	issue := &agent.StructuredResponseIssue{Code: "missing_property", Pointer: "/result", Expected: "object", Actual: "missing"}
	cp, err = c.recordCollectionResponseUsage(ctx, cp, []byte(`{"status":"success","summary":"looks like valid claims"}`), ExecutionUsage{}, issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.prepareCollectionClaims(ctx, cp); localResultRejectionReason(err) != RepairFailureInvalidSchema || !strings.Contains(err.Error(), "/result") {
		t.Fatalf("response rejection was lost: %v", err)
	}
	if _, err := c.putCollectionBlobRecord(ctx, cp, "claims", []byte(`{}`), issue, nil); err == nil {
		t.Fatal("response issue was allowed on an admitted claims artifact")
	}
	issue.Actual = "model-controlled instruction"
	if _, err := c.putCollectionBlobRecord(ctx, cp, "response", []byte(`{}`), issue, nil); err == nil {
		t.Fatal("untrusted diagnostic vocabulary was persisted")
	}
}

func TestResultCollectionResponseIssuesPreserveProgressAndRedaction(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprint("identical=", same), func(t *testing.T) {
			c, _, ctx, cp := beginCollectionFixture(t)
			issue := &agent.StructuredResponseIssue{Code: "type", Pointer: "/result", Expected: "object", Actual: "string"}
			first := `{"result":"first rejected claims"}`
			for turn := range 2 {
				var err error
				cp, err = c.reserveResultCollection(ctx, cp, fmt.Sprint("response-", turn), []byte(`{"tools":[]}`))
				if err != nil {
					t.Fatal(err)
				}
				raw := first
				if turn == 1 && !same {
					raw = `{"result":"different rejected claims"}`
				}
				cp, err = c.recordCollectionResponseUsage(ctx, cp, []byte(raw), ExecutionUsage{}, issue)
				if err != nil {
					t.Fatal(err)
				}
				cp, err = c.recordCollectionFormatFailure(ctx, cp)
				if errors.Is(err, errResultCollectionNoProgress) != (same && turn == 1) || (err != nil && !errors.Is(err, errResultCollectionNoProgress)) {
					t.Fatalf("raw response progress misclassified: %v", err)
				}
			}
		})
	}

	t.Run("redaction", func(t *testing.T) {
		c, _, ctx, cp := beginCollectionFixture(t)
		cp, err := c.reserveResultCollection(ctx, cp, "sensitive-response", []byte(`{"tools":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		const secret = "sk-ResponseEvidenceFixtureCredential123456789"
		issue := &agent.StructuredResponseIssue{Code: "type", Pointer: "/result", Expected: "object", Actual: "array"}
		cp, err = c.recordCollectionResponseUsage(ctx, cp, []byte(`{"result":[],"api_key":"`+secret+`"}`), ExecutionUsage{}, issue)
		if err != nil {
			t.Fatal(err)
		}
		blob, err := c.readCollectionBlobRecord(ctx, cp, "response", cp.Response)
		if err != nil || strings.Contains(string(blob.Data), secret) || !strings.Contains(string(blob.Data), `"result":[]`) || blob.ResponseIssue == nil || *blob.ResponseIssue != *issue {
			t.Fatalf("redaction lost arguments or leaked credentials: %v", err)
		}
	})
}

func TestResultCollectionResponseIssueSurvivesRecoveryRestart(t *testing.T) {
	c, item, ctx, cp := beginCollectionFixture(t)
	cp, err := c.reserveResultCollection(ctx, cp, "malformed-envelope", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	const raw = `{"result":"{\"status\":\"success\",\"summary\":\"saved report\"}"}`
	issue := &agent.StructuredResponseIssue{Code: "type", Pointer: "/result", Expected: "object", Actual: "string"}
	cp, err = c.recordCollectionResponseUsage(ctx, cp, []byte(raw), ExecutionUsage{}, issue)
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.blockResultCollection(ctx, cp, RepairFailureInvalidSchema)
	if err != nil {
		t.Fatal(err)
	}
	c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureInvalidSchema, Checkpoint: cp, Cause: errors.New(issue.Diagnostic())})
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "response envelope rejected", NeedsHuman, FailureProtocol, TaskBlocked, "saved report")
	c = reopenRecoveryCoordinator(t, c)
	a, err := c.AssessResultCollectionRecovery(t.Context(), item.ID)
	if err != nil || a.Disposition != RecoveryRepairClaims || a.AdditionalTurns != 0 || a.RemainingTurns != 1 || a.Checkpoint.Work != cp.Work {
		t.Fatalf("recovery lost the response rejection: %+v %v", a, err)
	}
	found := false
	for _, check := range a.Checks {
		if check.Code == "canonical_claims" && !check.Passed && strings.Contains(check.Diagnostic, "received string") && strings.Contains(check.Diagnostic, "/result") {
			found = true
		}
	}
	if !found {
		t.Fatal("recovery preview dropped the envelope-specific diagnostic")
	}
	c.executionRunID = "response-recovery"
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err != nil {
		t.Fatal(err)
	}
	c = reopenRecoveryCoordinator(t, c)
	current := currentResultCollection(c.todoItemByID(item.ID))
	blob, err := c.readCollectionBlobRecord(t.Context(), current, "response", current.Response)
	if err != nil || string(blob.Data) != raw || blob.ResponseIssue == nil || *blob.ResponseIssue != *issue || *current.Response != *cp.Response || current.Work != cp.Work || current.TurnsUsed != cp.TurnsUsed {
		t.Fatalf("recovery grant rewrote original arguments or work: %v", err)
	}
	ctx, err = c.restoreCollectionContext(WithSessionResume(t.Context()), current)
	if err != nil {
		t.Fatal(err)
	}
	prompt, _, _, err := c.collectionPrompt(ctx, current, "")
	if err != nil || !strings.Contains(prompt, `"actual":"string"`) || !strings.Contains(prompt, `"stage":"response_envelope"`) {
		t.Fatalf("restarted repair lost precise response feedback: %v", err)
	}
}
