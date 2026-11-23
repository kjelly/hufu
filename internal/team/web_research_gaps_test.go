package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestContractDataflowResearchAcceptsEmptyAndMixedFindings(t *testing.T) {
	t.Parallel()
	for _, external := range []bool{false, true} {
		for _, mixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("external=%t/mixed=%t", external, mixed), func(t *testing.T) {
				c := newTestCoordinator(t)
				session, err := LoadTeam(filepath.Join(bundledTeamsDir(t), "web-research"), nil, nil, DefaultProviderRegistry)
				if err != nil {
					t.Fatal(err)
				}
				c.session.ResultContracts = session.ResultContracts
				accept := func(item *TodoItem, payload *ResultPayload) {
					t.Helper()
					result := &TaskResult{TaskID: item.ID, Agent: item.Agent, Source: "submitted", Status: TaskResultStatusCompletedWithGaps, Summary: "Research process complete with inaccessible sources", StructuredPayload: payload}
					if err := validateResultEnvelope(c.session.ResultContracts[item.ResultContract.ID], result); err != nil {
						t.Fatal(err)
					}
					if !mixed {
						optimistic := *result
						optimistic.Status = TaskResultStatusSuccess
						if err := validateResultEnvelope(c.session.ResultContracts[item.ResultContract.ID], &optimistic); err == nil {
							t.Fatal("empty research claimed unqualified success")
						}
					}
					for _, contract := range session.ContractTasks {
						if contract.Agent == item.Agent {
							verification, err := ExecuteVerificationSpecWithTaskResult(t.Context(), "sh", c.projectDir, *contract.VerifySpec, result)
							if err != nil || verification.ExitCode != 0 {
								t.Fatalf("%s verification failed: %v", item.Agent, err)
							}
						}
					}
					if err := c.taskTracker.TodoList().SetTypedResult(item.ID, result); err != nil {
						t.Fatal(err)
					}
					c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "verified gaps")
				}
				var sources []string
				for index, name := range []string{"researcher", "countercheck"} {
					findings := []any{}
					if mixed && index == 1 {
						findings = append(findings, map[string]any{"claim": "A material proposition remains unverified", "assessment": "unverified", "reasoning": "Source unavailable", "sources": []any{}, "open_questions": "Original source could not be read"})
					}
					compiled := session.ResultContracts["schemas/research-v3.json"]
					ref := compiled.ref(true)
					item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: name, ResultContract: &ref}})[0]
					raw, _ := json.Marshal(map[string]any{"lens": []string{"primary_search", "countercheck"}[index], "question": "Question", "findings": findings, "disconfirmation_effort": "Alternative sources checked", "limitations": "source_unavailable: https://example.test/missing"})
					payload, err := adoptedCollectionPayload(t, c, item, raw, external, TaskResultStatusCompletedWithGaps)
					if err != nil {
						t.Fatal(err)
					}
					accept(item, payload)
					sources = append(sources, item.ID)
				}
				compiled := session.ResultContracts["schemas/audit-v3.json"]
				ref := compiled.ref(true)
				audit := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "verifier", ResultContract: &ref, EvidenceFrom: sources}})[0]
				plans, err := c.fixedCollectionPlans(audit.ID)
				if err != nil {
					t.Fatal(err)
				}
				decisions := map[string]any{}
				findings := []any{}
				for _, row := range plans[0].rows {
					decisions[row.Key] = map[string]any{"handling": "unverified", "reason": "Source unavailable"}
					findings = append(findings, map[string]any{"claim": "A material proposition remains unverified", "verdict": "unverified", "researcher_agreement": "not_applicable", "evidence_basis": "insufficient_evidence", "analysis": "No usable evidence", "sources": []any{}, "dissent": "Unknown", "limitations": "Source unavailable"})
				}
				raw, _ := json.Marshal(map[string]any{"kind": "evidence_audit", "audit_complete": true, "question": "Question", "summary": "Audit completed with gaps", "findings": findings, "overall_limitations": "Original sources unavailable", "claim_assessments": decisions})
				payload, err := adoptedCollectionPayload(t, c, audit, raw, external, TaskResultStatusCompletedWithGaps)
				if err != nil {
					t.Fatal(err)
				}
				accept(audit, payload)
				compiled = session.ResultContracts["schemas/synthesis-v3.json"]
				ref = compiled.ref(true)
				synthesis := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "synthesizer", ResultContract: &ref, EvidenceFrom: []string{audit.ID}}})[0]
				plans, err = c.fixedCollectionPlans(synthesis.ID)
				if err != nil {
					t.Fatal(err)
				}
				citations := map[string]any{}
				answer := []any{}
				for _, row := range plans[0].rows {
					choice := map[string]any{}
					for i := range 3 {
						choice[fmt.Sprintf("answer_%d", i)] = "omit"
					}
					for i := range 6 {
						choice[fmt.Sprintf("section_%d", i)] = "omit"
					}
					choice["answer_0"] = "cite"
					citations[row.Key] = choice
					answer = []any{map[string]any{"text": "Available evidence cannot establish the proposition."}}
				}
				value := map[string]any{"synthesis_complete": true, "answer": answer, "sections": []any{}, "unresolved": "No verified findings; source access gaps remain", "citations": citations}
				if !mixed {
					value["answer"] = []any{map[string]any{"text": "Invented uncited answer"}}
					raw, _ = json.Marshal(value)
					if _, err := adoptedCollectionPayload(t, c, synthesis, raw, external, TaskResultStatusCompletedWithGaps); err == nil {
						t.Fatal("empty audit allowed a fabricated answer without evidence")
					}
					value["answer"] = answer
				}
				raw, _ = json.Marshal(value)
				payload, err = adoptedCollectionPayload(t, c, synthesis, raw, external, TaskResultStatusCompletedWithGaps)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.validateSubmittedEvidenceInputs(synthesis.ID, payload); err != nil {
					t.Fatal(err)
				}
				accept(synthesis, payload)
				stored, err := decodeResultContractJSON(payload.Value)
				if err != nil {
					t.Fatal(err)
				}
				var report bytes.Buffer
				if err := compiled.finalReport.Execute(&report, finalReportTemplateValue(stored)); err != nil || !strings.Contains(report.String(), "Original sources unavailable") {
					t.Fatalf("final report lost limitations: err=%v report=%s", err, report.String())
				}
				if err := c.SetAcceptanceSpec(AcceptanceSpec{RequiredWorkers: []string{"researcher", "countercheck", "verifier", "synthesizer"}, RequiredWorkersAcceptGaps: true, RequireNoUnresolvedTasks: true}); err != nil {
					t.Fatal(err)
				}
				if result, err := c.runAcceptance(t.Context()); err != nil || !result.IsPassed() {
					t.Fatalf("completed pipeline with gaps failed worker acceptance: %+v err=%v", result, err)
				}
			})
		}
	}
}

func TestWebResearchUnavailableSourceCannotAuthorizeCitation(t *testing.T) {
	t.Parallel()
	compiled, err := compileResultContractSchema(filepath.Join(bundledTeamsDir(t), "web-research"), "schemas/research-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	const source = "https://example.test/missing"
	input, _ := json.Marshal(map[string]any{
		"lens": "primary_search", "question": "Question", "disconfirmation_effort": "Checked alternatives", "limitations": "Source unavailable",
		"findings": []any{map[string]any{
			"claim": "An unsupported proposition", "assessment": "supported", "reasoning": "Claimed support", "open_questions": "Missing original",
			"sources": []any{map[string]any{"title": "Missing page", "publisher": "Unknown", "publication_date": "unknown", "url": source, "source_type": "primary", "page_status": "fetched", "quote": "Unable to resolve handle", "relation": "Claimed support", "independence_group": "original"}},
		}},
	})
	output, _ := json.Marshal(map[string]any{"status": "source_unavailable", "url": source, "reason": "access_error", "citable": false, "diagnostic": "Unable to resolve handle"})
	tool := compiled.toolEvidence.Tools[0]
	records := []taskTranscriptRecord{
		{Event: "tool_call", ToolCallID: "snapshot", Tool: tool, Input: `{}`},
		{Event: "tool_result", ToolCallID: "snapshot", Tool: tool, Output: string(output)},
	}
	payload, err := validateStructuredResultPayload(compiled, compiled.ref(true), input, records)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		t.Fatal(err)
	}
	finding := stored.(map[string]any)["findings"].([]any)[0].(map[string]any)
	if payload.EvidenceDowngrades != 1 || finding["assessment"] != "unverified" || finding["sources"].([]any)[0].(map[string]any)["page_status"] != "unverified" {
		t.Fatal("source availability diagnostic authorized a factual quotation")
	}
}
