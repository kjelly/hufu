package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestValidateRequired(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		required []string
		wantErr  bool
	}{
		{"empty input with required", "", []string{"code"}, true},
		{"empty object with required", "{}", []string{"code"}, true},
		{"missing required field", `{"timeout": 10}`, []string{"code"}, true},
		{"required field present", `{"code": "print(1)"}`, []string{"code"}, false},
		{"required field empty string", `{"code": ""}`, []string{"code"}, true},
		{"multiple required all present", `{"path": "/tmp", "content": "hi"}`, []string{"path", "content"}, false},
		{"multiple required one missing", `{"path": "/tmp"}`, []string{"path", "content"}, true},
		{"no required fields", `{"path": "/tmp"}`, []string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRequired(tt.input, tt.required)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRequired() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateParamType(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		paramName    string
		expectedType string
		wantErr      bool
	}{
		{"string type valid", `{"code": "print(1)"}`, "code", "string", false},
		{"string type invalid", `{"code": 123}`, "code", "string", true},
		{"number type valid float", `{"timeout": 10.0}`, "timeout", "number", false},
		{"number type valid int", `{"timeout": 10}`, "timeout", "number", false},
		{"number type invalid", `{"timeout": "abc"}`, "timeout", "number", true},
		{"boolean type valid", `{"ignore_case": true}`, "ignore_case", "boolean", false},
		{"boolean type invalid", `{"ignore_case": "yes"}`, "ignore_case", "boolean", true},
		{"array type valid", `{"items": [1,2]}`, "items", "array", false},
		{"array type invalid", `{"items": "not-array"}`, "items", "array", true},
		{"missing param is ok", `{"other": 1}`, "missing", "string", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateParamType(tt.input, tt.paramName, tt.expectedType)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateParamType() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateToolInput(t *testing.T) {
	info := fantasy.ToolInfo{
		Name: "lua",
		Parameters: map[string]any{
			"code": map[string]any{
				"type": "string",
			},
			"timeout": map[string]any{
				"type": "number",
			},
		},
		Required: []string{"code"},
	}

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid input", `{"code": "print(1)", "timeout": 10}`, false},
		{"missing required", `{"timeout": 10}`, true},
		{"wrong type", `{"code": 123, "timeout": 10}`, true},
		{"empty required", `{"code": ""}`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateToolInput(tt.input, info)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateToolInput() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResolveAndValidatePathSymlinks(t *testing.T) {
	tmpDir := t.TempDir()
	realDir := tmpDir + "/real"
	linkDir := tmpDir + "/link"
	outsideDir := tmpDir + "/outside"

	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveAndValidatePath("test.txt", linkDir)
	if err != nil {
		t.Errorf("expected valid path in symlinked workDir, got error: %v", err)
	}
	_ = resolved
}

func TestGuardReview(t *testing.T) {
	tests := []struct {
		name         string
		guardRules   []string
		reviewer     GuardReviewFn
		wantApproved bool
		wantErrStr   string
	}{
		{
			name:       "no guard rules allows tool call",
			guardRules: nil,
			reviewer: func(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
				return false, "blocked", nil
			},
			wantApproved: true,
		},
		{
			name:         "nil reviewer allows tool call",
			guardRules:   []string{"no sudo"},
			reviewer:     nil,
			wantApproved: true,
		},
		{
			name:       "reviewer approves tool call",
			guardRules: []string{"no sudo"},
			reviewer: func(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
				return true, "", nil
			},
			wantApproved: true,
		},
		{
			name:       "reviewer rejects tool call",
			guardRules: []string{"no sudo"},
			reviewer: func(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
				return false, "uses sudo", nil
			},
			wantApproved: false,
			wantErrStr:   "Guard rule violation",
		},
		{
			name:       "reviewer error fails open",
			guardRules: []string{"no sudo"},
			reviewer: func(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
				return false, "", context.DeadlineExceeded
			},
			wantApproved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := &coreTool{
				info: fantasy.ToolInfo{
					Name: "bash",
					Parameters: map[string]any{
						"command": map[string]any{
							"type": "string",
						},
					},
					Required: []string{"command"},
				},
				handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
					return fantasy.NewTextResponse("ok"), nil
				},
				guardReviewer: tt.reviewer,
			}

			ctx := context.Background()
			if tt.guardRules != nil {
				ctx = context.WithValue(ctx, GuardRulesKey, tt.guardRules)
			}
			// Allow bash tool for tests (bash is high-risk and requires explicit allow)
			ctx = context.WithValue(ctx, AgentToolsAllowedKey, []string{"bash", "view", "write"})

			resp, err := ct.Run(ctx, fantasy.ToolCall{
				ID:    "1",
				Name:  "bash",
				Input: `{"command": "ls"}`,
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if tt.wantApproved {
				if resp.IsError {
					t.Errorf("expected approval, got error: %s", resp.Content)
				}
			} else {
				if !resp.IsError {
					t.Errorf("expected rejection, got success: %s", resp.Content)
				}
				if tt.wantErrStr != "" && !strings.Contains(resp.Content, tt.wantErrStr) {
					t.Errorf("error content %q should contain %q", resp.Content, tt.wantErrStr)
				}
			}
		})
	}
}
