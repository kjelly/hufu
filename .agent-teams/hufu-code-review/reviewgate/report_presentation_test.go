package main

import (
	"bytes"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"text/template"
)

func renderPresentationFixture(t *testing.T, report map[string]any) string {
	t.Helper()
	schema, document := contractSchema(t, "report-v1.json")
	if err := schema.Validate(report); err != nil {
		t.Fatal(err)
	}
	renderer, err := template.New("report").Option("missingkey=error").Parse(document["x-hufu-final-report"].(string))
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(report, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := renderer.Execute(&output, report); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(report, json.Deterministic(true))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("presentation changed the source payload")
	}
	return output.String()
}

func TestHumanReportKeepsTechnicalIdentityInAuditAppendix(t *testing.T) {
	_, _, report := schemaFixture()
	output := renderPresentationFixture(t, report)
	main, audit, ok := strings.Cut(output, "## 完整審查證據（稽核附錄）")
	if !ok {
		t.Fatal("missing audit appendix")
	}
	for _, expected := range []string{"### Prompt-only URL protection", "位置：team.yaml:20", "原始影響／說明：A private URL", "複核判定：證據不足、尚未確認（unverified）", "待補證據：URL validator behavior"} {
		if !strings.Contains(main, expected) {
			t.Fatalf("missing readable context %q: %s", expected, main)
		}
	}
	for _, technical := range []string{"task 4", "task 6", "payload", strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if strings.Contains(main, technical) || !strings.Contains(audit, technical) {
			t.Fatalf("technical identity %q was lost or leaked into the main report", technical)
		}
	}
	if strings.Contains(output, "來源 task ") || strings.Contains(output, "Critic 對象：task ") {
		t.Fatal("report still uses technical identifiers as source headings")
	}
	if !strings.Contains(audit, "### 獨立複核：綜合審查") {
		t.Fatal("source heading lost its readable review scope")
	}
}

func TestHumanReportAssociatesReusedFindingIndexesWithTheirOwnSources(t *testing.T) {
	_, _, report := schemaFixture()
	report["gap_resolutions"] = []any{}
	inputs := []any{}
	for sourceIndex := range 2 {
		review, critique, _ := schemaFixture()
		review["gaps"] = []any{}
		findings, decisions := []any{}, []any{}
		for findingIndex := range 2 {
			title := fmt.Sprintf("source-%d issue-%d", sourceIndex, findingIndex)
			location := fmt.Sprintf("source-%d.go:%d", sourceIndex, findingIndex+1)
			findings = append(findings, map[string]any{"summary": title, "location": location, "detail": "impact " + title,
				"severity": "info", "requires_critique": true, "evidence": []any{"sha256-" + strings.Repeat("b", 64)}})
			verdict := "confirmed"
			if sourceIndex == findingIndex {
				verdict = "rejected"
			}
			// JSON Schema integer values need not use an integer lexical spelling.
			decisions = append(decisions, map[string]any{"finding_index": jsonv1.Number(fmt.Sprintf("%d.0", findingIndex)),
				"verdict": verdict, "severity": "info", "rationale": "checked " + title,
				"evidence": []any{"sha256-" + strings.Repeat("b", 64)}, "missing_evidence": ""})
		}
		review["findings"], critique["decisions"] = findings, decisions
		source := critique["input_review"].([]any)[0].(map[string]any)
		source["task_id"] = fmt.Sprint(4 + sourceIndex)
		inputs = append(inputs, source, map[string]any{"task_id": fmt.Sprint(8 + sourceIndex), "payload_sha256": strings.Repeat("b", 64), "record": critique})
	}
	report["input_review"] = inputs
	output := renderPresentationFixture(t, report)
	main, _, _ := strings.Cut(output, "## 完整審查證據（稽核附錄）")
	for sourceIndex := range 2 {
		for findingIndex := range 2 {
			title := fmt.Sprintf("source-%d issue-%d", sourceIndex, findingIndex)
			_, block, found := strings.Cut(main, "### "+title+"\n")
			if !found {
				t.Fatalf("missing finding %s", title)
			}
			block, _, _ = strings.Cut(block, "\n### ")
			verdict := "confirmed"
			if sourceIndex == findingIndex {
				verdict = "rejected"
			}
			for _, expected := range []string{fmt.Sprintf("source-%d.go:%d", sourceIndex, findingIndex+1), "checked " + title, "（" + verdict + "）"} {
				if !strings.Contains(block, expected) {
					t.Fatalf("source/decision mismatch for %s: %s", title, block)
				}
			}
		}
	}
}

func TestHumanReportEmptySourcesRemainHonest(t *testing.T) {
	review, _, report := schemaFixture()
	review["findings"] = []any{}
	report["input_review"] = report["input_review"].([]any)[:1]
	output := renderPresentationFixture(t, report)
	for _, expected := range []string{"來源未提出具體 findings；此結果不代表程式沒有問題", "本次沒有獨立複核判定", "Only this snapshot lacks the registry"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("missing honest empty case %q", expected)
		}
	}
}

func TestHumanReportRejectsMissingFindingContext(t *testing.T) {
	schema, document := contractSchema(t, "report-v1.json")
	renderer, err := template.New("report").Option("missingkey=error").Parse(document["x-hufu-final-report"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"missing_location", "extra_decision"} {
		review, critique, report := schemaFixture()
		if scenario == "missing_location" {
			delete(review["findings"].([]any)[0].(map[string]any), "location")
		} else {
			critique["decisions"] = append(critique["decisions"].([]any), critique["decisions"].([]any)[0])
		}
		if err := schema.Validate(report); err == nil {
			t.Fatalf("invalid finding association passed schema: %s", scenario)
		}
		var output bytes.Buffer
		if err := renderer.Execute(&output, report); err == nil {
			t.Fatalf("renderer silently substituted context: %s", scenario)
		}
	}
}
