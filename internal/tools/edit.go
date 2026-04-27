package tools

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/fantasy"

	udiff "github.com/aymanbagabas/go-udiff"
)

type Edit struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

type editArgs struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
	Edits   []Edit `json:"edits"`
}

type replacement struct {
	oldText     string
	newText     string
	originalOld string
	originalNew string
	index       int
}

type matchedReplacement struct {
	replacement
	start          int
	end            int
	usedFuzzyMatch bool
}

func NewEditTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "edit",
			Description: "Edit a file by replacing exact text. Supports single edit via old_text/new_text, or multiple edits via the edits array. All edits are matched against the original file content and must be non-overlapping.",
			Parameters: map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to edit (relative or absolute)",
				},
				"old_text": map[string]any{
					"type":        "string",
					"description": "Exact text to find and replace (single-edit mode)",
				},
				"new_text": map[string]any{
					"type":        "string",
					"description": "New text to replace the old text with (single-edit mode)",
				},
				"edits": map[string]any{
					"type":        "array",
					"description": "Array of edits for multi-region replacement",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"old_text": map[string]any{
								"type":        "string",
								"description": "Exact text to find and replace for this edit",
							},
							"new_text": map[string]any{
								"type":        "string",
								"description": "New text for this edit",
							},
						},
						"required": []string{"old_text", "new_text"},
					},
				},
			},
			Required: []string{"path"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeEdit(ctx, call, cfg.WorkDir)
		},
	}
}

func executeEdit(_ context.Context, call fantasy.ToolCall, workDir string) (fantasy.ToolResponse, error) {
	var args editArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("failed to parse arguments: " + err.Error()), nil
	}
	if args.Path == "" {
		return fantasy.NewTextErrorResponse("path parameter is required"), nil
	}

	absPath, err := resolvePathWithWorkDir(args.Path, workDir)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}

	contentBytes, err := os.ReadFile(absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read file: %v", err)), nil
	}

	content := string(contentBytes)

	replacements, err := normalizeEditInput(args)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	newContent, applied, err := applyEdits(content, replacements)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	if err := os.WriteFile(absPath, []byte(newContent), 0644); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write file: %v", err)), nil
	}

	normalizedContent := strings.ReplaceAll(content, "\r\n", "\n")
	diff := udiff.Unified(absPath, absPath, normalizedContent, newContent)

	fuzzyCount := 0
	for _, m := range applied {
		if m.usedFuzzyMatch {
			fuzzyCount++
		}
	}

	var msg string
	if len(applied) == 1 {
		if fuzzyCount > 0 {
			msg = fmt.Sprintf("Applied edit (fuzzy match) to %s\n%s", args.Path, diff)
		} else {
			msg = fmt.Sprintf("Applied edit to %s\n%s", args.Path, diff)
		}
	} else {
		if fuzzyCount > 0 {
			msg = fmt.Sprintf("Applied %d edits (%d fuzzy) to %s\n%s", len(applied), fuzzyCount, args.Path, diff)
		} else {
			msg = fmt.Sprintf("Applied %d edits to %s\n%s", len(applied), args.Path, diff)
		}
	}

	return fantasy.NewTextResponse(msg), nil
}

func normalizeEditInput(args editArgs) ([]replacement, error) {
	singleMode := args.OldText != "" || args.NewText != ""
	multiMode := len(args.Edits) > 0

	if singleMode && multiMode {
		return nil, fmt.Errorf("cannot use old_text/new_text together with edits array")
	}
	if !singleMode && !multiMode {
		return nil, fmt.Errorf("must provide either old_text/new_text or edits array")
	}
	if singleMode {
		if args.OldText == "" {
			return nil, fmt.Errorf("old_text is required when using single-edit mode")
		}
		if args.NewText == "" {
			return nil, fmt.Errorf("new_text is required when using single-edit mode")
		}
		return []replacement{{
			oldText:     strings.ReplaceAll(args.OldText, "\r\n", "\n"),
			newText:     strings.ReplaceAll(args.NewText, "\r\n", "\n"),
			originalOld: args.OldText,
			originalNew: args.NewText,
			index:       0,
		}}, nil
	}

	var reps []replacement
	for i, edit := range args.Edits {
		if edit.OldText == "" {
			return nil, fmt.Errorf("edits[%d].old_text is required", i)
		}
		reps = append(reps, replacement{
			oldText:     strings.ReplaceAll(edit.OldText, "\r\n", "\n"),
			newText:     strings.ReplaceAll(edit.NewText, "\r\n", "\n"),
			originalOld: edit.OldText,
			originalNew: edit.NewText,
			index:       i,
		})
	}
	return reps, nil
}

func applyEdits(content string, edits []replacement) (string, []matchedReplacement, error) {
	normalizedContent := strings.ReplaceAll(content, "\r\n", "\n")

	var matched []matchedReplacement
	for _, edit := range edits {
		m, err := findMatch(normalizedContent, edit)
		if err != nil {
			return "", nil, err
		}
		matched = append(matched, *m)
	}

	sort.Slice(matched, func(i, j int) bool {
		return matched[i].start < matched[j].start
	})

	for i := 1; i < len(matched); i++ {
		if matched[i-1].end > matched[i].start {
			return "", nil, fmt.Errorf("edits[%d] and edits[%d] overlap", matched[i-1].index, matched[i].index)
		}
	}

	result := normalizedContent
	for i := len(matched) - 1; i >= 0; i-- {
		m := matched[i]
		result = result[:m.start] + m.newText + result[m.end:]
	}

	return result, matched, nil
}

func findMatch(content string, edit replacement) (*matchedReplacement, error) {
	count := strings.Count(content, edit.oldText)

	if count == 0 {
		idx, matchLen := fuzzyMatch(content, edit.oldText)
		if idx < 0 {
			return nil, fmt.Errorf("edits[%d]: could not find old_text in file", edit.index)
		}
		matchedText := content[idx : idx+matchLen]
		return &matchedReplacement{
			replacement: replacement{
				oldText:     matchedText,
				newText:     edit.newText,
				originalOld: edit.originalOld,
				originalNew: edit.originalNew,
				index:       edit.index,
			},
			start:          idx,
			end:            idx + matchLen,
			usedFuzzyMatch: true,
		}, nil
	}

	if count > 1 {
		return nil, fmt.Errorf("found %d matches for edits[%d].old_text; provide more context", count, edit.index)
	}

	idx := strings.Index(content, edit.oldText)
	return &matchedReplacement{
		replacement: edit,
		start:       idx,
		end:         idx + len(edit.oldText),
	}, nil
}

func fuzzyMatch(content, search string) (int, int) {
	normContent, contentMap := normalizeWithMap(content)
	normSearch := normalizeForFuzzy(search)

	if normSearch == "" {
		return -1, 0
	}

	idx := strings.Index(normContent, normSearch)
	if idx < 0 {
		return -1, 0
	}

	if strings.Count(normContent, normSearch) > 1 {
		return -1, 0
	}

	origStart := contentMap[idx]
	endNorm := idx + len(normSearch)
	var origEnd int
	if endNorm >= len(normContent) {
		origEnd = len(content)
	} else {
		origEnd = contentMap[endNorm]
	}

	return origStart, origEnd - origStart
}

func normalizeWithMap(s string) (string, []int) {
	var result []byte
	var mapping []int

	lines := strings.Split(s, "\n")
	origPos := 0
	for li, line := range lines {
		if li > 0 {
			result = append(result, '\n')
			mapping = append(mapping, origPos)
			origPos++
		}

		trimmed := strings.TrimRightFunc(line, unicode.IsSpace)
		for j := 0; j < len(trimmed); {
			r, size := utf8.DecodeRuneInString(trimmed[j:])
			repl := normalizeRune(r)
			for k := 0; k < len(repl); k++ {
				mapping = append(mapping, origPos+j)
			}
			result = append(result, repl...)
			j += size
		}

		origPos += len(line)
	}

	return string(result), mapping
}

func normalizeRune(r rune) string {
	switch r {
	case '\u201c', '\u201d':
		return "\""
	case '\u2018', '\u2019':
		return "'"
	case '\u2013', '\u2014':
		return "-"
	case '\u00a0':
		return " "
	default:
		return string(r)
	}
}

func normalizeForFuzzy(s string) string {
	norm, _ := normalizeWithMap(s)
	return norm
}
