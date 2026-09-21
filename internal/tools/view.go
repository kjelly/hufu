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
const maxBatchViewItems = 16

// maxBatchViewOutputSize keeps a batch within approximately the same context
// footprint as the largest supported single-file view, with room for line
// numbers and per-item framing.
const maxBatchViewOutputSize = maxViewSize + 64*1024

type viewItemArgs struct {
	FilePath    string `json:"file_path,omitempty"`
	ArtifactRef string `json:"artifact_ref,omitempty"`
	Offset      int    `json:"offset,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

type viewArgs struct {
	FilePath    string         `json:"file_path,omitempty"`
	ArtifactRef string         `json:"artifact_ref,omitempty"`
	Offset      int            `json:"offset,omitempty"`
	Limit       int            `json:"limit,omitempty"`
	Items       []viewItemArgs `json:"items,omitempty"`
}

func NewViewTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "view"
	return &coreTool{
		workspaceScope:         workspaceReadEnforcedScope(),
		artifactPathPolicySafe: true,
		info: fantasy.ToolInfo{
			Name:        "view",
			Description: "Read one or more files or artifacts. For a single read, provide exactly one filesystem file_path or runtime-issued opaque artifact_ref. For a bounded batch, provide items instead; each item has the same source, offset, and limit fields and is independently authorized. Use artifact_ref for worker/task outputs; never copy their display path or opaque ID into file_path, including under runtime/artifacts.",
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
				"items": map[string]any{
					"type":        "array",
					"description": "A batch of 1 to 16 independently authorized reads. Do not combine items with top-level source fields.",
					"minItems":    1,
					"maxItems":    maxBatchViewItems,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"file_path": map[string]any{
								"type":        "string",
								"description": "Filesystem path to read. Exactly one of file_path or artifact_ref is required per item.",
							},
							"artifact_ref": map[string]any{
								"type":        "string",
								"description": "Opaque runtime-issued artifact reference. Pass the ID unchanged.",
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
					},
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

	if args.Items != nil {
		if args.FilePath != "" || args.ArtifactRef != "" || args.Offset != 0 || args.Limit != 0 {
			return fantasy.NewTextErrorResponse("items cannot be combined with top-level file_path, artifact_ref, offset, or limit"), nil
		}
		if len(args.Items) == 0 {
			return fantasy.NewTextErrorResponse("items must contain at least one read"), nil
		}
		if len(args.Items) > maxBatchViewItems {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("items contains %d reads (max %d)", len(args.Items), maxBatchViewItems)), nil
		}
		for i, item := range args.Items {
			if err := validateViewItem(item); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("items[%d]: %v", i, err)), nil
			}
		}
		return executeBatchView(ctx, args.Items, workDir, cfg)
	}

	item := viewItemArgs{
		FilePath:    args.FilePath,
		ArtifactRef: args.ArtifactRef,
		Offset:      args.Offset,
		Limit:       args.Limit,
	}
	if err := validateViewItem(item); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return executeViewItem(ctx, item, workDir, cfg)
}

func validateViewItem(args viewItemArgs) error {
	if (args.FilePath == "") == (args.ArtifactRef == "") {
		return fmt.Errorf("exactly one of file_path or artifact_ref is required")
	}
	return nil
}

func executeBatchView(ctx context.Context, items []viewItemArgs, workDir string, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var body strings.Builder
	succeeded := 0
	failed := 0
	skipped := 0
	successfulBytes := 0
	outputLimitReached := false

	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("cancelled: %v", err)), nil
		}
		if outputLimitReached {
			writeBatchViewItem(&body, i, "skipped", "batch output limit was reached; retry this item in a smaller batch")
			skipped++
			continue
		}

		response, err := executeViewItem(ctx, item, workDir, cfg)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if response.IsError {
			writeBatchViewItem(&body, i, "error", response.Content)
			failed++
			continue
		}
		if successfulBytes+len(response.Content) > maxBatchViewOutputSize {
			writeBatchViewItem(&body, i, "error", fmt.Sprintf("batch output would exceed %d bytes; retry this item with a smaller limit or in a separate call", maxBatchViewOutputSize))
			failed++
			outputLimitReached = true
			continue
		}

		writeBatchViewItem(&body, i, "ok", response.Content)
		successfulBytes += len(response.Content)
		succeeded++
	}

	status := "ok"
	if succeeded == 0 {
		status = "error"
	} else if failed > 0 || skipped > 0 {
		status = "partial_failure"
	}
	var result strings.Builder
	fmt.Fprintf(&result, "<batch_view status=\"%s\" succeeded=\"%d\" failed=\"%d\" skipped=\"%d\">\n", status, succeeded, failed, skipped)
	result.WriteString(body.String())
	result.WriteString("</batch_view>\n")

	if succeeded == 0 {
		return fantasy.NewTextErrorResponse(result.String()), nil
	}
	return fantasy.NewTextResponse(result.String()), nil
}

func writeBatchViewItem(b *strings.Builder, index int, status, content string) {
	fmt.Fprintf(b, "<item index=\"%d\" status=\"%s\">\n", index, status)
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("</item>\n")
}

func executeViewItem(ctx context.Context, args viewItemArgs, workDir string, cfg ToolConfig) (fantasy.ToolResponse, error) {
	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cancelled: %v", err)), nil
	}

	if args.ArtifactRef != "" {
		return executeViewArtifact(ctx, args, cfg)
	}

	effectiveCfg := cfgWithMergedPaths(cfg, ctx)
	scoped, err := newScopedFileAccess(effectiveCfg, args.FilePath, false)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}
	var info os.FileInfo
	var f *os.File
	if scoped != nil {
		defer scoped.close()
		f, info, err = scoped.openRegular()
	} else {
		absPath, pathErr := checkPathOrConsent(args.FilePath, workDir, "read", effectiveCfg)
		err = pathErr
		if err == nil {
			info, err = os.Stat(absPath)
		}
		if err == nil && info.IsDir() {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("'%s' is a directory, not a file. Use the ls tool to list directory contents.", args.FilePath)), nil
		}
		if err == nil {
			f, err = os.Open(absPath)
		}
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot access '%s': %v", args.FilePath, err)), nil
	}
	defer f.Close()

	if info.Size() > maxViewSize {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file '%s' is too large (%d bytes, max %d). Use offset/limit to read portions.", args.FilePath, info.Size(), maxViewSize)), nil
	}

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

func executeViewArtifact(ctx context.Context, args viewItemArgs, cfg ToolConfig) (fantasy.ToolResponse, error) {
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
