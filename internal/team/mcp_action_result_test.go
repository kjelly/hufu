package team

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/mcp"
)

func TestMCPActionResultContract(t *testing.T) {
	text := func(value string) mcp.RuntimeToolContent { return mcp.RuntimeToolContent{Type: "text", Text: value} }
	oversized := strings.Repeat("x", maxMCPActionResultBytes+1)
	tests := []struct {
		name    string
		result  mcp.RuntimeToolResult
		want    map[string]any
		wantErr string
	}{
		{name: "structured object", result: mcp.RuntimeToolResult{StructuredContent: json.RawMessage(`{"summary":"ok"}`), Content: []mcp.RuntimeToolContent{text("fallback")}},
			want: map[string]any{"summary": "ok"}},
		{name: "structured null falls back to text", result: mcp.RuntimeToolResult{StructuredContent: json.RawMessage(`null`), Content: []mcp.RuntimeToolContent{text("plain")}},
			want: map[string]any{"result": "plain"}},
		{name: "structured array", result: mcp.RuntimeToolResult{StructuredContent: json.RawMessage(`[1]`)}, wantErr: "must be a JSON object"},
		{name: "structured invalid", result: mcp.RuntimeToolResult{StructuredContent: json.RawMessage(`{"a":`)}, wantErr: "not valid JSON"},
		{name: "structured duplicate key", result: mcp.RuntimeToolResult{StructuredContent: json.RawMessage(`{"a":1,"a":2}`)}, wantErr: "not valid JSON"},
		{name: "structured oversized", result: mcp.RuntimeToolResult{StructuredContent: json.RawMessage(`{"a":"` + oversized + `"}`)}, wantErr: "byte limit"},
		{name: "one text block", result: mcp.RuntimeToolResult{Content: []mcp.RuntimeToolContent{text(`{"not":"parsed"}`)}},
			want: map[string]any{"result": `{"not":"parsed"}`}},
		{name: "no blocks", result: mcp.RuntimeToolResult{}, wantErr: "exactly one text block, got 0"},
		{name: "two text blocks", result: mcp.RuntimeToolResult{Content: []mcp.RuntimeToolContent{text("a"), text("b")}}, wantErr: "got 2 block(s)"},
		{name: "image block", result: mcp.RuntimeToolResult{Content: []mcp.RuntimeToolContent{{Type: "image"}}}, wantErr: "[image]"},
		{name: "oversized text", result: mcp.RuntimeToolResult{Content: []mcp.RuntimeToolContent{text(oversized)}}, wantErr: "byte limit"},
		{name: "tool error", result: mcp.RuntimeToolResult{IsError: true, Content: []mcp.RuntimeToolContent{text("failed: api_token=mcp-result-secret")}}, wantErr: mcpActionToolError + ": failed:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mcpActionResult(tt.result)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if !strings.HasPrefix(err.Error(), mcpActionToolError+":") && !strings.HasPrefix(err.Error(), mcpActionResultInvalid+":") {
					t.Fatalf("error %q lacks a reason code", err)
				}
				if strings.Contains(err.Error(), "mcp-result-secret") {
					t.Fatalf("tool error leaked a secret: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("mcpActionResult: %v", err)
			}
			if !reflect.DeepEqual(got.Outputs, tt.want) || got.Artifacts != nil {
				t.Fatalf("result = %#v, want outputs %#v", got, tt.want)
			}
		})
	}
}

func TestMCPActionReceiptPinsTheBoundDescriptor(t *testing.T) {
	fake := newMCPActionFake(collectDebugTool("v1"))
	c, _, provider := newMCPCatalogActionRuntime(t, fake, true)
	if _, err := runMCPCatalogAction(c, `{"service":"api"}`); err != nil {
		t.Fatal(err)
	}
	binding, _ := provider.boundTarget()
	receipt := readRuntimeActionReceipt(t, c.session.Workspace)
	if receipt.Provider != "mcp:diagnostics/collect_debug" || receipt.ProviderDescriptorSHA256 != binding.descriptorSHA256 || receipt.Status != "success" {
		t.Fatalf("receipt provider=%q descriptor=%q status=%q, want the bound MCP target", receipt.Provider, receipt.ProviderDescriptorSHA256, receipt.Status)
	}

	commandCoordinator, _ := newCommandActionCoordinator(t, []string{"/bin/sh", "-c", `printf '{"outputs":{}}'`}, "run-command-receipt")
	if digest := commandCoordinator.phaseWorkflow.providerDescriptorDigest("structured-actions"); digest != "" {
		t.Fatalf("command provider descriptor digest = %q, want none", digest)
	}
}

func TestMCPActionFailsInvalidResultsAfterTheCall(t *testing.T) {
	summarySchema := &RunInputSchema{Type: "object", Properties: map[string]RunInputSchema{"summary": {Type: "string"}}, RequiredProperties: []string{"summary"}}
	resultSchema := &RunInputSchema{Type: "object", Properties: map[string]RunInputSchema{"result": {Type: "string"}}, RequiredProperties: []string{"result"}}
	tests := []struct {
		name    string
		result  *mcpgo.CallToolResult
		schema  *RunInputSchema
		wantErr string
	}{
		{name: "tool error", result: mcpgo.NewToolResultError("boom"), wantErr: mcpActionToolError + ": boom"},
		{name: "two blocks", result: &mcpgo.CallToolResult{Content: []mcpgo.Content{mcpgo.NewTextContent("a"), mcpgo.NewTextContent("b")}}, wantErr: mcpActionResultInvalid},
		{name: "above the canonical output limit", result: mcpgo.NewToolResultStructuredOnly(map[string]any{"summary": strings.Repeat("x", maxRuntimeOutputBytes)}), wantErr: "canonical runtime outputs"},
		{name: "output schema mismatch", result: mcpgo.NewToolResultStructuredOnly(map[string]any{"other": "x"}), schema: summarySchema, wantErr: "team_action_output_invalid"},
		{name: "text result matches a result schema", result: mcpgo.NewToolResultText("collected"), schema: resultSchema},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newMCPActionFake(collectDebugTool("v1"))
			fake.setHandler(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) { return tt.result, nil })
			c, _, _ := newMCPCatalogActionRuntime(t, fake, true)
			c.session.ActionCatalog.Entries[0].OutputSchema = tt.schema
			_, err := runMCPCatalogAction(c, `{"service":"api"}`)
			if calls := fake.calls.Load(); calls != 1 {
				t.Fatalf("CallTool count = %d, want 1", calls)
			}
			got := c.taskTracker.TodoList().Items()[0]
			if tt.wantErr == "" {
				if err != nil || got.Status != TaskDone || got.TypedResult.RuntimeOutputs["result"] != "collected" {
					t.Fatalf("error = %v, task = %s %#v", err, got.Status, got.TypedResult)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got.Status == TaskDone {
				t.Fatalf("error = %v, task status = %s, want failure %q", err, got.Status, tt.wantErr)
			}
			if receipt := readRuntimeActionReceipt(t, c.session.Workspace); receipt.Status != "failure" || !strings.Contains(receipt.Error, tt.wantErr) {
				t.Fatalf("receipt status=%q error=%q", receipt.Status, receipt.Error)
			}
		})
	}
}

func TestCompletedMCPActionIsNotReplayedOnResume(t *testing.T) {
	fake := newMCPActionFake(collectDebugTool("v1"))
	c, _, _ := newMCPCatalogActionRuntime(t, fake, true)
	task, item := addCatalogActionTodo(c, mcpCatalogActionTask(`{"service":"api"}`))
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
		t.Fatal(err)
	}
	if got := c.todoItemByID(item.ID); got == nil || got.Status != TaskDone || got.ExecutionReceipt == nil {
		t.Fatalf("task after the action = %#v, want done with a receipt", got)
	}
	resumed, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil || resumed != 0 {
		t.Fatalf("ResumeInterruptedTasks = %d, %v; want nothing to resume", resumed, err)
	}
	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("CallTool count = %d, want 1", calls)
	}
}
