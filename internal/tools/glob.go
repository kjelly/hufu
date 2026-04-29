package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/fantasy"
)

const defaultGlobLimit = 100

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

func NewGlobTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "glob",
			Description: "Search for files by glob pattern. Returns matching file paths. Uses ripgrep (rg) if available, falls back to Go implementation. Respects .gitignore. Limited to 100 results.",
			Parameters: map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "The glob pattern to match files against (e.g. '*.ts', '**/*.json', 'src/**/*.go')",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "The directory to search in (default: current directory)",
				},
			},
			Required: []string{"pattern"},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeGlob(ctx, call, cfg.WorkDir)
		},
	}
}

func executeGlob(ctx context.Context, call fantasy.ToolCall, workDir string) (fantasy.ToolResponse, error) {
	var args globArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("pattern parameter is required"), nil
	}
	if args.Pattern == "" {
		return fantasy.NewTextErrorResponse("pattern parameter is required"), nil
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

	paths, truncated, err := globFiles(ctx, args.Pattern, searchPath, defaultGlobLimit)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("glob failed: %v", err)), nil
	}

	if len(paths) == 0 {
		return fantasy.NewTextResponse("No files found."), nil
	}

	for i, p := range paths {
		rel, err := filepath.Rel(searchPath, p)
		if err == nil {
			paths[i] = filepath.ToSlash(rel)
		} else {
			paths[i] = filepath.ToSlash(p)
		}
	}

	sort.Slice(paths, func(i, j int) bool {
		return len(paths[i]) < len(paths[j])
	})

	var b strings.Builder
	for _, p := range paths {
		b.WriteString(p + "\n")
	}

	if truncated {
		b.WriteString("\n(Results are truncated. Use a more specific pattern to see more results.)")
	}

	return fantasy.NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
}

func globFiles(ctx context.Context, pattern, searchPath string, limit int) ([]string, bool, error) {
	paths, err := globWithRg(ctx, pattern, searchPath, limit)
	if err == nil && len(paths) > 0 {
		return paths, len(paths) >= limit, nil
	}

	return globWithWalk(pattern, searchPath, limit)
}

func globWithRg(ctx context.Context, pattern, searchPath string, limit int) ([]string, error) {
	rgArgs := []string{
		"--files",
		"-L",
		"--glob", pattern,
		"--max-depth", "100",
	}

	cmd := exec.CommandContext(ctx, "rg", rgArgs...)
	cmd.Dir = searchPath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("rg failed: %w: %s", err, stderr.String())
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return nil, nil
	}

	var paths []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		paths = append(paths, filepath.Join(searchPath, line))
		if len(paths) >= limit {
			break
		}
	}

	return paths, nil
}

func globWithWalk(pattern, searchPath string, limit int) ([]string, bool, error) {
	var paths []string
	truncated := false

	matcher := buildGlobMatcher(pattern)

	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(searchPath, path)
		if err != nil {
			return nil
		}
		if matcher(rel) {
			paths = append(paths, path)
			if len(paths) >= limit {
				truncated = true
				return fmt.Errorf("limit reached")
			}
		}
		return nil
	}

	filepath.Walk(searchPath, walkFn)

	return paths, truncated, nil
}

func buildGlobMatcher(pattern string) func(string) bool {
	base := filepath.Base(pattern)
	hasDirPrefix := strings.Contains(pattern, "/") || strings.Contains(pattern, "\\")

	return func(relPath string) bool {
		if hasDirPrefix {
			matched, _ := filepath.Match(pattern, relPath)
			return matched
		}
		matched, _ := filepath.Match(base, filepath.Base(relPath))
		return matched
	}
}
