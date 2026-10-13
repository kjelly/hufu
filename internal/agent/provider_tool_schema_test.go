package agent

import (
	json "encoding/json/v2"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"
)

type sharedSchemaTestTool struct {
	fantasy.AgentTool
	info fantasy.ToolInfo
}

func (tool sharedSchemaTestTool) Info() fantasy.ToolInfo { return tool.info }

func TestProviderSchemaNormalizationPreservesSharedAuthorizationSchema(t *testing.T) {
	tool := sharedSchemaTestTool{info: fantasy.ToolInfo{Name: "shared", Parameters: map[string]any{
		"values": map[string]any{"type": "array"},
		"choice": map[string]any{"type": []any{"string", "null"}, "enum": []any{"safe", nil}},
		"nested": map[string]any{"anyOf": []any{map[string]any{"type": "array"}}},
	}}}
	var nilMap map[string]any
	if cloneProviderSchemaValue(nilMap).(map[string]any) != nil {
		t.Fatal("schema clone changed nil wire value")
	}
	before, err := json.Marshal(tool.Info().Parameters, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	wrapped := providerToolsWithOwnedSchemas([]fantasy.AgentTool{tool})[0]
	var wg sync.WaitGroup
	errors := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			info := wrapped.Info()
			schema.Normalize(info.Parameters)
			_, err := SerializeProviderRequest(ProviderRequest{ModelID: "test", Tools: []fantasy.AgentTool{tool}})
			errors <- err
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	after, err := json.Marshal(tool.Info().Parameters, json.Deterministic(true))
	if err != nil || string(before) != string(after) {
		t.Fatalf("shared tool authorization schema mutated: %s -> %s", before, after)
	}
	info := wrapped.Info()
	info.Parameters["values"].(map[string]any)["type"] = "forged"
	if wrapped.Info().Parameters["values"].(map[string]any)["type"] != "array" {
		t.Fatal("SDK schema views share mutable nested containers")
	}
}
