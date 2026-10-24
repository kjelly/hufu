package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func prepareHufuDevIntegrationSession(t *testing.T, session *TeamSession, project, endpoint string, artifacts bool, retries int) {
	t.Helper()
	if err := session.SetCompatibilityWorkspaceScope(project); err != nil {
		t.Fatal(err)
	}
	session.Config.Generation.Model = "ollama/test"
	session.Config.Generation.ContextWindow = 32768
	session.Config.ProviderURL = endpoint
	session.Config.Timeout = 30
	session.Config.MaxRetries = retries
	session.Config.Acceptance = "test \"$(cat implementation.txt)\" = x"
	session.Config.AcceptanceSpec = &agent.AcceptanceSpec{Mode: "blocking", RequireNoUnresolvedTasks: true, Commands: []string{session.Config.Acceptance}}
	session.Config.ContextArtifacts = agent.DefaultContextArtifactsPolicy()
	session.Config.ContextArtifacts.Enabled = artifacts
	for _, def := range session.Agents {
		def.Generation.Model = "ollama/test"
		def.Generation.ContextWindow = 32768
		def.MaxRetries = retries
	}
}

func openHufuDevIntegrationCoordinator(t *testing.T, session *TeamSession, checkpoint *SessionData) *Coordinator {
	t.Helper()
	c, err := NewCoordinator(session, session.Config.ProviderURL, "", nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	if checkpoint == nil {
		checkpoint = NewSession()
	}
	c.SetSessionData(checkpoint)
	c.executionRunID = "run-dev-integration"
	c.initialPrompt = "Only write implementation.txt once; independently verify its content."
	c.initEventStore()
	return c
}

func newHufuDevIntegrationCoordinator(t *testing.T, artifacts bool) (*Coordinator, *atomic.Int32) {
	return newHufuDevIntegrationCoordinatorWithRetries(t, artifacts, 0)
}

func newHufuDevIntegrationCoordinatorWithRetries(t *testing.T, artifacts bool, retries int) (*Coordinator, *atomic.Int32) {
	t.Helper()
	session := loadHufuDevQualityTeam(t)
	requests := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/api/chat" || r.URL.Path == "/api/generate" || r.URL.Path == "/v1/responses" {
			requests.Add(1)
		}
		http.Error(w, "unexpected provider access", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	prepareHufuDevIntegrationSession(t, session, t.TempDir(), server.URL+"/v1", artifacts, retries)
	c := openHufuDevIntegrationCoordinator(t, session, nil)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	return c, requests
}

func hufuDevFixtureTool(ctx context.Context, call fantasy.AgentStreamCall, id string, tool fantasy.AgentTool, input string) (fantasy.ToolResponse, error) {
	name := tool.Info().Name
	if call.OnToolCall != nil {
		if err := call.OnToolCall(fantasy.ToolCallContent{ToolCallID: id, ToolName: name, Input: input}); err != nil {
			return fantasy.ToolResponse{}, err
		}
	}
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: id, Name: name, Input: input})
	if err != nil {
		return response, err
	}
	if call.OnToolResult != nil {
		var output fantasy.ToolResultOutputContent
		if response.IsError {
			output = fantasy.ToolResultOutputContentError{Error: errors.New(response.Content)}
		} else {
			output = fantasy.ToolResultOutputContentText{Text: response.Content}
		}
		if err := call.OnToolResult(fantasy.ToolResultContent{ToolCallID: id, ToolName: name, Result: output}); err != nil {
			return response, err
		}
	}
	return response, nil
}

func TestHufuDevWorkflowStructuredHandoffAndArtifacts(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	for _, tc := range []struct {
		name      string
		artifacts bool
		lines     int
		status    string
	}{
		{"off", false, 20000, TaskResultStatusSuccess},
		{"on", true, 20000, TaskResultStatusSuccess},
		{"oversized", true, 300000, TaskResultStatusSuccess},
		{"confirmed failure", false, 0, TaskResultStatusFailed},
		{"partial", false, 0, TaskResultStatusPartial},
		{"blocked", false, 0, TaskResultStatusBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, requests := newHufuDevIntegrationCoordinator(t, tc.artifacts)
			calls := map[string]int{}
			prompts := map[string]string{}
			var offloaded bool
			c.workerAgentOverride = &mockAgent{streamFunc: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				todoID, _ := ctx.Value(todoIDKey{}).(string)
				item := c.todoItemByID(todoID)
				role := item.Agent
				calls[role]++
				prompts[role] = call.Prompt
				var value map[string]any
				if err := json.Unmarshal(hufuDevPayload(role), &value); err != nil {
					return nil, err
				}
				status := TaskResultStatusSuccess
				bash := &policyGatedTool{inner: tools.NewBashTool(tools.WithWorkDir(c.projectDir), tools.WithAllowedPaths([]string{c.projectDir})), coordinator: c}
				if role == "implementation-engineer" {
					command := fmt.Sprintf("printf x >> implementation.txt; seq 1 %d", max(tc.lines, 1))
					input, _ := json.Marshal(map[string]any{"command": command})
					output, err := hufuDevFixtureTool(ctx, call, "bash-"+todoID, bash, string(input))
					if err != nil {
						return nil, err
					}
					if output.IsError {
						return nil, errors.New(output.Content)
					}
					offloaded = strings.Contains(output.Content, "[context artifact]")
					if offloaded {
						refs := contextArtifactRefPattern.FindStringSubmatch(output.Content)
						if len(refs) != 2 {
							return nil, errors.New("missing output reference")
						}
						full, _ := rebuildThroughView(t, c, ctx, refs[1])
						if !strings.Contains(full, "19999\n20000") {
							return nil, errors.New("offload lost output tail")
						}
					}
					value["changed_files"] = []string{"implementation.txt"}
				}
				if role == "verifier" {
					value["implementation_gap_assessments"] = map[string]any{}
					status = tc.status
					if status == TaskResultStatusSuccess || status == TaskResultStatusFailed {
						command := "grep -q '^x$' implementation.txt"
						if status == TaskResultStatusFailed {
							command = "grep -q '^absent$' implementation.txt"
						}
						input, _ := json.Marshal(map[string]any{"command": command})
						output, err := hufuDevFixtureTool(ctx, call, "bash-"+todoID, bash, string(input))
						if err != nil {
							return nil, err
						}
						exit, observed := transcriptExitCode("bash", output.Content)
						if !observed || output.IsError != (status == TaskResultStatusFailed) {
							return nil, fmt.Errorf("unexpected verifier output: %s", output.Content)
						}
						outcome := "pass"
						if exit != 0 {
							outcome = "fail"
						}
						value["checks"] = []any{map[string]any{"command": command, "outcome": outcome, "exit_code": exit, "note": "Observed fixture command"}}
						if status == TaskResultStatusFailed {
							value["failures"] = []string{"Confirmed fixture rejection"}
						}
					} else {
						value["gaps"] = []string{"Required verification unavailable in this fixture"}
					}
				}
				input, err := json.Marshal(map[string]any{"status": status, "summary": "Bounded fixture result", "structured_payload": value})
				if err != nil {
					return nil, err
				}
				output, err := hufuDevFixtureTool(ctx, call, "result-"+todoID, &submitResultTool{coordinator: c, todoID: todoID}, string(input))
				if err != nil {
					return nil, err
				}
				if output.IsError {
					return nil, errors.New(output.Content)
				}
				return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "submitted"}}}}, nil
			}}
			_, err := c.ExecuteTasks(withTestProtocolRepairInvocationContext(t.Context()), []TaskDef{
				{Agent: "runtime-engineer", Goal: "Inspect the synthetic requirement"},
				{Agent: "implementation-engineer", Goal: "Write one marker", DependsOn: []int{0}, Constraints: c.initialPrompt, Verify: "test \"$(cat implementation.txt)\" = x"},
				{Agent: "verifier", Goal: "Independently verify the marker", DependsOn: []int{0, 1}},
			})
			if tc.status == TaskResultStatusSuccess && err != nil {
				t.Fatal(err)
			}
			items := c.taskTracker.TodoList().Items()
			if len(items) != 3 {
				t.Fatalf("expected three admitted tasks: %d, %v", len(items), err)
			}
			for _, item := range items {
				if calls[item.Agent] != 1 || item.TypedResult == nil || item.TypedResult.StructuredPayload == nil || item.ResultContract == nil {
					t.Fatalf("missing bounded worker result for %s: calls=%d status=%s detail=%s", item.Agent, calls[item.Agent], item.Status, item.Detail)
				}
				if err := c.validateAcceptedDependencyPayload(item); err != nil {
					t.Fatal(err)
				}
			}
			if items[2].TypedResult.Status != tc.status {
				t.Fatal("canonical verifier status changed")
			}
			acceptance, acceptErr := c.runAcceptance(t.Context())
			if acceptance == nil || acceptance.IsPassed() != (tc.status == TaskResultStatusSuccess) {
				t.Fatalf("acceptance=%#v, %v", acceptance, acceptErr)
			}
			if offloaded != (tc.artifacts && tc.lines == 20000) {
				t.Fatalf("unexpected offload=%v", offloaded)
			}
			for _, role := range []string{"implementation-engineer", "verifier"} {
				if !strings.Contains(prompts[role], c.initialPrompt) || !strings.Contains(prompts[role], items[0].TypedResult.StructuredPayload.SHA256) {
					t.Fatalf("%s did not receive original request and accepted analysis", role)
				}
			}
			events, err := c.eventStore.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToTodoList(events)
			if len(replayed) != 3 {
				t.Fatalf("replayed %d tasks", len(replayed))
			}
			for i, item := range replayed {
				if item.TypedResult == nil || item.TypedResult.StructuredPayload == nil || item.TypedResult.StructuredPayload.SHA256 != items[i].TypedResult.StructuredPayload.SHA256 || item.Status != items[i].Status {
					t.Fatal("replay lost structured payload or terminal status")
				}
			}
			if requests.Load() != 0 {
				t.Fatal("fake workers unexpectedly requested a model generation")
			}
		})
	}
}

func TestHufuDevWorkflowRepairDoesNotReplayImplementation(t *testing.T) {
	c, requests := newHufuDevIntegrationCoordinatorWithRetries(t, false, 1)
	workerCalls, repairCalls := 0, 0
	var repairPrompts []string
	var repairErr error
	c.repairAgentOverride = &scriptedRepairAgent{
		calls: &repairCalls, prompts: &repairPrompts,
		onContext: func(ctx context.Context) {
			todoID, _ := ctx.Value(todoIDKey{}).(string)
			var payload map[string]any
			if err := json.Unmarshal(hufuDevPayload("implementation-engineer"), &payload); err != nil {
				repairErr = err
				return
			}
			payload["changed_files"] = []string{"implementation.txt"}
			input, err := json.Marshal(map[string]any{"status": "success", "summary": "Repaired result only", "structured_payload": payload})
			if err != nil {
				repairErr = err
				return
			}
			response, err := (&submitResultTool{coordinator: c, todoID: todoID}).Run(ctx, fantasy.ToolCall{ID: "repair-submit", Name: submitResultToolName, Input: string(input)})
			if err != nil {
				repairErr = err
			} else if response.IsError {
				repairErr = errors.New(response.Content)
			}
		},
	}
	c.workerAgentOverride = &mockAgent{streamFunc: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		id, _ := ctx.Value(todoIDKey{}).(string)
		item := c.todoItemByID(id)
		if item.Agent == "runtime-engineer" {
			var payload map[string]any
			if err := json.Unmarshal(hufuDevPayload(item.Agent), &payload); err != nil {
				return nil, err
			}
			input, err := json.Marshal(map[string]any{"status": "success", "summary": "Bounded analysis", "structured_payload": payload})
			if err != nil {
				return nil, err
			}
			response, err := hufuDevFixtureTool(ctx, call, "analysis-result", &submitResultTool{coordinator: c, todoID: id}, string(input))
			if err != nil {
				return nil, err
			}
			if response.IsError {
				return nil, errors.New(response.Content)
			}
			return &fantasy.AgentResult{}, nil
		}
		workerCalls++
		bash := &policyGatedTool{inner: tools.NewBashTool(tools.WithWorkDir(c.projectDir), tools.WithAllowedPaths([]string{c.projectDir})), coordinator: c}
		response, err := hufuDevFixtureTool(ctx, call, "write-once", bash, `{"command":"printf x >> implementation.txt"}`)
		if err != nil {
			return nil, err
		}
		if response.IsError {
			return nil, errors.New(response.Content)
		}
		// Real admission rejects the missing gaps field. The stream loop
		// detector must enter result-only repair rather than replay this write.
		input := `{"status":"success","summary":"Implementation done","structured_payload":{"changed_files":["implementation.txt"],"tests_added":[],"checks":[],"limitations":[]}}`
		for attempt := range maxRepeatedSubmitResultFailures {
			response, err = hufuDevFixtureTool(ctx, call, fmt.Sprintf("rejected-%d", attempt), &submitResultTool{coordinator: c, todoID: id}, input)
			if err != nil {
				return nil, err
			}
			if !response.IsError {
				return nil, errors.New("invalid payload accepted")
			}
		}
		return nil, errors.New("rejected submission loop was not stopped")
	}}
	_, err := c.ExecuteTasks(withTestProtocolRepairInvocationContext(t.Context()), []TaskDef{
		{Agent: "runtime-engineer", Goal: "Analyze the bounded fixture"},
		{Agent: "implementation-engineer", Goal: "Write one marker", DependsOn: []int{0}, Constraints: c.initialPrompt, Verify: "test \"$(cat implementation.txt)\" = x"},
	})
	if err != nil || repairErr != nil {
		t.Fatalf("execution=%v repair=%v", err, repairErr)
	}
	if workerCalls != 1 || repairCalls != 1 {
		t.Fatalf("worker=%d repair=%d", workerCalls, repairCalls)
	}
	marker, err := os.ReadFile(filepath.Join(c.projectDir, "implementation.txt"))
	if err != nil || string(marker) != "x" {
		t.Fatalf("implementation repeated: %q %v", marker, err)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 2 || items[1].Status != TaskDone || items[1].ExecutionReceipt == nil || items[1].ExecutionReceipt.RepairProvenance == nil || !items[1].ExecutionReceipt.RepairProvenance.Success {
		t.Fatal("repair receipt or completion missing")
	}
	if len(repairPrompts) != 1 || !strings.Contains(repairPrompts[0], "schemas/implementation-v1.json") || !strings.Contains(repairPrompts[0], items[0].TypedResult.StructuredPayload.SHA256) {
		t.Fatal("repair lost real schema or accepted source")
	}
	if requests.Load() != 0 {
		t.Fatal("fake repair requested a model generation")
	}
}

func TestHufuDevWorkflowSchemaDriftResumeAndRollback(t *testing.T) {
	first, requests := newHufuDevIntegrationCoordinator(t, false)
	first.workerAgentOverride = &mockAgent{streamFunc: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		id, _ := ctx.Value(todoIDKey{}).(string)
		var payload map[string]any
		if err := json.Unmarshal(hufuDevPayload("runtime-engineer"), &payload); err != nil {
			return nil, err
		}
		input, err := json.Marshal(map[string]any{"status": "success", "summary": "Completed analysis", "structured_payload": payload})
		if err != nil {
			return nil, err
		}
		response, err := (&submitResultTool{coordinator: first, todoID: id}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(input)})
		if err != nil {
			return nil, err
		}
		if response.IsError {
			return nil, errors.New(response.Content)
		}
		return &fantasy.AgentResult{}, nil
	}}
	if _, err := first.ExecuteTasks(withTestProtocolRepairInvocationContext(t.Context()), []TaskDef{{Agent: "runtime-engineer", Goal: "Analyze the fixture"}}); err != nil {
		t.Fatal(err)
	}
	original := first.taskTracker.TodoList().Items()[0]
	oldPayloadHash := original.TypedResult.StructuredPayload.SHA256
	oldContract := *original.ResultContract
	dir, workspace, project, endpoint := first.session.Dir, first.session.Workspace, first.projectDir, first.session.Config.ProviderURL
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	checkpoint := LoadSession(workspace)
	if checkpoint == nil || checkpoint.ExecutionPolicySnapshot == nil {
		t.Fatal("missing durable checkpoint")
	}
	if len(checkpoint.Tasks) != 1 || checkpoint.Tasks[0].ResultContract == nil || *checkpoint.Tasks[0].ResultContract != oldContract || checkpoint.Tasks[0].TypedResult == nil || checkpoint.Tasks[0].TypedResult.StructuredPayload == nil || checkpoint.Tasks[0].TypedResult.StructuredPayload.SHA256 != oldPayloadHash || checkpoint.Tasks[0].Status != TaskDone {
		t.Fatal("checkpoint lost the accepted contract, payload, or status")
	}
	schemaPath := filepath.Join(dir, "schemas/analysis-v1.json")
	originalSchema, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(originalSchema, &schema); err != nil {
		t.Fatal(err)
	}
	schema["$comment"] = "Different fixture revision"
	changedSchema, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(schemaPath, changedSchema, 0o600); err != nil {
		t.Fatal(err)
	}
	open := func() *Coordinator {
		session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
		if err != nil {
			t.Fatal(err)
		}
		prepareHufuDevIntegrationSession(t, session, project, endpoint, false, 0)
		return openHufuDevIntegrationCoordinator(t, session, LoadSession(workspace))
	}
	changed := open()
	if err := changed.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
		t.Fatalf("schema drift admission=%v", err)
	}
	if err := changed.Close(); err != nil {
		t.Fatal(err)
	}
	// Restoring the fixture definition does not rewrite the saved contract or
	// rerun completed work. A changed definition would require a new session.
	if err := os.WriteFile(schemaPath, originalSchema, 0o600); err != nil {
		t.Fatal(err)
	}
	restored := open()
	if err := restored.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	restored.workerAgentOverride = &mockAgent{streamFunc: func(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		calls++
		return nil, errors.New("completed task replayed")
	}}
	if _, err := restored.ResumeInterruptedTasks(withTestProtocolRepairInvocationContext(t.Context())); err != nil {
		t.Fatal(err)
	}
	events, err := restored.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)
	if len(replayed) != 1 || replayed[0].ResultContract == nil || *replayed[0].ResultContract != oldContract || replayed[0].TypedResult == nil || replayed[0].TypedResult.StructuredPayload == nil || replayed[0].TypedResult.StructuredPayload.SHA256 != oldPayloadHash || calls != 0 {
		t.Fatal("frozen history changed or completed work reran")
	}
	if requests.Load() != 0 {
		t.Fatal("resume contacted model generation")
	}
	// A fresh session can also execute the restored definition, independently
	// of the earlier checkpoint. This never rewrites the old session's events.
	fresh, freshRequests := newHufuDevIntegrationCoordinator(t, false)
	fresh.workerAgentOverride = &mockAgent{streamFunc: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		id, _ := ctx.Value(todoIDKey{}).(string)
		var payload map[string]any
		if err := json.Unmarshal(hufuDevPayload("runtime-engineer"), &payload); err != nil {
			return nil, err
		}
		input, err := json.Marshal(map[string]any{"status": "success", "summary": "Fresh restored fixture", "structured_payload": payload})
		if err != nil {
			return nil, err
		}
		response, err := (&submitResultTool{coordinator: fresh, todoID: id}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(input)})
		if err != nil {
			return nil, err
		}
		if response.IsError {
			return nil, errors.New(response.Content)
		}
		return &fantasy.AgentResult{}, nil
	}}
	if _, err := fresh.ExecuteTasks(withTestProtocolRepairInvocationContext(t.Context()), []TaskDef{{Agent: "runtime-engineer", Goal: "Analyze the restored fixture in a new session"}}); err != nil {
		t.Fatal(err)
	}
	freshItems := fresh.taskTracker.TodoList().Items()
	if len(freshItems) != 1 || freshItems[0].Status != TaskDone || freshItems[0].ResultContract == nil || *freshItems[0].ResultContract != oldContract || freshRequests.Load() != 0 {
		t.Fatal("fresh restored definition did not execute under its original contract")
	}
}
