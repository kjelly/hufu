package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
)

func modelInputTestContract(t *testing.T) *CompiledResultContract {
	t.Helper()
	root, err := decodeResultContractJSON([]byte(textEvidenceTestSchema(t, true)))
	if err != nil {
		t.Fatal(err)
	}
	schema := root.(map[string]any)
	spec := schema["x-hufu-tool-evidence"].(map[string]any)
	spec["require_model_delivery"], spec["model_delivery_pointer"] = true, "/delivery"
	item := resultProjectionSchemaAt(schema, "/findings/0/sources/0")
	item["properties"].(map[string]any)["delivery"] = map[string]any{"type": "object"}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "delivery.json", string(raw))
	compiled, err := compileResultContractSchema(dir, "delivery.json")
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func modelInputTestRequest(records []taskTranscriptRecord) agent.ProviderRequest {
	request := agent.ProviderRequest{InvocationID: "provider-step", ModelID: "fixture"}
	for _, r := range records {
		if r.Event == "tool_result" {
			request.Messages = append(request.Messages, toolResultMsg(r.ToolCallID, r.Output))
		}
	}
	return request
}

func appendObservedModelInput(t *testing.T, compiled *CompiledResultContract, records []taskTranscriptRecord) []taskTranscriptRecord {
	t.Helper()
	request := modelInputTestRequest(records)
	inputs, err := toolModelInputRecords(compiled.toolEvidence, records, request)
	if err != nil {
		t.Fatal(err)
	}
	records = append(records, inputs...)
	return append(records, taskTranscriptRecord{Event: "model_input_result", ProviderResult: &toolModelInputResult{InvocationID: request.InvocationID, ResponseObserved: true, Outcome: agent.ProviderInvocationSuccess}})
}

func TestToolModelInputRequiresObservedCompleteSource(t *testing.T) {
	compiled := modelInputTestContract(t)
	for _, scenario := range []string{"full", "captured_only", "omitted", "truncated", "modified", "duplicate", "wrong_call", "wrong_target", "unobserved", "cancelled", "provider_error", "different_snapshot", "later_omission", "receipt_hash_mismatch", "receipt_size_mismatch", "redaction_masked_change"} {
		t.Run(scenario, func(t *testing.T) {
			records := textEvidenceTestRecords()
			request := modelInputTestRequest(records)
			switch scenario {
			case "omitted":
				request.Messages = nil
			case "truncated":
				request.Messages[0] = toolResultMsg("observed", "Document: document-a\n### Content\nthe exact words\n### End\n")
			case "modified":
				request.Messages[0] = toolResultMsg("observed", strings.ReplaceAll(records[1].Output, "exact words", "different words"))
			case "duplicate":
				request.Messages = append(request.Messages, request.Messages[0])
			case "wrong_call":
				request.Messages[0] = toolResultMsg("another", records[1].Output)
			case "wrong_target":
				request.Messages[0] = toolResultMsg("observed", strings.ReplaceAll(records[1].Output, "document-a", "document-b"))
			case "redaction_masked_change":
				raw := strings.ReplaceAll(records[1].Output, "a document", "a document\nAuthorization: Bearer abc.def.ghi")
				records[1].Output = utils.RedactSecrets(raw)
				if raw == records[1].Output {
					t.Fatal("fixture was not redacted")
				}
				request.Messages[0] = toolResultMsg("observed", raw)
			}
			inputs, err := toolModelInputRecords(compiled.toolEvidence, records, request)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "receipt_hash_mismatch" {
				inputs[0].ModelInput.DeliveredSHA256 = toolInputDigest("other body")
			}
			if scenario == "receipt_size_mismatch" {
				inputs[0].ModelInput.DeliveredBytes++
			}
			if scenario != "captured_only" {
				records = append(records, inputs...)
			}
			result := &toolModelInputResult{InvocationID: request.InvocationID, ResponseObserved: true, Outcome: agent.ProviderInvocationSuccess}
			switch scenario {
			case "unobserved":
				result.ResponseObserved = false
			case "cancelled":
				result.Outcome = agent.ProviderInvocationCancelled
			case "provider_error":
				result.Outcome = agent.ProviderInvocationProviderError
			case "different_snapshot":
				records[1].Output = strings.ReplaceAll(records[1].Output, "a document", "another document")
			}
			records = append(records, taskTranscriptRecord{Event: "model_input_result", ProviderResult: result})
			if scenario == "later_omission" {
				request.Messages = nil
				request.InvocationID = "later"
				later, err := toolModelInputRecords(compiled.toolEvidence, records, request)
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, later...)
			}
			payload, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(evidenceTestPayload), records)
			if err != nil {
				t.Fatal(err)
			}
			valid := scenario == "full" || scenario == "later_omission"
			if (payload.EvidenceDowngrades == 0) != valid {
				t.Fatalf("unexpected source credit: %s", payload.Value)
			}
			value, err := decodeResultContractJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			status := evidenceString(value, "/findings/0/sources/0/delivery/status")
			if valid && status != "full" || scenario == "truncated" && status != "truncated" || scenario == "modified" && status != "modified" {
				t.Fatalf("incorrect delivery diagnosis: %s", payload.Value)
			}
		})
	}
}

func TestToolModelInputRejectsModelReceiptAndOmitsGuidance(t *testing.T) {
	compiled := modelInputTestContract(t)
	schema := visibleResultPayloadSchema(compiled.ref(true), compiled, true)
	if node := resultProjectionSchemaAt(schema, "/findings/0/sources/0/delivery"); node != nil {
		t.Fatal("model was asked to supply runtime input receipt")
	}
	for _, override := range []string{`null`, `{}`, `{"status":"full"}`} {
		raw := strings.Replace(evidenceTestPayload, `"call_id":"forged"`, `"call_id":"forged","delivery":`+override, 1)
		if _, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(raw)); err == nil {
			t.Fatal("model delivery override accepted")
		}
	}
	proposal := strings.Replace(evidenceTestPayload, `"call_id":"forged"`, `"call_id":"forged","delivery":{"status":"full"}`, 1)
	ref := compiled.ref(true)
	if _, err := canonicalExternalStructuredPayload(AttemptRequest{Task: TaskDef{ResultContract: &ref}, resultContractSchema: compiled}, &WorkerResultProposal{StructuredPayloadJSON: new(proposal)}, nil); err == nil {
		t.Fatal("external model forged delivery credit")
	}
}

func TestToolModelInputProviderBoundaryAndReplay(t *testing.T) {
	compiled := modelInputTestContract(t)
	c := newTestCoordinator(t)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	ref := compiled.ref(true)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", ResultContract: &ref}})[0]
	store, err := NewEventStore(c.session.Workspace, c.executionRunID, "delivery-session")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, c.executionRunID, 1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	for _, r := range textEvidenceTestRecords() {
		if err := transcript.append(r); err != nil {
			t.Fatal(err)
		}
	}
	// append is deliberately not used for the result count in the production
	// path; the fixture's manifest still contains the same canonical records.
	transcript.toolResults = 1
	ctx := context.WithValue(t.Context(), taskTranscriptKey{}, transcript)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, AgentName: "worker", AgentRole: "worker"})
	inner := &costAdmissionLanguageModel{}
	model := agent.NewAdmittedLanguageModelWithContext("ollama/gpt", inner, c.providerAdmission(), providerAdmissionTestContext(t, "ollama/gpt"))
	request := modelInputTestRequest(textEvidenceTestRecords())
	if _, err := model.Generate(ctx, fantasy.Call{Prompt: request.Messages}); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatal("provider call missing")
	}
	// A rejected forged receipt followed by result-only correction must reuse
	// the completed read and the same provider response, without another call.
	submitCtx := context.WithValue(occurrenceTestContext(c, item.ID, 1), taskTranscriptKey{}, transcript)
	submit := &submitResultTool{coordinator: c, todoID: item.ID}
	for _, forged := range []bool{true, false} {
		raw := evidenceTestPayload
		if forged {
			raw = strings.Replace(raw, `"call_id":"forged"`, `"call_id":"forged","delivery":{"status":"full"}`, 1)
		}
		response, err := submit.Run(submitCtx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"document checked","structured_payload":` + raw + `}`})
		if err != nil || response.IsError != forged {
			t.Fatalf("receipt repair: %+v %v", response, err)
		}
	}
	if inner.calls != 1 || c.GetTaskResult(item.ID).StructuredPayload.EvidenceDowngrades != 0 {
		t.Fatal("result repair replayed work or lost source credit")
	}
	payload, err := validateStructuredResultPayload(compiled, ref, []byte(evidenceTestPayload), transcript.evidenceRecords())
	if err != nil || payload.EvidenceDowngrades != 0 {
		t.Fatalf("provider delivery lost: %v %v", payload, err)
	}
	artifact, err := transcript.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := c.restoreTranscriptEvidence(t.Context(), &ExecutionReceipt{TranscriptRef: artifact.ID, TaskID: item.ID, RunID: c.executionRunID, Attempt: 1, ProducerID: "worker"}, c.executionRunID)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := validateStructuredResultPayload(compiled, ref, []byte(evidenceTestPayload), restored.evidenceRecords())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replayed.Value, payload.Value) || replayed.SHA256 != payload.SHA256 {
		t.Fatal("restart changed the original source receipt")
	}
	claims, err := modelDeliveryClaimsForRevalidation(compiled.toolEvidence, payload.Value)
	if err != nil {
		t.Fatal(err)
	}
	revalidated, err := validateStructuredResultPayload(compiled, ref, claims, restored.evidenceRecords())
	if err != nil || revalidated.SHA256 != payload.SHA256 {
		t.Fatalf("accepted receipt did not revalidate unchanged: %v", err)
	}
}

func TestToolModelInputRejectsInheritedWorkerRecorder(t *testing.T) {
	for _, scenario := range []string{"own", "custom_role", "auxiliary", "coordinator", "other_task", "other_agent", "other_attempt", "other_run", "repair", "collector"} {
		t.Run(scenario, func(t *testing.T) {
			transcript := &taskTranscript{todoID: "task", runID: "run", agent: "worker", attempt: 1}
			meta := InvocationMetadata{TaskID: "task", RunID: "run", AgentName: "worker", AgentRole: "worker", Attempt: 1}
			switch scenario {
			case "custom_role":
				meta.AgentRole = "reviewer"
			case "coordinator":
				meta.AgentRole = "coordinator"
			case "auxiliary":
				meta.AgentRole = "auxiliary"
			case "other_task":
				meta.TaskID = "another"
			case "other_agent":
				meta.AgentName = "other"
			case "other_attempt":
				meta.Attempt = 2
			case "other_run":
				meta.RunID = "another"
			}
			ctx := context.WithValue(withInvocationMetadata(t.Context(), meta), taskTranscriptKey{}, transcript)
			if scenario == "repair" {
				ctx = withProtocolRepairToolEvidence(ctx, transcript)
			}
			if scenario == "collector" {
				ctx = context.WithValue(ctx, taskTranscriptKey{}, (*taskTranscript)(nil))
				ctx = context.WithValue(ctx, resultCollectionEvidenceKey{}, transcript)
			}
			if (toolInputTranscript(ctx) != nil) != (scenario == "own" || scenario == "custom_role") {
				t.Fatal("inherited recorder earned source delivery")
			}
		})
	}
}

func TestToolModelInputVerifierRequiresOwnRead(t *testing.T) {
	dir := filepath.Join(bundledTeamsDir(t), "web-research")
	compiled, err := compileResultContractSchema(dir, "schemas/audit-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	if !compiled.toolEvidence.RequireModelDelivery || compiled.toolEvidence.AllowHistorical {
		t.Fatal("verifier lacks full fresh-read gate")
	}
	records := textEvidenceTestRecords()
	records[0].Tool = "playwright_researcher__browser_snapshot"
	records[1].Tool = records[0].Tool
	if got := matchingToolObservations(compiled.toolEvidence, records); len(got) != 0 {
		t.Fatal("verifier accepted researcher's read")
	}
}
