package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
)

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func NewReadTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "read",
			Description: "Read the contents of a file. Output is truncated to 2000 lines or 50KB. Use offset/limit for large files.",
			Parameters: map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to read (relative or absolute)",
				},
				"offset": map[string]any{
					"type":        "number",
					"description": "Line number to start reading from (1-indexed)",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": "Maximum number of lines to read",
				},
			},
			Required: []string{"path"},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeRead(ctx, call, cfg.WorkDir)
		},
	}
}

func executeRead(ctx context.Context, call fantasy.ToolCall, workDir string) (fantasy.ToolResponse, error) {
	var args readArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("path parameter is required"), nil
	}
	if args.Path == "" {
		return fantasy.NewTextErrorResponse("path parameter is required"), nil
	}

	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cancelled: %v", err)), nil
	}

	absPath, err := resolvePathWithWorkDir(args.Path, workDir)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot access '%s': %v", args.Path, err)), nil
	}
	if info.IsDir() {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("'%s' is a directory, not a file. Use the ls tool to list directory contents.", args.Path)), nil
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read file: %v", err)), nil
	}

	lines := strings.Split(string(content), "\n")
	totalLines := len(lines)

	offset := 0
	if args.Offset > 0 {
		offset = args.Offset - 1
		if offset >= totalLines {
			return fantasy.NewTextResponse(fmt.Sprintf("offset %d exceeds file length (%d lines)", args.Offset, totalLines)), nil
		}
		lines = lines[offset:]
	}

	maxLines := defaultMaxLines
	if args.Limit > 0 {
		maxLines = args.Limit
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}

	var result strings.Builder
	for i, line := range lines {
		lineNum := offset + i + 1
		fmt.Fprintf(&result, "%d: %s\n", lineNum, line)
	}

	output := result.String()
	tr := truncateHead(output, 0, defaultMaxBytes)

	if len(lines) < totalLines-offset {
		tr.Content += fmt.Sprintf("\n[showing lines %d-%d of %d total. Use offset=%d to continue reading]",
			offset+1, offset+len(lines), totalLines, offset+len(lines)+1)
	}

	return fantasy.NewTextResponse(tr.Content), nil
}
