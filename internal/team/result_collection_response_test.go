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

	"github.com/kjelly/hufu/internal/agent"
)

func TestResultCollectionRepairsResponseEnvelopeWithOriginalArguments(t *testing.T) {
	for _, tc := range []struct{ name, arguments, actual string }{
		{"encoded claims", `{"result":"{\"status\":\"success\",\"summary\":\"saved report\"}"}`, "string"},
		{"array", `{"result":[]}`, "array"},
		{"null", `{"result":null}`, "null"},
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
					if data.Previous != tc.arguments || data.Issue == nil || data.Issue.Pointer != "/result" || data.Issue.Actual != tc.actual || data.Issue.Expected != "object" || data.Feedback == nil || data.Feedback.Stage != "response_envelope" {
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
			if err := c.collectReturnedWork(ctx, c.session.Agents[item.Agent], receipt, "saved report", "original task"); err != nil {
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
				if _, err := c.prepareCollectionClaims(ctx, cp); localResultRejectionReason(err) != RepairFailureInvalidSchema {
					t.Fatal("rejected response arguments were admitted as claims")
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
	if _, err := c.putCollectionBlobWithIssue(ctx, cp, "claims", []byte(`{}`), issue); err == nil {
		t.Fatal("response issue was allowed on an admitted claims artifact")
	}
	issue.Actual = "model-controlled instruction"
	if _, err := c.putCollectionBlobWithIssue(ctx, cp, "response", []byte(`{}`), issue); err == nil {
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
	if err != nil || a.Disposition != RecoveryRepairClaims || a.AdditionalTurns != 2 || a.Checkpoint.Work != cp.Work {
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
