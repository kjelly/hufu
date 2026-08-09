package mcp

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
)

func TestMCPInputConfig_UnmarshalYAML(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantName string
		wantReq  bool
		wantType string
		wantDesc string
	}{
		{
			name:     "string shorthand",
			yaml:     "host",
			wantName: "host",
			wantReq:  true,
			wantType: "string",
		},
		{
			name:     "full object",
			yaml:     "{name: port, desc: 'port number', type: number, required: false}",
			wantName: "port",
			wantReq:  false,
			wantType: "number",
			wantDesc: "port number",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input agent.MCPInputConfig
			err := yaml.Unmarshal([]byte(tt.yaml), &input)
			if err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}
			if input.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", input.Name, tt.wantName)
			}
			if input.Required != tt.wantReq {
				t.Errorf("Required = %v, want %v", input.Required, tt.wantReq)
			}
			if input.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", input.Type, tt.wantType)
			}
			if input.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", input.Description, tt.wantDesc)
			}
		})
	}
}

func TestAgentMCPServer_ExecuteTool_WithDetailedInputs(t *testing.T) {
	tools := map[string]agent.MCPToolConfig{
		"echo-test": {
			Cmd: "echo \"$NAME says $GREETING\"",
			Inputs: []agent.MCPInputConfig{
				{Name: "name", Required: true},
				{Name: "greeting", Description: "The greeting to use", Type: "string"},
			},
		},
	}
	server := NewAgentMCPServer("test-agent", tools, "bash")

	input := `{"name": "Alice", "greeting": "Hello"}`
	result, err := server.executeTool(context.Background(), "echo-test", tools["echo-test"], "", "", "", input)
	if err != nil {
		t.Fatalf("executeTool failed: %v", err)
	}

	if result.IsError {
		t.Fatalf("result is error: %v", result.Content)
	}

	output := extractTextFromResult(result)
	if !strings.Contains(output, "Alice says Hello") {
		t.Errorf("unexpected output: %q", output)
	}
}
