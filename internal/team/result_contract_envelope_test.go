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

const resultEnvelopeTestSchema = `{"type":"object","required":["decisions"],"properties":{"decisions":{"type":"array","items":{"type":"object","required":["verdict"],"properties":{"verdict":{"enum":["confirmed","unverified"]}}}}},"x-hufu-result-envelope":{"type":"object","if":{"properties":{"structured_payload":{"properties":{"decisions":{"contains":{"properties":{"verdict":{"const":"unverified"}},"required":["verdict"]}}}}}},"then":{"properties":{"status":{"const":"completed_with_gaps"}}}}}`

func TestResultEnvelopeAdmissionAndRevalidation(t *testing.T) {
	for _, tc := range []struct {
		name, status, payload string
		valid                 bool
	}{
		{"settled", "success", `{"decisions":[{"verdict":"confirmed"}]}`, true},
		{"unverified", "completed_with_gaps", `{"decisions":[{"verdict":"unverified"}]}`, true},
		{"inconsistent", "success", `{"decisions":[{"verdict":"unverified"}]}`, false},
		{"empty", "success", `{"decisions":[]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"status":%q,"summary":"reviewed","structured_payload":%s}`, tc.status, tc.payload)
			c, item, ctx, cp := preparedCollectionClaimsFixture(t, resultEnvelopeTestSchema, body)
			err := c.commitPreparedCollection(ctx, cp)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if !tc.valid && (localResultRejectionReason(err) != RepairFailureInvalidSchema || c.GetTaskResult(item.ID) != nil) {
				t.Fatal("inconsistent envelope accepted or not repairable")
			}
			compiled := c.session.ResultContracts["claims.json"]
			result := &TaskResult{Status: tc.status, StructuredPayload: &ResultPayload{Value: []byte(tc.payload)}}
			if resultProjectionMatchesAccepted(result, compiled) != tc.valid {
				t.Fatal("contract upgrade ignored envelope consistency")
			}
			raw := tc.payload
			request := AttemptRequest{Task: TaskDef{ResultContract: new(compiled.ref(true))}, resultContractSchema: compiled}
			_, err = canonicalExternalStructuredPayload(request, &WorkerResultProposal{Status: tc.status, StructuredPayloadJSON: &raw}, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("external valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestResultEnvelopeSchemaFailsClosed(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{"properties":{"status":{"type":17}}}`, `{"$ref":"https://example.com/schema"}`} {
		_, err := CompileResultContractDocument("envelope.json", []byte(`{"type":"object","x-hufu-result-envelope":`+body+`}`))
		if err == nil {
			t.Fatalf("invalid envelope schema accepted: %s", body)
		}
	}
}

func TestResultCollectionRepairsEnvelopeWithoutRepeatingWork(t *testing.T) {
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
		n := calls.Add(1)
		prompt := fmt.Sprint(request["messages"])
		if !strings.Contains(prompt, "Result envelope schema") {
			t.Error("collector lost envelope rules")
		}
		if tools, _ := request["tools"].([]any); len(tools) != 0 {
			t.Error("collector exposed tools")
		}
		status := "success"
		if n == 2 {
			status = "completed_with_gaps"
			if !strings.Contains(prompt, "status/payload contract") {
				t.Error("repair lost envelope diagnostic")
			}
		}
		if n > 2 {
			t.Error("exceeded turn budget")
			http.Error(w, "extra turn", 400)
			return
		}
		body := fmt.Sprintf(`{"status":%q,"summary":"honest review","structured_payload":{"decisions":[{"verdict":"unverified"}]}}`, status)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"envelope\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
	}))
	t.Cleanup(server.Close)
	compiled, err := CompileResultContractDocument("envelope.json", []byte(resultEnvelopeTestSchema))
	if err != nil {
		t.Fatal(err)
	}
	c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true)}
	})
	worker := scriptWorker(textTurn("Finding remains unverified; original evidence is retained."))
	c.workerAgentOverride = worker
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	c.delegatedTasks = make(map[string]int)
	testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
	_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "review", Agent: "worker", Model: "ollama/test", Goal: "Review evidence", SideEffect: SideEffectNone, Execution: ExecutionContract{RequiresResult: true, ForbidArtifacts: true}}})
	if err != nil {
		t.Fatal(err)
	}
	item := c.taskTracker.TodoList().Items()[1]
	if item.Status != TaskDone || item.TypedResult.Status != "completed_with_gaps" || calls.Load() != 2 || worker.Calls() != 1 {
		t.Fatalf("status=%s calls=%d work=%d", item.Status, calls.Load(), worker.Calls())
	}
	if err := c.validateAcceptedDependencyPayload(item); err != nil {
		t.Fatal(err)
	}
	validated := make(map[string]bool)
	if err := c.validateAcceptedDependencyPayloadGraph(item, make(map[string]bool), validated); err != nil {
		t.Fatal(err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)[1]
	if !sameCanonicalEvidence(item.TypedResult, replayed.TypedResult) {
		t.Fatal("replay changed accepted evidence")
	}
	item.TypedResult.Status = "success"
	if err := c.validateAcceptedDependencyPayload(item); err == nil {
		t.Fatal("dependency accepted inconsistent envelope")
	}
	if err := c.validateAcceptedDependencyPayloadGraph(item, make(map[string]bool), validated); err == nil {
		t.Fatal("cached payload validation concealed inconsistent status")
	}
}

func TestResultEnvelopeOwnsItsReferenceNamespaceAndBounds(t *testing.T) {
	schema := `{"type":"object","x-hufu-result-envelope":{"$defs":{"completion":{"const":"completed_with_gaps"}},"properties":{"status":{"$ref":"#/$defs/completion"}}}}`
	compiled, err := CompileResultContractDocument("envelope.json", []byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	if err = validateResultEnvelope(compiled, &TaskResult{Status: "completed_with_gaps"}); err != nil {
		t.Fatal(err)
	}
	if err = validateResultEnvelope(compiled, &TaskResult{Status: "success"}); err == nil {
		t.Fatal("local envelope reference was not enforced")
	}
	oversized := `{"type":"object","x-hufu-result-envelope":{"description":"` + strings.Repeat("a", resultContractPromptSchemaMaxBytes) + `"}}`
	if _, err = CompileResultContractDocument("oversized.json", []byte(oversized)); err == nil {
		t.Fatal("unrenderable envelope rules were admitted")
	}
}

func TestResultEnvelopeRejectsMissingOptionalPayload(t *testing.T) {
	schema := `{"type":"object","x-hufu-result-envelope":{"properties":{"structured_payload":{"type":"object"}}}}`
	compiled, err := CompileResultContractDocument("optional.json", []byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	request := AttemptRequest{Task: TaskDef{ResultContract: new(compiled.ref(false))}, resultContractSchema: compiled}
	for _, proposal := range []*WorkerResultProposal{nil, {Status: "success"}} {
		if _, err = canonicalExternalStructuredPayload(request, proposal, nil); err == nil {
			t.Fatal("optional payload bypassed the envelope contract")
		}
	}
}
