package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebResearchReportCoversBothResearchTracksWithinItsBudget(t *testing.T) {
	for _, counts := range [][2]int{{11, 9}, {12, 12}} {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d+%d/external=%t", counts[0], counts[1], external), func(t *testing.T) {
				c := newBudgetCoordinator(t)
				c.session.ResultContracts = make(map[string]*CompiledResultContract)
				for _, name := range []string{"schemas/research-v1.json", "schemas/report-v1.json"} {
					compiled, err := compileResultContractSchema(filepath.Join(bundledTeamsDir(t), "web-research"), name)
					if err != nil {
						t.Fatal(err)
					}
					c.session.ResultContracts[name] = compiled
				}
				var ids []string
				var inputs, findings []any
				for i, count := range counts {
					lens := []string{"primary_search", "countercheck"}[i]
					var originals []any
					for j := range count {
						claim := fmt.Sprintf("Distinct claim from track %d item %d", i, j)
						originals = append(originals, map[string]any{"claim": claim, "assessment": "unverified", "reasoning": "Evidence unavailable", "sources": []any{}, "open_questions": "Missing evidence"})
						findings = append(findings, map[string]any{
							"claim": claim, "verdict": "unverified", "researcher_agreement": "not_applicable", "evidence_basis": "insufficient_evidence",
							"analysis": "Evidence unavailable", "sources": []any{}, "dissent": "Unknown", "limitations": "No supporting page evidence",
						})
					}
					raw, err := json.Marshal(map[string]any{"lens": lens, "question": "Question", "findings": originals, "disconfirmation_effort": "Checked contrary evidence", "limitations": "Source unavailable"})
					if err != nil {
						t.Fatal(err)
					}
					source := fixedCollectionAcceptedSource(t, c, c.session.ResultContracts["schemas/research-v1.json"], raw)
					ids = append(ids, source.ID)
					inputs = append(inputs, map[string]any{"task_id": source.ID, "payload_sha256": source.TypedResult.StructuredPayload.SHA256, "lens": lens})
				}
				compiled := c.session.ResultContracts["schemas/report-v1.json"]
				ref := compiled.ref(true)
				item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "verifier", ResultContract: &ref, EvidenceFrom: ids}})[0]
				plans, err := c.fixedCollectionPlans(item.ID)
				if err != nil {
					t.Fatal(err)
				}
				decisions := make(map[string]any)
				for _, row := range plans[0].rows {
					decisions[row.Key] = map[string]any{"handling": "unverified", "reason": "Evidence unavailable"}
				}
				value := map[string]any{
					"audit_complete": true, "input_review": inputs, "claim_assessments": decisions, "question": "Question",
					"summary": "Audit complete with evidence gaps", "overall_limitations": "Evidence unavailable", "findings": findings,
				}
				// The upper bound still applies; correction must retain every input.
				overflow := make([]any, 25)
				for i := range overflow {
					overflow[i] = findings[0]
				}
				value["findings"] = overflow
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := adoptedCollectionPayload(t, c, item, raw, external); err == nil || !strings.Contains(err.Error(), "maxItems") {
					t.Fatalf("oversized report was not rejected: %v", err)
				}
				value["findings"] = findings
				raw, err = json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				payload, err := adoptedCollectionPayload(t, c, item, raw, external)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.validateSubmittedEvidenceInputs(item.ID, payload); err != nil {
					t.Fatal(err)
				}
				stored, err := decodeResultContractJSON(payload.Value)
				if err != nil {
					t.Fatal(err)
				}
				result := stored.(map[string]any)
				if len(result["findings"].([]any)) != len(findings) || len(result["claim_assessments"].([]any)) != len(findings) {
					t.Fatal("report lost a distinct original claim")
				}
				var rendered bytes.Buffer
				if err := compiled.finalReport.Execute(&rendered, stored); err != nil {
					t.Fatal(err)
				}
				for _, finding := range findings {
					if !strings.Contains(rendered.String(), finding.(map[string]any)["claim"].(string)) {
						t.Fatal("rendered report omitted a distinct finding")
					}
				}
			})
		}
	}
}
