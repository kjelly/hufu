package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func referenceReview(t *testing.T, kind string) (task, record) {
	t.Helper()
	r := record{Kind: "review", Revision: "frozen-head", Key: "unit-0000", Lens: "documentation-risk",
		References: []referenceAssessment{{ID: "ref-000000", Kind: kind, Rationale: "The source describes removal from docs/tmp, with no tracked files remaining.", Evidence: []string{"snapshot-a"}}},
	}
	status := "success"
	switch kind {
	case "required":
		r.References[0].FindingIndex = new(0)
		r.Findings = []finding{{Summary: "Required path is absent", Severity: "warning", Evidence: []string{"snapshot-a"}, RequiresCritique: true}}
	case "uncertain":
		r.References[0].GapIndex = new(0)
		r.Gaps = []json.RawMessage{raw(t, map[string]string{"description": "Missing surrounding context", "missing_evidence": "Complete paragraph"})}
		status = "completed_with_gaps"
	}
	item := fixtureTask(t, "3", "review-documentation-workset", status, payload{Record: raw(t, r)}, r, nil)
	item.Binding.Bindings["reference_candidate_ids"] = `["ref-000000"]`
	return item, r
}

func TestReferenceAssessmentGateEnforcesModelClassification(t *testing.T) {
	for _, kind := range []string{"required", "historical", "deleted", "example", "uncertain"} {
		t.Run(kind, func(t *testing.T) {
			item, r := referenceReview(t, kind)
			inventory, err := collectReviewHandoffs(session{Tasks: []task{item}}, "run-1")
			if err != nil {
				t.Fatal(err)
			}
			if (kind == "required") != (len(inventory.RequiredCriticSources) == 1) {
				t.Fatalf("required finding critique coverage = %#v", inventory)
			}
			if (kind == "uncertain") != (len(inventory.RequiredGapRefs) == 1) {
				t.Fatalf("uncertain reference gap coverage = %#v", inventory)
			}
			if err := verifyReview(item, r); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, test := range []struct {
		name string
		kind string
		edit func(*task, *record)
	}{
		{"missing", "deleted", func(_ *task, r *record) { r.References = nil }},
		{"invented", "deleted", func(_ *task, r *record) { r.References[0].ID = "ref-000099" }},
		{"duplicate", "deleted", func(i *task, r *record) {
			i.Binding.Bindings["reference_candidate_ids"] = `["ref-000000","ref-000001"]`
			r.References = append(r.References, r.References[0])
		}},
		{"missing_binding", "deleted", func(i *task, _ *record) { delete(i.Binding.Bindings, "reference_candidate_ids") }},
		{"null_binding", "deleted", func(i *task, _ *record) { i.Binding.Bindings["reference_candidate_ids"] = "null" }},
		{"unknown_kind", "deleted", func(_ *task, r *record) { r.References[0].Kind = "safe" }},
		{"unread_evidence", "deleted", func(_ *task, r *record) { r.References[0].Evidence = []string{"unread"} }},
		{"empty_rationale", "deleted", func(_ *task, r *record) { r.References[0].Rationale = " " }},
		{"required_without_finding", "required", func(_ *task, r *record) { r.References[0].FindingIndex = nil }},
		{"required_bad_finding", "required", func(_ *task, r *record) { r.References[0].FindingIndex = new(-1) }},
		{"required_out_of_range", "required", func(_ *task, r *record) { r.References[0].FindingIndex = new(1) }},
		{"required_without_critique", "required", func(i *task, r *record) {
			r.Findings[0].Severity = "info"
			r.Findings[0].RequiresCritique = false
			i.Result.Findings = r.Findings
		}},
		{"uncertain_without_gap", "uncertain", func(_ *task, r *record) { r.References[0].GapIndex = nil }},
		{"uncertain_bad_gap", "uncertain", func(_ *task, r *record) { r.References[0].GapIndex = new(1) }},
		{"uncertain_as_success", "uncertain", func(i *task, _ *record) { i.Result.Status = "success" }},
		{"historical_with_finding", "historical", func(_ *task, r *record) { r.References[0].FindingIndex = new(0) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			item, r := referenceReview(t, test.kind)
			test.edit(&item, &r)
			if err := verifyReview(item, r); err == nil {
				t.Fatal("invalid reference assessment accepted")
			}
			replacePayload(t, &item, func(p *payload) { p.Record = raw(t, r) })
			if _, err := collectReviewHandoffs(session{Tasks: []task{item}}, "run-1"); err == nil {
				t.Fatal("inventory accepted invalid classification")
			}
		})
	}
}

func TestReferenceAssessmentSchemas(t *testing.T) {
	schema, _ := contractSchema(t, "review-v1.json")
	candidate := func(kind string) map[string]any {
		return map[string]any{"reference_id": "ref-000000", "kind": kind, "rationale": "Source describes removed scratch files", "evidence": []any{"sha256-" + strings.Repeat("a", 64)}, "finding_index": nil, "gap_index": nil}
	}
	for _, test := range []struct {
		name  string
		value func() any
		valid bool
	}{
		{"deleted", func() any { return []any{candidate("deleted")} }, true},
		{"historical", func() any { return []any{candidate("historical")} }, true},
		{"example", func() any { return []any{candidate("example")} }, true},
		{"required", func() any { c := candidate("required"); c["finding_index"] = 0; return []any{c} }, true},
		{"uncertain", func() any { c := candidate("uncertain"); c["gap_index"] = 0; return []any{c} }, true},
		{"required_without_finding", func() any { return []any{candidate("required")} }, false},
		{"uncertain_without_gap", func() any { return []any{candidate("uncertain")} }, false},
		{"unknown_kind", func() any { return []any{candidate("ignore")} }, false},
		{"no_evidence", func() any { c := candidate("deleted"); c["evidence"] = []any{}; return []any{c} }, false},
		{"extra_field", func() any { c := candidate("deleted"); c["exempt"] = true; return []any{c} }, false},
		{"null", func() any { return nil }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			review, _, _ := schemaFixture()
			review["reference_assessments"] = test.value()
			err := schema.Validate(map[string]any{"record": review})
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v: %v", test.valid, err)
			}
		})
	}
	review, _, _ := schemaFixture()
	delete(review, "reference_assessments")
	if err := schema.Validate(map[string]any{"record": review}); err == nil {
		t.Fatal("missing classification field accepted")
	}
}
