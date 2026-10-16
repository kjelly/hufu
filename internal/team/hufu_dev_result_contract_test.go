package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// Copy the real team, keeping schema loading and any runtime state inside the
// test directory. No global credentials or existing workspace are required.
func loadHufuDevQualityTeam(t *testing.T) *TeamSession {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(bundledTeamsDir(t), "hufu-dev"))); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "team.yaml")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "workspace: workspace", "workspace: "+filepath.Join(dir, "state"), 1))
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func hufuDevPayload(role string) []byte {
	switch role {
	case "implementation-engineer":
		return []byte(`{"changed_files":[],"tests_added":[],"checks":[],"limitations":[],"gaps":[]}`)
	case "verifier":
		return []byte(`{"checks":[],"failures":[],"gaps":[]}`)
	default:
		return []byte(`{"scope":"Assigned boundary","observations":[],"risks":[],"recommendations":[],"gaps":[]}`)
	}
}

func TestHufuDevResultContractsPreserveWorkerBoundaries(t *testing.T) {
	session := loadHufuDevQualityTeam(t)
	roles := []struct{ name, schema, tools, effect, recovery string }{
		{"runtime-engineer", "analysis", "view,grep,glob,ls,bash", "none", "retry"},
		{"integration-engineer", "analysis", "view,grep,glob,ls,bash", "none", "retry"},
		{"implementation-engineer", "implementation", "view,write,edit,multiedit,bash,grep,glob,ls,lua", "workspace_write", "manual"},
		{"verifier", "verification", "view,bash,grep,glob,ls", "none", "retry"},
	}
	for _, role := range roles {
		t.Run(role.name, func(t *testing.T) {
			def := session.Agents[role.name]
			// Loading adds the existing wait_for helper to each worker; pin the
			// effective baseline rather than only the authoring frontmatter.
			if def == nil || def.Tools != role.tools+",wait_for" || def.SideEffect != role.effect || def.Recovery != role.recovery {
				t.Fatalf("worker boundary changed for %s", role.name)
			}
			ref := session.AgentResultContracts[role.name]
			if !ref.RequireStructured || ref.ID != "schemas/"+role.schema+"-v1.json" {
				t.Fatalf("unexpected contract: %#v", ref)
			}
			compiled := session.ResultContracts[ref.ID]
			if compiled == nil || len(compiled.CanonicalSchema) > 8<<10 {
				t.Fatal("missing or oversized worker schema")
			}
			payload, err := validateStructuredResultPayload(compiled, ref, hufuDevPayload(role.name))
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(payload.Value)
			if payload.SHA256 != hex.EncodeToString(digest[:]) || payload.Contract != ref {
				t.Fatal("payload was not sealed to the admitted contract")
			}
			if compiled.evidenceInputs != nil || compiled.evidenceReferences != nil {
				t.Fatal("simple structured results unexpectedly require evidence_from")
			}
		})
	}
	policy := session.Config.Delegation
	names := []string{"runtime-engineer", "integration-engineer", "implementation-engineer", "verifier"}
	if !slices.Equal(policy.AllowedWorkers, names) || !slices.Equal(policy.NoRedispatchAfterSuccess, names) ||
		!slices.Equal(policy.RequireTaskContract, []string{"implementation-engineer"}) ||
		!slices.Equal(policy.ShareRequestWith, []string{"implementation-engineer", "verifier"}) {
		t.Fatalf("delegation boundary changed: %#v", policy)
	}
	acceptance := session.Config.AcceptanceSpec
	if acceptance == nil || acceptance.Mode != "blocking" || !acceptance.RequireNoUnresolvedTasks ||
		!slices.Equal(acceptance.Commands, []string{"go test ./...", "go vet ./...", "golangci-lint run"}) {
		t.Fatalf("blocking acceptance changed: %#v", acceptance)
	}
}

func TestHufuDevCheckSchemaRejectsContradictoryObservations(t *testing.T) {
	session := loadHufuDevQualityTeam(t)
	for _, role := range []string{"implementation-engineer", "verifier"} {
		ref := session.AgentResultContracts[role]
		compiled := session.ResultContracts[ref.ID]
		for _, tc := range []struct {
			name, outcome, note string
			exit                any
			valid               bool
		}{
			{"pass", "pass", "", 0, true},
			{"fail", "fail", "Observed test failure", 1, true},
			{"not run", "unverified", "Process could not start", nil, true},
			{"infra exit", "unverified", "Missing required executable", 127, true},
			{"pass nonzero", "pass", "", 1, false},
			{"pass null", "pass", "", nil, false},
			{"fail zero", "fail", "", 0, false},
			{"fail null", "fail", "", nil, false},
			{"unknown without reason", "unverified", " \t", nil, false},
			{"unknown enum", "unknown", "No conclusion", nil, false},
			{"negative exit", "fail", "Killed", -1, false},
		} {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				var value map[string]any
				if err := json.Unmarshal(hufuDevPayload(role), &value); err != nil {
					t.Fatal(err)
				}
				value["checks"] = []any{map[string]any{"command": "go test ./...", "outcome": tc.outcome, "exit_code": tc.exit, "note": tc.note}}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				_, err = validateStructuredResultPayload(compiled, ref, raw)
				if (err == nil) != tc.valid {
					t.Fatalf("validation = %v, want valid %v", err, tc.valid)
				}
			})
		}
	}
}

func TestHufuDevStructuredSubmissionKeepsIncompleteStatuses(t *testing.T) {
	session := loadHufuDevQualityTeam(t)
	for _, role := range []string{"runtime-engineer", "integration-engineer", "implementation-engineer", "verifier"} {
		for _, status := range []string{TaskResultStatusSuccess, TaskResultStatusFailed, TaskResultStatusPartial, TaskResultStatusBlocked} {
			t.Run(role+"/"+status, func(t *testing.T) {
				ref := session.AgentResultContracts[role]
				c := &Coordinator{session: session, taskTracker: NewTaskTracker(), executionRunID: "run-dev-contract"}
				item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: role, Desc: "bounded fixture", ResultContract: &ref}})[0]
				ctx := occurrenceTestContext(c, item.ID, 1)
				tool := &submitResultTool{coordinator: c, todoID: item.ID}
				// Even a negative submission must carry a valid structured payload.
				input, err := json.Marshal(map[string]any{"status": status, "summary": "Bounded fixture"})
				if err != nil {
					t.Fatal(err)
				}
				response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(input)})
				if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
					t.Fatalf("missing payload was accepted: %#v, %v", response, err)
				}
				var value map[string]any
				if err := json.Unmarshal(hufuDevPayload(role), &value); err != nil {
					t.Fatal(err)
				}
				input, err = json.Marshal(map[string]any{"status": status, "summary": "Bounded fixture", "structured_payload": value})
				if err != nil {
					t.Fatal(err)
				}
				response, err = tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(input)})
				if err != nil || response.IsError {
					t.Fatalf("valid %s rejected: %#v, %v", status, response, err)
				}
				result := c.GetTaskResult(item.ID)
				if result == nil || result.Status != status || result.StructuredPayload == nil {
					t.Fatalf("canonical status or payload lost: %#v", result)
				}
			})
		}
	}
}

func TestHufuDevSchemasRejectMissingUnknownAndUnboundedFields(t *testing.T) {
	session := loadHufuDevQualityTeam(t)
	for _, role := range []string{"runtime-engineer", "implementation-engineer", "verifier"} {
		ref := session.AgentResultContracts[role]
		for _, mutate := range []func(map[string]any){
			func(value map[string]any) { delete(value, "gaps") },
			func(value map[string]any) { value["extra"] = true },
			func(value map[string]any) { value["gaps"] = "not an array" },
			func(value map[string]any) { value["gaps"] = []string{strings.Repeat("a", 1001)} },
			func(value map[string]any) { value["gaps"] = []string{" \n"} },
			func(value map[string]any) { value["gaps"] = slices.Repeat([]string{"gap"}, 51) },
		} {
			var value map[string]any
			if err := json.Unmarshal(hufuDevPayload(role), &value); err != nil {
				t.Fatal(err)
			}
			mutate(value)
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := validateStructuredResultPayload(session.ResultContracts[ref.ID], ref, raw); err == nil {
				t.Fatalf("%s accepted invalid payload: %s", role, raw)
			}
		}
	}
}

func TestHufuDevWorkerClaimsCannotSatisfyAcceptance(t *testing.T) {
	session := loadHufuDevQualityTeam(t)
	ref := session.AgentResultContracts["verifier"]
	// The model claims a passing check; the independent acceptance actually
	// executes a failing command. A valid payload must not stand in for it.
	payload, err := validateStructuredResultPayload(session.ResultContracts[ref.ID], ref,
		[]byte(`{"checks":[{"command":"go test ./...","outcome":"pass","exit_code":0,"note":""}],"failures":[],"gaps":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	c := newBudgetCoordinator(t)
	c.projectDir = t.TempDir()
	c.session = session
	if err := c.SetAcceptanceSpec(AcceptanceSpec{Mode: "blocking", Commands: []string{"exit 7"}}); err != nil {
		t.Fatal(err)
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "verifier", Desc: "claimed pass", ResultContract: &ref}})[0]
	if err := c.taskTracker.TodoList().SetTypedResult(item.ID, &TaskResult{
		Status: TaskResultStatusSuccess, Source: "submitted", StructuredPayload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "claimed pass")
	acceptance, err := c.runAcceptance(t.Context())
	if err == nil || acceptance == nil || acceptance.IsPassed() {
		t.Fatalf("worker claim bypassed acceptance: %#v, %v", acceptance, err)
	}
}
