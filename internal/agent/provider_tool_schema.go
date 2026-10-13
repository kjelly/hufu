package agent

import (
	"slices"

	"charm.land/fantasy"
)

// Fantasy normalizes schemas in place. Give both the SDK and request admission
// owned containers, so normalization cannot mutate a shared tool definition or
// race another worker's authorization digest. Scalar types remain unchanged.
func cloneProviderSchemaValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		if value == nil {
			return value
		}
		copy := make(map[string]any, len(value))
		for key, child := range value {
			copy[key] = cloneProviderSchemaValue(child)
		}
		return copy
	case []any:
		if value == nil {
			return value
		}
		copy := make([]any, len(value))
		for index, child := range value {
			copy[index] = cloneProviderSchemaValue(child)
		}
		return copy
	default:
		return value
	}
}

type ownedSchemaTool struct{ fantasy.AgentTool }

func (tool ownedSchemaTool) Info() fantasy.ToolInfo {
	info := tool.AgentTool.Info()
	if info.Parameters != nil {
		info.Parameters = cloneProviderSchemaValue(info.Parameters).(map[string]any)
	}
	info.Required = slices.Clone(info.Required)
	return info
}

func providerToolsWithOwnedSchemas(original []fantasy.AgentTool) []fantasy.AgentTool {
	tools := make([]fantasy.AgentTool, len(original))
	for index, tool := range original {
		if tool != nil {
			tools[index] = ownedSchemaTool{AgentTool: tool}
		}
	}
	return tools
}
