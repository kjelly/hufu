//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

const defaultGrepLimit = 100
const maxGrepArgumentBytes = 128 * 1024

var errGrepBackendUnavailable = errors.New("grep backend unavailable")

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Include    string `json:"include,omitempty"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	Literal    bool   `json:"literal_text,omitempty"`
	Context    int    `json:"context,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Glob       string `json:"glob,omitempty"`
}

func NewGrepTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "grep"
	return &coreTool{
		artifactPathPolicySafe: true,
		info: fantasy.ToolInfo{
			Name:        "grep",
			Description: "Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Hufu workspace execution records (session/journal files and logs under the workspace directory) are excluded unless the search path points inside the workspace. Output truncated to 100 matches or 50KB.",
			Parameters: map[string]any{
				"pattern":      map[string]any{"type": "string", "description": "The regex pattern to search for in file contents"},
				"path":         map[string]any{"type": "string", "description": "Directory or file to search in (default: current directory)"},
				"include":      map[string]any{"type": "string", "description": "File pattern to include (e.g. '*.go', '*.{ts,tsx}')"},
				"glob":         map[string]any{"type": "string", "description": "File pattern to include (alias for include)"},
				"ignore_case":  map[string]any{"type": "boolean", "description": "Case-insensitive search (default: false)"},
				"literal_text": map[string]any{"type": "boolean", "description": "Treat pattern as literal text instead of regex (default: false)"},
				"context":      map[string]any{"type": "number", "description": "Number of context lines before and after each match (default: 0)"},
				"limit":        map[string]any{"type": "number", "description": "Maximum number of matches to return (default: 100)"},
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
	if err := parseArgs(call.Input, &args); err != nil || args.Pattern == "" {
		return fantasy.NewTextErrorResponse("pattern parameter is required"), nil
	}

	limit := defaultGrepLimit
	if args.Limit > 0 {
		limit = args.Limit
	}
	effectiveCfg := cfgWithMergedPaths(cfg, ctx)
	searchPath, err := resolveSearchRoot(args.Path, workDir, "search", effectiveCfg)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}
	globPattern := args.Include
	if globPattern == "" {
		globPattern = args.Glob
	}
	wsName := workspaceDirName(cfg)
	excludeRecords := !pathHasComponent(searchPath, wsName)
	policy := effectiveCfg.ArtifactPathPolicy
	policyActive := policy != nil && len(policy.BlockedPaths) > 0

	// No policy and exact-file searches retain the backend's direct operand
	// semantics. resolveSearchRoot has already enforced an exact path.
	if !policyActive || !isDirectory(searchPath) {
		result, err := grepWithRg(ctx, args, searchPath, globPattern, limit, wsName, excludeRecords)
		if err == nil {
			return result, nil
		}
		return grepFallback(ctx, args, searchPath, limit, wsName, excludeRecords)
	}

	// Artifact authorization subtracts from rg's native directory selection;
	// it must not replace that selection with a filepath.Walk.
	candidates, rgAvailable, err := collectNativeRgCandidates(ctx, searchPath, globPattern, wsName, excludeRecords, policy)
	if err != nil && rgAvailable {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("grep failed: %v", err)), nil
	}
	if !rgAvailable {
		candidates, err = collectNativeFallbackCandidates(ctx, searchPath, wsName, excludeRecords, policy)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("grep failed: %v", err)), nil
		}
		return grepFallbackCandidates(ctx, args, searchPath, globPattern, limit, wsName, excludeRecords, candidates)
	}

	result, err := grepWithRgCandidates(ctx, args, searchPath, globPattern, limit, wsName, excludeRecords, candidates)
	if errors.Is(err, errGrepBackendUnavailable) {
		// GNU grep has different recursive, hidden, ignored, and include
		// semantics, so do not reuse the rg candidate set on fallback.
		candidates, err = collectNativeFallbackCandidates(ctx, searchPath, wsName, excludeRecords, policy)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("grep failed: %v", err)), nil
		}
		return grepFallbackCandidates(ctx, args, searchPath, globPattern, limit, wsName, excludeRecords, candidates)
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("grep failed: %v", err)), nil
	}
	return result, nil
}

func grepWithRg(ctx context.Context, args grepArgs, searchPath, globPattern string, limit int, wsName string, excludeRecords bool) (fantasy.ToolResponse, error) {
	return grepWithRgCandidates(ctx, args, searchPath, globPattern, limit, wsName, excludeRecords, nil)
}

func grepWithRgCandidates(ctx context.Context, args grepArgs, searchPath, globPattern string, limit int, wsName string, excludeRecords bool, candidates []string) (fantasy.ToolResponse, error) {
	if candidates != nil && len(candidates) == 0 {
		return fantasy.NewTextResponse("No matches found."), nil
	}
	baseArgs := []string{"--line-number", "--no-heading", "--color=never", "--max-count=" + strconv.Itoa(limit)}
	if candidates != nil {
		baseArgs = append(baseArgs, "--with-filename")
	}
	if args.IgnoreCase {
		baseArgs = append(baseArgs, "--ignore-case")
	}
	if args.Literal {
		baseArgs = append(baseArgs, "--fixed-strings")
	}
	if args.Context > 0 {
		baseArgs = append(baseArgs, fmt.Sprintf("--context=%d", args.Context))
	}
	if globPattern != "" {
		baseArgs = append(baseArgs, "--glob="+globPattern)
	}
	if excludeRecords {
		for _, g := range workspaceRecordRgGlobs(wsName) {
			baseArgs = append(baseArgs, "--glob="+g)
		}
	}

	operands := []string{searchPath}
	if candidates != nil {
		operands = candidates
	}
	var output strings.Builder
	for _, batch := range grepPathBatches(operands, grepFixedArgumentBytes(baseArgs, args.Pattern), maxGrepArgumentBytes) {
		rgArgs := append(append([]string(nil), baseArgs...), "-e", args.Pattern, "--")
		rgArgs = append(rgArgs, batch...)
		cmd := exec.CommandContext(ctx, "rg", rgArgs...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				if exitErr.ExitCode() == 1 {
					continue
				}
				if exitErr.ExitCode() == 2 {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("rg error: %s", stderr.String())), nil
				}
			}
			if errors.Is(err, exec.ErrNotFound) {
				return fantasy.ToolResponse{}, errGrepBackendUnavailable
			}
			return fantasy.ToolResponse{}, fmt.Errorf("running rg: %w", err)
		}
		output.Write(stdout.Bytes())
	}
	outputText := output.String()
	if outputText == "" {
		return fantasy.NewTextResponse("No matches found."), nil
	}
	lines := strings.Split(outputText, "\n")
	for i, line := range lines {
		lines[i] = truncateLine(line, grepMaxLineLen)
	}
	tr := truncateHead(strings.Join(lines, "\n"), limit, defaultMaxBytes)
	return fantasy.NewTextResponse(tr.Content + formatTruncationNotice(tr)), nil
}

func collectNativeRgCandidates(ctx context.Context, searchPath, globPattern, wsName string, excludeRecords bool, policy *ArtifactPathPolicy) ([]string, bool, error) {
	if _, err := exec.LookPath("rg"); err != nil {
		return nil, false, nil
	}
	rgArgs := []string{"--files", "--null"}
	if globPattern != "" {
		rgArgs = append(rgArgs, "--glob", globPattern)
	}
	if excludeRecords {
		for _, g := range workspaceRecordRgGlobs(wsName) {
			rgArgs = append(rgArgs, "--glob", g)
		}
	}
	rgArgs = append(rgArgs, searchPath)
	cmd := exec.CommandContext(ctx, "rg", rgArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return []string{}, true, nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("rg file discovery failed: %s", stderr.String())
	}
	return filterNativeCandidates(bytes.Split(stdout.Bytes(), []byte{0}), policy), true, nil
}

// GNU grep recursively considers hidden and ignored files, does not follow
// directory symlinks or symlink files discovered below a directory, and
// accepts explicit files. An explicit symlink-directory operand is the one
// exception: grep -r follows that root. This fallback enumeration preserves
// those native recursive candidates while subtracting artifacts.
func collectNativeFallbackCandidates(ctx context.Context, root, wsName string, excludeRecords bool, policy *ArtifactPathPolicy) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	walkRoot := root
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("stat fallback grep root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolve fallback grep root: %w", err)
		}
		resolvedInfo, err := os.Stat(resolvedRoot)
		if err != nil {
			return nil, fmt.Errorf("stat resolved fallback grep root: %w", err)
		}
		if !resolvedInfo.IsDir() {
			return nil, nil
		}
		walkRoot = resolvedRoot
	}

	canonicalRoot := canonicalPathForAuthorization(root)
	candidates := make([]string, 0)
	err = filepath.Walk(walkRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		candidatePath := path
		if walkRoot != root {
			rel, err := filepath.Rel(walkRoot, path)
			if err != nil {
				return err
			}
			candidatePath = filepath.Join(root, rel)
		}
		if info.IsDir() {
			canonical, allowed := artifactTraversalCandidate(candidatePath, canonicalRoot, policy)
			if !allowed || isArtifactPathBlocked(canonical, policy) {
				return filepath.SkipDir
			}
			return nil
		}
		// filepath.Walk reports nested symlinks without following them. GNU grep
		// -r likewise excludes those entries; only the explicit root symlink is
		// resolved above.
		if !info.Mode().IsRegular() {
			return nil
		}
		canonical, allowed := artifactTraversalCandidate(candidatePath, canonicalRoot, policy)
		if !allowed || isArtifactPathBlocked(canonical, policy) {
			return nil
		}
		if excludeRecords && isWorkspaceRecordPath(candidatePath, wsName) {
			return nil
		}
		candidates = append(candidates, candidatePath)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk fallback grep candidates: %w", err)
	}
	return candidates, nil
}

func filterNativeCandidates(raw [][]byte, policy *ArtifactPathPolicy) []string {
	candidates := make([]string, 0, len(raw))
	for _, entry := range raw {
		if len(entry) == 0 {
			continue
		}
		path := string(entry)
		if isArtifactPathBlocked(canonicalPathForAuthorization(path), policy) {
			continue
		}
		candidates = append(candidates, path)
	}
	return candidates
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func grepPathBatches(paths []string, fixedBytes, maxBytes int) [][]string {
	if len(paths) == 0 {
		return nil
	}
	batches := make([][]string, 0, (len(paths)+31)/32)
	batch := make([]string, 0)
	bytesUsed := fixedBytes
	for _, path := range paths {
		pathBytes := len(path) + 1
		if len(batch) > 0 && bytesUsed+pathBytes > maxBytes {
			batches = append(batches, batch)
			batch = nil
			bytesUsed = fixedBytes
		}
		batch = append(batch, path)
		bytesUsed += pathBytes
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

func grepFixedArgumentBytes(baseArgs []string, pattern string) int {
	fixed := 0
	for _, arg := range baseArgs {
		fixed += len(arg) + 1
	}
	for _, arg := range []string{"-e", pattern, "--"} {
		fixed += len(arg) + 1
	}
	return fixed
}

func grepFallback(ctx context.Context, args grepArgs, searchPath string, limit int, wsName string, excludeRecords bool) (fantasy.ToolResponse, error) {
	return grepFallbackCandidates(ctx, args, searchPath, "", limit, wsName, excludeRecords, nil)
}

func grepFallbackCandidates(ctx context.Context, args grepArgs, searchPath, globPattern string, limit int, wsName string, excludeRecords bool, candidates []string) (fantasy.ToolResponse, error) {
	if candidates != nil && len(candidates) == 0 {
		return fantasy.NewTextResponse("No matches found."), nil
	}
	baseArgs := []string{"-rn", "--color=never"}
	if candidates != nil {
		baseArgs = append(baseArgs, "-H")
	}
	if args.IgnoreCase {
		baseArgs = append(baseArgs, "-i")
	}
	if args.Literal {
		baseArgs = append(baseArgs, "-F")
	}
	if args.Context > 0 {
		baseArgs = append(baseArgs, fmt.Sprintf("-C%d", args.Context))
	}
	if globPattern == "" {
		globPattern = args.Include
		if globPattern == "" {
			globPattern = args.Glob
		}
	}
	if globPattern != "" {
		baseArgs = append(baseArgs, "--include="+globPattern)
	}
	operands := []string{searchPath}
	if candidates != nil {
		operands = candidates
	}
	var output strings.Builder
	for _, batch := range grepPathBatches(operands, grepFixedArgumentBytes(baseArgs, args.Pattern), maxGrepArgumentBytes) {
		grepArgs := append(append([]string(nil), baseArgs...), "-e", args.Pattern, "--")
		grepArgs = append(grepArgs, batch...)
		cmd := exec.CommandContext(ctx, "grep", grepArgs...)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				continue
			}
			return fantasy.NewTextErrorResponse(fmt.Sprintf("grep error: %v", err)), nil
		}
		output.Write(stdout.Bytes())
	}
	outputText := output.String()
	if excludeRecords {
		outputText = filterWorkspaceRecordLines(outputText, wsName)
	}
	if strings.TrimSpace(outputText) == "" {
		return fantasy.NewTextResponse("No matches found."), nil
	}
	tr := truncateHead(outputText, limit, defaultMaxBytes)
	return fantasy.NewTextResponse(tr.Content + formatTruncationNotice(tr)), nil
}
