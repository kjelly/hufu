package mcp

import "testing"

func TestMCPToolDescriptorSHA256CanonicalizesRequiredArrays(t *testing.T) {
	left := MCPTool{
		Name: "github__issue", ServerName: "github", OrigName: "issue", Description: "Inspect issue",
		InputSchema: map[string]any{
			"type": "object", "required": []any{"repo", "number"},
			"properties": map[string]any{
				"filter": map[string]any{"type": "object", "required": []any{"state", "label"}},
			},
		},
	}
	right := cloneMCPTool(left)
	right.InputSchema["required"] = []any{"number", "repo"}
	right.InputSchema["properties"].(map[string]any)["filter"].(map[string]any)["required"] = []any{"label", "state"}
	leftHash, err := MCPToolDescriptorSHA256(left)
	if err != nil {
		t.Fatalf("left descriptor hash: %v", err)
	}
	rightHash, err := MCPToolDescriptorSHA256(right)
	if err != nil {
		t.Fatalf("right descriptor hash: %v", err)
	}
	if leftHash != rightHash {
		t.Fatalf("canonical hashes differ: %s != %s", leftHash, rightHash)
	}
	right.Description = "Changed"
	changedHash, err := MCPToolDescriptorSHA256(right)
	if err != nil {
		t.Fatalf("changed descriptor hash: %v", err)
	}
	if changedHash == leftHash {
		t.Fatal("description change did not alter descriptor hash")
	}
}

func TestSnapshotToolDescriptorsDeepCopiesAndSorts(t *testing.T) {
	manager := NewMCPToolManager("", "")
	manager.tools = []MCPTool{
		{Name: "z__last", InputSchema: map[string]any{"properties": map[string]any{"x": map[string]any{"type": "string"}}}, Required: []string{"x"}},
		{Name: "a__first", InputSchema: map[string]any{"type": "object"}},
	}
	snapshot := manager.SnapshotToolDescriptors()
	if len(snapshot) != 2 || snapshot[0].Name != "a__first" || snapshot[1].Name != "z__last" {
		t.Fatalf("snapshot order = %#v", snapshot)
	}
	snapshot[1].Required[0] = "mutated"
	snapshot[1].InputSchema["properties"].(map[string]any)["x"] = "mutated"
	if manager.tools[0].Required[0] != "x" {
		t.Fatal("snapshot shares required slice with manager")
	}
	if _, ok := manager.tools[0].InputSchema["properties"].(map[string]any)["x"].(map[string]any); !ok {
		t.Fatal("snapshot shares schema map with manager")
	}
}
