package webresearch_test

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"strings"
	"testing"
	"text/template"
)

// Accounting is submitted before evidence binding and may still contain an
// optimistic explanation after the runtime downgrades a finding's verdict.
func TestReportUsesBoundVerdictInsteadOfAccountingReason(t *testing.T) {
	raw, err := os.ReadFile("../schemas/report-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Report string `json:"x-hufu-final-report"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	report, err := template.New("report").Option("missingkey=error").Parse(schema.Report)
	if err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []string{"supported", "contradicted", "mixed", "unverified"} {
		t.Run(verdict, func(t *testing.T) {
			const staleReason = "引文已驗證，列入 findings#1 supported（僅甲的部分）"
			pageStatus, citationStatus := "checked", "matched"
			if verdict == "unverified" {
				pageStatus, citationStatus = "unverified", "mismatch"
			}
			value := map[string]any{
				"question": "查核三人的發言", "audit_complete": true,
				"input_review": []any{}, "overall_limitations": "乙、丙發言仍待查核",
				"findings": []any{map[string]any{
					"claim": "甲表示有三項障礙", "verdict": verdict,
					"analysis": "已檢查來源內容", "researcher_agreement": "not_applicable",
					"dissent": "無", "limitations": "引文不符時不得視為已驗證",
					"sources": []any{map[string]any{
						"title": "來源", "publisher": "出版者", "publication_date": "unknown",
						"url": "https://example.com/source", "page_status": pageStatus,
						"fetch_status": "fetched", "citation_status": citationStatus,
						"tool_call_id": "snapshot-1", "quote": "待核對的引文",
					}},
				}},
				"claim_assessments": []any{map[string]any{
					"source_task_id": "1", "source_index": 0,
					"claim":    "甲提出三項障礙、乙提出七問、丙表示短期無計畫",
					"handling": "addressed", "reason": staleReason,
				}},
			}
			var out bytes.Buffer
			if err := report.Execute(&out, value); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if strings.Contains(text, staleReason) || strings.Count(text, "判定：") != 1 {
				t.Fatalf("report emitted competing verdicts: %s", text)
			}
			for _, want := range []string{
				"判定：" + verdict,
				"以下為研究輸入，並非已確認事實",
				"addressed 不代表已證實",
				"甲提出三項障礙、乙提出七問、丙表示短期無計畫",
				"處理：addressed", "乙、丙發言仍待查核",
				"https://example.com/source", "抓取：fetched；引用驗證：" + citationStatus,
			} {
				if !strings.Contains(text, want) {
					t.Errorf("report lost %q: %s", want, text)
				}
			}
		})
	}
}
