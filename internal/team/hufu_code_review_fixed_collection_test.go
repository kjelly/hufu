package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestHufuCodeReviewRuntimeOwnsAll51GapIdentities(t *testing.T) {
	t.Parallel()
	session := loadHufuCodeReviewTeam(t)
	c := newTestCoordinator(t)
	c.session.ResultContracts = session.ResultContracts
	sourceSchema := hufuCodeReviewLegacySchema(t, session, "schemas/review-v1.json")
	reportSchema := hufuCodeReviewLegacySchema(t, session, "schemas/report-v2.json")
	if sourceSchema == nil || reportSchema == nil {
		t.Fatal("loaded team lacks its review/report contracts")
	}
	var ids []string
	var inputs []any
	for i, count := range []int{25, 24, 2} {
		schema, lens := sourceSchema, "general"
		if i == 2 {
			schema, lens = hufuCodeReviewLegacySchema(t, session, "schemas/documentation-review-v1.json"), "documentation-risk"
		}
		gaps := make([]any, 0, count)
		for j := range count {
			gaps = append(gaps, map[string]any{"description": fmt.Sprintf("local limitation %d/%d", i, j), "missing_evidence": "independent evidence", "kind": "semantic_uncertainty", "requested_evidence": []any{}, "limitations": []any{}})
		}
		key := fmt.Sprintf("unit-%04d", i)
		if i == 1 {
			key = "supplement-0007"
		}
		record := map[string]any{"kind": "review", "review_revision": "head", "workset_key": key, "lens": lens, "findings": []any{}, "gaps": gaps, "invariant_assessments": []any{}, "reference_assessments": []any{}}
		raw, err := json.Marshal(map[string]any{"record": record})
		if err != nil {
			t.Fatal(err)
		}
		ref := schema.ref(true)
		source := seedReviewSourceWorkset(t, c, TodoSpec{Agent: "reviewer", ResultContract: &ref}, key, "head", lens)
		payload, err := validateStructuredResultPayload(schema, ref, raw)
		if err != nil {
			t.Fatal(err)
		}
		source.Status = TaskDone
		source.TypedResult = &TaskResult{TaskID: source.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload}
		ids = append(ids, source.ID)
		inputs = append(inputs, map[string]any{"task_id": source.ID, "payload_sha256": payload.SHA256, "record": record})
	}
	ref := reportSchema.ref(true)
	prompt := resultContractPromptSection(&ref, reportSchema, false)
	if !strings.Contains(prompt, "```json\n") || !strings.Contains(prompt, `"gap_resolutions":`) {
		t.Fatal("report submission schema does not fit in the runtime prompt")
	}
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
	decisions["invented_gap_2"] = map[string]any{"status": "resolved_cross_workset", "resolving_task_id": "foreign", "evidence": []any{"sha256-" + strings.Repeat("a", 64)}}
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if payload, err, repairable := c.validateStructuredPayloadClaims(t.Context(), item.ID, raw, false); payload != nil || err == nil || repairable || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("foreign producer in an extra row was not independently rejected: %v (repairable=%t)", err, repairable)
	}
	delete(decisions, "invented_gap_2")
	firstKey := plans[0].rows[0].Key
	firstDecision := decisions[firstKey]
	delete(decisions, firstKey)
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if payload, err, repairable := c.validateStructuredPayloadClaims(t.Context(), item.ID, raw, false); payload != nil || err == nil || !repairable || !strings.Contains(err.Error(), "exactly 51") {
		t.Fatalf("missing model decision lost its shape diagnostic: %v (repairable=%t)", err, repairable)
	}
	decisions[firstKey] = firstDecision
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
	for _, field := range []string{"input_review", "scope", "test_verification", "snapshot_coverage", "supplement_outcomes"} {
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
	t.Parallel()
	for _, tc := range []struct{ name, schema, key, lens string }{
		{"documentation", "schemas/documentation-review-v1.json", "unit-0000", "documentation-risk"},
		{"supplement", "schemas/review-v1.json", "supplement-0000", "general"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := loadHufuCodeReviewTeam(t)
			c := newTestCoordinator(t)
			c.session.ResultContracts = session.ResultContracts
			store, err := NewEventStore(c.session.Workspace, "empty-critique", "session")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			store.SetBranchID("main")
			c.eventStore, c.eventJournal = store, eventStoreJournal{store: store}
			sourceSchema := hufuCodeReviewLegacySchema(t, session, tc.schema)
			criticSchema := hufuCodeReviewLegacySchema(t, session, "schemas/critique-v2.json")
			sourceRef, criticRef := sourceSchema.ref(true), criticSchema.ref(true)
			sourceSpec := TodoSpec{Agent: "reviewer", ResultContract: &sourceRef}
			if tc.name == "documentation" {
				// Match the source identity in the native final-audit fixture.
				c.taskTracker.TodoList().advanceIDFloor(12)
				sourceSpec.ContractID = "review-documentation-workset"
			}
			source := seedReviewSourceWorkset(t, c, sourceSpec, tc.key, "frozen-head", tc.lens)
			raw := fmt.Appendf(nil, `{"record":{"kind":"review","review_revision":"frozen-head","workset_key":%q,"lens":%q,"findings":[],"gaps":[],"invariant_assessments":[],"reference_assessments":[]}}`, tc.key, tc.lens)
			payload, err := validateStructuredResultPayload(sourceSchema, sourceRef, raw)
			if err != nil {
				t.Fatal(err)
			}
			source.Status = TaskDone
			source.TypedResult = &TaskResult{TaskID: source.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "critic", ResultContract: &criticRef, EvidenceFrom: []string{source.ID}}})[0]
			assertCritiqueClaimsHaveOneSemanticLocation(t, c, item.ID)
			_, err = c.decodeCollectionClaims(item.ID, []byte(`{"status":"success","summary":"audit complete","audit_complete":true,"structured_payload":{"record":{"kind":"critique","decisions":{}}}}`))
			if err == nil || localResultRejectionReason(err) != RepairFailureInvalidSchema || !strings.Contains(err.Error(), "audit_complete") {
				t.Fatalf("historical collector accepted the misplaced completion field: %v", err)
			}
			for _, marker := range []string{"true", "false", "null"} {
				raw := fmt.Appendf(nil, `{"audit_complete":%s,"record":{"kind":"critique","decisions":{}}}`, marker)
				if result, rejection := c.structuredPayloadForSubmission(item.ID, raw); result != nil || !strings.Contains(rejection, "/audit_complete is runtime-owned") {
					t.Fatalf("worker completion override %s was not rejected: %s", marker, rejection)
				}
			}
			tool := (&submitResultTool{coordinator: c, todoID: item.ID}).Info()
			shape := tool.Parameters["structured_payload"].(map[string]any)
			if properties, _ := shape["properties"].(map[string]any); properties["audit_complete"] != nil {
				t.Fatal("completion field was exposed to the model")
			}
			result, rejection := c.structuredPayloadForSubmission(item.ID, []byte(`{"record":{"kind":"critique","decisions":{}}}`))
			if rejection != "" {
				t.Fatal(rejection)
			}
			if tc.name == "documentation" {
				fixture, err := os.ReadFile(filepath.Join(hufuCodeReviewTeamDir(t), "reviewgate", "testdata", "direct-empty-critique.json"))
				if err != nil {
					t.Fatal(err)
				}
				raw, err := decodeResultContractJSON(fixture)
				if err != nil {
					t.Fatal(err)
				}
				expected, err := json.Marshal(raw, json.Deterministic(true))
				if err != nil || !bytes.Equal(expected, result.Value) {
					t.Fatalf("native audit fixture differs from runtime assembly: %s (%v)", result.Value, err)
				}
			}
			value, err := decodeResultContractJSON(result.Value)
			if err != nil {
				t.Fatal(err)
			}
			storedCritique := value.(map[string]any)
			if storedCritique["audit_complete"] != true {
				t.Fatal("runtime did not complete the validated empty collection")
			}
			record := storedCritique["record"].(map[string]any)
			inputs := record["input_review"].([]any)
			if len(inputs) != 1 || evidenceString(inputs[0], "/task_id") != source.ID || evidenceString(inputs[0], "/payload_sha256") != payload.SHA256 || len(record["decisions"].([]any)) != 0 {
				t.Fatalf("incorrect canonical source or empty coverage: %s", result.Value)
			}
			// Even copying the exact accepted source is forbidden at the worker boundary.
			record["decisions"] = map[string]any{}
			delete(storedCritique, "audit_complete")
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
			reportRef := hufuCodeReviewLegacySchema(t, session, "schemas/report-v2.json").ref(true)
			reportItem := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "synthesizer", ResultContract: &reportRef, EvidenceFrom: []string{source.ID, item.ID}}})[0]
			reportValue, err := decodeResultContractJSON([]byte(hufuCodeReviewReportFixture))
			if err != nil {
				t.Fatal(err)
			}
			report := reportValue.(map[string]any)
			delete(report, "input_review")
			outcomes := []any{}
			if tc.name == "supplement" {
				outcomes = []any{map[string]any{"source_task_id": source.ID, "gap_index": 0, "kind": "missing_dependency", "workset_key": "", "limitations": []any{map[string]any{"kind": "unavailable_path", "detail": "Requested source is absent from the frozen revision"}}}}
			}
			expectedReceipts := bindHufuCodeReviewReceiptFixture(t, c, report, outcomes)
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
			if !bytes.Equal(restored.Value, reportPayload.Value) || restored.SHA256 != reportPayload.SHA256 || c.validateSubmittedEvidenceInputs(t.Context(), reportItem.ID, &restored) != nil {
				t.Fatal("complete report receipts did not survive persistence/replay")
			}
			reportInputs := canonical["input_review"].([]any)
			if len(reportInputs) != 2 || evidenceString(reportInputs[1], "/task_id") != item.ID || evidenceString(reportInputs[1], "/payload_sha256") != result.SHA256 || evidenceString(reportInputs[1], "/record/kind") != "critique" || evidenceString(reportInputs[1], "/record/input_review/0/task_id") != source.ID || evidenceString(reportInputs[1], "/record/input_review/0/payload_sha256") != payload.SHA256 {
				t.Fatal("runtime-owned report lost nested critic source identity or hash")
			}
		})
	}
}

// Supply native action receipts using the same fixture and verification path as
// fact_refs. Models submit neither the scope nor the failed-test diagnostic.
func bindHufuCodeReviewReceiptFixture(t *testing.T, c *Coordinator, report map[string]any, outcomes ...[]any) map[string]any {
	t.Helper()
	baseline, template := runtimeOutputRefFixture(t)
	c.sessionData = baseline.sessionData
	c.session.RunInputDefinitions = baseline.session.RunInputDefinitions
	test := report["test_verification"].(map[string]any)
	test["completed"], test["passed"], test["exit_code"] = true, false, 1
	test["diagnostic"] = strings.Repeat("FAIL snapshot α: expected {suffix}\n", 1000)
	expected := map[string]any{
		"scope": report["scope"], "test_verification": test,
		"supplement_outcomes": []any{},
		"snapshot_coverage": map[string]any{
			"mode": "diff", "snapshot_id": "", "content_sha256": "", "revision": "",
			"batch": 0, "batch_count": 0, "expected": []any{}, "completed": []any{}, "excluded": []any{}, "uncompleted": []any{},
		},
	}
	if len(outcomes) > 0 {
		expected["supplement_outcomes"] = outcomes[0]
	}
	for field, sourceID := range map[string]string{
		"scope": "produce-workset", "test_verification": "verify-targeted-go-tests", "snapshot_coverage": "inventory-review-handoffs",
	} {
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runtime", PlanTaskID: sourceID, ContractID: sourceID}})[0]
		id := item.ID
		*item = *template
		item.ID, item.PlanTaskID, item.ContractID = id, sourceID, sourceID
		result := cloneTaskResult(template.TypedResult)
		item.TypedResult = result
		result.TaskID = id
		receipt := *template.ExecutionReceipt
		receipt.TaskID, receipt.RunID = id, c.executionRunID
		item.ExecutionReceipt = &receipt
		output := field
		if field == "test_verification" {
			output = "targeted_go_tests"
		}
		nativeOutputs := map[string]any{output: expected[field]}
		if field == "snapshot_coverage" {
			nativeOutputs["supplement_outcomes"] = expected["supplement_outcomes"]
			delete(report, "supplement_outcomes")
		}
		outputs, digest, err := CanonicalizeRuntimeOutputs(nativeOutputs)
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
	t.Parallel()
	for _, route := range []string{"local", "external"} {
		t.Run(route, func(t *testing.T) {
			session := loadHufuCodeReviewTeam(t)
			c := newTestCoordinator(t)
			c.session.ResultContracts = session.ResultContracts
			sourceSchema := hufuCodeReviewLegacySchema(t, session, "schemas/review-v1.json")
			// The active team uses v3 bound decisions. Retain v1 submission
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
			record := map[string]any{"kind": "review", "review_revision": "head", "workset_key": "supplement-0002", "lens": "general", "findings": findings, "gaps": []any{}, "invariant_assessments": []any{}, "reference_assessments": []any{}}
			raw, err := json.Marshal(map[string]any{"record": record})
			if err != nil {
				t.Fatal(err)
			}
			sourceRef := sourceSchema.ref(true)
			source := seedReviewSourceWorkset(t, c, TodoSpec{Agent: "reviewer", ResultContract: &sourceRef}, "supplement-0002", "head", "general", "file.go")
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
			if err := c.validateSubmittedEvidenceInputs(t.Context(), item.ID, payload); err != nil {
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
			findings[0].(map[string]any)["summary"] = "replacement"
			raw, err = json.Marshal(map[string]any{"record": record})
			if err != nil {
				t.Fatal(err)
			}
			source.TypedResult.StructuredPayload, err = validateStructuredResultPayload(sourceSchema, sourceRef, raw)
			if err != nil {
				t.Fatal(err)
			}
			if c.validateSubmittedEvidenceInputs(t.Context(), item.ID, payload) == nil {
				t.Fatal("old accepted result survived source replacement")
			}
			if _, rejection := submit(); rejection == "" {
				t.Fatal("stale decision keys accepted")
			}
		})
	}
}

func TestHufuCodeReviewContractSourcesReachCritiqueAndReport(t *testing.T) {
	t.Parallel()
	session := loadHufuCodeReviewTeam(t)
	for _, task := range session.ContractTasks {
		if task.ID != "review-primary-workset" && task.ID != "review-documentation-workset" && task.ID != "review-documentation-escalation" && task.ID != "review-supplement-workset" {
			continue
		}
		for _, count := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s/findings_%d", task.ID, count), func(t *testing.T) {
				c := newTestCoordinator(t)
				c.session.ResultContracts = session.ResultContracts
				schema := session.ResultContracts[task.ResultContract.ID]
				lens := "general"
				key := "unit-0000"
				if task.ID == "review-supplement-workset" {
					key = "supplement-0000"
				}
				if task.ID == "review-documentation-workset" || task.ID == "review-documentation-escalation" {
					lens = "documentation-risk"
				}
				findings := make([]any, count)
				for i := range count {
					finding := map[string]any{"summary": fmt.Sprintf("finding %d", i), "detail": "bounded observation", "severity": "info", "location": "doc.md:1", "evidence": []any{"sha256-" + strings.Repeat("a", 64)}, "requires_critique": false}
					if task.ID == "review-primary-workset" || task.ID == "review-supplement-workset" {
						finding["location"], finding["source_excerpt"] = "./source.go: 2", "func Value() int { return 1 }"
					}
					findings[i] = finding
				}
				record := map[string]any{"findings": findings, "gaps": []any{map[string]any{"description": "bounded review", "missing_evidence": "caller", "kind": "semantic_uncertainty", "requested_evidence": []any{}, "limitations": []any{}}}, "invariant_assessments": []any{}, "reference_assessments": []any{}}
				claims := map[string]any{"record": record}
				raw, err := json.Marshal(claims)
				if err != nil {
					t.Fatal(err)
				}
				sources := map[string]any{"/record/kind": "review", "/record/review_revision": "head", "/record/workset_key": key, "/record/lens": lens}
				raw, err = assembleResultRuntimeInputs(raw, schema.runtimeInputs, sources)
				if err != nil {
					t.Fatal(err)
				}
				ref := schema.ref(true)
				payload, err := validateStructuredResultPayload(schema, ref, raw)
				if err != nil {
					t.Fatal(err)
				}
				record["kind"] = "review"
				override, err := json.Marshal(claims)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := assembleResultRuntimeInputs(override, schema.runtimeInputs, sources); err == nil || !strings.Contains(err.Error(), "/record/kind") {
					t.Fatalf("source accepted a manual kind override: %v", err)
				}
				source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: task.Agent, ResultContract: &ref}})[0]
				source.Status = TaskDone
				source.TypedResult = &TaskResult{TaskID: source.ID, Status: TaskResultStatusCompletedWithGaps, StructuredPayload: payload}
				for _, consumer := range []struct {
					schema string
					rows   int
				}{{"schemas/critique-v3.json", count}, {"schemas/report-v3.json", 1}} {
					consumerRef := session.ResultContracts[consumer.schema].ref(true)
					item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "consumer", ResultContract: &consumerRef, EvidenceFrom: []string{source.ID}}})[0]
					plans, err := c.fixedCollectionPlans(item.ID)
					if err != nil {
						t.Fatalf("%s rejected %s: %v", consumer.schema, ref.ID, err)
					}
					if len(plans) != 1 || len(plans[0].rows) != consumer.rows || len(plans[0].binding.Sources) != 1 {
						t.Fatalf("%s lost source/rows: %#v", consumer.schema, plans)
					}
				}
			})
		}
	}
}

func TestHufuCodeReviewAuthenticatesReferencesWithInvalidGapStatus(t *testing.T) {
	schema := loadHufuCodeReviewTeam(t).ResultContracts["schemas/report-v3.json"]
	const sourceValue = `{"record":{"kind":"review"}}`
	artifact := "sha256-" + strings.Repeat("a", 64)
	sources := []TaskResult{{
		TaskID: "other-review", Status: TaskResultStatusSuccess,
		StructuredPayload: &ResultPayload{Value: []byte(sourceValue), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(sourceValue)))},
		FilesRead:         []FileRef{{Path: artifact}},
	}}
	const key = "gap-1"
	plans := []resultFixedCollectionPlan{{
		spec: schema.fixedCollections[0],
		rows: []resultFixedCollectionRow{{Key: key, Fixed: map[string]any{"source_task_id": "original", "gap_index": 0}}},
	}}
	t.Run("missing model decision", func(t *testing.T) {
		wire := []byte(`{"gap_resolutions":{}}`)
		_, _, err := assembleResultFixedCollections(wire, plans)
		if _, shape := errors.AsType[*resultBindingShapeError](err); !shape || !strings.Contains(err.Error(), "exactly 1") {
			t.Fatalf("missing frozen row lost its repair diagnostic: %v", err)
		}
		value, err := decodeUniqueJSON(wire)
		if err != nil {
			t.Fatal(err)
		}
		if err := projectFixedCollectionSourceRows(value, plans); err != nil {
			t.Fatal(err)
		}
		if err := validateResultEvidenceReferences(value, schema.evidenceReferences, []string{"original", "other-review"}, sources, nil); err != nil {
			t.Fatalf("no readable rows claimed a source: %v", err)
		}
	})
	for _, tc := range []struct {
		name, status, producer string
		evidence               []string
		want                   RepairFailureReason
	}{
		{"retained", "retained", "", nil, ""},
		{"resolved with accepted evidence", "resolved_cross_workset", "other-review", []string{artifact}, ""},
		{"invalid status with accepted evidence", "unknown", "other-review", []string{artifact}, RepairFailureInvalidSchema},
		{"invalid status with foreign producer", "unknown", "foreign", []string{artifact}, RepairFailureSource},
		{"resolved with foreign producer", "resolved_cross_workset", "foreign", []string{artifact}, RepairFailureSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := json.Marshal(map[string]any{
				"gap_resolutions": map[string]any{
					key: map[string]any{"status": tc.status, "resolving_task_id": tc.producer, "evidence": tc.evidence},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			assembled, _, err := assembleResultFixedCollections(wire, plans)
			if err != nil {
				t.Fatal(err)
			}
			value, err := decodeUniqueJSON(assembled)
			if err != nil {
				t.Fatal(err)
			}
			err = validateResultEvidenceReferences(value, schema.evidenceReferences, []string{"original", "other-review"}, sources, nil)
			var got RepairFailureReason
			if err != nil {
				got = RepairFailureSource
				if _, shape := errors.AsType[*resultEvidenceReferenceShapeError](err); shape {
					got = RepairFailureInvalidSchema
				}
			}
			if got != tc.want {
				t.Fatalf("classification = %q, want %q: %v", got, tc.want, err)
			}
		})
	}
}

func TestResultEvidenceReferenceConditionUsesComposedSchema(t *testing.T) {
	const template = `{"$defs":{"text":{"type":"string"}},"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","properties":{"status":%s,"producer":{"type":"string"},"paths":{"type":"array","items":{"type":"string"}}}%s}%s}},"x-hufu-evidence-references":{"items_pointer":"/rows","task_id_pointer":"/producer","references_pointer":"/paths","when":{"pointer":"/status","value":"active"}}}`
	const source = `{"verified":true}`
	sources := []TaskResult{{TaskID: "accepted", Status: TaskResultStatusSuccess,
		StructuredPayload: &ResultPayload{Value: []byte(source), SHA256: sha256HexBytes([]byte(source))},
		FilesRead:         []FileRef{{Path: "source.go"}},
	}}
	for _, tc := range []struct{ name, field, itemSibling, arraySibling string }{
		{"allOf row", `{"type":"string"}`, `,"allOf":[{"properties":{"status":{"enum":["active","inactive"]}}}]`, ""},
		{"ref with sibling enum", `{"$ref":"#/$defs/text","enum":["active","inactive"]}`, "", ""},
		{"array contains", `{"type":"string"}`, "", `,"contains":{"properties":{"status":{"const":"unknown"}}},"minContains":0,"maxContains":0`},
		{"conditional rejection", `{"type":"string"}`, `,"if":{"properties":{"status":{"const":"unknown"}}},"then":false`, ""},
		{"conditional property name", `{"type":"string"}`, `,"if":{"properties":{"status":{"const":"unknown"}}},"then":{"propertyNames":{"not":{"const":"status"}}}`, ""},
		{"conditional disallowed property", `{"type":"string"}`, `,"if":{"properties":{"status":{"const":"unknown"}}},"then":{"properties":{"producer":true,"paths":true},"additionalProperties":false}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled := compileFixtureJSONSchema(t, "composed-condition", fmt.Sprintf(template, tc.field, tc.itemSibling, tc.arraySibling))
			for _, proof := range []struct {
				status, producer string
				want             RepairFailureReason
			}{
				{"inactive", "foreign", ""},
				{"active", "accepted", ""},
				{"unknown", "accepted", RepairFailureInvalidSchema},
				{"unknown", "foreign", RepairFailureSource},
			} {
				row := map[string]any{"status": proof.status, "producer": proof.producer, "paths": []any{"source.go"}}
				err := validateResultEvidenceReferences(map[string]any{"rows": []any{row}}, compiled.evidenceReferences, []string{"accepted"}, sources, nil)
				var got RepairFailureReason
				if err != nil {
					got = RepairFailureSource
					if _, shape := errors.AsType[*resultEvidenceReferenceShapeError](err); shape {
						got = RepairFailureInvalidSchema
					}
				}
				if got != proof.want {
					t.Fatalf("%s / %s: got %q, want %q (%v)", proof.status, proof.producer, got, proof.want, err)
				}
			}
		})
	}
}

func assertCritiqueClaimsHaveOneSemanticLocation(t *testing.T, c *Coordinator, taskID string) {
	t.Helper()
	info, err := collectionGuidanceInfo(c, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if fields := slices.Sorted(maps.Keys(info.Parameters)); !slices.Equal(fields, []string{"status", "structured_payload", "summary"}) {
		t.Fatalf("critic claims expose duplicate semantic channels: %v", fields)
	}
	// Even empty optional copies are rejected, rather than silently discarded.
	for _, field := range []string{"details", "findings", "evidence", "risks", "decisions", "open_questions", "confidence"} {
		input := fmt.Sprintf(`{"status":"blocked","summary":"missing source","%s":null}`, field)
		if _, err := c.decodeCollectionClaims(taskID, []byte(input)); err == nil || localResultRejectionReason(err) != RepairFailureInvalidSchema {
			t.Fatalf("undeclared outer %s was not rejected: %v", field, err)
		}
	}
}
