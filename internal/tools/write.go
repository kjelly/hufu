package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"

	udiff "github.com/aymanbagabas/go-udiff"
)

type writeArgs struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`

	Path string `json:"path"`
}

func NewWriteTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "write"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "write",
			Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories. Returns a diff if the file already exists.",
			Parameters: map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "The path to the file to write (relative or absolute)",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The content to write to the file",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file (deprecated: use file_path instead)",
				},
			},
			Required: []string{"file_path", "content"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeWrite(ctx, call, cfg)
		},
	}
}

func executeWrite(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var args writeArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("file_path and content parameters are required"), nil
	}

	filePath := args.FilePath
	if filePath == "" {
		filePath = args.Path
	}
	if filePath == "" {
		return fantasy.NewTextErrorResponse("file_path parameter is required"), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content parameter is required"), nil
	}

	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cancelled: %v", err)), nil
	}

	absPath, err := resolveAndValidatePathWithConsent(filePath, cfg)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}

	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to create directories: %v", err)), nil
	}

	existingContent, existingErr := os.ReadFile(absPath)
	if existingErr == nil {
		if string(existingContent) == args.Content {
			return fantasy.NewTextResponse(fmt.Sprintf("File %s already contains the exact content (no changes)", filePath)), nil
		}

		normalizedOld := strings.ReplaceAll(string(existingContent), "\r\n", "\n")
		normalizedNew := strings.ReplaceAll(args.Content, "\r\n", "\n")
		diff := udiff.Unified(absPath, absPath, normalizedOld, normalizedNew)

		if err := os.WriteFile(absPath, []byte(args.Content), 0o644); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write file: %v", err)), nil
		}

		return fantasy.NewTextResponse(fmt.Sprintf("Wrote %d bytes to %s\n%s", len(args.Content), filePath, diff)), nil
	}

	if err := os.WriteFile(absPath, []byte(args.Content), 0o644); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write file: %v", err)), nil
	}

	return fantasy.NewTextResponse(fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), filePath)), nil
}
