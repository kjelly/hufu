package team

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/tools"
)

func workerObservationPolicy() mcp.WorkerToolPolicy {
	return mcp.WorkerToolPolicy{SideEffect: "none", Filesystem: "none", InputSchema: map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"service"},
		"properties": map[string]any{"service": map[string]any{"type": "string", "enum": []any{"public"}}},
	}}
}

func workerObservationFixture(t *testing.T) (*Coordinator, *mcpActionFake, context.Context) {
	t.Helper()
	c := newDirectTypedCoordinator(t, "source__read_document", nil, nil)
	manager := mcp.NewMCPToolManager("", "")
	t.Cleanup(func() { _ = manager.Close() })
	cfg := mcp.MCPServerConfig{AllowedTools: []string{"read_document"}, ToolPolicies: map[string]mcp.WorkerToolPolicy{"read_document": workerObservationPolicy()}}
	fake := newMCPActionFake(mcpgo.NewTool("read_document", mcpgo.WithString("service", mcpgo.Required()), mcpgo.WithString("filename")))
	fake.setHandler(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(textEvidenceTestRecords()[1].Output), nil
	})
	attachMCPActionFake(t, manager, "source", cfg, fake)
	c.mcpManager = manager
	c.session.MCPServers = map[string]mcp.MCPServerConfig{"source": cfg}
	ctx := gatewayTestContext(t, c, dynamicToolGatewayName, "source__read_document")
	ctx = context.WithValue(ctx, tools.ArtifactPathPolicyKey, workerArtifactPathPolicy(c.session.Agents["worker"], false, nil))
	ctx = context.WithValue(ctx, tools.AgentReadOnlyExecutionKey, true)
	ctx = mcp.WithToolAuthorizer(ctx, func(callCtx context.Context, server, tool, input string) error {
		if denial, _ := c.authorizeDynamicLogicalInvocation(callCtx, "worker", server+"__"+tool, input); denial != "" {
			return fmt.Errorf("%s", denial)
		}
		return nil
	})
	return c, fake, ctx
}

func TestMCPWorkerObservationDirectAndGatewayPolicy(t *testing.T) {
	for _, path := range []string{"direct", "gateway"} {
		for _, scenario := range []string{"allowed", "enum_case", "arguments", "duplicate", "no_net", "agent_no_net", "context_no_net", "denied", "session_denied", "no_literal_grant", "unscoped_no_net", "unscoped_undeclared", "bound"} {
			t.Run(path+"_"+scenario, func(t *testing.T) {
				c, fake, ctx := workerObservationFixture(t)
				input := `{"service":"public"}`
				switch scenario {
				case "enum_case":
					input = `{"service":"PUBLIC"}`
				case "arguments":
					input = `{"service":"public","filename":"/tmp/output"}`
				case "duplicate":
					input = `{"service":"private","service":"PUBLIC"}`
				case "unscoped_no_net", "unscoped_undeclared":
					ctx = context.WithValue(ctx, tools.ArtifactPathPolicyKey, tools.ArtifactPathPolicy{})
					ctx = context.WithValue(ctx, tools.AgentReadOnlyExecutionKey, false)
					if scenario == "unscoped_no_net" {
						c.noNet = true
					} else {
						c.session.Agents["worker"].Tools = "view"
					}
				case "no_net":
					c.noNet = true
				case "agent_no_net":
					c.session.Agents["worker"].NoNet = true
				case "context_no_net":
					ctx = context.WithValue(ctx, tools.AgentNetworkBlockKey, true)
				case "denied":
					c.session.Config.ToolsDenied = []string{"source__read_document"}
				case "session_denied":
					ctx = context.WithValue(ctx, tools.AgentToolsSessionPermissionsKey, map[string]bool{"source__read_document": false})
				case "no_literal_grant":
					c.session.Agents["worker"].Tools = "view"
				case "bound":
					ctx = context.WithValue(ctx, tools.ArtifactPathPolicyKey, workerArtifactPathPolicy(c.session.Agents["worker"], true, nil))
				}
				var response fantasy.ToolResponse
				var err error
				if path == "direct" {
					response, err = (&policyGatedTool{coordinator: c, inner: c.mcpManager.AsAgentTools()[0]}).Run(ctx, fantasy.ToolCall{ID: "observed", Input: input})
				} else {
					descriptor := c.mcpManager.SnapshotToolDescriptors()[0]
					fingerprint, hashErr := mcp.MCPToolDescriptorSHA256(descriptor)
					if hashErr != nil {
						t.Fatal(hashErr)
					}
					gateway := newDynamicToolGateway(c, c.mcpManager, []DynamicToolTarget{{Name: descriptor.Name, InputSchema: descriptor.InputSchema, WorkerInputSchema: descriptor.WorkerPolicy.InputSchema, DescriptorSHA256: fingerprint}})
					response, err = gateway.Run(ctx, fantasy.ToolCall{ID: "observed", Input: `{"action":"call","target":"source__read_document","arguments":` + input + `}`})
				}
				allowed := scenario == "allowed" || scenario == "enum_case"
				if err != nil || response.IsError == allowed || (fake.calls.Load() > 0) != allowed {
					t.Fatalf("allowed=%v calls=%d response=%#v err=%v", allowed, fake.calls.Load(), response, err)
				}
			})
		}
	}
}

func TestMCPWorkerPolicyAuthenticityAndFrozenIsolation(t *testing.T) {
	c, _, ctx := workerObservationFixture(t)
	if !c.workerMCPObservationGrant(ctx, "source__read_document", c.mcpManager.AsAgentTools()[0]) {
		t.Fatal("authentic adapter denied")
	}
	lookalike := &recordingTool{name: "source__read_document"}
	if c.workerMCPObservationGrant(ctx, "source__read_document", lookalike) {
		t.Fatal("lookalike adapter inherited policy")
	}
	fake := newMCPActionFake(mcpgo.NewTool("read_document", mcpgo.WithString("service", mcpgo.Required())))
	attachMCPActionFake(t, c.mcpManager, "counter", c.session.MCPServers["source"], fake)
	snapshot, err := c.resolveNewTaskToolAuthorization(ctx, TaskDef{Agent: "worker", SideEffect: SideEffectNone}, c.session.Agents["worker"])
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Targets) != 1 || snapshot.Targets[0].Name != "source__read_document" {
		t.Fatalf("cross-worker catalog exposure: %#v", snapshot.Targets)
	}
	if err := c.bindMCPWorkerPolicies(); err != nil {
		t.Fatal(err)
	}
	changed := workerObservationPolicy()
	changed.InputSchema["required"] = []any{}
	if err := c.mcpManager.BindWorkerToolPolicy("source__read_document", changed); err == nil {
		t.Fatal("mismatched startup policy admitted")
	}
	c.session.MCPServers["missing"] = c.session.MCPServers["source"]
	if err := c.bindMCPWorkerPolicies(); err == nil {
		t.Fatal("missing configured worker tool admitted")
	}
}

func TestMCPWorkerTextEvidenceThroughGateway(t *testing.T) {
	c, fake, ctx := workerObservationFixture(t)
	descriptor := c.mcpManager.SnapshotToolDescriptors()[0]
	fingerprint, err := mcp.MCPToolDescriptorSHA256(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	gateway := newDynamicToolGateway(c, c.mcpManager, []DynamicToolTarget{{Name: descriptor.Name, InputSchema: descriptor.InputSchema, WorkerInputSchema: descriptor.WorkerPolicy.InputSchema, DescriptorSHA256: fingerprint}})
	transcript, err := newTaskTranscriptForAttempt(t.TempDir(), "task", c.executionRunID, 1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transcript.Close() }()
	accumulator := newDynamicInvocationAccumulator(1)
	ctx = withDynamicToolInvocationSink(ctx, accumulator.record, dynamicInvocationDiagnosticsReporter(c, transcript, new(toolCallEvidence), "worker", "task"))
	response, err := gateway.Run(ctx, fantasy.ToolCall{ID: "observed", Input: `{"action":"call","target":"source__read_document","arguments":{"service":"public"}}`})
	if err != nil || response.IsError || fake.calls.Load() != 1 {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	compiled := textEvidenceTestContract(t, true)
	payload, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(evidenceTestPayload), transcript.evidenceRecords())
	if err != nil || payload.EvidenceDowngrades != 0 {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}
	receipts, truncated := accumulator.snapshot()
	if truncated != 0 || len(receipts) != 1 || receipts[0].LogicalTool != descriptor.Name {
		t.Fatalf("logical receipts=%#v", receipts)
	}
}

func TestMCPWorkerPolicyBlocksResumeDrift(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	workspace := t.TempDir()
	start := func(policy mcp.WorkerToolPolicy, command string) *Coordinator {
		c := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 3)
		c.session.MCPServers = map[string]mcp.MCPServerConfig{"source": {Command: []string{command}, ToolPolicies: map[string]mcp.WorkerToolPolicy{"read_document": policy}}}
		if err := c.rebuildExecutionPolicyStateForTest(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := start(workerObservationPolicy(), "original")
	first.initEventStore()
	if err := first.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	closeWorkerModelResumeStore(t, first)
	checkpoint := LoadSession(workspace)
	if checkpoint == nil || len(checkpoint.ExecutionPolicySnapshot.MCPWorkerPolicies) != 1 {
		t.Fatal("worker policy missing from checkpoint")
	}
	for _, name := range []string{"same", "schema", "command", "legacy"} {
		t.Run(name, func(t *testing.T) {
			policy := workerObservationPolicy()
			command := "original"
			if name == "schema" {
				policy.InputSchema["required"] = []any{}
			}
			if name == "command" {
				command = "different"
			}
			second := start(policy, command)
			if name == "legacy" {
				legacy, err := newExecutionPolicyStateForVersion(second, executionPolicyLegacySnapshotVersion)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := second.executionPolicySnapshotMatchesCurrent(legacy.snapshot); err == nil || !strings.Contains(err.Error(), "cannot pin MCP worker") {
					t.Fatalf("legacy resume: %v", err)
				}
				return
			}
			second.SetSessionData(checkpoint)
			second.initEventStore()
			err := second.checkRunAdmission()
			if (err == nil) != (name == "same") {
				t.Fatalf("admission: %v", err)
			}
			if name != "same" && !strings.Contains(err.Error(), "snapshot drift detected") {
				t.Fatalf("drift error: %v", err)
			}
			closeWorkerModelResumeStore(t, second)
		})
	}
}

// Test-only fixture rebuild after configuring policy, before any admission.
func (c *Coordinator) rebuildExecutionPolicyStateForTest() error {
	state, err := newExecutionPolicyState(c)
	if err != nil {
		return err
	}
	c.executionPolicy = state
	return nil
}
