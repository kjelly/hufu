package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"gopkg.in/yaml.v3"
)

func observationTestPolicy() WorkerToolPolicy {
	return WorkerToolPolicy{SideEffect: "none", Filesystem: "none", InputSchema: map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"service"},
		"properties": map[string]any{"service": map[string]any{"type": "string", "enum": []any{"public"}}},
	}}
}

func TestWorkerPolicyConfigurationAndValidation(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			config := MCPServerConfig{Command: []string{"test"}, ToolPolicies: map[string]WorkerToolPolicy{"observe": observationTestPolicy()}}
			var decoded MCPServerConfig
			if format == "json" {
				data, err := json.Marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
			} else {
				data, err := yaml.Marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				if err := yaml.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateWorkerToolPolicies(decoded); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, name := range []string{"effect", "filesystem", "open_schema", "external_ref", "invalid_schema", "excluded", "missing_allowlist"} {
		t.Run(name, func(t *testing.T) {
			policy := observationTestPolicy()
			config := MCPServerConfig{ToolPolicies: map[string]WorkerToolPolicy{"observe": policy}}
			switch name {
			case "effect":
				policy.SideEffect = "external_write"
			case "filesystem":
				policy.Filesystem = "read"
			case "open_schema":
				delete(policy.InputSchema, "additionalProperties")
			case "external_ref":
				policy.InputSchema["$ref"] = "https://invalid.example/schema"
			case "invalid_schema":
				policy.InputSchema["required"] = 2
			case "excluded":
				config.ExcludedTools = []string{"observe"}
			case "missing_allowlist":
				config.AllowedTools = []string{"other"}
			}
			config.ToolPolicies["observe"] = policy
			if err := ValidateWorkerToolPolicies(config); err == nil {
				t.Fatal("unsafe policy accepted")
			}
		})
	}
}

func TestWorkerPolicyRejectsArgumentsBeforeTransport(t *testing.T) {
	manager := NewMCPToolManager("", "")
	fake := newRuntimeTestServer(t, map[string]func(mcpgo.CallToolRequest) *mcpgo.CallToolResult{"observe": textResult("exact words")})
	attachRuntimeTestServer(t, manager, "source", MCPServerConfig{ToolPolicies: map[string]WorkerToolPolicy{"observe": observationTestPolicy()}}, fake)
	tool := manager.SnapshotToolDescriptors()[0]
	digest, err := MCPToolDescriptorSHA256(tool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithToolAuthorizer(t.Context(), allowAll)
	for _, input := range []string{`{}`, `{"service":"private"}`, `{"service":"public","filename":"/tmp/output"}`, `[]`, `{"service":"private","service":"public"}`, `{"service":"public"} {}`} {
		_, _, err := manager.ExecuteAuthorizedTool(ctx, tool.Name, digest, input)
		if !IsToolAuthorizationError(err) {
			t.Fatalf("input %s: %v", input, err)
		}
	}
	if _, _, err := manager.ExecuteAuthorizedTool(t.Context(), tool.Name, digest, `{"service":"public"}`); !IsToolAuthorizationError(err) {
		t.Fatal("missing authorizer accepted")
	}
	if _, _, err := manager.ExecuteTool(ctx, tool.Name, `{"service":"public"}`); !IsToolAuthorizationError(err) {
		t.Fatal("legacy execution bypassed policy")
	}
	if fake.calls.Load() != 0 {
		t.Fatal("rejected input reached transport")
	}
	response, err := manager.AsAgentTools()[0].Run(ctx, fantasy.ToolCall{Input: `{"service":"public"}`})
	if err != nil || response.IsError || response.Content != "exact words" || fake.calls.Load() != 1 {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	denied := WithToolAuthorizer(t.Context(), func(context.Context, string, string, string) error { return context.Canceled })
	_, _, err = manager.ExecuteAuthorizedTool(denied, tool.Name, digest, `{"service":"public"}`)
	if !IsToolAuthorizationError(err) || fake.calls.Load() != 1 {
		t.Fatal("runtime denial did not precede transport")
	}
}

func TestWorkerPolicyDescriptorAndSnapshotsAreImmutable(t *testing.T) {
	manager := NewMCPToolManager("", "")
	fake := newRuntimeTestServer(t, map[string]func(mcpgo.CallToolRequest) *mcpgo.CallToolResult{"observe": textResult("ok")})
	attachRuntimeTestServer(t, manager, "source", MCPServerConfig{ToolPolicies: map[string]WorkerToolPolicy{"observe": observationTestPolicy()}}, fake)
	tool := manager.SnapshotToolDescriptors()[0]
	digest, err := MCPToolDescriptorSHA256(tool)
	if err != nil {
		t.Fatal(err)
	}
	tool.WorkerPolicy.InputSchema["required"] = []any{}
	changed, err := MCPToolDescriptorSHA256(tool)
	if err != nil || digest == changed {
		t.Fatal("policy change did not change descriptor")
	}
	live, err := MCPToolDescriptorSHA256(manager.SnapshotToolDescriptors()[0])
	if err != nil || live != digest {
		t.Fatal("detached snapshot mutated manager")
	}
	policy, _ := manager.WorkerPolicy(tool.Name)
	policy.SideEffect = "external_write"
	livePolicy, _ := manager.WorkerPolicy(tool.Name)
	if livePolicy.SideEffect != "none" {
		t.Fatal("policy query leaked mutable state")
	}
	if _, err := manager.ReserveRuntimeTool("source", "observe"); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.WorkerPolicy(tool.Name); ok {
		t.Fatal("reserved runtime tool has worker grant")
	}
}

func TestWorkerPolicyMissingToolFailsInitialization(t *testing.T) {
	manager := NewMCPToolManager("", "")
	fake := newRuntimeTestServer(t, map[string]func(mcpgo.CallToolRequest) *mcpgo.CallToolResult{"other": textResult("ok")})
	cli, err := client.NewInProcessClient(fake.server)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	err = manager.AttachClient(t.Context(), "source", MCPServerConfig{ToolPolicies: map[string]WorkerToolPolicy{"observe": observationTestPolicy()}}, cli)
	if err == nil || !strings.Contains(err.Error(), "was not listed") {
		t.Fatalf("missing tool: %v", err)
	}
}

func TestEmptyMCPArgumentsRemainObjectOnWire(t *testing.T) {
	for _, input := range []string{"", "{}", " {} "} {
		t.Run(input, func(t *testing.T) {
			arguments, err := decodeToolArguments(input)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: "observe", Arguments: arguments}})
			if err != nil {
				t.Fatal(err)
			}
			var request map[string]any
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Fatal(err)
			}
			params := request["params"].(map[string]any)
			object, ok := params["arguments"].(map[string]any)
			if !ok || len(object) != 0 {
				t.Fatalf("empty argument object lost on wire: %s", raw)
			}
		})
	}
}
