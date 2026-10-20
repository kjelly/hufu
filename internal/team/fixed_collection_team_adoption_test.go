package team

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func fixedCollectionAcceptedSource(t *testing.T, c *Coordinator, schema *CompiledResultContract, raw []byte) *TodoItem {
	t.Helper()
	ref := schema.ref(true)
	payload, err := validateStructuredResultPayload(schema, ref, raw)
	if err != nil {
		t.Fatal(err)
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "producer", ResultContract: &ref}})[0]
	item.Status = TaskDone
	item.TypedResult = &TaskResult{TaskID: item.ID, Agent: item.Agent, Source: "submitted", Status: TaskResultStatusSuccess, StructuredPayload: payload}
	return item
}

func adoptedFixedCollectionFixture(t *testing.T, teamName string) (*Coordinator, *TodoItem, []resultFixedCollectionPlan, map[string]any) {
	t.Helper()
	dir := filepath.Join(bundledTeamsDir(t), teamName)
	sourcePath, targetPath := "schemas/coding-task-contract-v1.json", "schemas/criteria-assessment-v1.json"
	if teamName == "web-research" {
		sourcePath, targetPath = "schemas/research-v1.json", "schemas/report-v1.json"
	} else if teamName == "hufu-dev" {
		sourcePath, targetPath = "schemas/implementation-v1.json", "schemas/verification-v1.json"
	}
	c := newBudgetCoordinator(t)
	c.session.ResultContracts = make(map[string]*CompiledResultContract)
	for _, path := range []string{sourcePath, targetPath} {
		compiled, err := compileResultContractSchema(dir, path)
		if err != nil {
			t.Fatal(err)
		}
		c.session.ResultContracts[path] = compiled
	}
	if err := validateFixedCollectionSourceContracts(c.session.ResultContracts); err != nil {
		t.Fatal(err)
	}
	var inputs []any
	var ids []string
	count := 1
	if teamName == "web-research" {
		count = 2
	}
	for i := range count {
		value := map[string]any{"readiness": "ready", "objective": "Implement the request", "deliverables": []string{"behavior"}, "constraints": []string{}, "acceptance_criteria": []string{"First criterion", "Second criterion"}, "verification_commands": []string{"go test ./..."}}
		if teamName == "hufu-dev" {
			value = map[string]any{"changed_files": []string{}, "tests_added": []string{}, "checks": []any{}, "limitations": []string{}, "gaps": []string{"First gap", "Second gap"}}
		} else if teamName == "web-research" {
			lens := []string{"primary_search", "countercheck"}[i]
			// Both tracks can contain the same proposition. Accounting must
			// preserve both occurrences; semantic deduplication is separate.
			value = map[string]any{"lens": lens, "question": "Question", "findings": []any{map[string]any{"claim": "Shared original claim", "assessment": "unverified", "reasoning": "Unavailable primary evidence", "sources": []any{}, "open_questions": "Missing evidence"}}, "disconfirmation_effort": "Checked contrary evidence", "limitations": "Source unavailable"}
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		source := fixedCollectionAcceptedSource(t, c, c.session.ResultContracts[sourcePath], raw)
		ids = append(ids, source.ID)
		if teamName == "web-research" {
			inputs = append(inputs, map[string]any{"task_id": source.ID, "payload_sha256": source.TypedResult.StructuredPayload.SHA256, "lens": value["lens"]})
		}
	}
	ref := c.session.ResultContracts[targetPath].ref(true)
	spec := TodoSpec{Agent: "verifier", ResultContract: &ref, DependsOn: ids}
	if teamName == "web-research" {
		spec.DependsOn, spec.EvidenceFrom = nil, ids
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{spec})[0]
	plans, err := c.fixedCollectionPlans(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	value := map[string]any{}
	if teamName == "hufu-dev" {
		value = map[string]any{"checks": []any{}, "failures": []string{}, "gaps": []string{"Verification incomplete"}}
	} else if teamName == "web-research" {
		value = map[string]any{"audit_complete": true, "input_review": inputs, "question": "Question", "summary": "Completed audit with evidence gaps", "overall_limitations": "Evidence unavailable", "findings": []any{map[string]any{"claim": "Shared original claim", "verdict": "unverified", "researcher_agreement": "agree", "evidence_basis": "insufficient_evidence", "analysis": "Not established", "sources": []any{}, "dissent": "Neither track has proof", "limitations": "Evidence unavailable"}}}
	}
	decisions := make(map[string]any)
	for _, row := range plans[0].rows {
		decision := map[string]any{"reason": "Required evidence remains unavailable"}
		decision[plans[0].spec.ChoiceFields[0]] = "unverified"
		if teamName == "hufu-coding" {
			decision["evidence"] = []string{}
		}
		decisions[row.Key] = decision
	}
	value[strings.TrimPrefix(plans[0].spec.ItemsPointer, "/")] = decisions
	return c, item, plans, value
}

func adoptedCollectionPayload(t *testing.T, c *Coordinator, item *TodoItem, raw []byte, external bool) (*ResultPayload, error) {
	t.Helper()
	if external {
		input := string(raw)
		request := AttemptRequest{Task: taskDefFromTodoItem(item), resultContractSchema: c.session.ResultContracts[item.ResultContract.ID], resultEvidenceInputs: c.dependencyResultsForTask(item.ID)}
		return canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: &input}, nil)
	}
	payload, rejection := c.structuredPayloadForSubmission(item.ID, raw)
	if rejection != "" {
		return nil, fmt.Errorf("%s", rejection)
	}
	return payload, nil
}

func TestAdoptedFixedCollectionsPreserveMembershipAndAllowCorrection(t *testing.T) {
	for _, teamName := range []string{"hufu-coding", "web-research", "hufu-dev"} {
		for _, external := range []bool{false, true} {
			for _, mistake := range []string{"missing", "extra", "fixed field", "invalid choice", "array"} {
				t.Run(fmt.Sprintf("%s/external=%t/%s", teamName, external, mistake), func(t *testing.T) {
					c, item, plans, value := adoptedFixedCollectionFixture(t, teamName)
					field := strings.TrimPrefix(plans[0].spec.ItemsPointer, "/")
					valid, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					decisions := value[field].(map[string]any)
					key := plans[0].rows[0].Key
					switch mistake {
					case "missing":
						delete(decisions, key)
					case "extra":
						decisions["invented"] = decisions[key]
					case "fixed field":
						decisions[key].(map[string]any)["source_task_id"] = "foreign"
					case "invalid choice":
						decisions[key].(map[string]any)[plans[0].spec.ChoiceFields[0]] = "invented"
					case "array":
						value[field] = []any{}
					}
					invalid, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := adoptedCollectionPayload(t, c, item, invalid, external); err == nil {
						t.Fatal("invalid decision was accepted")
					}
					payload, err := adoptedCollectionPayload(t, c, item, valid, external)
					if err != nil {
						t.Fatal(err)
					}
					if err := c.validateSubmittedEvidenceInputs(item.ID, payload); err != nil {
						t.Fatal(err)
					}
					canonical, err := decodeResultContractJSON(payload.Value)
					if err != nil {
						t.Fatal(err)
					}
					rows := canonical.(map[string]any)[field].([]any)
					if len(rows) != 2 || len(payload.CollectionBindings[0].Sources) != len(c.dependencyResultsForTask(item.ID)) {
						t.Fatal("input membership or provenance was lost")
					}
					for i, row := range rows {
						if evidenceString(row, "/source_task_id") != plans[0].rows[i].Fixed["source_task_id"] {
							t.Fatal("source identity was changed")
						}
					}
				})
			}
		}
	}
}

func TestAdoptedFixedCollectionNegativeReportsRemainSubmittable(t *testing.T) {
	for _, teamName := range []string{"hufu-coding", "hufu-dev"} {
		for _, status := range []string{TaskResultStatusFailed, TaskResultStatusPartial, TaskResultStatusBlocked} {
			t.Run(teamName+"/"+status, func(t *testing.T) {
				c, item, plans, value := adoptedFixedCollectionFixture(t, teamName)
				if status == TaskResultStatusFailed {
					field := strings.TrimPrefix(plans[0].spec.ItemsPointer, "/")
					for _, decision := range value[field].(map[string]any) {
						row := decision.(map[string]any)
						row["assessment"] = "unresolved"
						if teamName == "hufu-coding" {
							row["assessment"], row["evidence"] = "unmet", []string{"observed missing behavior at source.go:42"}
						}
					}
				}
				submission := map[string]any{"status": status, "summary": "Evidence-qualified negative report", "structured_payload": value}
				if status == TaskResultStatusFailed {
					submission["findings"] = []any{map[string]any{"category": "correctness", "severity": "error", "summary": "Required behavior is missing", "detail": "Observed missing behavior at source.go:42"}}
				}
				input, err := json.Marshal(submission)
				if err != nil {
					t.Fatal(err)
				}
				tool := &submitResultTool{coordinator: c, todoID: item.ID}
				response, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{Name: submitResultToolName, Input: string(input)})
				if err != nil || response.IsError {
					t.Fatalf("negative verdict could not be submitted: %s, %v", response.Content, err)
				}
				item.TypedResult = c.GetTaskResult(item.ID)
				if item.TypedResult.Status != status || isGenuineWorkerReportedFailure(item) != (status == TaskResultStatusFailed) {
					t.Fatal("negative verdict lost its failure/remediation semantics")
				}
				if status == TaskResultStatusFailed {
					remediation := buildRemediationContext(item.ID, item)
					if remediation.FailureClass != FailureSemanticRejection || len(remediation.Findings) != 1 || remediation.Findings[0].Detail != "Observed missing behavior at source.go:42" {
						t.Fatal("negative fixed-collection verdict lost its actionable remediation evidence")
					}
				}
			})
		}
	}
}

func TestFixedCollectionDependencyScopeIsExplicitAndBounded(t *testing.T) {
	c, sources, item, _ := fixedCollectionFixture(t, 1)
	item.DependsOn, item.EvidenceFrom = []string{sources[0].ID}, nil
	if _, err := c.fixedCollectionPlans(item.ID); err == nil {
		t.Fatal("default evidence_from scope silently widened to dependencies")
	}
	compiled := c.session.ResultContracts[item.ResultContract.ID]
	compiled.fixedCollections[0].SourceScope = "dependency_results"
	if plans, err := c.fixedCollectionPlans(item.ID); err != nil || len(plans[0].rows) != 1 {
		t.Fatalf("declared dependency scope did not select the direct source: %v", err)
	}
	bridge := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "bridge", DependsOn: []string{sources[0].ID}}})[0]
	bridge.Status = TaskDone
	bridge.TypedResult = &TaskResult{TaskID: bridge.ID, Status: TaskResultStatusSuccess}
	item.DependsOn = []string{bridge.ID}
	if _, err := c.fixedCollectionPlans(item.ID); err == nil {
		t.Fatal("dependency scope walked an undeclared transitive source")
	}
	item.DependsOn = nil
	if _, err := c.fixedCollectionPlans(item.ID); err == nil {
		t.Fatal("dependency scope read an unrelated session result")
	}
	root, err := decodeResultContractJSON([]byte(fixedCollectionTestSchema))
	if err != nil {
		t.Fatal(err)
	}
	root.(map[string]any)["x-hufu-fixed-collections"].([]any)[0].(map[string]any)["source_scope"] = "all_tasks"
	if _, err := compileResultFixedCollections(root.(map[string]any)); err == nil {
		t.Fatal("unbounded source scope was accepted")
	}
}

func TestAdoptedFixedCollectionSourceReplacementInvalidatesDecisions(t *testing.T) {
	for _, teamName := range []string{"hufu-coding", "web-research", "hufu-dev"} {
		t.Run(teamName, func(t *testing.T) {
			c, item, plans, value := adoptedFixedCollectionFixture(t, teamName)
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := adoptedCollectionPayload(t, c, item, raw, false)
			if err != nil {
				t.Fatal(err)
			}
			source := c.todoItemByID(plans[0].binding.Sources[0].TaskID)
			var changed map[string]any
			if err := json.Unmarshal(source.TypedResult.StructuredPayload.Value, &changed); err != nil {
				t.Fatal(err)
			}
			if teamName == "hufu-coding" {
				changed["objective"] = "Changed objective with identical criterion count"
			} else if teamName == "hufu-dev" {
				changed["limitations"] = []string{"Changed implementation evidence"}
			} else {
				changed["limitations"] = "Changed research evidence"
			}
			newRaw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			newPayload, err := validateStructuredResultPayload(c.session.ResultContracts[source.ResultContract.ID], *source.ResultContract, newRaw)
			if err != nil {
				t.Fatal(err)
			}
			source.TypedResult.StructuredPayload = newPayload
			if err := c.taskTracker.TodoList().SetTypedResult(source.ID, source.TypedResult); err != nil {
				t.Fatal(err)
			}
			if err := c.validateSubmittedEvidenceInputs(item.ID, payload); err == nil {
				t.Fatal("old canonical result survived changed input evidence")
			}
			for _, external := range []bool{false, true} {
				if _, err := adoptedCollectionPayload(t, c, item, raw, external); err == nil {
					t.Fatal("old decision keys survived input replacement")
				}
			}
		})
	}
}

func TestAdoptedFixedCollectionEmptyInputIsNotMissingEvidence(t *testing.T) {
	c, item, plans, value := adoptedFixedCollectionFixture(t, "hufu-dev")
	source := c.todoItemByID(plans[0].binding.Sources[0].TaskID)
	payload, err := validateStructuredResultPayload(c.session.ResultContracts[source.ResultContract.ID], *source.ResultContract, hufuDevPayload("implementation-engineer"))
	if err != nil {
		t.Fatal(err)
	}
	source.TypedResult.StructuredPayload = payload
	if err := c.taskTracker.TodoList().SetTypedResult(source.ID, source.TypedResult); err != nil {
		t.Fatal(err)
	}
	value["implementation_gap_assessments"] = map[string]any{}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, external := range []bool{false, true} {
		accepted, err := adoptedCollectionPayload(t, c, item, raw, external)
		if err != nil {
			t.Fatal(err)
		}
		if len(accepted.CollectionBindings) != 1 || len(accepted.CollectionBindings[0].Sources) != 1 {
			t.Fatal("empty inventory lost its accepted source identity")
		}
	}
	item.DependsOn = nil
	if _, err := adoptedCollectionPayload(t, c, item, raw, false); err == nil {
		t.Fatal("missing producer was mistaken for an empty accepted inventory")
	}
}
