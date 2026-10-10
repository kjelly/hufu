package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func contractSchema(t *testing.T, name string) (*jsonschema.Schema, map[string]any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	url := "https://hufu.invalid/" + name
	if err := compiler.AddResource(url, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(url)
	if err != nil {
		t.Fatal(err)
	}
	return schema, document
}

func schemaFixture() (map[string]any, map[string]any, map[string]any) {
	review := map[string]any{"kind": "review", "review_revision": "frozen-head", "workset_key": "unit-0001", "lens": "general",
		"invariant_assessments": []any{}, "findings": []any{map[string]any{"summary": "Prompt-only URL protection", "detail": "A private URL can pass the configured tool restriction", "severity": "info", "location": "team.yaml:20", "evidence": []any{"sha256-" + strings.Repeat("b", 64)}, "requires_critique": true}},
		"gaps": []any{map[string]any{"description": "Only this snapshot lacks the registry", "missing_evidence": "AllTools body"}},
	}
	input := map[string]any{"task_id": "4", "payload_sha256": strings.Repeat("a", 64), "record": review}
	critique := map[string]any{"kind": "critique", "input_review": []any{input}, "decisions": []any{map[string]any{
		"finding_index": 0, "verdict": "unverified", "severity": "info", "rationale": "Intended egress evidence is unavailable", "evidence": []any{}, "missing_evidence": "URL validator behavior",
	}}}
	report := map[string]any{"scope": map[string]any{"requested": map[string]any{"kind": "last_n", "count": 10, "history": "first_parent", "head": "HEAD"}, "resolved": map[string]any{"base": "base", "head": "frozen-head", "selected_commit_count": 10}, "satisfied": true}, "test_verification": map[string]any{"passed": true, "reviewed_revision": "frozen-head", "command": []any{"go", "test"}, "artifact_id": "tests-artifact"}, "audit_complete": true, "coverage": "completed_with_gaps", "input_review": []any{input, map[string]any{"task_id": "6", "payload_sha256": strings.Repeat("b", 64), "record": critique}},
		"gap_resolutions": []any{map[string]any{"source_task_id": "4", "gap_index": 0, "status": "resolved_cross_workset", "resolving_task_id": "3", "evidence": []any{"sha256-" + strings.Repeat("a", 64)}, "explanation": "Sibling task 3 read AllTools; original local limitation retained"}},
	}
	scope := report["scope"].(map[string]any)
	scope["requested_input_hash"] = "sha256:" + strings.Repeat("a", 64)
	scope["observed_budget"] = map[string]any{"changed_paths": 1, "total_diff_bytes": 1, "total_diff_lines": 1, "workset_items": 1}
	resolved := scope["resolved"].(map[string]any)
	resolved["available_commit_count"], resolved["history_exhausted"], resolved["repository_shallow"] = 10, false, false
	tests := report["test_verification"].(map[string]any)
	tests["artifact_sha256"], tests["requested_input_hash"] = strings.Repeat("a", 64), "sha256:"+strings.Repeat("a", 64)
	tests["base_revision"], tests["packages"], tests["exit_code"] = "frozen-head", []any{"./internal/example"}, 0
	tests["timed_out"], tests["output_truncated"] = false, false
	return review, critique, report
}

func TestTeamResultSchemasRejectFalseCompletion(t *testing.T) {
	tests := []struct {
		name, file string
		value      func() any
		valid      bool
	}{
		{"review", "review-v1.json", func() any { r, _, _ := schemaFixture(); return map[string]any{"record": r} }, true},
		{"review_annotated_artifact_reference", "review-v1.json", func() any {
			r, _, _ := schemaFixture()
			f := r["findings"].([]any)[0].(map[string]any)
			f["evidence"] = []any{"sha256-" + strings.Repeat("a", 64) + " (source snapshot)"}
			return map[string]any{"record": r}
		}, false},
		{"review_uses_workset_id_as_item_key", "review-v1.json", func() any {
			r, _, _ := schemaFixture()
			r["workset_key"] = "workset-d37c2a42e8aa4d3c3d8df19b"
			return map[string]any{"record": r}
		}, false},
		{"warning_without_critique", "review-v1.json", func() any {
			r, _, _ := schemaFixture()
			f := r["findings"].([]any)[0].(map[string]any)
			f["severity"] = "warning"
			f["requires_critique"] = false
			return map[string]any{"record": r}
		}, false},
		{"critic_unverified", "critique-v1.json", func() any { _, c, _ := schemaFixture(); return map[string]any{"audit_complete": true, "record": c} }, true},
		{"critic_claims_partial_audit_complete", "critique-v1.json", func() any { _, c, _ := schemaFixture(); return map[string]any{"audit_complete": false, "record": c} }, false},
		{"critic_wrong_source_count", "critique-v1.json", func() any {
			_, c, _ := schemaFixture()
			c["input_review"] = []any{}
			return map[string]any{"audit_complete": true, "record": c}
		}, false},
		{"critic_unverified_without_reason", "critique-v1.json", func() any {
			_, c, _ := schemaFixture()
			c["decisions"].([]any)[0].(map[string]any)["missing_evidence"] = ""
			return map[string]any{"audit_complete": true, "record": c}
		}, false},
		{"critic_confirmed_without_evidence", "critique-v1.json", func() any {
			_, c, _ := schemaFixture()
			d := c["decisions"].([]any)[0].(map[string]any)
			d["verdict"] = "confirmed"
			d["missing_evidence"] = ""
			return map[string]any{"audit_complete": true, "record": c}
		}, false},
		{"critic_annotated_artifact_reference", "critique-v1.json", func() any {
			_, c, _ := schemaFixture()
			d := c["decisions"].([]any)[0].(map[string]any)
			d["verdict"], d["missing_evidence"] = "confirmed", ""
			d["evidence"] = []any{"sha256-" + strings.Repeat("b", 64) + " (read in chunks)"}
			return map[string]any{"audit_complete": true, "record": c}
		}, false},
		{"report_with_original_gaps", "report-v1.json", func() any { _, _, r := schemaFixture(); return r }, true},
		{"report_combines_artifacts_under_one_resolver", "report-v1.json", func() any {
			_, _, r := schemaFixture()
			g := r["gap_resolutions"].([]any)[0].(map[string]any)
			g["evidence"] = []any{"sha256-" + strings.Repeat("a", 64), "sha256-" + strings.Repeat("b", 64)}
			return r
		}, false},
		{"report_retains_combined_proof_gap", "report-v1.json", func() any {
			_, _, r := schemaFixture()
			g := r["gap_resolutions"].([]any)[0].(map[string]any)
			g["status"], g["resolving_task_id"], g["evidence"] = "retained", "", []any{}
			g["explanation"] = "Sibling reviews cover different parts; the bounded synthesis did not establish a single observed artifact answering the whole gap."
			return r
		}, true},
		{"report_no_inputs", "report-v1.json", func() any { _, _, r := schemaFixture(); r["input_review"] = []any{}; return r }, false},
		{"report_annotated_gap_reference", "report-v1.json", func() any {
			_, _, r := schemaFixture()
			r["gap_resolutions"].([]any)[0].(map[string]any)["evidence"] = []any{"sha256-" + strings.Repeat("a", 64) + " (sibling snapshot)"}
			return r
		}, false},
		{"resolved_gap_without_proof", "report-v1.json", func() any {
			_, _, r := schemaFixture()
			r["gap_resolutions"].([]any)[0].(map[string]any)["evidence"] = []any{}
			return r
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schema, _ := contractSchema(t, test.file)
			if err := schema.Validate(test.value()); (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}

func TestFinalReportPreservesSourceFindingAndOriginalGap(t *testing.T) {
	_, document := contractSchema(t, "report-v1.json")
	reportTemplate, err := template.New("report").Option("missingkey=error").Parse(document["x-hufu-final-report"].(string))
	if err != nil {
		t.Fatal(err)
	}
	_, _, report := schemaFixture()
	var output bytes.Buffer
	if err := reportTemplate.Execute(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"completed_with_gaps", "Prompt-only URL protection", "Only this snapshot lacks the registry", "Critic 對象：task 4", "unverified", "URL validator behavior", "resolved_cross_workset", "Sibling task 3 read AllTools"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("render omitted %q: %s", expected, output.String())
		}
	}
}

func critiqueCoverageFixture(count int) (map[string]any, map[string]any) {
	review, critique, report := schemaFixture()
	finding := review["findings"].([]any)[0].(map[string]any)
	decision := critique["decisions"].([]any)[0].(map[string]any)
	findings, decisions := []any{}, []any{}
	for index := range count {
		findings = append(findings, maps.Clone(finding))
		decisionCopy := maps.Clone(decision)
		decisionCopy["finding_index"] = index
		decisions = append(decisions, decisionCopy)
	}
	review["findings"], critique["decisions"] = findings, decisions
	return map[string]any{"audit_complete": true, "record": critique}, report
}

func TestCriticAndReportSchemasRequireCompleteOrderedSourceCoverage(t *testing.T) {
	for _, filename := range []string{"critique-v1.json", "report-v1.json"} {
		t.Run(filename, func(t *testing.T) {
			schema, _ := contractSchema(t, filename)
			for _, count := range []int{0, 1, 2, 32, 33} {
				t.Run(fmt.Sprint(count), func(t *testing.T) {
					critique, report := critiqueCoverageFixture(count)
					var value any = critique
					if filename == "report-v1.json" {
						value = report
					}
					if err := schema.Validate(value); (err == nil) != (count >= 1 && count <= 32) {
						t.Fatalf("source count %d: %v", count, err)
					}
				})
			}
			for _, scenario := range []string{"missing", "duplicate", "reordered", "out_of_range"} {
				t.Run(scenario, func(t *testing.T) {
					critique, report := critiqueCoverageFixture(2)
					record := critique["record"].(map[string]any)
					decisions := record["decisions"].([]any)
					switch scenario {
					case "missing":
						record["decisions"] = decisions[:1]
					case "duplicate":
						decisions[1].(map[string]any)["finding_index"] = 0
					case "reordered":
						decisions[0], decisions[1] = decisions[1], decisions[0]
					case "out_of_range":
						decisions[1].(map[string]any)["finding_index"] = 2
					}
					var value any = critique
					if filename == "report-v1.json" {
						value = report
					}
					if err := schema.Validate(value); err == nil {
						t.Fatalf("accepted %s finding coverage", scenario)
					}
				})
			}
		})
	}
}

func TestReportRequiresCompleteRuntimeAttestations(t *testing.T) {
	schema, document := contractSchema(t, "report-v1.json")
	properties := document["properties"].(map[string]any)
	for _, name := range []string{"scope", "test_verification"} {
		required := properties[name].(map[string]any)["required"].([]any)
		for _, field := range required {
			t.Run(name+"/"+field.(string), func(t *testing.T) {
				_, _, report := schemaFixture()
				delete(report[name].(map[string]any), field.(string))
				if err := schema.Validate(report); err == nil {
					t.Fatal("accepted incomplete runtime attestation")
				}
			})
		}
	}
}

func TestCompleteContractsFitRuntimePromptBudget(t *testing.T) {
	for _, name := range []string{"review-v1.json", "critique-v1.json", "report-v1.json"} {
		_, document := contractSchema(t, name)
		canonical, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if len(canonical) > 32<<10 {
			t.Fatalf("%s: %d canonical bytes exceed runtime full-schema prompt limit", name, len(canonical))
		}
	}
}
