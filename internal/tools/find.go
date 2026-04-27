package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

type findArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func NewFindTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "find",
			Description: "Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output truncated to 1000 results or 50KB.",
			Parameters: map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Glob pattern to match files, e.g. '*.ts', '**/*.json'",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory to search in (default: current directory)",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": "Maximum number of results (default: 1000)",
				},
			},
			Required: []string{"pattern"},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeFind(ctx, call, cfg.WorkDir)
		},
	}
}

func executeFind(ctx context.Context, call fantasy.ToolCall, workDir string) (fantasy.ToolResponse, error) {
	var args findArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("pattern parameter is required"), nil
	}
	if args.Pattern == "" {
		return fantasy.NewTextErrorResponse("pattern parameter is required"), nil
	}

	limit := 1000
	if args.Limit > 0 {
		limit = args.Limit
	}

	searchPath := "."
	if args.Path != "" {
		resolved, err := resolvePathWithWorkDir(args.Path, workDir)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
		}
		searchPath = resolved
	} else if workDir != "" {
		searchPath = workDir
	}

	result, err := findWithFd(ctx, args.Pattern, searchPath, limit)
	if err == nil {
		return result, nil
	}

	return findWithCmd(ctx, args.Pattern, searchPath, limit)
}

func findWithFd(ctx context.Context, pattern, searchPath string, limit int) (fantasy.ToolResponse, error) {
	fdArgs := []string{
		"--glob", pattern,
		"--hidden",
		"--max-results", strconv.Itoa(limit),
		".",
	}

	cmd := exec.CommandContext(ctx, "fd", fdArgs...)
	cmd.Dir = searchPath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("fd failed: %w: %s", err, stderr.String())
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return fantasy.NewTextResponse("No files found."), nil
	}

	tr := truncateHead(output, limit, defaultMaxBytes)
	return fantasy.NewTextResponse(tr.Content), nil
}

func findWithCmd(ctx context.Context, pattern, searchPath string, limit int) (fantasy.ToolResponse, error) {
	findArgs := []string{searchPath, "-name", pattern, "-type", "f"}

	cmd := exec.CommandContext(ctx, "find", findArgs...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() != 0 {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("find command failed: %v", err)), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("find command failed: %v", err)), nil
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return fantasy.NewTextResponse("No files found."), nil
	}

	lines := strings.Split(output, "\n")
	if len(lines) > limit {
		lines = lines[:limit]
		output = strings.Join(lines, "\n")
		output += fmt.Sprintf("\n[truncated: showing %d of more results]", limit)
	}

	tr := truncateHead(output, limit, defaultMaxBytes)
	return fantasy.NewTextResponse(tr.Content), nil
}
