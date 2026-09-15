package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

const mcpToolDescriptorVersion = 1

type mcpToolDescriptorV1 struct {
	Version     int            `json:"version"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	ServerName  string         `json:"server_name"`
	NativeName  string         `json:"native_name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// MCPToolDescriptorSHA256 fingerprints the complete immutable logical MCP
// descriptor used when an occurrence freezes its authorization ceiling.
func MCPToolDescriptorSHA256(tool MCPTool) (string, error) {
	schema, err := canonicalJSONMap(tool.InputSchema)
	if err != nil {
		return "", fmt.Errorf("canonicalize input schema: %w", err)
	}
	payload, err := json.Marshal(mcpToolDescriptorV1{
		Version: mcpToolDescriptorVersion, Name: tool.Name, Kind: "mcp",
		ServerName: tool.ServerName, NativeName: tool.OrigName,
		Description: tool.Description, InputSchema: schema,
	})
	if err != nil {
		return "", fmt.Errorf("marshal MCP tool descriptor: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func captureMCPInputSchema(raw json.RawMessage, typed any) (map[string]any, error) {
	if len(raw) > 0 {
		return decodeMCPInputSchema(raw)
	}
	payload, err := json.Marshal(typed)
	if err != nil {
		return nil, err
	}
	return decodeMCPInputSchema(payload)
}

func decodeMCPInputSchema(payload []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var cloned map[string]any
	if err := decoder.Decode(&cloned); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("input schema contains more than one JSON value")
		}
		return nil, err
	}
	return cloned, nil
}

func cloneMCPTool(tool MCPTool) MCPTool {
	cloned := tool
	cloned.InputSchema, _ = canonicalJSONMap(tool.InputSchema)
	cloned.Parameters, _ = canonicalJSONMap(tool.Parameters)
	cloned.Required = slices.Clone(tool.Required)
	return cloned
}

func canonicalJSONMap(value map[string]any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	cloned, err := decodeMCPInputSchema(payload)
	if err != nil {
		return nil, err
	}
	canonicalizeRequiredArrays(cloned)
	if _, err := json.Marshal(cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func canonicalizeRequiredArrays(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			canonicalizeRequiredArrays(child)
			if key != "required" {
				continue
			}
			values, ok := child.([]any)
			if !ok {
				continue
			}
			strings := make([]string, 0, len(values))
			allStrings := true
			for _, candidate := range values {
				text, ok := candidate.(string)
				if !ok {
					allStrings = false
					break
				}
				strings = append(strings, text)
			}
			if !allStrings {
				continue
			}
			slices.Sort(strings)
			for i := range strings {
				values[i] = strings[i]
			}
			typed[key] = values[:len(strings)]
		}
	case []any:
		for _, child := range typed {
			canonicalizeRequiredArrays(child)
		}
	}
}
