//go:build linux || darwin
// +build linux darwin

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
		workspaceScope:         workspaceReadWriteEnforcedScope(),
		artifactPathPolicySafe: true,
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
	if response, denied := ReadOnlyMutationDenied(ctx, "write"); denied {
		return response, nil
	}
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

	effectiveCfg := cfgWithMergedPaths(cfg, ctx)
	scoped, err := newScopedFileAccess(effectiveCfg, filePath, true)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}
	if scoped != nil {
		defer scoped.close()
		return executeScopedWrite(ctx, scoped, filePath, args.Content)
	}
	absPath, err := resolveAndValidateWritePathWithConsent(filePath, effectiveCfg)
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

func executeScopedWrite(ctx context.Context, access *scopedFileAccess, displayPath, content string) (fantasy.ToolResponse, error) {
	info, err := access.destinationInfo()
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to inspect file: %v", err)), nil
	}
	var existing []byte
	if info != nil && info.Mode().IsRegular() {
		existing, _, err = access.readRegular()
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read file: %v", err)), nil
		}
		if string(existing) == content {
			return fantasy.NewTextResponse(fmt.Sprintf("File %s already contains the exact content (no changes)", displayPath)), nil
		}
	}
	if err := access.writeAtomic(ctx, []byte(content), info, 0o644); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write file: %v", err)), nil
	}
	if info == nil || !info.Mode().IsRegular() {
		return fantasy.NewTextResponse(fmt.Sprintf("Wrote %d bytes to %s", len(content), displayPath)), nil
	}
	normalizedOld := strings.ReplaceAll(string(existing), "\r\n", "\n")
	normalizedNew := strings.ReplaceAll(content, "\r\n", "\n")
	diff := udiff.Unified(access.absolute, access.absolute, normalizedOld, normalizedNew)
	return fantasy.NewTextResponse(fmt.Sprintf("Wrote %d bytes to %s\n%s", len(content), displayPath, diff)), nil
}
