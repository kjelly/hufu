package team

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
)

func TestHufuCodeReviewRuntimeOwnsAll51GapIdentities(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	c := newBudgetCoordinator(t)
	c.session.ResultContracts = session.ResultContracts
	sourceSchema := session.ResultContracts["schemas/review-v1.json"]
	reportSchema := session.ResultContracts["schemas/report-v1.json"]
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
	report["input_review"], report["coverage"], report["gap_resolutions"] = inputs, "completed_with_gaps", decisions
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
