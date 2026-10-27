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

func TestResultCollectionRepairsInvariantClaimsWithOriginalLimits(t *testing.T) {
	schema, err := json.Marshal(map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"findings", "assessments"},
		"properties": map[string]any{"findings": submitResultToolInfo(taskResultSubmissionContract{}).Parameters["findings"], "assessments": invariantAssessmentClaimsSchema(false)},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileResultContractDocument("invariants.json", schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		mode         InvariantVerificationMode
		summary      string
		severity     string
		unknown      bool
		repeat       bool
		projection   bool
		wantCalls    int32
		wantAccepted bool
		wantDone     bool
	}{
		{name: "severity", mode: InvariantVerificationReport, summary: "violation", severity: "warning", wantCalls: 2, wantAccepted: true, wantDone: true},
		{name: "severity_with_payload_projections", mode: InvariantVerificationReport, summary: "violation", severity: "warning", projection: true, wantCalls: 2, wantAccepted: true, wantDone: true},
		{name: "gate_stays_blocked", mode: InvariantVerificationGate, summary: "violation", severity: "warning", wantCalls: 2, wantAccepted: true},
		{name: "summary_limit", mode: InvariantVerificationReport, summary: collectionInvariantSummary(1000), severity: "error", wantCalls: 1, wantAccepted: true, wantDone: true},
		{name: "summary_repair", mode: InvariantVerificationReport, summary: collectionInvariantSummary(1002), severity: "error", wantCalls: 2, wantAccepted: true, wantDone: true},
		{name: "summary_exhausted", mode: InvariantVerificationReport, summary: collectionInvariantSummary(1002), severity: "error", repeat: true, wantCalls: 2},
		{name: "severity_exhausted", mode: InvariantVerificationReport, summary: "violation", severity: "warning", repeat: true, wantCalls: 2},
		{name: "unknown_invariant", mode: InvariantVerificationReport, summary: "violation", severity: "error", unknown: true, wantCalls: 1},
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
				prompt := fmt.Sprint(request["messages"])
				if !strings.Contains(prompt, "safe (error)") || !strings.Contains(prompt, "well below its schema maxLength") {
					t.Error("collector lost invariant definition or length guidance")
				}
				if exposed, _ := request["tools"].([]any); len(exposed) != 0 {
					t.Error("collector exposed tools")
				}
				n := calls.Add(1)
				if n > tc.wantCalls {
					t.Error("unexpected extra collection turn")
					http.Error(w, "extra turn", 400)
					return
				}
				summary, severity := tc.summary, tc.severity
				if n == 2 && !tc.repeat {
					summary, severity = "violation", "error"
				}
				if n == 2 && !strings.Contains(prompt, "format_diagnostic") {
					t.Error("repair omitted diagnosis")
				}
				id := "safe"
				if tc.unknown {
					id = "foreign"
				}
				body := fmt.Sprintf(`{"status":"success","summary":"review complete","details":"complete underlying explanation","findings":[{"summary":"observed violation","severity":%q}],"invariant_assessments":[{"invariant_id":%q,"status":"violated","summary":%q,"finding_index":0}]}`, severity, id, summary)
				if tc.projection {
					var claims map[string]any
					if err := json.Unmarshal([]byte(body), &claims); err != nil {
						t.Error(err)
						return
					}
					claims["structured_payload"] = map[string]any{"findings": claims["findings"], "assessments": claims["invariant_assessments"]}
					encoded, err := json.Marshal(claims)
					if err != nil {
						t.Error(err)
						return
					}
					body = string(encoded)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"invariants\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
			}))
			t.Cleanup(server.Close)
			c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
				c.session.Config.MaxRetries = 0
				c.session.Agents["worker"].MaxRetries = 0
				c.session.InvariantCatalog = []InvariantDefinition{{ID: "safe", Statement: "Preserve the declared safety boundary", Severity: InvariantSeverityError, AppliesTo: []string{"*"}}}
				if tc.projection {
					c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
					c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true)}
				}
			})
			worker := scriptWorker(textTurn("Observed a violation of safe. Preserve the finding and original evidence."))
			c.workerAgentOverride = worker
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			var verify *VerificationSpec
			if tc.projection {
				verify = &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
					{Pointer: "/findings", Op: "equals_projection", Value: map[string]any{"pointer": "/structured_payload/value/findings", "fields": []string{"summary", "severity"}}},
					{Pointer: "/invariant_verification/assessments", Op: "equals_projection", Value: map[string]any{"pointer": "/structured_payload/value/assessments", "fields": []string{"invariant_id", "status", "summary"}, "optional_fields": []string{"finding_index"}}},
				}}
			}
			_, err := c.ExecuteTasks(t.Context(), []TaskDef{{ID: "review", Agent: "worker", Model: "ollama/test", Goal: "Review safety", Phase: PhaseVerify, InvariantVerification: tc.mode, VerifySpec: verify, SideEffect: SideEffectNone, Execution: ExecutionContract{RequiresResult: true, ForbidArtifacts: true}}})
			item := c.taskTracker.TodoList().Items()[1]
			if (err == nil) != tc.wantDone {
				t.Fatalf("status=%s detail=%s err=%v", item.Status, item.Detail, err)
			}
			checkpoint := currentResultCollection(item)
			if worker.Calls() != 1 || calls.Load() != tc.wantCalls || checkpoint == nil || checkpoint.TurnsUsed != int(tc.wantCalls) {
				t.Fatalf("work=%d collection=%d checkpoint=%+v detail=%s", worker.Calls(), calls.Load(), checkpoint, item.Detail)
			}
			if tc.wantAccepted {
				if checkpoint.State != ResultCollectionCommitted || item.TypedResult == nil || item.TypedResult.InvariantVerification == nil {
					t.Fatal("valid repaired claims were not admitted")
				}
				assessment := item.TypedResult.InvariantVerification.Assessments[0]
				if assessment.Status != InvariantViolated || assessment.Severity != InvariantSeverityError || item.TypedResult.Findings[0].Severity != "error" {
					t.Fatal("repair lowered severity or erased violation")
				}
				if tc.name == "summary_limit" && assessment.Summary != tc.summary {
					t.Fatal("boundary summary was truncated")
				}
			} else if item.TypedResult != nil || checkpoint.State != ResultCollectionBlocked {
				t.Fatal("invalid invariant claims were accepted")
			}
			events, readErr := c.EventJournal().ReadEvents(t.Context())
			if readErr != nil {
				t.Fatal(readErr)
			}
			replayed := ReduceToTodoList(events)[1]
			if replayed.Status != item.Status || currentResultCollection(replayed).TurnsUsed != checkpoint.TurnsUsed {
				t.Fatal("replay changed invariant outcome or budget")
			}
		})
	}
}

func collectionInvariantSummary(size int) string {
	var b strings.Builder
	for i := range size {
		b.WriteRune(rune(0x4e00 + i))
	}
	return b.String()
}
