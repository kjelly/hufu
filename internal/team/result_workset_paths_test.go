package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

const worksetPathResultSchema = `{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","properties":{"location":{"type":"string"},"summary":{"type":"string"}},"required":["location","summary"],"additionalProperties":false}}},"required":["findings"],"additionalProperties":false,"x-hufu-workset-paths":{"pointers":["/findings/*/location"],"format":"path_line"}}`

const worksetLineInventory = `{"sources":[{"path":"dir/source.go","available_spans":[{"start_line":1,"end_line":804}]},{"path":"dir/short.go","available_spans":[{"start_line":1,"end_line":9}]}]}`

func withWorksetLineSpans(document string) string {
	return strings.Replace(document, `"format":"path_line"`, `"format":"path_line","line_spans":{"artifact_kind":"source"}`, 1)
}

func TestResultWorksetPathsCompile(t *testing.T) {
	for _, tc := range []struct {
		name, document string
		valid          bool
	}{
		{"valid", worksetPathResultSchema, true},
		{"frozen_line_spans", withWorksetLineSpans(worksetPathResultSchema), true},
		{"blank_artifact_kind", strings.Replace(withWorksetLineSpans(worksetPathResultSchema), `"artifact_kind":"source"`, `"artifact_kind":""`, 1), false},
		{"unknown_format", strings.Replace(worksetPathResultSchema, "path_line", "fuzzy", 1), false},
		{"unknown_member", strings.Replace(worksetPathResultSchema, `"format":"path_line"`, `"format":"path_line","guess":true`, 1), false},
		{"not_string", strings.Replace(worksetPathResultSchema, `"location":{"type":"string"}`, `"location":{"type":"integer"}`, 1), false},
		{"duplicate", strings.Replace(worksetPathResultSchema, `["/findings/*/location"]`, `["/findings/*/location","/findings/*/location"]`, 1), false},
		{"artifact_overlap", strings.TrimSuffix(worksetPathResultSchema, "}") + `,"x-hufu-evidence-keys":{"pointers":["/findings/*/location"]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompileResultContractDocument("paths.json", []byte(tc.document))
			if (err == nil) != tc.valid {
				t.Fatalf("compile valid=%t: %v", tc.valid, err)
			}
		})
	}
}

func TestResultWorksetPathsResolveWithinFrozenAssignment(t *testing.T) {
	c, producer, child, _ := runtimeWorksetInputFixture(t, "dir/source.go")
	compiled, err := CompileResultContractDocument("paths.json", []byte(worksetPathResultSchema))
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts[compiled.ID] = compiled
	ref := compiled.ref(true)
	child.ResultContract = &ref
	lookup := func() (worksetPathSelection, error) {
		return c.verifiedWorksetPathSelection(t.Context(), child.ID, compiled.worksetPaths)
	}
	selection, err := lookup()
	if err != nil || len(selection.paths) != 1 {
		t.Fatalf("choices=%v: %v", selection.paths, err)
	}
	var key string
	for candidate := range selection.paths {
		key = candidate
	}
	for _, tc := range []struct {
		name, location string
		valid          bool
	}{
		{"key", key + ":7", true},
		{"exact", "dir/source.go:7", true},
		{"typo", "dir/soruce.go:7", false},
		{"foreign_key", "path_000000000000:7", false},
		{"symbol", "dir/source.go:Func", false},
		{"range", "dir/source.go:7-9", false},
		{"zero", key + ":0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			submission := map[string]any{"status": "success", "summary": "unchanged", "details": "dir/soruce.go:7", "structured_payload": map[string]any{"findings": []any{map[string]any{"location": tc.location, "summary": "keep the original claim"}}}}
			body, _ := json.Marshal(submission)
			resolved, err := c.resolveResultEvidenceKeys(t.Context(), child.ID, string(body))
			if (err == nil) != tc.valid {
				t.Fatalf("resolve valid=%t: %v", tc.valid, err)
			}
			if !tc.valid {
				return
			}
			var claims map[string]any
			if err := json.Unmarshal([]byte(resolved), &claims); err != nil {
				t.Fatal(err)
			}
			if claims["summary"] != "unchanged" || claims["details"] != "dir/soruce.go:7" {
				t.Fatal("resolver rewrote unrelated claims")
			}
			payload, _ := json.Marshal(claims["structured_payload"])
			for _, external := range []bool{false, true} {
				var admitted *ResultPayload
				if external {
					original, _ := json.Marshal(submission["structured_payload"])
					raw := string(original)
					admitted, err = canonicalExternalStructuredPayload(AttemptRequest{Task: taskDefFromTodoItem(child), resultContractSchema: compiled,
						resultWorksetPaths: lookup}, &WorkerResultProposal{StructuredPayloadJSON: &raw}, nil)
				} else {
					var rejection string
					admitted, rejection = c.structuredPayloadForSubmission(child.ID, payload)
					if rejection != "" {
						t.Fatal(rejection)
					}
				}
				if err != nil || admitted == nil || !strings.Contains(string(admitted.Value), `"location":"dir/source.go:7"`) {
					t.Fatalf("admission external=%t: %v", external, err)
				}
				if err := c.validateSubmittedEvidenceInputs(child.ID, admitted); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if err := c.CommitTaskResetForRetry(t.Context(), producer.ID, "retry producer"); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitTaskTransition(t.Context(), producer.ID, TaskPending, TaskInProgress, "new producer occurrence", "", attemptStartMetadata(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup(); err == nil {
		t.Fatal("stale producer admitted")
	}
	_, err = c.resolveResultEvidenceKeys(t.Context(), child.ID, `{"status":"success","summary":"keep","structured_payload":{"findings":[{"location":"dir/soruce.go:7","summary":"keep"}]}}`)
	if err == nil || localResultRejectionReason(err) != RepairFailureSource || isResultReferenceChoiceError(err) {
		t.Fatal("path typo concealed a stale producer or earned format repair")
	}
}

func TestResultWorksetPathsCheckFrozenSourceSpansOnEverySubmissionRoute(t *testing.T) {
	c := newTestCoordinator(t)
	c.taskTracker.TodoList().SetRunID(c.executionRunID)
	document := withWorksetLineSpans(worksetPathResultSchema)
	compiled, err := CompileResultContractDocument("paths.json", []byte(document))
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	_, manifest := runtimeWorksetProducerFixture(t, c, []byte(`{"schema_version":1,"items":[{"key":"sample","touched_paths":["dir/source.go","dir/short.go"]}]}`), PutArtifactRequest{Kind: "source", Path: "line-inventory.json", Content: []byte(worksetLineInventory)})
	expansion, err := c.expandFanOutTasks([]TaskDef{{ID: "inspect", Agent: "worker", FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "seed", Artifact: manifest.ID}, GoalTemplate: "Inspect frozen sources"}}})
	if err != nil || len(expansion) != 1 {
		t.Fatalf("expand: %v", err)
	}
	ref := compiled.ref(true)
	child := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", PlanTaskID: expansion[0].ID, WorksetBinding: expansion[0].WorksetBinding, ResultContract: &ref}})[0]
	receipts, err := buildWorksetReceipts(expansion, []string{child.ID}, c.executionRunID)
	if err != nil {
		t.Fatal(err)
	}
	child.WorksetReceipt = receipts[child.WorksetBinding.WorksetID]
	lookup := func() (worksetPathSelection, error) {
		return c.verifiedWorksetPathSelection(t.Context(), child.ID, compiled.worksetPaths)
	}
	for _, test := range []struct {
		name, location string
		valid          bool
	}{
		{"end_line", "dir/short.go:9", true},
		{"wrong_assigned_path", "dir/short.go:67", false},
		{"intended_path", "dir/source.go:67", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := `{"findings":[{"location":"` + test.location + `","summary":"keep the claim"}]}`
			submission := `{"status":"success","summary":"keep","structured_payload":` + payload + `}`
			_, localErr := c.resolveResultEvidenceKeys(t.Context(), child.ID, submission)
			_, externalErr := canonicalExternalStructuredPayload(AttemptRequest{Task: taskDefFromTodoItem(child), resultContractSchema: compiled, resultWorksetPaths: lookup},
				&WorkerResultProposal{StructuredPayloadJSON: &payload}, nil)
			for _, err := range []error{localErr, externalErr} {
				if test.valid && err != nil || !test.valid && !isResultReferenceChoiceError(err) {
					t.Fatalf("submission valid=%t: %v", test.valid, err)
				}
			}
			result, err := validateStructuredResultPayload(compiled, ref, []byte(payload))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.validateAcceptedEvidenceInputs(child.ID, result); (err == nil) != test.valid {
				t.Fatalf("stored result valid=%t: %v", test.valid, err)
			}
		})
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.verifiedWorksetPathSelection(cancelled, child.ID, compiled.worksetPaths)
	if localResultRejectionReason(err) != RepairFailureSource || isResultReferenceChoiceError(err) {
		t.Fatalf("cancelled source verification must fail as a source error: %v", err)
	}
}

func TestResultWorksetPathsRequireUnambiguousAssignedSourceSpans(t *testing.T) {
	for _, test := range []struct {
		name, inventory string
		valid           bool
	}{
		{"empty_file", `{"sources":[{"path":"a","available_spans":[]}]}`, true},
		{"missing_path", `{"sources":[{"path":"b","available_spans":[]}]}`, false},
		{"missing_spans", `{"sources":[{"path":"a"}]}`, false},
		{"duplicate_path", `{"sources":[{"path":"a","available_spans":[]},{"path":"a","available_spans":[]}]}`, false},
		{"reversed_span", `{"sources":[{"path":"a","available_spans":[{"start_line":3,"end_line":2}]}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			spans, err := decodeWorksetLineSpans([]byte(test.inventory), []string{"a"})
			if (err == nil) != test.valid {
				t.Fatalf("inventory valid=%t: %v", test.valid, err)
			}
			if test.valid {
				paths := worksetPathSelection{paths: map[string]string{"key": "a"}, lineSpans: spans}
				if _, err := paths.resolve("key:1"); !isResultReferenceChoiceError(err) {
					t.Fatalf("empty source unexpectedly allowed a citation: %v", err)
				}
			}
		})
	}
}

func TestAcceptedWorksetPathRepairPreservesLineAndClaims(t *testing.T) {
	original := &TaskResult{Status: TaskResultStatusSuccess, Summary: "unchanged", Confidence: 1, Facts: map[string]any{"large": jsontext.Value(`9007199254740993`)}, StructuredPayload: &ResultPayload{Value: jsontext.Value(`{"findings":[{"location":"dir/soruce.go:7","summary":"unchanged","severity":"error"}]}`)}}
	pointers := []string{"/findings/0/location"}
	want, err := acceptedResultSemanticHashWithPaths(original, pointers, pointers)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, location, summary string
		same                    bool
	}{
		{"path_only", "dir/source.go:7", "unchanged", true},
		{"line", "dir/source.go:8", "unchanged", false},
		{"summary", "dir/source.go:7", "changed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := cloneTaskResult(original)
			body, _ := json.Marshal(map[string]any{"findings": []any{map[string]any{"location": tc.location, "summary": tc.summary, "severity": "error"}}})
			next.StructuredPayload.Value = body
			got, err := acceptedResultSemanticHashWithPaths(next, pointers, pointers)
			if err != nil || (got == want) != tc.same {
				t.Fatalf("semantic freeze same=%t: %v", tc.same, err)
			}
		})
	}
	for _, reference := range []string{"path_issued", "dir/source.go", "path_issued:8", "dir/foreign.go"} {
		value, err := decodeUniqueJSON(original.StructuredPayload.Value)
		if err != nil {
			t.Fatal(err)
		}
		err = replaceAcceptedWorksetPath(value, AcceptedReferenceSelection{Pointer: pointers[0], Reference: reference}, worksetPathSelection{paths: map[string]string{"path_issued": "dir/source.go"}})
		valid := reference == "path_issued" || reference == "dir/source.go"
		if (err == nil) != valid {
			t.Fatalf("selection %q: %v", reference, err)
		}
		if valid {
			text, _ := resolveJSONPointer(value, pointers[0])
			if text != "dir/source.go:7" {
				t.Fatal("selection changed the original line")
			}
		}
	}
}
