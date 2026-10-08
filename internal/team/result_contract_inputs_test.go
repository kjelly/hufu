package team

import (
	"encoding/json"
	"strings"
	"testing"
	"text/template"

	"charm.land/fantasy"
)

func reviewedInputsFixture(t *testing.T) (*Coordinator, *TodoItem, *TodoItem, map[string]any) {
	t.Helper()
	c, source, audit, _, _ := evidenceDeliveryFixture(t)
	dir := t.TempDir()
	schema := map[string]any{
		"type":     "object",
		"required": []string{"done", "reviews"},
		"x-hufu-evidence-inputs": map[string]any{
			"items_pointer": "/reviews", "task_id_pointer": "/origin",
			"hash_pointer": "/digest", "complete_pointer": "/done",
			"value_bindings": map[string]string{"/category": "/category"},
		},
		"properties": map[string]any{
			"done": map[string]any{"type": "boolean"},
			"reviews": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "required": []string{"origin", "digest", "category"},
				"properties": map[string]any{
					"origin":   map[string]any{"type": "string"},
					"digest":   map[string]any{"type": "string"},
					"category": map[string]any{"type": "string"},
				},
			}},
		},
	}
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	writeResultContractSchema(t, dir, "review.json", string(data))
	writeResultContractSchema(t, dir, "source.json", "{\"type\":\"object\",\"required\":[\"category\"],\"properties\":{\"category\":{\"type\":\"string\"}}}")
	for _, name := range []string{"source.json", "review.json"} {
		contract, err := compileResultContractSchema(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		c.session.ResultContracts[contract.ID] = contract
		ref := contract.ref(true)
		if name == "source.json" {
			source.ResultContract = &ref
			payload, err := validateStructuredResultPayload(contract, ref, []byte("{\"category\":\"original_document\"}"))
			if err != nil {
				t.Fatal(err)
			}
			source.TypedResult.StructuredPayload = payload
		} else {
			audit.ResultContract = &ref
		}
	}
	audit.Execution.RequiresEvidence = false
	value := map[string]any{"done": true, "reviews": []any{map[string]any{
		"origin": source.ID, "digest": source.TypedResult.StructuredPayload.SHA256,
		"category": "original_document",
	}}}
	return c, source, audit, value
}

func TestResultEvidenceInputBinding(t *testing.T) {
	for _, name := range []string{"valid", "quoted_id", "wrong_id", "wrong_hash", "wrong_value", "duplicate", "omitted", "partial_empty", "partial_forged", "tampered_source", "missing_source"} {
		t.Run(name, func(t *testing.T) {
			c, source, audit, value := reviewedInputsFixture(t)
			entries := value["reviews"].([]any)
			entry := entries[0].(map[string]any)
			switch name {
			case "quoted_id":
				entry["origin"] = "\"" + source.ID + "\""
			case "wrong_id":
				entry["origin"] = "unknown"
			case "wrong_hash", "partial_forged":
				entry["digest"] = strings.Repeat("0", 64)
			case "wrong_value":
				entry["category"] = "another_category"
			case "duplicate":
				value["reviews"] = append(entries, entry)
			case "omitted", "partial_empty":
				value["reviews"] = []any{}
			case "tampered_source":
				source.TypedResult.StructuredPayload.Value = json.RawMessage("{\"category\":\"changed\"}")
			case "missing_source":
				source.TypedResult = nil
			}
			if strings.HasPrefix(name, "partial_") {
				value["done"] = false
			}
			contract, err := c.compiledResultContract(audit.ResultContract)
			if err != nil {
				t.Fatal(err)
			}
			err = validateResultEvidenceInputs(value, contract.evidenceInputs, audit.EvidenceFrom, c.dependencyResultsForTask(audit.ID))
			wantValid := name == "valid" || name == "partial_empty"
			if (err == nil) != wantValid {
				t.Fatalf("valid=%v error=%v", wantValid, err)
			}
		})
	}
}

func TestResultEvidenceInputSubmissionRoutes(t *testing.T) {
	for _, route := range []string{"local", "external"} {
		for _, forged := range []bool{false, true} {
			name := route + "/valid"
			if forged {
				name = route + "/forged"
			}
			t.Run(name, func(t *testing.T) {
				c, _, audit, value := reviewedInputsFixture(t)
				if forged {
					value["reviews"].([]any)[0].(map[string]any)["origin"] = "unknown"
				}
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if route == "local" {
					response, err := (&submitResultTool{coordinator: c, todoID: audit.ID}).Run(occurrenceTestContext(c, audit.ID, 1), fantasy.ToolCall{Input: "{\"status\":\"success\",\"summary\":\"review\",\"structured_payload\":" + string(data) + "}"})
					if err != nil {
						t.Fatal(err)
					}
					if response.IsError != forged {
						t.Fatalf("response=%#v", response)
					}
					if forged && c.GetTaskResult(audit.ID) != nil {
						t.Fatal("forged review published")
					}
				} else {
					contract, err := c.compiledResultContract(audit.ResultContract)
					if err != nil {
						t.Fatal(err)
					}
					request := AttemptRequest{Task: taskDefFromTodoItem(audit), resultContractSchema: contract, resultEvidenceInputs: c.dependencyResultsForTask(audit.ID)}
					_, err = canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(string(data))})
					if (err != nil) != forged {
						t.Fatalf("error=%v", err)
					}
					request.resultEvidenceInputs = nil
					if _, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(string(data))}); err == nil {
						t.Fatal("missing accepted inputs allowed")
					}
				}
			})
		}
	}
}

func TestResultEvidenceInputReplayRejectsSourceDrift(t *testing.T) {
	c, source, audit, value := reviewedInputsFixture(t)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := c.compiledResultContract(audit.ResultContract)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := validateStructuredResultPayload(contract, *audit.ResultContract, data)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var restored ResultPayload
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if err := c.validateSubmittedEvidenceInputs(audit.ID, &restored); err != nil {
		t.Fatal(err)
	}
	contract.finalReport = template.Must(template.New("review").Parse("Review complete: {{.done}}"))
	audit.Status = TaskDone
	audit.TypedResult = &TaskResult{TaskID: audit.ID, Status: TaskResultStatusSuccess, StructuredPayload: &restored}
	if _, selected, err := c.renderedContractFinalReport(); err != nil || !selected {
		t.Fatalf("valid persisted report rejected: selected=%v err=%v", selected, err)
	}
	sourceContract, err := c.compiledResultContract(source.ResultContract)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := validateStructuredResultPayload(sourceContract, *source.ResultContract, []byte("{\"category\":\"replacement\"}"))
	if err != nil {
		t.Fatal(err)
	}
	source.TypedResult.StructuredPayload = changed
	if err := c.validateSubmittedEvidenceInputs(audit.ID, &restored); err == nil {
		t.Fatal("stale review accepted after source replacement")
	}
	if _, _, err := c.renderedContractFinalReport(); err == nil {
		t.Fatal("final report rendered stale input binding")
	}
}

func TestResultEvidenceInputExtensionRejectsInvalidDeclarations(t *testing.T) {
	for _, raw := range []string{"null", "{}", "{\"unknown\":true}", "{\"items_pointer\":\"not/a/pointer\",\"task_id_pointer\":\"/id\",\"hash_pointer\":\"/hash\",\"complete_pointer\":\"/done\"}"} {
		t.Run(raw, func(t *testing.T) {
			var declaration any
			if err := json.Unmarshal([]byte(raw), &declaration); err != nil {
				t.Fatal(err)
			}
			if _, err := compileResultEvidenceInputs(map[string]any{"x-hufu-evidence-inputs": declaration}); err == nil {
				t.Fatal("invalid extension accepted")
			}
		})
	}
}
