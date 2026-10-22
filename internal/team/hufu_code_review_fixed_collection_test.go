package team

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestHufuCodeReviewRuntimeOwnsAll51GapIdentities(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	value, err := decodeResultContractJSON([]byte(hufuCodeReviewReportFixture))
	if err != nil {
		t.Fatal(err)
	}
	report := value.(map[string]any)
	tests := report["test_verification"].(map[string]any)
	diagnostic := strings.Repeat("snapshot test failure α\n", 3500)
	tests["passed"], tests["exit_code"], tests["diagnostic"] = false, 1, diagnostic
	c, nativeScope := runtimeOutputRefFixture(t)
	nativeScope.ContractID, nativeScope.PlanTaskID = "produce-workset", "produce-workset"
	outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{"scope": report["scope"], "targeted_go_tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	nativeScope.TypedResult.RuntimeOutputs, nativeScope.TypedResult.RuntimeOutputsHash = outputs, digest
	nativeScope.ExecutionReceipt.RuntimeOutputsHash = digest
	nativeTests := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runtime", ContractID: "verify-targeted-go-tests", PlanTaskID: "verify-targeted-go-tests"}})[0]
	nativeTests.Status = TaskDone
	nativeTests.RunInputSnapshotID, nativeTests.RunInputSnapshotHash = nativeScope.RunInputSnapshotID, nativeScope.RunInputSnapshotHash
	nativeTests.MaterializedActionPayloadHash = nativeScope.MaterializedActionPayloadHash
	copiedResult, copiedReceipt := *nativeScope.TypedResult, *nativeScope.ExecutionReceipt
	copiedResult.TaskID, copiedReceipt.TaskID, copiedReceipt.ActionInvocationID = nativeTests.ID, nativeTests.ID, "action-tests"
	nativeTests.TypedResult, nativeTests.ExecutionReceipt = &copiedResult, &copiedReceipt
	c.session.ResultContracts = session.ResultContracts
	sourceSchema := session.ResultContracts["schemas/review-v1.json"]
	reportSchema := session.ResultContracts["schemas/report-v1.json"]
	var ids []string
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
	delete(report, "input_review")
	delete(report, "scope")
	delete(report, "test_verification")
	report["coverage"], report["gap_resolutions"] = "completed_with_gaps", decisions
	decisions["invented_gap_2"] = decisions[plans[0].rows[0].Key]
	raw, err := json.Marshal(report)
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
	rows := canonical.(map[string]any)["gap_resolutions"].([]any)
	inputs := canonical.(map[string]any)["input_review"].([]any)
	boundTests := canonical.(map[string]any)["test_verification"].(map[string]any)
	if boundTests["diagnostic"] != diagnostic || boundTests["passed"] != false {
		t.Fatal("synthesis summarized or changed receipt-verified test evidence")
	}
	if len(inputs) != len(ids) {
		t.Fatal("runtime did not bind every accepted report source")
	}
	for i, input := range inputs {
		entry := input.(map[string]any)
		source := c.todoItemByID(ids[i]).TypedResult.StructuredPayload
		if entry["task_id"] != ids[i] || entry["payload_sha256"] != source.SHA256 {
			t.Fatal("runtime substituted a report source identity or hash")
		}
	}
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

func TestHufuCodeReviewCriticSubmitsOnlyDecisionsForAllFindings(t *testing.T) {
	for _, route := range []string{"local", "external"} {
		t.Run(route, func(t *testing.T) {
			session := loadHufuCodeReviewTeam(t)
			c := newBudgetCoordinator(t)
			c.session.ResultContracts = session.ResultContracts
			sourceSchema := session.ResultContracts["schemas/review-v1.json"]
			criticSchema := session.ResultContracts["schemas/critique-v1.json"]
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
