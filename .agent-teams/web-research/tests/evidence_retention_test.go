package webresearch_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"text/template"
)

func completeSynthesis(count int) map[string]any {
	v := synthesisWithAudit("unverified")
	delete(v["answer"].([]any)[0].(map[string]any), "finding_indices")
	delete(v["sections"].([]any)[0].(map[string]any), "finding_indices")
	audit := v["input_review"].([]any)[0].(map[string]any)
	first := audit["findings"].([]any)[0].(map[string]any)
	var findings, citations []any
	for i := range count {
		finding := make(map[string]any)
		for k, value := range first {
			finding[k] = value
		}
		finding["claim"] = fmt.Sprintf("獨立發現 %d", i)
		findings = append(findings, finding)
		row := map[string]any{"audit_task_id": "3", "finding_index": i, "claim": finding["claim"]}
		for j := range 3 {
			row[fmt.Sprintf("answer_%d", j)] = "omit"
		}
		for j := range 6 {
			row[fmt.Sprintf("section_%d", j)] = "omit"
		}
		if i == count-1 {
			row["answer_0"], row["section_0"] = "cite", "cite"
		}
		citations = append(citations, row)
	}
	audit["findings"], v["citations"] = findings, citations
	return v
}

func TestCompleteEvidenceOnlyLimitsPresentation(t *testing.T) {
	doc, schema := researchSchema(t, "synthesis-v2")
	report, err := template.New("complete").Option("missingkey=error").Parse(doc["x-hufu-final-report"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{14, 37} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			v := completeSynthesis(count)
			if err := schema.Validate(v); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := report.Execute(&out, v); err != nil {
				t.Fatal(err)
			}
			for i := range count {
				if !strings.Contains(out.String(), fmt.Sprintf("**獨立發現 %d**", i)) {
					t.Fatalf("finding %d was omitted", i)
				}
			}
			if !strings.Contains(out.String(), fmt.Sprintf("(#查核-%d)", count-1)) {
				t.Fatal("last finding could not be cited")
			}
		})
	}
	for _, scenario := range []string{"uncited answer", "uncited section", "nonexistent paragraph", "invalid choice", "too many answer paragraphs"} {
		t.Run(scenario, func(t *testing.T) {
			v := completeSynthesis(14)
			row := v["citations"].([]any)[13].(map[string]any)
			switch scenario {
			case "uncited answer":
				row["answer_0"] = "omit"
			case "uncited section":
				row["section_0"] = "omit"
			case "nonexistent paragraph":
				row["answer_1"] = "cite"
			case "invalid choice":
				row["answer_0"] = "supported"
			case "too many answer paragraphs":
				v["answer"] = []any{v["answer"].([]any)[0], v["answer"].([]any)[0], v["answer"].([]any)[0], v["answer"].([]any)[0]}
			}
			if err := schema.Validate(v); err == nil {
				t.Fatal("invalid presentation or citation accepted")
			}
		})
	}
}

func TestCompleteResearchRetainsFindingsAndNecessarySources(t *testing.T) {
	_, schema := researchSchema(t, "research-v2")
	_, legacy := researchSchema(t, "research-v1")
	var findings []any
	for i := range 37 {
		var sources []any
		for j := range 5 {
			sources = append(sources, map[string]any{"title": "原始記錄", "publisher": "來源", "publication_date": "unknown", "url": fmt.Sprintf("https://example.com/%d/%d", i, j), "source_type": "primary", "page_status": "unverified", "quote": "", "relation": "必要但尚未取得的來源", "independence_group": fmt.Sprint(j)})
		}
		findings = append(findings, map[string]any{"claim": fmt.Sprintf("獨立發現 %d", i), "assessment": "unverified", "reasoning": "來源尚不可用", "sources": sources, "open_questions": "待確認"})
	}
	v := map[string]any{"lens": "countercheck", "question": "原始問題", "findings": findings, "disconfirmation_effort": "已尋找相反證據", "limitations": "來源仍待確認"}
	if err := schema.Validate(v); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Validate(v); err == nil {
		t.Fatal("historical schema was silently broadened")
	}
	v["findings"] = []any{}
	if err := schema.Validate(v); err == nil {
		t.Fatal("empty completed research accepted")
	}
}

func TestCompleteAuditRequiresBothSourceLenses(t *testing.T) {
	_, schema := researchSchema(t, "audit-v2")
	for _, scenario := range []string{"both", "empty", "one", "same_lens"} {
		t.Run(scenario, func(t *testing.T) {
			v := acceptedAudit("unverified")
			inputs := v["input_review"].([]any)
			switch scenario {
			case "empty":
				v["input_review"] = []any{}
			case "one":
				v["input_review"] = inputs[:1]
			case "same_lens":
				inputs[1].(map[string]any)["lens"] = "primary_search"
			}
			if err := schema.Validate(v); (err == nil) != (scenario == "both") {
				t.Fatalf("source validation: %v", err)
			}
		})
	}
}
