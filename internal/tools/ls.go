//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/fantasy"
)

const maxLSFiles = 1000

type lsArgs struct {
	Path   string   `json:"path,omitempty"`
	Ignore []string `json:"ignore,omitempty"`
	Depth  int      `json:"depth,omitempty"`
}

func NewLsTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "ls"
	return &coreTool{
		artifactPathPolicySafe: true,
		info: fantasy.ToolInfo{
			Name:        "ls",
			Description: "List filesystem directory contents as an indented tree. Shows file and directory names with proper nesting. Includes dotfiles. This tool cannot resolve opaque artifact IDs; use view with artifact_ref to read a worker/task output. Limited to 1000 entries.",
			Parameters: map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Filesystem directory path to list (default: current directory). This does not resolve opaque artifact IDs.",
				},
				"ignore": map[string]any{
					"type":        "array",
					"description": "List of glob patterns to ignore (e.g. ['node_modules', '*.log'])",
					"items": map[string]any{
						"type": "string",
					},
				},
				"depth": map[string]any{
					"type":        "number",
					"description": "Maximum depth to traverse (default: unlimited)",
				},
			},
			Required: []string{},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeLs(ctx, call, cfg.WorkDir, cfg)
		},
	}
}

func executeLs(ctx context.Context, call fantasy.ToolCall, workDir string, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var args lsArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cancelled: %v", err)), nil
	}

	effectiveCfg := cfgWithMergedPaths(cfg, ctx)
	dirPath, err := resolveSearchRoot(args.Path, workDir, "list", effectiveCfg)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}

	info, err := os.Stat(dirPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot access '%s': %v", args.Path, err)), nil
	}
	if !info.IsDir() {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("'%s' is not a directory", args.Path)), nil
	}

	ignorePatterns := args.Ignore
	if len(ignorePatterns) == 0 {
		ignorePatterns = []string{}
	}

	maxDepth := args.Depth
	if maxDepth <= 0 {
		maxDepth = -1
	}

	entries, truncated, err := listDirectoryTreeWithPolicy(dirPath, ignorePatterns, maxDepth, maxLSFiles, effectiveCfg.ArtifactPathPolicy)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to list directory: %v", err)), nil
	}

	if len(entries) == 0 {
		return fantasy.NewTextResponse("(empty directory)"), nil
	}

	output := formatTree(dirPath, entries)
	if truncated {
		output += fmt.Sprintf("\n\n[truncated: showing %d of more entries]", maxLSFiles)
	}

	return fantasy.NewTextResponse(output), nil
}

type fileEntry struct {
	relPath string
	name    string
	isDir   bool
	depth   int
}

func listDirectoryTreeWithPolicy(root string, ignore []string, maxDepth, maxFiles int, policy *ArtifactPathPolicy) ([]fileEntry, bool, error) {
	ignoreMap := make(map[string]bool)
	for _, p := range ignore {
		ignoreMap[p] = true
	}

	var entries []fileEntry
	truncated := false

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		if _, allowed := artifactTraversalCandidate(path, root, policy); !allowed {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		parts := strings.Split(filepath.ToSlash(rel), "/")
		for _, part := range parts {
			if ignoreMap[part] {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		depth := len(parts) - 1
		if maxDepth >= 0 && depth > maxDepth {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if len(entries) >= maxFiles {
			truncated = true
			return fmt.Errorf("limit reached")
		}

		entries = append(entries, fileEntry{
			relPath: rel,
			name:    info.Name(),
			isDir:   info.IsDir(),
			depth:   depth,
		})

		return nil
	})

	if err != nil && truncated {
		err = nil
	}

	return entries, truncated, err
}

func formatTree(root string, entries []fileEntry) string {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relPath < entries[j].relPath
	})

	var b strings.Builder
	b.WriteString("- " + filepath.Base(root) + "/\n")

	for _, e := range entries {
		indent := strings.Repeat("  ", e.depth+1)
		name := e.name
		if e.isDir {
			name += "/"
		}
		fmt.Fprintf(&b, "%s- %s\n", indent, name)
	}

	return strings.TrimRight(b.String(), "\n")
}
