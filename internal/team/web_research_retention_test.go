package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestContractDataflowPreservesResearchBeyondPresentationLimits(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%t", external), func(t *testing.T) {
			c := newTestCoordinator(t)
			session, err := LoadTeam(filepath.Join(bundledTeamsDir(t), "web-research"), nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatal(err)
			}
			accept := func(item *TodoItem, payload *ResultPayload) {
				t.Helper()
				result := &TaskResult{TaskID: item.ID, Agent: item.Agent, Source: "submitted", Status: TaskResultStatusCompletedWithGaps, Summary: "Complete with evidence gaps", StructuredPayload: payload.clone()}
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
				c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "verified")
			}
			c.session.ResultContracts = make(map[string]*CompiledResultContract)
			for _, name := range []string{"research-v2", "audit-v2", "synthesis-v2"} {
				id := "schemas/" + name + ".json"
				compiled, err := compileResultContractSchema(filepath.Join(bundledTeamsDir(t), "web-research"), id)
				if err != nil {
					t.Fatal(err)
				}
				c.session.ResultContracts[id] = compiled
			}
			if err := validateFixedCollectionSourceContracts(c.session.ResultContracts); err != nil {
				t.Fatal(err)
			}
			var ids []string
			var findings []any
			for track, count := range []int{14, 19} {
				var originals []any
				for i := range count {
					claim := fmt.Sprintf("Track %d atomic claim %d", track, i)
					originals = append(originals, map[string]any{"claim": claim, "assessment": "unverified", "reasoning": "Evidence unavailable", "sources": []any{}, "open_questions": "Missing evidence"})
					findings = append(findings, map[string]any{"claim": claim, "verdict": "unverified", "researcher_agreement": "not_applicable", "evidence_basis": "insufficient_evidence", "analysis": "Evidence unavailable", "sources": []any{}, "dissent": "Unknown", "limitations": "Missing evidence"})
				}
				raw, err := json.Marshal(map[string]any{"lens": []string{"primary_search", "countercheck"}[track], "question": "Question", "findings": originals, "disconfirmation_effort": "Checked contrary evidence", "limitations": "Missing evidence"})
				if err != nil {
					t.Fatal(err)
				}
				schema := c.session.ResultContracts["schemas/research-v2.json"]
				ref := schema.ref(true)
				source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: []string{"researcher", "countercheck"}[track], ContractID: []string{"primary-research", "counter-research"}[track], ResultContract: &ref}})[0]
				payload, err := validateStructuredResultPayload(schema, ref, raw)
				if err != nil {
					t.Fatal(err)
				}
				accept(source, payload)
				ids = append(ids, source.ID)
			}
			auditSchema := c.session.ResultContracts["schemas/audit-v2.json"]
			ref := auditSchema.ref(true)
			audit := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "verifier", ContractID: "evidence-audit", ResultContract: &ref, EvidenceFrom: ids}})[0]
			plans, err := c.fixedCollectionPlans(audit.ID)
			if err != nil || len(plans[0].rows) != 33 {
				t.Fatalf("research inventory lost claims: %v", err)
			}
			decisions := make(map[string]any)
			for _, row := range plans[0].rows {
				decisions[row.Key] = map[string]any{"handling": "unverified", "reason": "Missing evidence"}
			}
			value := map[string]any{"kind": "evidence_audit", "audit_complete": true, "claim_assessments": decisions, "question": "Question", "summary": "Completed audit with gaps", "findings": findings, "overall_limitations": "Missing evidence"}
			key := plans[0].rows[0].Key
			first := decisions[key]
			delete(decisions, key)
			raw, _ := json.Marshal(value)
			if _, err := adoptedCollectionPayload(t, c, audit, raw, external); err == nil {
				t.Fatal("missing original claim was accepted")
			}
			decisions[key] = first
			raw, _ = json.Marshal(value)
			payload, err := adoptedCollectionPayload(t, c, audit, raw, external)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.validateSubmittedEvidenceInputs(audit.ID, payload); err != nil {
				t.Fatal(err)
			}
			// Carry the complete accepted binding and its original source scope.
			accept(audit, payload)
			schema := c.session.ResultContracts["schemas/synthesis-v2.json"]
			ref = schema.ref(true)
			synthesis := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "synthesizer", ContractID: "research-synthesis", ResultContract: &ref, EvidenceFrom: []string{audit.ID}}})[0]
			plans, err = c.fixedCollectionPlans(synthesis.ID)
			if err != nil || len(plans[0].rows) != 33 {
				t.Fatalf("audit inventory lost findings: %v", err)
			}
			citations := make(map[string]any)
			for i, row := range plans[0].rows {
				choice := map[string]any{}
				for j := range 3 {
					choice[fmt.Sprintf("answer_%d", j)] = "omit"
				}
				for j := range 6 {
					choice[fmt.Sprintf("section_%d", j)] = "omit"
				}
				if i == 32 {
					choice["answer_0"] = "cite"
				}
				citations[row.Key] = choice
			}
			value = map[string]any{"synthesis_complete": true, "answer": []any{map[string]any{"text": "Available evidence cannot resolve the question."}}, "sections": []any{}, "unresolved": "Missing evidence", "citations": citations}
			last := citations[plans[0].rows[32].Key].(map[string]any)
			last["finding_index"] = 999
			raw, _ = json.Marshal(value)
			if _, err := adoptedCollectionPayload(t, c, synthesis, raw, external); err == nil {
				t.Fatal("model override of runtime-owned finding index accepted")
			}
			delete(last, "finding_index")
			last["answer_1"] = "cite"
			raw, _ = json.Marshal(value)
			if _, err := adoptedCollectionPayload(t, c, synthesis, raw, external); err == nil {
				t.Fatal("citation of nonexistent paragraph accepted")
			}
			last["answer_1"] = "omit"
			paragraph := value["answer"].([]any)[0].(map[string]any)
			paragraph["text_note"] = ""
			raw, _ = json.Marshal(value)
			if _, err := adoptedCollectionPayload(t, c, synthesis, raw, external); err == nil {
				t.Fatal("undeclared paragraph property was accepted")
			}
			delete(paragraph, "text_note")
			raw, _ = json.Marshal(value)
			payload, err = adoptedCollectionPayload(t, c, synthesis, raw, external)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.validateSubmittedEvidenceInputs(synthesis.ID, payload); err != nil {
				t.Fatal(err)
			}
			stored, err := decodeResultContractJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			bound := stored.(map[string]any)
			boundAudit := bound["input_review"].([]any)[0].(map[string]any)
			if len(boundAudit["findings"].([]any)) != 33 || len(boundAudit["claim_assessments"].([]any)) != 33 || len(bound["citations"].([]any)) != 33 {
				t.Fatal("final result truncated accepted evidence")
			}
			var rendered bytes.Buffer
			if err := schema.finalReport.Execute(&rendered, finalReportTemplateValue(stored)); err != nil {
				t.Fatal(err)
			}
			for _, finding := range findings {
				if !strings.Contains(rendered.String(), finding.(map[string]any)["claim"].(string)) {
					t.Fatal("final report omitted an original finding")
				}
			}
			if !strings.Contains(rendered.String(), "(#查核-32)") || strings.Contains(rendered.String(), "(#查核-999)") {
				t.Fatal("final report has invalid citation bindings")
			}
			accept(synthesis, payload)
			if err := c.SetAcceptanceSpec(AcceptanceSpec{RequiredWorkers: []string{"researcher", "countercheck", "verifier", "synthesizer"}, RequiredWorkersAcceptGaps: true, RequireNoUnresolvedTasks: true}); err != nil {
				t.Fatal(err)
			}
			if result, err := c.runAcceptance(t.Context()); err != nil || !result.IsPassed() {
				t.Fatalf("complete evidence pipeline failed acceptance: %+v %v", result, err)
			}
		})
	}
}

func TestContractDataflowBindsEveryResearchSourceDiagnostic(t *testing.T) {
	compiled, err := compileResultContractSchema(filepath.Join(bundledTeamsDir(t), "web-research"), "schemas/research-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var sources []any
	var records []taskTranscriptRecord
	for i := range 5 {
		url := fmt.Sprintf("https://example.com/source/%d", i)
		quote := "exact source words"
		if i == 4 {
			quote = "mismatched quotation"
		}
		sources = append(sources, map[string]any{"title": "Source", "publisher": "Publisher", "publication_date": "unknown", "url": url, "source_type": "primary", "page_status": "fetched", "quote": quote, "relation": "Required evidence", "independence_group": fmt.Sprint(i)})
		callID := fmt.Sprintf("snapshot-%d", i)
		records = append(records, taskTranscriptRecord{Event: "tool_call", Tool: compiled.toolEvidence.Tools[0], ToolCallID: callID, Input: `{}`}, taskTranscriptRecord{Event: "tool_result", Tool: compiled.toolEvidence.Tools[0], ToolCallID: callID, Output: fmt.Sprintf("### Page\n- Page URL: %s\n### Snapshot\n```yaml\n- paragraph: exact source words\n```\n", url)})
	}
	raw, err := json.Marshal(map[string]any{"lens": "primary_search", "question": "Question", "findings": []any{map[string]any{"claim": "One atomic claim", "assessment": "supported", "reasoning": "Source evidence", "sources": sources, "open_questions": "Unresolved scope"}}, "disconfirmation_effort": "Checked contrary evidence", "limitations": "Evidence limits"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := validateStructuredResultPayload(compiled, compiled.ref(true), raw, records)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		t.Fatal(err)
	}
	finding := value.(map[string]any)["findings"].([]any)[0].(map[string]any)
	bound := finding["sources"].([]any)
	if len(bound) != 5 || finding["assessment"] != "unverified" || payload.EvidenceDowngrades != 1 || bound[4].(map[string]any)["quote"] != "mismatched quotation" {
		t.Fatal("source count repair erased evidence or promoted a mismatched quote")
	}
	for i, source := range bound {
		want := "matched"
		if i == 4 {
			want = "mismatch"
		}
		if source.(map[string]any)["citation_status"] != want {
			t.Fatalf("source %d lost its original citation status", i)
		}
	}
}
