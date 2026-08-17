//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"

	udiff "github.com/aymanbagabas/go-udiff"
)

type multiEditOperation struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type multiEditArgs struct {
	FilePath string               `json:"file_path"`
	Edits    []multiEditOperation `json:"edits"`
}

type failedEdit struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

func NewMultiEditTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "multiedit"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "multiedit",
			Description: "Apply multiple edit operations to a single file in one atomic write. Each edit finds and replaces text independently. Supports replace_all for each edit. Partial success: applied edits are kept, failed edits are reported.",
			Parameters: map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "The path to the file to edit (relative or absolute)",
				},
				"edits": map[string]any{
					"type":        "array",
					"description": "Array of edit operations to apply sequentially",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"old_string": map[string]any{
								"type":        "string",
								"description": "The text to replace",
							},
							"new_string": map[string]any{
								"type":        "string",
								"description": "The text to replace it with",
							},
							"replace_all": map[string]any{
								"type":        "boolean",
								"description": "Replace all occurrences of old_string (default false)",
							},
						},
						"required": []string{"old_string", "new_string"},
					},
				},
			},
			Required: []string{"file_path", "edits"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeMultiEdit(ctx, call, cfg)
		},
	}
}

func executeMultiEdit(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
	if response, denied := ReadOnlyMutationDenied(ctx, "multiedit"); denied {
		return response, nil
	}
	var args multiEditArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("failed to parse arguments: " + err.Error()), nil
	}
	if args.FilePath == "" {
		return fantasy.NewTextErrorResponse("file_path parameter is required"), nil
	}
	if len(args.Edits) == 0 {
		return fantasy.NewTextErrorResponse("edits parameter must not be empty"), nil
	}

	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cancelled: %v", err)), nil
	}

	absPath, err := resolveAndValidateWritePathWithConsent(args.FilePath, cfgWithMergedPaths(cfg, ctx))
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}

	isCreate := args.Edits[0].OldString == "" && args.Edits[0].NewString != ""

	if isCreate {
		return processMultiEditWithCreation(absPath, args)
	}
	return processMultiEditExistingFile(absPath, args)
}

func processMultiEditWithCreation(absPath string, args multiEditArgs) (fantasy.ToolResponse, error) {
	if _, err := os.Stat(absPath); err == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file %s already exists", args.FilePath)), nil
	}

	content := args.Edits[0].NewString
	var failed []failedEdit
	appliedCount := 1

	for i, edit := range args.Edits[1:] {
		idx := i + 1
		newContent, err := applyMultiEditToContent(content, edit)
		if err != nil {
			failed = append(failed, failedEdit{Index: idx, Error: err.Error()})
			continue
		}
		content = newContent
		appliedCount++
	}

	if err := os.MkdirAll(getDir(absPath), 0o755); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to create directories: %v", err)), nil
	}

	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write file: %v", err)), nil
	}

	return buildMultiEditResult(args.FilePath, "", content, appliedCount, failed), nil
}

func processMultiEditExistingFile(absPath string, args multiEditArgs) (fantasy.ToolResponse, error) {
	contentBytes, err := os.ReadFile(absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read file: %v", err)), nil
	}

	originalContent := string(contentBytes)
	content := strings.ReplaceAll(originalContent, "\r\n", "\n")

	var failed []failedEdit
	appliedCount := 0

	for i, edit := range args.Edits {
		newContent, err := applyMultiEditToContent(content, edit)
		if err != nil {
			failed = append(failed, failedEdit{Index: i, Error: err.Error()})
			continue
		}
		content = newContent
		appliedCount++
	}

	if appliedCount == 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("all %d edits failed", len(args.Edits))), nil
	}

	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write file: %v", err)), nil
	}

	return buildMultiEditResult(args.FilePath, originalContent, content, appliedCount, failed), nil
}

func applyMultiEditToContent(content string, edit multiEditOperation) (string, error) {
	if edit.OldString == "" && edit.NewString == "" {
		return content, nil
	}
	if edit.OldString == "" {
		return "", fmt.Errorf("old_string cannot be empty for content replacement")
	}

	oldNormalized := strings.ReplaceAll(edit.OldString, "\r\n", "\n")
	newNormalized := strings.ReplaceAll(edit.NewString, "\r\n", "\n")

	if edit.ReplaceAll {
		count := strings.Count(content, oldNormalized)
		if count == 0 {
			return "", fmt.Errorf("old_string not found in file")
		}
		return strings.ReplaceAll(content, oldNormalized, newNormalized), nil
	}

	count := strings.Count(content, oldNormalized)
	if count == 0 {
		idx, matchLen := fuzzyMatch(content, oldNormalized)
		if idx < 0 {
			return "", fmt.Errorf("old_string not found in file")
		}
		matchedText := content[idx : idx+matchLen]
		return strings.Replace(content, matchedText, newNormalized, 1), nil
	}
	if count > 1 {
		return "", fmt.Errorf("old_string appears %d times; use replace_all to replace all occurrences or provide more context", count)
	}

	return strings.Replace(content, oldNormalized, newNormalized, 1), nil
}

func buildMultiEditResult(filePath, oldContent, newContent string, appliedCount int, failed []failedEdit) fantasy.ToolResponse {
	var b strings.Builder

	if oldContent != "" {
		normalizedOld := strings.ReplaceAll(oldContent, "\r\n", "\n")
		diff := udiff.Unified(filePath, filePath, normalizedOld, newContent)
		fmt.Fprintf(&b, "Applied %d edit(s) to %s\n", appliedCount, filePath)
		b.WriteString(diff)
	} else {
		fmt.Fprintf(&b, "Created %s with %d edit(s)\n", filePath, appliedCount)
	}

	if len(failed) > 0 {
		fmt.Fprintf(&b, "\n%d edit(s) failed:\n", len(failed))
		for _, f := range failed {
			fmt.Fprintf(&b, "  - edits[%d]: %s\n", f.Index, f.Error)
		}
	}

	return fantasy.NewTextResponse(b.String())
}

func getDir(path string) string {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return "."
	}
	return path[:i]
}
