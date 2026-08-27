//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
)

// maxViewSize bounds a single artifact/file read while allowing the review
// runtime's bounded diff partitions to be consumed as one complete evidence
// object. Callers can still use offset/limit for larger files.
const maxViewSize = 512 * 1024
const defaultViewLimit = 2000
const maxLineLength = 2000

type viewArgs struct {
	FilePath    string `json:"file_path,omitempty"`
	ArtifactRef string `json:"artifact_ref,omitempty"`
	Offset      int    `json:"offset,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

func NewViewTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "view"
	return &coreTool{
		artifactPathPolicySafe: true,
		info: fantasy.ToolInfo{
			Name:        "view",
			Description: "Read a file by either a filesystem file_path or a runtime-issued opaque artifact_ref. Use artifact_ref for worker/task outputs; never copy their display path or opaque ID into file_path, including under runtime/artifacts. Artifact references are resolved and authorized by hufu without path consent. Exactly one source is required.",
			Parameters: map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Filesystem path to the file to read (relative or absolute). Do not put an opaque artifact ID here; use artifact_ref instead.",
				},
				"artifact_ref": map[string]any{
					"type":        "string",
					"description": "Opaque runtime-issued artifact reference. Pass the ID unchanged; do not alter it, replace it with a displayed path, or turn it into runtime/artifacts/<id>.",
				},
				"offset": map[string]any{
					"type":        "number",
					"description": "The line number to start reading from (0-based, default 0)",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": "The number of lines to read (default 2000)",
				},
			},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeView(ctx, call, cfg.WorkDir, cfg)
		},
	}
}

func executeView(ctx context.Context, call fantasy.ToolCall, workDir string, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var args viewArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if (args.FilePath == "") == (args.ArtifactRef == "") {
		return fantasy.NewTextErrorResponse("exactly one of file_path or artifact_ref is required"), nil
	}

	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cancelled: %v", err)), nil
	}

	if args.ArtifactRef != "" {
		return executeViewArtifact(ctx, args, cfg)
	}

	absPath, err := checkPathOrConsent(args.FilePath, workDir, "read", cfgWithMergedPaths(cfg, ctx))
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot access '%s': %v", args.FilePath, err)), nil
	}
	if info.IsDir() {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("'%s' is a directory, not a file. Use the ls tool to list directory contents.", args.FilePath)), nil
	}

	if info.Size() > maxViewSize {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file '%s' is too large (%d bytes, max %d). Use offset/limit to read portions.", args.FilePath, info.Size(), maxViewSize)), nil
	}

	f, err := os.Open(absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to open file: %v", err)), nil
	}
	defer f.Close()

	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultViewLimit
	}

	content, totalLines, hasMore, err := readTextFile(f, offset, limit)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read file: %v", err)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<%s>\n", args.FilePath)
	b.WriteString(content)
	fmt.Fprintf(&b, "</%s>\n", args.FilePath)

	if hasMore {
		fmt.Fprintf(&b, "\n[showing lines %d-%d of %d total. Use offset=%d to continue reading]",
			offset, offset+limit-1, totalLines, offset+limit)
	}

	return fantasy.NewTextResponse(b.String()), nil
}

func executeViewArtifact(ctx context.Context, args viewArgs, cfg ToolConfig) (fantasy.ToolResponse, error) {
	if cfg.ArtifactOpener == nil {
		return fantasy.NewTextErrorResponse("invalid artifact_ref: opaque artifact access is unavailable"), nil
	}
	reader, err := cfg.ArtifactOpener(ctx, args.ArtifactRef)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid artifact_ref: %v", err)), nil
	}
	defer reader.Close()

	data, err := io.ReadAll(io.LimitReader(reader, maxViewSize+1))
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read artifact: %v", err)), nil
	}
	if len(data) > maxViewSize {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("artifact is too large (%d+ bytes, max %d)", maxViewSize, maxViewSize)), nil
	}

	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultViewLimit
	}
	content, totalLines, hasMore, err := readTextFile(bytes.NewReader(data), offset, limit)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read artifact: %v", err)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<artifact:%s>\n", args.ArtifactRef)
	b.WriteString(content)
	fmt.Fprintf(&b, "</artifact:%s>\n", args.ArtifactRef)
	if hasMore {
		fmt.Fprintf(&b, "\n[showing lines %d-%d of %d total. Use offset=%d to continue reading]", offset, offset+limit-1, totalLines, offset+limit)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

type lineScanner struct {
	scanner *bufio.Scanner
}

func newLineScanner(r io.Reader) *lineScanner {
	s := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	s.Buffer(buf, 1024*1024)
	return &lineScanner{scanner: s}
}

func (s *lineScanner) scan() bool   { return s.scanner.Scan() }
func (s *lineScanner) text() string { return s.scanner.Text() }
func (s *lineScanner) err() error   { return s.scanner.Err() }

func readTextFile(f io.Reader, offset, limit int) (string, int, bool, error) {
	scanner := newLineScanner(f)
	lineNum := 0
	var lines []string
	totalLines := 0

	for scanner.scan() {
		if lineNum < offset {
			lineNum++
			continue
		}
		if len(lines) >= limit {
			for scanner.scan() {
				totalLines++
			}
			totalLines++
			hasMore := totalLines > offset+limit
			return formatLines(lines, offset), totalLines, hasMore, nil
		}
		line := scanner.text()
		if utf8.RuneCountInString(line) > maxLineLength {
			line = safeTruncateString(line, maxLineLength) + "..."
		}
		lines = append(lines, line)
		lineNum++
		totalLines++
	}

	if err := scanner.err(); err != nil {
		return "", 0, false, err
	}

	hasMore := false
	return formatLines(lines, offset), totalLines, hasMore, nil
}

func formatLines(lines []string, offset int) string {
	var b strings.Builder
	maxLineNumWidth := len(fmt.Sprintf("%d", offset+len(lines)))
	for i, line := range lines {
		lineNum := offset + i + 1
		fmt.Fprintf(&b, "%*d %s\n", maxLineNumWidth, lineNum, line)
	}
	return b.String()
}
