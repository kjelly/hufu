package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestHufuCodeReviewRuntimeOwnsAll51GapIdentities(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	c := newBudgetCoordinator(t)
	c.session.ResultContracts = session.ResultContracts
	sourceSchema := session.ResultContracts["schemas/review-v1.json"]
	reportSchema := session.ResultContracts["schemas/report-v2.json"]
	if sourceSchema == nil || reportSchema == nil {
		t.Fatal("loaded team lacks its review/report contracts")
	}
	var ids []string
	var inputs []any
	for i, count := range []int{25, 24, 2} {
		gaps := make([]any, 0, count)
		for j := range count {
			gaps = append(gaps, map[string]any{"description": fmt.Sprintf("local limitation %d/%d", i, j), "missing_evidence": "independent evidence"})
		}
		record := map[string]any{"kind": "review", "review_revision": "head", "workset_key": fmt.Sprintf("unit-%04d", i), "lens": "general", "findings": []any{}, "gaps": gaps, "invariant_assessments": []any{}, "reference_assessments": []any{}}
		raw, err := json.Marshal(map[string]any{"record": record})
		if err != nil {
			t.Fatal(err)
		}
		ref := sourceSchema.ref(true)
		source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", ResultContract: &ref, WorksetBinding: &WorksetBinding{Bindings: map[string]string{"reference_candidate_ids": "[]"}}}})[0]
		payload, err := validateStructuredResultPayload(sourceSchema, ref, raw)
		if err != nil {
			t.Fatal(err)
		}
		source.Status = TaskDone
		source.TypedResult = &TaskResult{TaskID: source.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload}
		ids = append(ids, source.ID)
		inputs = append(inputs, map[string]any{"task_id": source.ID, "payload_sha256": payload.SHA256, "record": record})
	}
	ref := reportSchema.ref(true)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "synthesizer", ResultContract: &ref, EvidenceFrom: ids}})[0]
	plans, err := c.fixedCollectionPlans(item.ID)
	if err != nil || len(plans) != 1 || len(plans[0].rows) != 51 {
		t.Fatalf("wrong runtime gap universe: plans=%d err=%v", len(plans), err)
	}
	decisions := make(map[string]any)
	for _, row := range plans[0].rows {
		decisions[row.Key] = map[string]any{"status": "retained", "resolving_task_id": "", "evidence": []any{}, "explanation": "bounded evidence did not resolve this limitation"}
	}
	value, err := decodeResultContractJSON([]byte(hufuCodeReviewReportFixture))
	if err != nil {
		t.Fatal(err)
	}
	report := value.(map[string]any)
	delete(report, "input_review")
	bindHufuCodeReviewReceiptFixture(t, c, report)
	report["coverage"], report["gap_resolutions"] = "completed_with_gaps", decisions
	// Even an exact worker-authored copy is forbidden. The runtime owns the
	// complete source records and hashes, leaving only semantic decisions to
	// the synthesizer instead of requiring a large model-generated echo.
	report["input_review"] = inputs
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, rejection := c.structuredPayloadForSubmission(item.ID, raw); !strings.Contains(rejection, "omit runtime-owned evidence inputs") {
		t.Fatalf("worker source copy was accepted: %s", rejection)
	}
	delete(report, "input_review")
	decisions["invented_gap_2"] = decisions[plans[0].rows[0].Key]
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if payload, rejection := c.structuredPayloadForSubmission(item.ID, raw); payload != nil || !strings.Contains(rejection, "exactly 51") {
		t.Fatalf("52nd gap was not rejected before publication: %s", rejection)
	}
	delete(decisions, "invented_gap_2")
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	payload, rejection := c.structuredPayloadForSubmission(item.ID, raw)
	if rejection != "" {
		t.Fatal(rejection)
	}
	canonical, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		t.Fatal(err)
	}
	actualInputs, err := json.Marshal(canonical.(map[string]any)["input_review"], json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	expectedInputs, err := json.Marshal(inputs, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	if string(actualInputs) != string(expectedInputs) {
		t.Fatal("runtime-owned report inputs did not preserve every source record and hash")
	}
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	shape := tool.Info().Parameters["structured_payload"].(map[string]any)
	for _, field := range []string{"input_review", "scope", "test_verification"} {
		if _, advertised := shape["properties"].(map[string]any)[field]; advertised {
			t.Fatalf("runtime-owned %s advertised as model output", field)
		}
	}
	rows := canonical.(map[string]any)["gap_resolutions"].([]any)
	if len(rows) != 51 || rows[50].(map[string]any)["source_task_id"] != ids[2] || fmt.Sprint(rows[50].(map[string]any)["gap_index"]) != "1" {
		t.Fatal("runtime did not preserve the 51 original source/index pairs")
	}
	// The schema exposes the finite semantic choice, but the existing evidence
	// contract must still reject a resolution with no observed artifact.
	first := decisions[plans[0].rows[0].Key].(map[string]any)
	first["status"] = "resolved_cross_workset"
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, rejection := c.structuredPayloadForSubmission(item.ID, raw); rejection == "" {
		t.Fatal("a finite choice bypassed evidence requirements")
	}
}

func TestHufuCodeReviewEmptyCritiqueOwnsSource(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	c := newBudgetCoordinator(t)
	c.session.ResultContracts = session.ResultContracts
	c.session.Workspace = t.TempDir()
	store, err := NewEventStore(c.session.Workspace, "empty-critique", "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetBranchID("main")
	c.eventStore, c.eventJournal = store, eventStoreJournal{store: store}
	sourceSchema := session.ResultContracts["schemas/review-v1.json"]
	criticSchema := session.ResultContracts["schemas/critique-v2.json"]
	sourceRef, criticRef := sourceSchema.ref(true), criticSchema.ref(true)
	source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", ResultContract: &sourceRef, WorksetBinding: &WorksetBinding{Bindings: map[string]string{"reference_candidate_ids": "[]"}}}})[0]
	raw := []byte(`{"record":{"kind":"review","review_revision":"head","workset_key":"unit-0000","lens":"general","findings":[],"gaps":[],"invariant_assessments":[],"reference_assessments":[]}}`)
	payload, err := validateStructuredResultPayload(sourceSchema, sourceRef, raw)
	if err != nil {
		t.Fatal(err)
	}
	source.Status = TaskDone
	source.TypedResult = &TaskResult{TaskID: source.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "critic", ResultContract: &criticRef, EvidenceFrom: []string{source.ID}}})[0]
	result, rejection := c.structuredPayloadForSubmission(item.ID, []byte(`{"audit_complete":true,"record":{"kind":"critique","decisions":{}}}`))
	if rejection != "" {
		t.Fatal(rejection)
	}
	value, err := decodeResultContractJSON(result.Value)
	if err != nil {
		t.Fatal(err)
	}
	record := value.(map[string]any)["record"].(map[string]any)
	inputs := record["input_review"].([]any)
	if len(inputs) != 1 || evidenceString(inputs[0], "/task_id") != source.ID || evidenceString(inputs[0], "/payload_sha256") != payload.SHA256 || len(record["decisions"].([]any)) != 0 {
		t.Fatalf("incorrect canonical source or empty coverage: %s", result.Value)
	}
	// Even copying the exact accepted source is forbidden at the worker boundary.
	record["decisions"] = map[string]any{}
	raw, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, rejection = c.structuredPayloadForSubmission(item.ID, raw); !strings.Contains(rejection, "runtime-owned evidence inputs") {
		t.Fatalf("worker source override accepted: %s", rejection)
	}
	// Synthesis owns neither the review nor the nested critic record. The
	// stored report must preserve both complete accepted records and hashes.
	item.Status = TaskDone
	item.TypedResult = &TaskResult{TaskID: item.ID, Status: TaskResultStatusSuccess, StructuredPayload: result}
	reportRef := session.ResultContracts["schemas/report-v2.json"].ref(true)
	reportItem := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "synthesizer", ResultContract: &reportRef, EvidenceFrom: []string{source.ID, item.ID}}})[0]
	reportValue, err := decodeResultContractJSON([]byte(hufuCodeReviewReportFixture))
	if err != nil {
		t.Fatal(err)
	}
	report := reportValue.(map[string]any)
	delete(report, "input_review")
	expectedReceipts := bindHufuCodeReviewReceiptFixture(t, c, report)
	report["gap_resolutions"] = map[string]any{}
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	reportPayload, rejection := c.structuredPayloadForSubmission(reportItem.ID, raw)
	if rejection != "" {
		t.Fatal(rejection)
	}
	stored, err := decodeResultContractJSON(reportPayload.Value)
	if err != nil {
		t.Fatal(err)
	}
	canonical := stored.(map[string]any)
	for field, expected := range expectedReceipts {
		actual, err := json.Marshal(canonical[field], json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(expected, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, want) {
			t.Fatalf("runtime lost complete %s receipt", field)
		}
		report[field] = expected
		override, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if _, rejection := c.structuredPayloadForSubmission(reportItem.ID, override); !strings.Contains(rejection, "runtime-owned") {
			t.Fatalf("model supplied %s accepted: %s", field, rejection)
		}
		delete(report, field)
	}
	encoded, err := json.Marshal(reportPayload)
	if err != nil {
		t.Fatal(err)
	}
	var restored ResultPayload
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Value, reportPayload.Value) || restored.SHA256 != reportPayload.SHA256 || c.validateSubmittedEvidenceInputs(reportItem.ID, &restored) != nil {
		t.Fatal("complete report receipts did not survive persistence/replay")
	}
	reportInputs := canonical["input_review"].([]any)
	if len(reportInputs) != 2 || evidenceString(reportInputs[1], "/task_id") != item.ID || evidenceString(reportInputs[1], "/payload_sha256") != result.SHA256 || evidenceString(reportInputs[1], "/record/kind") != "critique" || evidenceString(reportInputs[1], "/record/input_review/0/task_id") != source.ID || evidenceString(reportInputs[1], "/record/input_review/0/payload_sha256") != payload.SHA256 {
		t.Fatal("runtime-owned report lost nested critic source identity or hash")
	}
}

// Supply native action receipts using the same fixture and verification path as
// fact_refs. Models submit neither the scope nor the failed-test diagnostic.
func bindHufuCodeReviewReceiptFixture(t *testing.T, c *Coordinator, report map[string]any) map[string]any {
	t.Helper()
	baseline, template := runtimeOutputRefFixture(t)
	c.executionRunID = baseline.executionRunID
	c.sessionData = baseline.sessionData
	c.session.RunInputDefinitions = baseline.session.RunInputDefinitions
	test := report["test_verification"].(map[string]any)
	test["completed"], test["passed"], test["exit_code"] = true, false, 1
	test["diagnostic"] = strings.Repeat("FAIL snapshot α: expected {suffix}\n", 1000)
	expected := map[string]any{"scope": report["scope"], "test_verification": test}
	for field, sourceID := range map[string]string{"scope": "produce-workset", "test_verification": "verify-targeted-go-tests"} {
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runtime", PlanTaskID: sourceID, ContractID: sourceID}})[0]
		id := item.ID
		*item = *template
		item.ID, item.PlanTaskID, item.ContractID = id, sourceID, sourceID
		result := cloneTaskResult(template.TypedResult)
		item.TypedResult = result
		result.TaskID = id
		receipt := *template.ExecutionReceipt
		receipt.TaskID = id
		item.ExecutionReceipt = &receipt
		output := field
		if field == "test_verification" {
			output = "targeted_go_tests"
		}
		outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{output: expected[field]})
		if err != nil {
			t.Fatal(err)
		}
		result.RuntimeOutputs, result.RuntimeOutputsHash = outputs, digest
		receipt.RuntimeOutputsHash = digest
		c.storeSubmittedTaskResult(id, result)
		delete(report, field)
	}
	return expected
}

func TestHufuCodeReviewCriticSubmitsOnlyDecisionsForAllFindings(t *testing.T) {
	for _, route := range []string{"local", "external"} {
		t.Run(route, func(t *testing.T) {
			session := loadHufuCodeReviewTeam(t)
			c := newBudgetCoordinator(t)
			c.session.ResultContracts = session.ResultContracts
			sourceSchema := session.ResultContracts["schemas/review-v1.json"]
			// The active team uses v2 bound decisions. Retain v1 submission
			// compatibility coverage for both local and external workers.
			criticSchema, err := compileResultContractSchema(hufuCodeReviewTeamDir(t), "schemas/critique-v1.json")
			if err != nil {
				t.Fatal(err)
			}
			session.ResultContracts[criticSchema.ID] = criticSchema
			findings := make([]any, 6)
			for i := range findings {
				findings[i] = map[string]any{"summary": fmt.Sprintf("finding %d", i), "detail": "source detail", "severity": "info", "location": "file.go:1", "evidence": []any{"sha256-" + strings.Repeat("a", 64)}, "requires_critique": i%2 == 0}
			}
			record := map[string]any{"kind": "review", "review_revision": "head", "workset_key": "unit-0002", "lens": "documentation-risk", "findings": findings, "gaps": []any{}, "invariant_assessments": []any{}, "reference_assessments": []any{}}
			raw, err := json.Marshal(map[string]any{"record": record})
			if err != nil {
				t.Fatal(err)
			}
			sourceRef := sourceSchema.ref(true)
			source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", ResultContract: &sourceRef, WorksetBinding: &WorksetBinding{Bindings: map[string]string{"reference_candidate_ids": "[]"}}}})[0]
			sourcePayload, err := validateStructuredResultPayload(sourceSchema, sourceRef, raw)
			if err != nil {
				t.Fatal(err)
			}
			source.Status, source.TypedResult = TaskDone, &TaskResult{TaskID: source.ID, Status: TaskResultStatusSuccess, StructuredPayload: sourcePayload}
			ref := criticSchema.ref(true)
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "critic", ResultContract: &ref, EvidenceFrom: []string{source.ID}}})[0]
			plans, err := c.fixedCollectionPlans(item.ID)
			if err != nil || len(plans) != 1 || len(plans[0].rows) != 6 {
				t.Fatalf("wrong finding universe: plans=%d err=%v", len(plans), err)
			}
			decisions := make(map[string]any)
			for _, row := range plans[0].rows {
				decisions[row.Key] = map[string]any{"verdict": "unverified", "severity": "info", "rationale": "insufficient independent evidence", "evidence": []any{}, "missing_evidence": "caller behavior unavailable"}
			}
			body := map[string]any{"audit_complete": "true", "record": map[string]any{"kind": "critique", "decisions": decisions}}
			submit := func() (*ResultPayload, string) {
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				if route == "external" {
					request := AttemptRequest{Task: taskDefFromTodoItem(item), resultContractSchema: criticSchema, resultEvidenceInputs: c.dependencyResultsForTask(item.ID)}
					payload, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(string(raw))}, nil)
					if err != nil {
						return nil, err.Error()
					}
					return payload, ""
				}
				response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{Input: `{"status":"completed_with_gaps","summary":"audited all findings","structured_payload":` + string(raw) + `}`})
				if err != nil {
					t.Fatal(err)
				}
				if response.IsError {
					return nil, response.Content
				}
				return c.GetTaskResult(item.ID).StructuredPayload, ""
			}
			first := decisions[plans[0].rows[0].Key].(map[string]any)
			first["finding_index"] = 5
			if _, rejection := submit(); rejection == "" {
				t.Fatal("model-supplied index accepted")
			}
			delete(first, "finding_index")
			decisions["invented_key"] = first
			if _, rejection := submit(); rejection == "" {
				t.Fatal("invented membership accepted")
			}
			delete(decisions, "invented_key")
			body["record"].(map[string]any)["input_review"] = []any{}
			if _, rejection := submit(); rejection == "" {
				t.Fatal("model-supplied source copy accepted")
			}
			delete(body["record"].(map[string]any), "input_review")
			payload, rejection := submit()
			if rejection != "" {
				t.Fatal(rejection)
			}
			if err := c.validateSubmittedEvidenceInputs(item.ID, payload); err != nil {
				t.Fatal(err)
			}
			value, err := decodeResultContractJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			stored := value.(map[string]any)["record"].(map[string]any)
			inputs := stored["input_review"].([]any)
			entry := inputs[0].(map[string]any)
			if len(inputs) != 1 || entry["task_id"] != source.ID || entry["payload_sha256"] != sourcePayload.SHA256 {
				t.Fatal("source identity or hash was lost")
			}
			for i, raw := range stored["decisions"].([]any) {
				if fmt.Sprint(raw.(map[string]any)["finding_index"]) != fmt.Sprint(i) {
					t.Fatal("runtime did not preserve ascending finding indices")
				}
			}
			record["review_revision"] = "replacement"
			raw, err = json.Marshal(map[string]any{"record": record})
			if err != nil {
				t.Fatal(err)
			}
			source.TypedResult.StructuredPayload, err = validateStructuredResultPayload(sourceSchema, sourceRef, raw)
			if err != nil {
				t.Fatal(err)
			}
			if c.validateSubmittedEvidenceInputs(item.ID, payload) == nil {
				t.Fatal("old accepted result survived source replacement")
			}
			if _, rejection := submit(); rejection == "" {
				t.Fatal("stale decision keys accepted")
			}
		})
	}
}
