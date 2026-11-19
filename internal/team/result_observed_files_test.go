package team

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func observedFilesTestSchema() string {
	return strings.Replace(artifactKeysTestSchema, `"pointers":`, `"runtime_owned_files_read":true,"pointers":`, 1)
}

func TestRuntimeOwnedFilesReadAdmission(t *testing.T) {
	for _, scenario := range []string{"full_id", "short_key", "empty", "unread", "other_model", "corrupt", "override", "null_override", "empty_override", "typo_full_id", "typo_key", "typo_full_id_receipt", "typo_key_receipt"} {
		t.Run(scenario, func(t *testing.T) {
			f := artifactKeysFixtureForSchema(t, observedFilesTestSchema())
			if scenario != "unread" && scenario != "empty" {
				f.readSource(t)
			}
			evidence := f.ref.ID
			if scenario == "short_key" {
				evidence = f.key
			}
			if strings.HasPrefix(scenario, "typo_full_id") {
				last := "0"
				if strings.HasSuffix(evidence, last) {
					last = "1"
				}
				evidence = evidence[:len(evidence)-1] + last
			} else if strings.HasPrefix(scenario, "typo_key") {
				evidence = "ev_000000000000"
			}
			payload := fmt.Sprintf(`{"claims":[{"evidence":[%q]}]}`, evidence)
			if scenario == "empty" {
				payload = `{"claims":[]}`
			}
			fields := map[string]any{"status": "success", "summary": "reviewed", "structured_payload": jsontext.Value(payload)}
			if strings.HasSuffix(scenario, "_receipt") {
				fields["receipt_ids"] = []string{"forged"}
			}
			switch scenario {
			case "other_model":
				f.ctx = withInvocationMetadata(f.ctx, InvocationMetadata{RunID: "run-key", TaskID: f.item.ID, AgentName: f.item.Agent, Attempt: 1, ModelExecutionID: "different-model"})
			case "corrupt":
				if err := os.WriteFile(filepath.Join(f.c.session.Workspace, logsDir, "artifacts", "data", f.ref.ID), []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "override":
				fields["files_read"] = []any{map[string]any{"path": f.ref.ID}}
			case "null_override":
				fields["files_read"] = nil
			case "empty_override":
				fields["files_read"] = []any{}
			}
			tool := &submitResultTool{coordinator: f.c, todoID: f.item.ID}
			if _, exposed := tool.Info().Parameters["files_read"]; exposed {
				t.Fatal("runtime-owned field exposed to the model")
			}
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			response, err := tool.Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(raw)})
			valid := scenario == "full_id" || scenario == "short_key" || scenario == "empty"
			if err != nil || response.IsError == valid {
				t.Fatalf("valid=%t response=%+v error=%v", valid, response, err)
			}
			result := f.c.GetTaskResult(f.item.ID)
			if !valid {
				if result != nil {
					t.Fatal("rejected evidence published")
				}
				if strings.HasSuffix(scenario, "_receipt") {
					if submissionFailureMetadata(response.Metadata) != RepairFailureSource {
						t.Fatal("citation typo concealed a forged receipt")
					}
					return
				}
				if strings.HasPrefix(scenario, "typo_") {
					if reason, replay := classifyRepairFailure(failureResponseSteps(response), nil); reason != RepairFailureInvalidSchema || replay {
						t.Fatalf("citation typo prevented result-only protocol repair: reason=%s work_replay=%t", reason, replay)
					}
					if !strings.Contains(response.Content, f.key) {
						t.Fatal("invalid citation did not receive exact issued choices")
					}
					if !strings.Contains(response.Content, "/structured_payload/claims/0/evidence/0") {
						t.Fatalf("invalid citation lacks an exact field path: %s", response.Content)
					}
					fields["structured_payload"] = jsontext.Value(fmt.Sprintf(`{"claims":[{"evidence":[%q]}]}`, f.key))
					repaired, err := json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					response, err = tool.Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(repaired)})
					accepted := f.c.GetTaskResult(f.item.ID)
					if err != nil || response.IsError || accepted == nil || len(accepted.FilesRead) != 1 || accepted.FilesRead[0].Path != f.ref.ID {
						t.Fatalf("same-work citation correction failed: %+v %v", response, err)
					}
				}
				return
			}
			expected := []FileRef{}
			if scenario != "empty" {
				expected = append(expected, FileRef{Path: f.ref.ID})
			}
			if !slices.Equal(result.FilesRead, expected) {
				t.Fatalf("read projection=%+v", result.FilesRead)
			}
			if err := f.c.CommitTaskTransition(t.Context(), f.item.ID, TaskPending, TaskInProgress, "work returned", "", nil); err != nil {
				t.Fatal(err)
			}
			if err := f.c.CommitTaskTransition(t.Context(), f.item.ID, TaskInProgress, TaskDone, "accepted", "", nil); err != nil {
				t.Fatal(err)
			}
			events, err := f.c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			restored := todoItemByID(ReduceToTodoList(events), f.item.ID)
			if restored.TypedResult == nil || !slices.Equal(restored.TypedResult.FilesRead, result.FilesRead) {
				t.Fatal("replay lost canonical read evidence")
			}
		})
	}
}

func TestRuntimeOwnedFilesReadRejectsExternalProposal(t *testing.T) {
	compiled, err := CompileResultContractDocument("reads.json", []byte(observedFilesTestSchema()))
	if err != nil {
		t.Fatal(err)
	}
	for _, reads := range [][]string{nil, {}, {"claimed-read"}} {
		proposal := &WorkerResultProposal{Status: "success", Summary: "reviewed", FilesRead: reads, StructuredPayloadJSON: new(`{"claims":[]}`)}
		request := AttemptRequest{Task: TaskDef{ResultContract: new(compiled.ref(true))}, resultContractSchema: compiled}
		if _, err := canonicalExternalStructuredPayload(request, proposal, nil); err == nil {
			t.Fatal("external proposal fabricated native read authority")
		}
	}
}

func TestRuntimeOwnedFilesReadPreflightsNativeCapability(t *testing.T) {
	compiled, err := CompileResultContractDocument("reads.json", []byte(observedFilesTestSchema()))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"native", "external", "no_view", "denied", "mcp"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestCoordinator(t)
			def := c.session.Agents["worker"]
			def.Tools = "view"
			switch mode {
			case "external":
				def.SubagentProvider = "codex"
			case "no_view":
				def.Tools = ""
			case "denied":
				c.session.Config.ToolsDenied = []string{"view"}
			case "mcp":
				def.ForceMCP = true
			}
			if err := validateResultDecisionCatalog(c.session, def, compiled); (err == nil) != (mode == "native") {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
		})
	}
}

func TestRuntimeOwnedFilesReadSelectionSchema(t *testing.T) {
	for _, shape := range []string{"array", "fixed_rows", "no_reads"} {
		t.Run(shape, func(t *testing.T) {
			f := artifactKeysFixtureForSchema(t, observedFilesTestSchema())
			if shape != "no_reads" {
				f.readSource(t)
			}
			compiled := f.c.session.ResultContracts["keys.json"]
			refs, err := f.c.verifiedResultEvidenceKeyRefs(f.ctx, f.item.ID, compiled)
			if err != nil {
				t.Fatal(err)
			}
			info, err := collectionGuidanceInfo(f.c, f.item.ID)
			if err != nil {
				t.Fatal(err)
			}
			schema := info.Parameters["structured_payload"].(map[string]any)
			if shape == "fixed_rows" {
				properties := schema["properties"].(map[string]any)
				row := properties["claims"].(map[string]any)["items"]
				properties["claims"] = map[string]any{"type": "object", "properties": map[string]any{"issued_row": row}, "required": []string{"issued_row"}, "additionalProperties": false}
			}
			constrainResultEvidenceChoices(schema, compiled.evidenceKeys, refs)
			for _, value := range []string{f.key, f.ref.ID, "ev_000000000000"} {
				claims := any([]any{map[string]any{"evidence": []string{value}}})
				if shape == "fixed_rows" {
					claims = map[string]any{"issued_row": map[string]any{"evidence": []string{value}}}
				}
				raw, err := json.Marshal(map[string]any{"status": "success", "summary": "unchanged judgment", "structured_payload": map[string]any{"claims": claims}})
				if err != nil {
					t.Fatal(err)
				}
				valid := shape != "no_reads" && (value == f.key || value == f.ref.ID)
				// The collector's complete schema/feedback validator supports
				// impossible empty-source selections, including JSON Schema not.
				issues, _, err := resultSubmissionIssues(string(raw), info)
				if err != nil || (len(issues) == 0) != valid {
					t.Fatalf("shape=%s choice=%s valid=%t issues=%v error=%v", shape, value, valid, issues, err)
				}
			}
			if shape != "fixed_rows" {
				if issues, _, err := resultSubmissionIssues(`{"status":"success","summary":"no findings","structured_payload":{"claims":[]}}`, info); err != nil || len(issues) != 0 {
					t.Fatalf("empty sources cannot report an empty review: %v %v", issues, err)
				}
			}
		})
	}
}
