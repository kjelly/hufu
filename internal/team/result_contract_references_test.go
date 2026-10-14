package team

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"charm.land/fantasy"
)

func referenceBindingFixture(t *testing.T) (*Coordinator, *TodoItem, *TodoItem, map[string]any) {
	t.Helper()
	c, source, audit, value := reviewedInputsFixture(t)
	source.TypedResult.FilesRead = []FileRef{{Path: "opaque-source"}}
	contract := c.session.ResultContracts[audit.ResultContract.ID]
	contract.evidenceReferences = &resultEvidenceReferencesSpec{
		ItemsPointer: "/reviews", TaskIDPointer: "/origin", ReferencesPointer: "/refs",
		SourceMatch: &resultReferenceMatch{Pointer: "/category", Value: "original_document"},
	}
	value["reviews"].([]any)[0].(map[string]any)["refs"] = []any{"opaque-source"}
	return c, source, audit, value
}

func TestResultReferenceOwnershipAndReads(t *testing.T) {
	for _, name := range []string{"valid", "wrong_task", "wrong_reference", "missing_reads", "wrong_source", "empty_refs", "not_array", "not_string", "retained", "self_reference"} {
		t.Run(name, func(t *testing.T) {
			c, source, audit, value := referenceBindingFixture(t)
			contract := c.session.ResultContracts[audit.ResultContract.ID]
			spec := *contract.evidenceReferences
			entry := value["reviews"].([]any)[0].(map[string]any)
			reads := []FileRef{{Path: "opaque-source"}}
			switch name {
			case "wrong_task":
				entry["origin"] = "unknown"
			case "wrong_reference":
				entry["refs"] = []any{"unread"}
			case "missing_reads":
				reads = nil
			case "wrong_source":
				spec.SourceMatch.Value = "other_category"
			case "empty_refs":
				entry["refs"] = []any{}
			case "not_array":
				entry["refs"] = "opaque-source"
			case "not_string":
				entry["refs"] = []any{false}
			case "retained":
				spec.When = &resultReferenceMatch{Pointer: "/state", Value: "resolved"}
				entry["state"] = "retained"
				entry["refs"] = []any{}
			case "self_reference":
				spec.ExcludeTaskIDPointer = "/source"
				entry["source"] = source.ID
			}
			err := validateResultEvidenceReferences(value, &spec, audit.EvidenceFrom, c.dependencyResultsForTask(audit.ID), &reads)
			valid := name == "valid" || name == "retained"
			if (err == nil) != valid {
				t.Fatalf("valid=%v, error=%v", valid, err)
			}
		})
	}
}

func TestResultReferenceRejectedSubmissionCanBeCorrected(t *testing.T) {
	for _, route := range []string{"local", "external"} {
		t.Run(route, func(t *testing.T) {
			c, source, audit, value := referenceBindingFixture(t)
			source.TypedResult.FilesRead = nil
			for _, accepted := range []bool{false, true} {
				if accepted {
					source.TypedResult.FilesRead = []FileRef{{Path: "opaque-source"}}
				}
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if route == "external" {
					request := AttemptRequest{Task: taskDefFromTodoItem(audit), resultContractSchema: c.session.ResultContracts[audit.ResultContract.ID], resultEvidenceInputs: c.dependencyResultsForTask(audit.ID)}
					_, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(string(data))}, []FileRef{{Path: "opaque-source"}})
					if (err == nil) != accepted {
						t.Fatalf("accepted=%v, err=%v", accepted, err)
					}
				} else {
					input := `{"status":"success","summary":"review","files_read":[{"path":"opaque-source"}],"structured_payload":` + string(data) + `}`
					response, err := (&submitResultTool{coordinator: c, todoID: audit.ID}).Run(occurrenceTestContext(c, audit.ID, 1), fantasy.ToolCall{Input: input})
					if err != nil || response.IsError == accepted {
						t.Fatalf("accepted=%v, response=%#v, err=%v", accepted, response, err)
					}
					if !accepted && c.GetTaskResult(audit.ID) != nil {
						t.Fatal("rejected references were published")
					}
				}
			}
		})
	}
}

func TestReferenceExtensionPreflight(t *testing.T) {
	valid := map[string]any{"items_pointer": "/links", "task_id_pointer": "/origin", "references_pointer": "/refs"}
	for _, name := range []string{"valid", "missing", "pointer", "unknown", "condition"} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(valid)
			var spec map[string]any
			if err := json.Unmarshal(data, &spec); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "missing":
				delete(spec, "items_pointer")
			case "pointer":
				spec["task_id_pointer"] = "origin"
			case "unknown":
				spec["guess"] = true
			case "condition":
				spec["when"] = map[string]any{"pointer": "/status"}
			}
			_, err := compileResultEvidenceReferences(map[string]any{"x-hufu-evidence-references": spec})
			if (err == nil) != (name == "valid") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestReferenceOwnershipFeedbackNamesAcceptedReaders(t *testing.T) {
	c, source, audit, value := referenceBindingFixture(t)
	results := c.dependencyResultsForTask(audit.ID)
	other := *source.TypedResult
	other.TaskID = "other"
	results = append(results, other)
	results[0].FilesRead = nil
	err := validateResultEvidenceReferences(value, c.session.ResultContracts[audit.ResultContract.ID].evidenceReferences, append(audit.EvidenceFrom, "other"), results, nil)
	if err == nil || !strings.Contains(err.Error(), "accepted readers: [other]") {
		t.Fatalf("feedback=%v", err)
	}
}

func TestExternalResultReferenceChecksCanonicalReadsWithoutRequiredDelivery(t *testing.T) {
	for _, name := range []string{"valid", "missing_submitter_reads", "missing_source_reads"} {
		t.Run(name, func(t *testing.T) {
			c, source, audit, value := referenceBindingFixture(t)
			const path = "source.txt"
			source.TypedResult.FilesRead = []FileRef{{Path: path}}
			value["reviews"].([]any)[0].(map[string]any)["refs"] = []any{path}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, path), []byte("accepted source"), 0o600); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			proposal := &WorkerResultProposal{Status: TaskResultStatusSuccess, Summary: "review", StructuredPayloadJSON: new(string(data)), FilesRead: []string{"./" + path}}
			if name == "missing_submitter_reads" {
				proposal.FilesRead = nil
			}
			if name == "missing_source_reads" {
				source.TypedResult.FilesRead = nil
			}
			request := scopedAttemptRequest("codex", audit.ID)
			request.Task = taskDefFromTodoItem(audit)
			request.resultContractSchema = c.session.ResultContracts[audit.ResultContract.ID]
			request.resultEvidenceInputs = c.dependencyResultsForTask(audit.ID)
			result, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), request, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
			if name != "valid" {
				if err == nil || result != nil {
					t.Fatalf("unbound external references admitted: result=%#v err=%v", result, err)
				}
				return
			}
			if err != nil || len(result.FilesRead) != 1 || result.FilesRead[0].Path != path {
				t.Fatalf("canonical read binding rejected: result=%#v err=%v", result, err)
			}
		})
	}
}

func TestReplayedResultReferenceChecksWithoutRequiredDelivery(t *testing.T) {
	for _, name := range []string{"valid", "missing_submitter_reads", "missing_source_reads", "tampered_source_hash"} {
		t.Run(name, func(t *testing.T) {
			c, source, audit, value := referenceBindingFixture(t)
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := validateStructuredResultPayload(c.session.ResultContracts[audit.ResultContract.ID], *audit.ResultContract, data)
			if err != nil {
				t.Fatal(err)
			}
			audit.Status = TaskDone
			audit.TypedResult = &TaskResult{TaskID: audit.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload, FilesRead: []FileRef{{Path: "opaque-source"}}}
			switch name {
			case "missing_submitter_reads":
				audit.TypedResult.FilesRead = nil
			case "missing_source_reads":
				source.TypedResult.FilesRead = nil
			case "tampered_source_hash":
				source.TypedResult.StructuredPayload.SHA256 = "forged"
			}
			var events []RunEvent
			for _, item := range c.taskTracker.TodoList().Items() {
				data, err := json.Marshal(taskTransitionPayload(item))
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventTaskCreated), TaskID: item.ID, Payload: data})
			}
			items, err := ReplayTodoList(events)
			if err != nil {
				t.Fatal(err)
			}
			c.taskTracker = &TaskTracker{todo: &TodoList{items: items}}
			err = c.validateCompletedEvidenceDeliveries()
			if (err == nil) != (name == "valid") {
				t.Fatalf("replay reference validation: %v", err)
			}
			if name != "valid" {
				response, err := (&finishTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Input: `{"response":"completed"}`})
				if err != nil || !response.IsError || c.finishCalled.Load() {
					t.Fatalf("invalid replay escaped finish: response=%#v err=%v", response, err)
				}
			}
			c.session.ResultContracts[audit.ResultContract.ID].finalReport = template.Must(template.New("report").Parse("Complete: {{.done}}"))
			_, configured, err := c.renderedContractFinalReport()
			if !configured || (err == nil) != (name == "valid") {
				t.Fatalf("replayed final report reference validation: configured=%v err=%v", configured, err)
			}
		})
	}
}
