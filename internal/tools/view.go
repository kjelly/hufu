//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
)

const maxViewSize = 100 * 1024
const defaultViewLimit = 2000
const maxLineLength = 2000

type viewArgs struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

func NewViewTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "view"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "view",
			Description: "Read the contents of a file. Output includes line numbers and is wrapped in file path tags. Supports relative, absolute, ~, $HOME, and ${HOME} paths. Supports offset/limit for large files. Truncates lines longer than 2000 chars.",
			Parameters: map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "The path to the file to read (relative or absolute)",
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
			Required: []string{"file_path"},
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
	if args.FilePath == "" {
		return fantasy.NewTextErrorResponse("file_path parameter is required"), nil
	}

	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cancelled: %v", err)), nil
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

func readTextFile(f *os.File, offset, limit int) (string, int, bool, error) {
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
