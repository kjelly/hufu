package team

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"charm.land/fantasy"
)

const evidenceTestSchema = `{
 "type":"object","required":["findings"],
 "x-hufu-tool-evidence":{
  "groups_pointer":"/findings","items_pointer":"/sources","tool":"read_document",
  "input_pointer":"/key","value_pointer":"/key","output_pointer":"/text",
  "quote_pointer":"/quote","status_pointer":"/state","call_id_pointer":"/call_id",
  "verified_status":"checked","unverified_status":"unavailable",
  "group_fallback":{"/verdict":"unverified","/analysis":"missing observed evidence"},
  "group_append":{"/limitations":"Evidence downgraded."}
 },
 "x-hufu-final-report":"{{range .findings}}Verdict: {{.verdict}}\n{{.analysis}}\nLimits: {{.limitations}}\n{{range .sources}}Source: {{.key}} {{.state}} {{.quote}} {{.call_id}}\n{{end}}{{end}}",
 "properties":{"findings":{"type":"array","items":{
  "type":"object","required":["verdict","analysis","limitations","sources"],
  "properties":{"verdict":{"enum":["supported","unverified"]},"analysis":{"type":"string"},"limitations":{"type":"string"},
   "sources":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["key","state","quote"],
    "properties":{"key":{"type":"string"},"state":{"enum":["checked","unavailable"]},"quote":{"type":"string"},"call_id":{"type":"string"}}}}
  }
 }}}
}`

const evidenceTestPayload = `{"findings":[{"verdict":"supported","analysis":"conclusive","limitations":"Original limitation.","sources":[{"key":"document-a","state":"checked","quote":"exact words","call_id":"forged"}]}]}`

func evidenceTestContract(t *testing.T) *CompiledResultContract {
	t.Helper()
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "evidence.json", evidenceTestSchema)
	compiled, err := compileResultContractSchema(dir, "evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func evidenceTestRecords() []taskTranscriptRecord {
	return []taskTranscriptRecord{
		{Event: "tool_call", Tool: "read_document", ToolCallID: "observed", Input: `{"key":"document-a"}`},
		{Event: "tool_result", Tool: "read_document", ToolCallID: "observed", Output: `{"text":"the exact words in a document"}`},
	}
}

func TestResultContractToolEvidence(t *testing.T) {
	compiled := evidenceTestContract(t)
	for _, name := range []string{"success", "absent", "failed", "wrong_target", "wrong_quote", "spliced_quote", "search", "unpaired", "empty", "malformed"} {
		t.Run(name, func(t *testing.T) {
			records := evidenceTestRecords()
			switch name {
			case "absent":
				records = nil
			case "failed":
				records[1].Error = true
			case "wrong_target":
				records[0].Input = `{"key":"document-b"}`
			case "wrong_quote":
				records[1].Output = `{"text":"other words"}`
			case "spliced_quote":
				records[1].Output = `{"text":"the exact ... words in a document"}`
			case "search":
				records[0].Tool = "search"
				records[1].Tool = "search"
			case "unpaired":
				records = records[1:]
			case "empty":
				records[1].Output = `{"text":""}`
			case "malformed":
				records[1].Output = `{"text":"truncated`
			}
			payload, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(evidenceTestPayload), records)
			if err != nil {
				t.Fatal(err)
			}
			value, err := decodeResultContractJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			verdict, call := evidenceString(value, "/findings/0/verdict"), evidenceString(value, "/findings/0/sources/0/call_id")
			if name == "success" {
				if verdict != "supported" || call != "observed" || payload.EvidenceDowngrades != 0 {
					t.Fatalf("bad binding: %s", payload.Value)
				}
			} else {
				if verdict != "unverified" || call != "" || payload.EvidenceDowngrades != 1 || evidenceString(value, "/findings/0/sources/0/quote") != "" {
					t.Fatalf("not downgraded: %s", payload.Value)
				}
				if !strings.Contains(evidenceString(value, "/findings/0/limitations"), "Original limitation.") {
					t.Fatal("original limits lost")
				}
			}
		})
	}
}

func TestExternalToolEvidenceFailsClosed(t *testing.T) {
	compiled := evidenceTestContract(t)
	ref := compiled.ref(true)
	payload, err := canonicalExternalStructuredPayload(AttemptRequest{Task: TaskDef{ResultContract: &ref}, resultContractSchema: compiled}, &WorkerResultProposal{StructuredPayloadJSON: new(evidenceTestPayload)})
	if err != nil || payload.EvidenceDowngrades != 1 {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}
}

func TestSubmitResultUsesOnlyOwnTranscript(t *testing.T) {
	for _, scope := range []string{"own", "other_task", "other_run", "other_attempt", "other_agent"} {
		t.Run(scope, func(t *testing.T) {
			compiled := evidenceTestContract(t)
			ref := compiled.ref(true)
			c := &Coordinator{session: &TeamSession{ResultContracts: map[string]*CompiledResultContract{compiled.ID: compiled}}, taskTracker: NewTaskTracker()}
			c.executionRunID = "run-evidence"
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", ResultContract: &ref}})[0]
			id := item.ID
			run, attempt, worker := "run-evidence", 1, "worker"
			switch scope {
			case "other_task":
				id = "other-task"
			case "other_run":
				run = "other-run"
			case "other_attempt":
				attempt = 2
			case "other_agent":
				worker = "other-worker"
			}
			transcript, err := newTaskTranscriptForAttempt(t.TempDir(), id, run, attempt, worker)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = transcript.f.Close() }()
			for _, r := range evidenceTestRecords() {
				if r.Event == "tool_call" {
					err = transcript.RecordToolCall(r.ToolCallID, r.Tool, r.Input)
				} else {
					err = transcript.RecordToolResult(r.ToolCallID, r.Tool, r.Output, r.Error)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.WithValue(occurrenceTestContext(c, item.ID, 1), taskTranscriptKey{}, transcript)
			response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"all proven","structured_payload":` + evidenceTestPayload + `}`})
			if err != nil || response.IsError {
				t.Fatalf("response=%#v err=%v", response, err)
			}
			stored := c.GetTaskResult(item.ID)
			if stored == nil || (stored.StructuredPayload.EvidenceDowngrades == 0) != (scope == "own") {
				t.Fatalf("stored=%#v", stored)
			}
			if scope != "own" && stored.Summary == "all proven" {
				t.Fatal("unsafe summary survived")
			}
			encoded, err := json.Marshal(map[string]any{"typed_result": stored})
			if err != nil {
				t.Fatal(err)
			}
			redacted, err := redactJSONPreservingRuntimeOutputs(encoded)
			if err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, redacted); err != nil {
				t.Fatal(err)
			}
			var restored struct {
				TypedResult TaskResult `json:"typed_result"`
			}
			if err := json.Unmarshal(compact.Bytes(), &restored); err != nil {
				t.Fatal(err)
			}
			if restored.TypedResult.StructuredPayload.SHA256 != stored.StructuredPayload.SHA256 || string(restored.TypedResult.StructuredPayload.Value) != string(stored.StructuredPayload.Value) {
				t.Fatal("binding changed in persistence")
			}
		})
	}
}

func TestToolEvidenceIgnoresWritableTranscript(t *testing.T) {
	transcript, err := newTaskTranscriptForAttempt(t.TempDir(), "task", "run", 1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	var forged bytes.Buffer
	for _, record := range evidenceTestRecords() {
		if err := json.NewEncoder(&forged).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(transcript.path, forged.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	records := transcript.evidenceRecords()
	if len(records) != 0 {
		t.Fatalf("accepted disk-authored evidence: records=%#v", records)
	}
}

func TestFinishSealsContractReport(t *testing.T) {
	compiled := evidenceTestContract(t)
	ref := compiled.ref(true)
	payload, err := validateStructuredResultPayload(compiled, ref, []byte(evidenceTestPayload))
	if err != nil {
		t.Fatal(err)
	}
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", ResultContract: &ref}})[0]
	item.Status = TaskDone
	item.TypedResult = &TaskResult{Status: TaskResultStatusSuccess, StructuredPayload: payload}
	response, err := (&finishTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Input: `{"response":"Everything is certainly true"}`})
	if err != nil || response.IsError || !c.finishCalled.Load() {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	final := c.LastRunResult()
	if final == nil || !strings.Contains(final.Response, "Verdict: unverified") || strings.Contains(final.Response, "certainly") || !strings.Contains(final.Response, "Original limitation.") {
		t.Fatalf("unsealed final result: %#v", final)
	}
	if got := c.canonicalFinishedResponse("FINISHED:Everything is true"); got != final.Response {
		t.Fatalf("final projection changed report: %q", got)
	}
}

func TestContractFinalReportPreservesEvidence(t *testing.T) {
	compiled := evidenceTestContract(t)
	ref := compiled.ref(true)
	payload, err := validateStructuredResultPayload(compiled, ref, []byte(evidenceTestPayload))
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{ResultContracts: map[string]*CompiledResultContract{compiled.ID: compiled}}, taskTracker: NewTaskTracker()}
	if _, configured, err := c.renderedContractFinalReport(); !configured || err == nil {
		t.Fatal("missing report did not fail closed")
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", ResultContract: &ref}})[0]
	item.Status = TaskDone
	item.TypedResult = &TaskResult{Status: TaskResultStatusSuccess, StructuredPayload: payload}
	text, configured, err := c.renderedContractFinalReport()
	if err != nil || !configured || !strings.Contains(text, "Verdict: unverified") || !strings.Contains(text, "Original limitation.") || !strings.Contains(text, "Source: document-a") || strings.Contains(text, "conclusive") {
		t.Fatalf("report=%q err=%v", text, err)
	}
	payload.SHA256 = "forged"
	if _, _, err := c.renderedContractFinalReport(); err == nil {
		t.Fatal("tampered payload accepted")
	}
}

func TestToolEvidenceEmptyAndMixedSources(t *testing.T) {
	compiled := evidenceTestContract(t)
	for _, sources := range []string{`[]`, `[{"key":"document-a","state":"checked","quote":"exact words"},{"key":"document-b","state":"checked","quote":"missing"}]`} {
		value, err := decodeResultContractJSON([]byte(evidenceTestPayload))
		if err != nil {
			t.Fatal(err)
		}
		items, err := decodeResultContractJSON([]byte(sources))
		if err != nil {
			t.Fatal(err)
		}
		group := value.(map[string]any)["findings"].([]any)[0].(map[string]any)
		group["sources"] = items
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := validateStructuredResultPayload(compiled, compiled.ref(true), raw, evidenceTestRecords())
		if err != nil || payload.EvidenceDowngrades != 1 {
			t.Fatalf("payload=%#v err=%v", payload, err)
		}
		value, err = decodeResultContractJSON(payload.Value)
		if err != nil || evidenceString(value, "/findings/0/verdict") != "unverified" {
			t.Fatalf("group not downgraded: %s err=%v", payload.Value, err)
		}
	}
}

func TestResultContractExtensionsRejectInvalidPolicy(t *testing.T) {
	for _, spec := range []string{`null`, `{"unknown":true}`, `{"groups_pointer":"invalid"}`} {
		if _, _, err := compileResultContractExtensions(map[string]any{"x-hufu-tool-evidence": json.RawMessage(spec)}); err == nil {
			t.Fatalf("accepted %s", spec)
		}
	}
	if _, _, err := compileResultContractExtensions(map[string]any{"x-hufu-final-report": "{{invalid_function}}"}); err == nil {
		t.Fatal("invalid template accepted")
	}
}

func TestAggregateResearchDoesNotClaimFixedAndVerified(t *testing.T) {
	completed := &RunResult{GoalSatisfied: true, Outcome: RunOutcomeCompleted, GoalMode: GoalModeOutcome, Acceptance: &AcceptanceResult{State: AcceptancePassed}}
	aggregated := AggregateRunResults([]*RunResult{completed}, nil, RunStats{})
	if aggregated.FixedAndVerified || FormatCanonicalStatus(&aggregated) != "Execution completed successfully" {
		t.Fatalf("overstated completion: %#v", aggregated)
	}
	completed.FixedAndVerified = true
	aggregated = AggregateRunResults([]*RunResult{completed}, nil, RunStats{})
	if !aggregated.FixedAndVerified {
		t.Fatal("explicit verification lost")
	}
}
