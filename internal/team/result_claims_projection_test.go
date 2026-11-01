package team

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

const projectionTestSchema = `{"type":"object","required":["findings","assessments"],"properties":{"findings":{"type":"array","maxItems":32,"items":{"type":"object","properties":{"summary":{"type":"string"},"severity":{"type":"string","enum":["error","warning","info"]}},"required":["summary","severity"],"additionalProperties":false}},"assessments":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"invariant_id":{"type":"string"},"status":{"type":"string","enum":["preserved","violated","unknown"]},"summary":{"type":"string"},"finding_index":{"type":"integer"}},"required":["invariant_id","status","summary"],"additionalProperties":false}}},"additionalProperties":false,"x-hufu-result-projections":{"findings":"/findings","invariant_assessments":"/assessments","bind_invariant_severity":true}}`

func TestResultClaimsProjectionBindsInvariantSeverity(t *testing.T) {
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"single semantic source", `{"findings":[{"summary":"boundary defect"}],"assessments":[{"invariant_id":"safe","status":"violated","summary":"observed violation","finding_index":0}]}`, true},
		{"override", `{"findings":[{"summary":"boundary defect","severity":"error"}],"assessments":[{"invariant_id":"safe","status":"violated","summary":"observed violation","finding_index":0}]}`, false},
		{"foreign invariant", `{"findings":[{"summary":"boundary defect"}],"assessments":[{"invariant_id":"foreign","status":"violated","summary":"observed violation","finding_index":0}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, item, manifest := invariantAttestationFixture(t, InvariantVerificationReport, InvariantDefinition{ID: "safe", Statement: "Preserve boundary", Severity: InvariantSeverityError})
			compiled, err := CompileResultContractDocument("projected.json", []byte(projectionTestSchema))
			if err != nil {
				t.Fatal(err)
			}
			ref := compiled.ref(true)
			item.ResultContract = &ref
			c.session.ResultContracts = map[string]*CompiledResultContract{ref.ID: compiled}
			c.taskTracker.TodoList().Restore([]*TodoItem{item})
			item = c.todoItemByID(item.ID)
			// The original receipt is an identity reference; source admission still
			// verifies the immutable context definition and its hash.
			c.taskTracker.TodoList().SetExecutionReceipt(item.ID, &ExecutionReceipt{Attempt: 1, ModelExecutionID: manifest.ModelExecutionID})
			input, err := c.projectResultClaims(t.Context(), item.ID, SubmitResultInput{StructuredPayload: []byte(test.body)})
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
			if err != nil {
				return
			}
			if len(input.Findings) != 1 || input.Findings[0].Severity != "error" {
				t.Fatalf("findings=%+v", input.Findings)
			}
			payload, err := validateStructuredResultPayload(compiled, ref, input.StructuredPayload)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(payload.Value), `"severity":"error"`) {
				t.Fatal("canonical payload lost runtime severity")
			}
			info := submitResultToolInfo(taskResultSubmissionContract{claimsProjection: compiled.claimsProjection, InvariantVerification: InvariantVerificationReport})
			for _, name := range []string{"findings", "invariant_assessments"} {
				if _, ok := info.Parameters[name]; ok {
					t.Fatalf("advertised duplicated %s", name)
				}
			}
			value := map[string]any{}
			if err = json.Unmarshal(input.StructuredPayload, &value); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResultClaimsProjectionRejectsEnvelopeCopies(t *testing.T) {
	spec := &resultClaimsProjection{Findings: "/findings"}
	for _, findings := range [][]Finding{{}, {{Summary: "copied"}}} {
		if _, err := projectClaimsInput(SubmitResultInput{Findings: findings, StructuredPayload: []byte(`{"findings":[]}`)}, spec); err == nil {
			t.Fatal("accepted model envelope copy")
		}
	}
}

func TestResultClaimsProjectionExternalAndLocalUseOnePayload(t *testing.T) {
	schema := strings.ReplaceAll(projectionTestSchema, `,"bind_invariant_severity":true`, ``)
	compiled, err := CompileResultContractDocument("claims.json", []byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	raw := `{"findings":[{"summary":"` + strings.Repeat("long ", 900) + `","severity":"warning"}],"assessments":[]}`
	local, err := projectClaimsInput(SubmitResultInput{StructuredPayload: []byte(raw)}, compiled.claimsProjection)
	if err != nil {
		t.Fatal(err)
	}
	request := AttemptRequest{Task: TaskDef{ResultContract: &ref}, resultContractSchema: compiled}
	external, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: &raw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := validateStructuredResultPayload(compiled, ref, local.StructuredPayload)
	if err != nil {
		t.Fatal(err)
	}
	if external.SHA256 != canonical.SHA256 || string(external.Value) != string(canonical.Value) {
		t.Fatal("transports changed canonical payload")
	}
	request = scopedAttemptRequest("codex", "task")
	request.Task = TaskDef{Agent: "worker", ResultContract: &ref}
	request.resultContractSchema = compiled
	full, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), request, AttemptResult{ResultProposal: &WorkerResultProposal{Status: "success", Summary: "complete", StructuredPayloadJSON: &raw}}, WorkspaceDelta{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Findings) != 1 || full.Findings[0].Summary != local.Findings[0].Summary {
		t.Fatal("external canonicalization truncated projected semantics")
	}
	for _, override := range []string{`"findings":null,`, `"findings":[],`, `"invariant_assessments":null,`, `"invariant_assessments":[],`} {
		body := `{"status":"success","summary":"done",` + override + `"structured_payload_json":` + string(mustProjectionJSON(t, raw)) + `}`
		proposal, err := DecodeWorkerResultProposal([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = canonicalExternalStructuredPayload(request, proposal, nil); err == nil {
			t.Fatal("external accepted runtime-owned field", override)
		}
	}
	for _, info := range []map[string]any{codexWorkerResultProposalSchemaForProjection(compiled.claimsProjection)} {
		properties := info["properties"].(map[string]any)
		if properties["findings"] != nil || properties["invariant_assessments"] != nil {
			t.Fatal("external schema advertised copies")
		}
	}
}

func mustProjectionJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value, collectionDurationFormat)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestResultClaimsProjectionSchemaRejectsUnboundedOrMissingSources(t *testing.T) {
	for _, schema := range []string{
		strings.Replace(projectionTestSchema, `"maxItems":32,`, "", 1),
		strings.Replace(projectionTestSchema, `"items":{"type":"object"`, `"items":{"type":"integer"`, 1),
		strings.Replace(projectionTestSchema, `"maxItems":64`, `"maxItems":129`, 1),
		strings.Replace(projectionTestSchema, `"findings":"/findings"`, `"findings":"/missing"`, 1),
		strings.Replace(projectionTestSchema, `"invariant_assessments":"/assessments"`, `"invariant_assessments":"/findings"`, 1),
	} {
		if _, err := CompileResultContractDocument("invalid.json", []byte(schema)); err == nil {
			t.Fatal("invalid projection schema accepted")
		}
	}
}

func TestResultClaimsProjectionRevalidationRejectsDivergentEnvelope(t *testing.T) {
	compiled, err := CompileResultContractDocument("projection.json", []byte(projectionTestSchema))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"same", "finding_copy", "claim_copy", "missing_attestation"} {
		t.Run(name, func(t *testing.T) {
			result := &TaskResult{Findings: []Finding{{Summary: "observed", Severity: "info"}}, StructuredPayload: &ResultPayload{Value: []byte(`{"findings":[{"summary":"observed","severity":"info"}],"assessments":[{"invariant_id":"safe","status":"preserved","summary":"verified"}]}`)}, InvariantVerification: &InvariantVerificationResult{Assessments: []InvariantAssessment{{InvariantID: "safe", Status: InvariantPreserved, Summary: "verified"}}}}
			switch name {
			case "finding_copy":
				result.Findings[0].Summary = "different"
			case "claim_copy":
				result.InvariantVerification.Assessments[0].Summary = "different"
			case "missing_attestation":
				result.InvariantVerification = nil
			}
			if resultProjectionMatchesAccepted(result, compiled) != (name == "same") {
				t.Fatal("revalidation did not enforce the new envelope projection")
			}
		})
	}
}

func TestResultClaimsProjectionRequiresInvariantModeOnStaticTasks(t *testing.T) {
	compiled, err := CompileResultContractDocument("projection.json", []byte(projectionTestSchema))
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	for _, mode := range []InvariantVerificationMode{"", InvariantVerificationReport, InvariantVerificationGate} {
		for _, inherit := range []bool{false, true} {
			session := &TeamSession{ContractTasks: []TaskDef{{ID: "review", Agent: "worker", InvariantVerification: mode, ResultContract: &ref}}}
			defaults := map[string]ResultContractRef{}
			if inherit {
				session.ContractTasks[0].ResultContract = nil
				defaults["worker"] = ref
			}
			err := validateResultProjectionModes(session, map[string]*CompiledResultContract{compiled.ID: compiled}, defaults)
			if (err == nil) != (mode != "") {
				t.Fatalf("mode=%s inherit=%v err=%v", mode, inherit, err)
			}
		}
	}
}

func TestResultClaimsProjectionRejectsNativeRewritingOverlap(t *testing.T) {
	root, err := decodeResultContractJSON([]byte(evidenceTestSchema))
	if err != nil {
		t.Fatal(err)
	}
	document := root.(map[string]any)
	document["x-hufu-result-projections"] = map[string]any{"findings": "/findings"}
	document["properties"].(map[string]any)["findings"].(map[string]any)["maxItems"] = 32
	_, err = CompileResultContractDocument("overlap.json", mustProjectionJSON(t, document))
	if err == nil || !strings.Contains(err.Error(), "tool-evidence rewriting") {
		t.Fatalf("native rewrite can change projected claims: %v", err)
	}
}
