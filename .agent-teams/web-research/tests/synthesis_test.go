package webresearch_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kjelly/hufu/internal/team"
)

func researchSchema(t *testing.T, name string) (map[string]any, *jsonschema.Schema) {
	t.Helper()
	raw, err := os.ReadFile("../schemas/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := team.CompileResultContractDocument("schemas/"+name+".json", raw); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	const location = "https://fixture.invalid/schema.json"
	if err := compiler.AddResource(location, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		t.Fatal(err)
	}
	return doc, schema
}

func acceptedAudit(verdict string) map[string]any {
	return map[string]any{
		"kind": "evidence_audit", "audit_complete": true,
		"question": "哪些情況需要迴避？", "summary": "來源稽核完成",
		"input_review": []any{
			map[string]any{"task_id": "1", "payload_sha256": strings.Repeat("a", 64), "lens": "primary_search"},
			map[string]any{"task_id": "2", "payload_sha256": strings.Repeat("b", 64), "lens": "countercheck"},
		},
		"findings": []any{map[string]any{
			"claim": "某項義務有特定適用條件", "verdict": verdict,
			"researcher_agreement": "partial", "evidence_basis": "insufficient_evidence",
			"analysis": "以接受後的證據判定為準", "dissent": "兩路研究適用範圍不同",
			"limitations": "缺少一項原始文件",
			"sources": []any{map[string]any{
				"title": "原始說明", "publisher": "發布機關", "publication_date": "unknown",
				"url": "https://example.com/record", "source_type": "official_primary",
				"page_status": "unverified", "fetch_status": "fetched", "citation_status": "mismatch",
				"tool_call_id": "snapshot-1", "quote": "保留供診斷的原提交引文",
				"relation": "本次引文未通過綁定", "independence_group": "original_record",
			}},
		}},
		"overall_limitations": "須確認個案身分及原始文件；舊法資料不能直接當作現行法。",
		"claim_assessments": []any{map[string]any{
			"source_task_id": "1", "source_index": 0, "claim": "原研究曾作肯定結論",
			"handling": "addressed", "reason": "stale optimistic accounting reason",
		}},
	}
}

func synthesisWithAudit(verdict string) map[string]any {
	audit := acceptedAudit(verdict)
	audit["task_id"], audit["payload_sha256"] = "3", strings.Repeat("c", 64)
	return map[string]any{
		"synthesis_complete": true,
		"answer": []any{map[string]any{
			"text":            "目前證據不足以作無條件結論；須先確認適用身分與行為。",
			"finding_indices": []any{0},
		}},
		"sections": []any{map[string]any{
			"title": "適用條件與證據缺口", "text": "適用範圍須與資料中的限定條件一併閱讀。",
			"finding_indices": []any{0},
		}},
		"unresolved":   "原始文件尚未取得，不能把流程完成視為已證實。",
		"input_review": []any{audit},
	}
}

func TestSynthesisRequiresOneCompletedAuditAndValidCitations(t *testing.T) {
	_, schema := researchSchema(t, "synthesis-v1")
	tests := []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{name: "complete audit with unverified finding", valid: true},
		{name: "unfinished synthesis remains representable", valid: true, change: func(v map[string]any) {
			v["synthesis_complete"] = false
		}},
		{name: "no audit", change: func(v map[string]any) { v["input_review"] = []any{} }},
		{name: "raw research", change: func(v map[string]any) {
			v["input_review"].([]any)[0].(map[string]any)["kind"] = "research"
		}},
		{name: "unfinished audit", change: func(v map[string]any) {
			v["input_review"].([]any)[0].(map[string]any)["audit_complete"] = false
		}},
		{name: "multiple audits", change: func(v map[string]any) {
			v["input_review"] = append(v["input_review"].([]any), v["input_review"].([]any)[0])
		}},
		{name: "missing citation", change: func(v map[string]any) {
			v["answer"].([]any)[0].(map[string]any)["finding_indices"] = []any{}
		}},
		{name: "answer citation outside accepted findings", change: func(v map[string]any) {
			v["answer"].([]any)[0].(map[string]any)["finding_indices"] = []any{1}
		}},
		{name: "section citation outside accepted findings", change: func(v map[string]any) {
			v["sections"].([]any)[0].(map[string]any)["finding_indices"] = []any{1}
		}},
		{name: "negative citation", change: func(v map[string]any) {
			v["answer"].([]any)[0].(map[string]any)["finding_indices"] = []any{-1}
		}},
		{name: "no direct answer", change: func(v map[string]any) { v["answer"] = []any{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := synthesisWithAudit("unverified")
			if tt.change != nil {
				tt.change(v)
			}
			if err := schema.Validate(v); (err == nil) != tt.valid {
				t.Fatalf("valid=%v, validation error=%v", tt.valid, err)
			}
		})
	}
}

func TestSynthesisCitationBoundsFollowAcceptedFindingCount(t *testing.T) {
	_, schema := researchSchema(t, "synthesis-v1")
	for count := 1; count <= 24; count++ {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			v := synthesisWithAudit("unverified")
			audit := v["input_review"].([]any)[0].(map[string]any)
			finding := audit["findings"].([]any)[0]
			findings := make([]any, count)
			for i := range findings {
				findings[i] = finding
			}
			audit["findings"] = findings
			paragraph := v["answer"].([]any)[0].(map[string]any)
			paragraph["finding_indices"] = []any{count - 1}
			if err := schema.Validate(v); err != nil {
				t.Fatalf("last existing finding rejected: %v", err)
			}
			paragraph["finding_indices"] = []any{count}
			if err := schema.Validate(v); err == nil {
				t.Fatal("nonexistent finding accepted")
			}
		})
	}
}

func TestSynthesisPreservesBoundVerdictsAndEvidence(t *testing.T) {
	doc, _ := researchSchema(t, "synthesis-v1")
	report, err := template.New("synthesis").Option("missingkey=error").Parse(doc["x-hufu-final-report"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []string{"supported", "contradicted", "mixed", "unverified"} {
		t.Run(verdict, func(t *testing.T) {
			v := synthesisWithAudit(verdict)
			var out bytes.Buffer
			if err := report.Execute(&out, v); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if strings.Index(text, "目前證據不足") > strings.Index(text, "## 詳細查核與來源") {
				t.Fatal("synthesis must precede the audit appendix")
			}
			for _, want := range []string{
				"目前證據不足", "(#查核-0)", "（" + verdict + "）", "判定：" + verdict,
				"查核結果：task 3", strings.Repeat("c", 64),
				"https://example.com/record", "引用驗證：mismatch", "snapshot-1",
				"保留供診斷的原提交引文", "兩路研究適用範圍不同", "缺少一項原始文件",
				"舊法資料不能直接當作現行法", "addressed 不代表已證實",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("final report lost %q", want)
				}
			}
			if strings.Contains(text, "stale optimistic accounting reason") {
				t.Fatal("report rendered pre-binding accounting as a factual conclusion")
			}
		})
	}
}

func TestSynthesisCompletionVerifierRejectsIncompleteResults(t *testing.T) {
	session, err := team.LoadTeam("..", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var spec *team.VerificationSpec
	for _, task := range session.ContractTasks {
		if task.Agent == "synthesizer" {
			spec = task.VerifySpec
		}
	}
	if spec == nil {
		t.Fatal("missing synthesis verification")
	}
	for _, scenario := range []string{"complete with evidence gaps", "unfinished synthesis", "unfinished audit", "missing answer", "missing audit"} {
		t.Run(scenario, func(t *testing.T) {
			value := synthesisWithAudit("unverified")
			switch scenario {
			case "unfinished synthesis":
				value["synthesis_complete"] = false
			case "unfinished audit":
				value["input_review"].([]any)[0].(map[string]any)["audit_complete"] = false
			case "missing answer":
				value["answer"] = []any{}
			case "missing audit":
				value["input_review"] = []any{}
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			result := &team.TaskResult{
				TaskID: "4", Agent: "synthesizer", Status: team.TaskResultStatusCompletedWithGaps,
				StructuredPayload: &team.ResultPayload{Value: raw},
			}
			verification, err := team.ExecuteVerificationSpecWithTaskResult(t.Context(), "sh", t.TempDir(), *spec, result)
			wantSuccess := scenario == "complete with evidence gaps"
			if (err == nil) != wantSuccess || verification == nil || (verification.ExitCode == 0) != wantSuccess {
				t.Fatalf("want success=%v, verification=%+v, err=%v", wantSuccess, verification, err)
			}
		})
	}
}

func TestAuditRequiresBothIndependentLenses(t *testing.T) {
	_, schema := researchSchema(t, "audit-v1")
	for _, lens := range []string{"countercheck", "primary_search"} {
		v := acceptedAudit("unverified")
		v["input_review"].([]any)[1].(map[string]any)["lens"] = lens
		if err := schema.Validate(v); (err == nil) != (lens == "countercheck") {
			t.Fatalf("second lens %s: %v", lens, err)
		}
	}
}

func TestResearchPipelineRequiresSynthesisAfterAudit(t *testing.T) {
	session, err := team.LoadTeam("..", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(session.Config.Workflow.Phases, []string{"prepare", "audit", "verify"}) {
		t.Fatalf("workflow phases: %v", session.Config.Workflow.Phases)
	}
	if findings := team.ValidateTeamTaskContracts(session); len(findings) != 0 {
		t.Fatalf("invalid contracts: %+v", findings)
	}
	for _, worker := range []string{"researcher", "countercheck", "verifier", "synthesizer"} {
		if !slices.Contains(session.Config.AcceptanceSpec.RequiredWorkers, worker) {
			t.Fatalf("acceptance can bypass %s", worker)
		}
	}
	finalReports := 0
	for _, compiled := range session.ResultContracts {
		var doc map[string]any
		if err := json.Unmarshal(compiled.CanonicalSchema, &doc); err != nil {
			t.Fatal(err)
		}
		if doc["x-hufu-final-report"] != nil {
			finalReports++
			if compiled.ID != "schemas/synthesis-v1.json" {
				t.Fatalf("another worker owns final rendering: %s", compiled.ID)
			}
		}
	}
	if finalReports != 1 {
		t.Fatalf("expected exactly one final report, got %d", finalReports)
	}
	for _, task := range session.ContractTasks {
		switch task.Agent {
		case "verifier":
			if task.Phase != team.PhaseAudit || !task.Execution.RequiresEvidence || task.Execution.MaxEvidenceSources != 2 {
				t.Fatalf("audit handoff: %+v", task)
			}
		case "synthesizer":
			if task.Phase != team.PhaseVerify || !task.Execution.RequiresEvidence || task.Execution.MaxEvidenceSources != 1 || !slices.Equal(task.Execution.ToolSequence, []string{"submit_result"}) {
				t.Fatalf("synthesis handoff or tool boundary: %+v", task)
			}
		}
	}
	// Ensure the whole audit (including sources and diagnostics) is bound by
	// runtime, rather than asking the synthesizer to copy selected fragments.
	synthesis, _ := researchSchema(t, "synthesis-v1")
	audit, _ := researchSchema(t, "audit-v1")
	binding := synthesis["x-hufu-evidence-inputs"].(map[string]any)
	if binding["runtime_owned"] != true {
		t.Fatal("synthesis source audit is model-owned")
	}
	fields := binding["value_bindings"].(map[string]any)
	for name := range audit["properties"].(map[string]any) {
		if fields["/"+name] != "/"+name {
			t.Fatalf("audit field %s is not preserved verbatim", name)
		}
	}
	if session.Agents["synthesizer"].Tools != "submit_result" {
		t.Fatal("synthesizer has unexpected tools")
	}
}
