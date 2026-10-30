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
	for _, scenario := range []string{"full_id", "short_key", "empty", "unread", "other_model", "corrupt", "override", "null_override", "empty_override"} {
		t.Run(scenario, func(t *testing.T) {
			f := artifactKeysFixtureForSchema(t, observedFilesTestSchema())
			if scenario != "unread" && scenario != "empty" {
				f.readSource(t)
			}
			evidence := f.ref.ID
			if scenario == "short_key" {
				evidence = f.key
			}
			payload := fmt.Sprintf(`{"claims":[{"evidence":[%q]}]}`, evidence)
			if scenario == "empty" {
				payload = `{"claims":[]}`
			}
			fields := map[string]any{"status": "success", "summary": "reviewed", "structured_payload": jsontext.Value(payload)}
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
