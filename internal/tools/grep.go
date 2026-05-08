//go:build linux || darwin
// +build linux darwin

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

const defaultGrepLimit = 100

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Include    string `json:"include,omitempty"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	Literal    bool   `json:"literal_text,omitempty"`
	Context    int    `json:"context,omitempty"`
	Limit      int    `json:"limit,omitempty"`

	Glob string `json:"glob,omitempty"`
}

func NewGrepTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "grep"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "grep",
			Description: "Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Output truncated to 100 matches or 50KB.",
			Parameters: map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "The regex pattern to search for in file contents",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory or file to search in (default: current directory)",
				},
				"include": map[string]any{
					"type":        "string",
					"description": "File pattern to include (e.g. '*.go', '*.{ts,tsx}')",
				},
				"glob": map[string]any{
					"type":        "string",
					"description": "File pattern to include (alias for include)",
				},
				"ignore_case": map[string]any{
					"type":        "boolean",
					"description": "Case-insensitive search (default: false)",
				},
				"literal_text": map[string]any{
					"type":        "boolean",
					"description": "Treat pattern as literal text instead of regex (default: false)",
				},
				"context": map[string]any{
					"type":        "number",
					"description": "Number of context lines before and after each match (default: 0)",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": "Maximum number of matches to return (default: 100)",
				},
			},
			Required: []string{"pattern"},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeGrep(ctx, call, cfg.WorkDir, cfg)
		},
	}
}

func executeGrep(ctx context.Context, call fantasy.ToolCall, workDir string, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var args grepArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("pattern parameter is required"), nil
	}
	if args.Pattern == "" {
		return fantasy.NewTextErrorResponse("pattern parameter is required"), nil
	}

	limit := defaultGrepLimit
	if args.Limit > 0 {
		limit = args.Limit
	}

	searchPath := "."
	if args.Path != "" {
		resolved, err := checkPathOrConsent(args.Path, workDir, "search", cfgWithMergedPaths(cfg, ctx))
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
		}
		searchPath = resolved
	} else if workDir != "" {
		searchPath = workDir
	}

	globPattern := args.Include
	if globPattern == "" {
		globPattern = args.Glob
	}

	result, err := grepWithRg(ctx, args, searchPath, globPattern, limit)
	if err == nil {
		return result, nil
	}

	return grepFallback(ctx, args, searchPath, limit)
}

func grepWithRg(ctx context.Context, args grepArgs, searchPath, globPattern string, limit int) (fantasy.ToolResponse, error) {
	rgArgs := []string{
		"--line-number",
		"--no-heading",
		"--color=never",
		"--max-count=" + strconv.Itoa(limit),
	}

	if args.IgnoreCase {
		rgArgs = append(rgArgs, "--ignore-case")
	}
	if args.Literal {
		rgArgs = append(rgArgs, "--fixed-strings")
	}
	if args.Context > 0 {
		rgArgs = append(rgArgs, fmt.Sprintf("--context=%d", args.Context))
	}
	if globPattern != "" {
		rgArgs = append(rgArgs, "--glob="+globPattern)
	}

	rgArgs = append(rgArgs, args.Pattern, searchPath)

	cmd := exec.CommandContext(ctx, "rg", rgArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 1 {
				return fantasy.NewTextResponse("No matches found."), nil
			}
			if exitErr.ExitCode() == 2 {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("rg error: %s", stderr.String())), nil
			}
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("rg failed: %v", err)), nil
	}

	output := stdout.String()
	if output == "" {
		return fantasy.NewTextResponse("No matches found."), nil
	}

	lines := strings.Split(output, "\n")
	for i, line := range lines {
		lines[i] = truncateLine(line, grepMaxLineLen)
	}
	output = strings.Join(lines, "\n")

	tr := truncateHead(output, limit, defaultMaxBytes)
	return fantasy.NewTextResponse(tr.Content), nil
}

func grepFallback(ctx context.Context, args grepArgs, searchPath string, limit int) (fantasy.ToolResponse, error) {
	grepArgs := []string{"-rn", "--color=never"}

	if args.IgnoreCase {
		grepArgs = append(grepArgs, "-i")
	}
	if args.Literal {
		grepArgs = append(grepArgs, "-F")
	}
	if args.Context > 0 {
		grepArgs = append(grepArgs, fmt.Sprintf("-C%d", args.Context))
	}

	globPattern := args.Include
	if globPattern == "" {
		globPattern = args.Glob
	}
	if globPattern != "" {
		grepArgs = append(grepArgs, "--include="+globPattern)
	}

	grepArgs = append(grepArgs, args.Pattern, searchPath)

	cmd := exec.CommandContext(ctx, "grep", grepArgs...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return fantasy.NewTextResponse("No matches found."), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("grep error: %v", err)), nil
	}

	output := stdout.String()
	if output == "" {
		return fantasy.NewTextResponse("No matches found."), nil
	}

	tr := truncateHead(output, limit, defaultMaxBytes)
	return fantasy.NewTextResponse(tr.Content), nil
}
