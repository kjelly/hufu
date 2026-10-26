package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebResearchReportRetainsSourcesAndPreciseGaps(t *testing.T) {
	contract, err := compileResultContractSchema(filepath.Join("..", "..", ".agent-teams", "web-research"), "schemas/report-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	sources := make([]any, 0, 4)
	var records []taskTranscriptRecord
	for _, name := range []string{"matched", "mismatch", "failed", "absent"} {
		url := "https://example.com/" + name
		sources = append(sources, map[string]any{
			"title": name, "publisher": "Example", "publication_date": "unknown", "url": url,
			"source_type": "primary", "page_status": "checked", "quote": "exact words",
			"relation": "Source needed for this conclusion", "independence_group": name,
		})
		if name != "absent" {
			body := "exact words"
			if name == "mismatch" {
				body = "different punctuation and words"
			}
			output := fmt.Sprintf("### Page\n- Page URL: %s\n### Snapshot\n```yaml\n- paragraph: %s\n```\n", url, body)
			records = append(records,
				taskTranscriptRecord{Event: "tool_call", Tool: contract.toolEvidence.Tools[0], ToolCallID: name, Input: `{}`},
				taskTranscriptRecord{Event: "tool_result", Tool: contract.toolEvidence.Tools[0], ToolCallID: name, Output: output, Error: name == "failed"},
			)
		}
	}
	raw, err := json.Marshal(map[string]any{
		"claim_assessments": []any{}, "audit_complete": false, "input_review": []any{}, "question": "one atomic question",
		"summary": "Evidence gaps", "overall_limitations": "Input audit incomplete",
		"findings": []any{map[string]any{
			"claim": "one atomic claim", "verdict": "supported", "researcher_agreement": "not_applicable",
			"evidence_basis": "single_source", "analysis": "claimed support", "sources": sources,
			"dissent": "unknown", "limitations": "Retain original limitations",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := validateStructuredResultPayload(contract, contract.ref(true), raw, records)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		t.Fatal(err)
	}
	bound := value.(map[string]any)["findings"].([]any)[0].(map[string]any)["sources"].([]any)
	if len(bound) != 4 {
		t.Fatal("material sources dropped")
	}
	if payload.EvidenceDowngrades != 1 {
		t.Fatal("unverified conclusion not downgraded")
	}
	var report bytes.Buffer
	if err := contract.finalReport.Execute(&report, value); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"判定：unverified", "抓取：fetched；引用驗證：matched", "抓取：fetched；引用驗證：mismatch", "抓取：failed；引用驗證：unavailable", "抓取：not_attempted；引用驗證：unavailable", "Retain original limitations"} {
		if !strings.Contains(report.String(), want) {
			t.Fatalf("report missing %q: %s", want, report.String())
		}
	}
}

func TestWebResearchPromptJSONExamplesSatisfyContracts(t *testing.T) {
	dir := filepath.Join("..", "..", ".agent-teams", "web-research")
	for _, tc := range []struct{ name, version string }{{"researcher", "v1"}, {"countercheck", "v1"}, {"verifier", "v1"}, {"verifier", "v2"}} {
		t.Run(tc.name+"_"+tc.version, func(t *testing.T) {
			name := tc.name
			content, err := os.ReadFile(filepath.Join(dir, name+".md"))
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := RenderTemplateText(string(content), name+".md", map[string]string{"TEAM_NAME": "web-research", "report_contract": tc.version})
			if err != nil {
				t.Fatal(err)
			}
			_, example, ok := strings.Cut(rendered, "```json\n")
			if !ok {
				t.Fatal("prompt has no JSON example")
			}
			example, _, ok = strings.Cut(example, "\n```")
			if !ok {
				t.Fatal("JSON example has no closing fence")
			}
			var submission SubmitResultInput
			if err := json.Unmarshal([]byte(example), &submission); err != nil {
				t.Fatal(err)
			}
			schema := "schemas/research-v1.json"
			if name == "verifier" {
				schema = "schemas/report-" + tc.version + ".json"
			}
			compiled, err := compileResultContractSchema(dir, schema)
			if err != nil {
				t.Fatal(err)
			}
			// The example shows the issued-key submission shape. This check
			// validates its stored shape; runtime assembly is covered by the
			// real-team fixed collection tests with accepted research inputs.
			if name == "verifier" {
				var value map[string]any
				if err := json.Unmarshal(submission.StructuredPayload, &value); err != nil {
					t.Fatal(err)
				}
				value["claim_assessments"] = []any{}
				submission.StructuredPayload, err = json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
			}
			if compiled.evidenceInputs != nil && compiled.evidenceInputs.runtimeOwned() {
				if _, missingErr := assembleRuntimeEvidenceInputs(submission.StructuredPayload, compiled.evidenceInputs, nil, nil); missingErr == nil {
					t.Fatal("missing sources accepted")
				}
				var sources []TaskResult
				for i, lens := range []string{"primary_search", "countercheck"} {
					raw := []byte(fmt.Sprintf(`{"lens":%q}`, lens))
					sources = append(sources, TaskResult{TaskID: fmt.Sprint(i + 1), Status: TaskResultStatusSuccess, StructuredPayload: &ResultPayload{Value: raw, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}})
				}
				submission.StructuredPayload, err = assembleRuntimeEvidenceInputs(submission.StructuredPayload, compiled.evidenceInputs, []string{"1", "2"}, sources)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = validateStructuredResultPayload(compiled, compiled.ref(true), submission.StructuredPayload)
			if err != nil {
				t.Fatalf("prompt example does not satisfy contract: %v", err)
			}
			if submission.Status != TaskResultStatusPartial {
				t.Fatal("minimal missing-evidence example claims success")
			}
		})
	}
}

func TestWebResearchCompleteAuditRequiresBothInputLenses(t *testing.T) {
	compiled, err := compileResultContractSchema(filepath.Join("..", "..", ".agent-teams", "web-research"), "schemas/report-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"complete", "missing_inputs", "same_lens", "incomplete"} {
		t.Run(name, func(t *testing.T) {
			inputs := []any{
				map[string]any{"task_id": "1", "payload_sha256": strings.Repeat("a", 64), "lens": "primary_search"},
				map[string]any{"task_id": "2", "payload_sha256": strings.Repeat("b", 64), "lens": "countercheck"},
			}
			if name == "missing_inputs" || name == "incomplete" {
				inputs = []any{}
			} else if name == "same_lens" {
				inputs[1].(map[string]any)["lens"] = "primary_search"
			}
			raw, err := json.Marshal(map[string]any{
				"claim_assessments": []any{}, "audit_complete": name != "incomplete", "input_review": inputs,
				"question": "check an event", "summary": "insufficient evidence", "overall_limitations": "missing observations",
				"findings": []any{map[string]any{
					"claim": "one atomic claim", "verdict": "unverified", "researcher_agreement": "not_applicable",
					"evidence_basis": "insufficient_evidence", "analysis": "not established", "sources": []any{}, "dissent": "unknown", "limitations": "missing evidence",
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = validateStructuredResultPayload(compiled, compiled.ref(true), raw)
			if wantValid := name == "complete" || name == "incomplete"; (err == nil) != wantValid {
				t.Fatalf("input-review contract %s: %v", name, err)
			}
		})
	}
}
