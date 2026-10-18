package team

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/mcp"
)

// readOnlyRecoveryManager serves source__read_document under a declared
// observation policy and sink__write_note without one.
func readOnlyRecoveryManager(t *testing.T) *mcp.MCPToolManager {
	t.Helper()
	manager := mcp.NewMCPToolManager("", "")
	t.Cleanup(func() { _ = manager.Close() })
	source := mcp.MCPServerConfig{AllowedTools: []string{"read_document"}, ToolPolicies: map[string]mcp.WorkerToolPolicy{"read_document": workerObservationPolicy()}}
	attachMCPActionFake(t, manager, "source", source, newMCPActionFake(mcpgo.NewTool("read_document", mcpgo.WithString("service", mcpgo.Required()))))
	sink := mcp.MCPServerConfig{AllowedTools: []string{"write_note"}}
	attachMCPActionFake(t, manager, "sink", sink, newMCPActionFake(mcpgo.NewTool("write_note", mcpgo.WithString("text", mcpgo.Required()))))
	return manager
}

func gatewayCallInput(target string) string {
	return `{"action":"call","target":"` + target + `","arguments":{"service":"public"}}`
}

func TestCoordinatorReadOnlyToolCallClassifiesDeclaredMCPObservations(t *testing.T) {
	c := &Coordinator{mcpManager: readOnlyRecoveryManager(t)}
	cases := []struct {
		name     string
		tool     string
		input    string
		readOnly bool
	}{
		{"static observation", "view", `{"file_path":"a.go"}`, true},
		{"static mutation", "write", `{"file_path":"a.go"}`, false},
		{"gateway search", dynamicToolGatewayName, `{"action":"search","query":"document"}`, true},
		{"gateway inspect", dynamicToolGatewayName, `{"action":"inspect","target":"source__read_document"}`, true},
		{"gateway call to declared observation", dynamicToolGatewayName, gatewayCallInput("source__read_document"), true},
		{"gateway call without worker policy", dynamicToolGatewayName, gatewayCallInput("sink__write_note"), false},
		{"gateway call to unknown target", dynamicToolGatewayName, gatewayCallInput("source__missing"), false},
		{"gateway call missing arguments", dynamicToolGatewayName, `{"action":"call","target":"source__read_document"}`, false},
		{"gateway unsupported action", dynamicToolGatewayName, `{"action":"exec","target":"source__read_document","arguments":{}}`, false},
		{"gateway malformed input", dynamicToolGatewayName, `{bad-json`, false},
		{"direct declared observation", "source__read_document", `{"service":"public"}`, true},
		{"direct tool without worker policy", "sink__write_note", `{"text":"x"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.readOnlyToolCall(tc.tool, tc.input); got != tc.readOnly {
				t.Fatalf("readOnlyToolCall(%q, %s) = %t, want %t", tc.tool, tc.input, got, tc.readOnly)
			}
		})
	}

	var none *Coordinator
	if none.readOnlyToolCall(dynamicToolGatewayName, gatewayCallInput("source__read_document")) {
		t.Fatal("without a manager no MCP policy can classify a gateway call as read-only")
	}
}

func TestProtocolRecoveryTreatsDeclaredGatewayObservationsAsReadOnly(t *testing.T) {
	c := &Coordinator{mcpManager: readOnlyRecoveryManager(t)}
	cases := []struct {
		name     string
		target   string
		readOnly bool
	}{
		{"declared observation", "source__read_document", true},
		{"undeclared tool", "sink__write_note", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{
				fantasy.ToolCallContent{ToolCallID: "search-1", ToolName: "web_search", Input: `{"query":"assistant assault"}`},
				fantasy.ToolResultContent{ToolCallID: "search-1", ToolName: "web_search", Result: fantasy.ToolResultOutputContentText{Text: "search lead"}},
				fantasy.ToolCallContent{ToolCallID: "gateway-1", ToolName: dynamicToolGatewayName, Input: gatewayCallInput(tc.target)},
				fantasy.ToolResultContent{ToolCallID: "gateway-1", ToolName: dynamicToolGatewayName, Result: fantasy.ToolResultOutputContentText{Text: "snapshot quotation"}},
			}}}}
			if protocolAttemptWasReadOnly(steps, isReadOnlyToolCall) {
				t.Fatal("the static taxonomy alone must not classify dynamic MCP calls")
			}
			if got := protocolAttemptWasReadOnly(steps, c.readOnlyToolCall); got != tc.readOnly {
				t.Fatalf("protocolAttemptWasReadOnly = %t, want %t", got, tc.readOnly)
			}
			if got := attemptChangedNothing(steps, nil, c.readOnlyToolCall); got != tc.readOnly {
				t.Fatalf("attemptChangedNothing = %t, want %t", got, tc.readOnly)
			}
			evidence, observed := protocolRepairObservationEvidence(steps, c.readOnlyToolCall)
			if !observed || !strings.Contains(evidence, "search lead") {
				t.Fatalf("static observation missing from repair evidence: %q", evidence)
			}
			if got := strings.Contains(evidence, "snapshot quotation"); got != tc.readOnly {
				t.Fatalf("gateway result in repair evidence = %t, want %t: %q", got, tc.readOnly, evidence)
			}
		})
	}
}

// The 2026-10-15 web-research researcher used only gateway-proxied Playwright
// observations, then omitted submit_result and failed its result-only repair.
// The static taxonomy rejected use_dynamic_tool, so the side_effect:none task
// was blocked for reconciliation instead of receiving its one fresh attempt.
func TestProtocolRepairFailureAfterDeclaredGatewayObservationRetriesOnce(t *testing.T) {
	cases := []struct {
		name        string
		target      string
		rejected    bool
		neverRetry  bool
		persistent  bool
		workerCalls int
		done        bool
	}{
		{name: "declared observation retries", target: "source__read_document", workerCalls: 2, done: true},
		{name: "rejected reporting call retries", target: "source__read_document", rejected: true, workerCalls: 2, done: true},
		{name: "rejected reporting retry stays bounded", target: "source__read_document", rejected: true, persistent: true, workerCalls: 2},
		{name: "never policy prevents replay", target: "source__read_document", rejected: true, neverRetry: true, workerCalls: 1},
		{name: "undeclared tool does not replay", target: "sink__write_note", workerCalls: 1},
		{name: "rejected reporting after undeclared tool does not replay", target: "sink__write_note", rejected: true, workerCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workerCalls, repairCalls := 0, 0
			c := &Coordinator{
				session: &TeamSession{
					Workspace: t.TempDir(),
					Config:    agent.TeamConfig{Name: "gateway-observation-fallback", Timeout: 30, MaxRetries: 1},
					Agents: map[string]*agent.AgentDef{
						"researcher": {
							Name: "researcher", Role: "worker", MaxRetries: 1,
							SideEffect: string(SideEffectNone), Recovery: string(RecoveryRetry),
							Generation: agent.GenerationParams{Model: "test"},
						},
					},
				},
				sessionTime:    time.Now(),
				taskTracker:    NewTaskTracker(),
				reportStatus:   func(StatusEvent) {},
				taskCache:      newDefaultTaskCache(taskCacheDependencies{}),
				executionRunID: "run-gateway-observation-fallback",
				mcpManager:     readOnlyRecoveryManager(t),
			}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "researcher", Desc: "research the event"}})[0]
			worker := &resultContractWorker{
				calls: &workerCalls, rejectedSubmission: tc.rejected,
				toolCall: &fantasy.ToolCallContent{ToolCallID: "gateway-1", ToolName: dynamicToolGatewayName, Input: gatewayCallInput(tc.target)},
			}
			if !tc.persistent {
				worker.onSecond = func() {
					c.storeSubmittedTaskResult(item.ID, &TaskResult{
						TaskID: item.ID, Agent: "researcher", Status: "success", Summary: "fresh attempt submitted", Source: "submitted",
					})
				}
			}
			c.workerAgentOverride = worker
			c.repairAgentOverride = &scriptedRepairAgent{calls: &repairCalls}
			if tc.rejected {
				c.repairAgentOverride = &scriptedRepairAgent{calls: &repairCalls, steps: func(int) []fantasy.StepResult { return invalidSchemaRepairSteps() }}
			}
			recovery := RecoveryRetry
			if tc.neverRetry {
				recovery = RecoveryNever
			}

			// A required structured payload, like research-v1, rules out
			// promoting the worker's prose as a read-only handoff.
			_, _ = c.executeTask(withTestProtocolRepairInvocationContext(context.Background()), TaskDef{
				Agent: "researcher", Goal: "research the event", SideEffect: SideEffectNone, Recovery: recovery, Execution: ExecutionContract{RequiresResult: true},
				ResultContract: &ResultContractRef{ID: "schemas/research.json", SchemaSHA256: strings.Repeat("a", 64), RequireStructured: true},
			}, item.ID)
			if workerCalls != tc.workerCalls {
				t.Fatalf("worker calls = %d, want %d", workerCalls, tc.workerCalls)
			}
			if got := c.taskTracker.TodoList().Items()[0]; (got.Status == TaskDone) != tc.done {
				t.Fatalf("task status = %s, want done=%t", got.Status, tc.done)
			}
			if tc.rejected && tc.workerCalls == 2 && repairCalls < 2 {
				t.Fatalf("worker retried before bounded result-only repair: %d repair calls", repairCalls)
			}
			if tc.rejected {
				got := c.taskTracker.TodoList().Items()[0]
				raw, err := json.Marshal(taskTransitionPayload(got))
				if err != nil {
					t.Fatal(err)
				}
				replayed := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: got.ID, Payload: raw}})
				if len(replayed) != 1 || len(replayed[0].ExecutionReceipts) == 0 {
					t.Fatal("replay lost the original attempt's execution receipts")
				}
				provenance := replayed[0].ExecutionReceipts[0].RepairProvenance
				if provenance == nil || provenance.Success || len(provenance.History) != 2 || provenance.History[0].FailureReason != RepairFailureInvalidSchema {
					t.Fatalf("replay lost the failed repair diagnostics: %#v", provenance)
				}
			}
		})
	}
}
