//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"

	udiff "github.com/aymanbagabas/go-udiff"
)

type editArgs struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`

	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
	Edits   []Edit `json:"edits"`
}

type Edit struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

func NewEditTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "edit"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "edit",
			Description: "Edit a file by replacing exact text. Supports single edit via old_string/new_text, or multiple edits via the edits array. All edits are matched against the original file content and must be non-overlapping. Use replace_all to replace all occurrences of old_string.",
			Parameters: map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "The path to the file to edit (relative or absolute)",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "The text to replace (empty for new file creation)",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "The text to replace it with (empty for deletion)",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace all occurrences of old_string (default false)",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file (deprecated: use file_path instead)",
				},
				"old_text": map[string]any{
					"type":        "string",
					"description": "Text to find (deprecated: use old_string instead)",
				},
				"new_text": map[string]any{
					"type":        "string",
					"description": "Replacement text (deprecated: use new_string instead)",
				},
				"edits": map[string]any{
					"type":        "array",
					"description": "Array of edits for multi-region replacement (deprecated: use multiedit tool instead)",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"old_text": map[string]any{
								"type":        "string",
								"description": "Exact text to find and replace",
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
			Required: []string{"file_path"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeEdit(ctx, call, cfg)
		},
	}
}

func executeEdit(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var args editArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("failed to parse arguments: " + err.Error()), nil
	}

	filePath := args.FilePath
	if filePath == "" {
		filePath = args.Path
	}
	if filePath == "" {
		return fantasy.NewTextErrorResponse("file_path parameter is required"), nil
	}

	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cancelled: %v", err)), nil
	}

	absPath, err := resolveAndValidatePathWithConsent(filePath, cfgWithMergedPaths(cfg, ctx))
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid path: %v", err)), nil
	}

	oldString := args.OldString
	if oldString == "" {
		oldString = args.OldText
	}
	newString := args.NewString
	if newString == "" && args.NewText != "" {
		newString = args.NewText
	}

	if len(args.Edits) > 0 {
		replacements, err := normalizeEditInput(args)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return applyEditsAndWrite(absPath, filePath, replacements)
	}

	if oldString == "" && newString != "" {
		if _, err := os.Stat(absPath); err == nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("file %s already exists", filePath)), nil
		}
		dir := absPath[:strings.LastIndex(absPath, "/")]
		if dir == "" {
			dir = "."
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to create directories: %v", err)), nil
		}
		if err := os.WriteFile(absPath, []byte(newString), 0o644); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write file: %v", err)), nil
		}
		return fantasy.NewTextResponse(fmt.Sprintf("Created %s", filePath)), nil
	}

	if oldString == "" && newString == "" {
		return fantasy.NewTextErrorResponse("must provide old_string/new_string, or edits array"), nil
	}

	replacements := []replacement{
		{
			oldText:     strings.ReplaceAll(oldString, "\r\n", "\n"),
			newText:     strings.ReplaceAll(newString, "\r\n", "\n"),
			originalOld: oldString,
			originalNew: newString,
			index:       0,
			replaceAll:  args.ReplaceAll,
		},
	}

	return applyEditsAndWrite(absPath, filePath, replacements)
}

func normalizeEditInput(args editArgs) ([]replacement, error) {
	if len(args.Edits) == 0 {
		return nil, fmt.Errorf("edits array is empty")
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

func applyEditsAndWrite(absPath, displayPath string, replacements []replacement) (fantasy.ToolResponse, error) {
	contentBytes, err := os.ReadFile(absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read file: %v", err)), nil
	}

	content := string(contentBytes)
	normalizedContent := strings.ReplaceAll(content, "\r\n", "\n")

	var matched []matchedReplacement
	for _, edit := range replacements {
		m, err := findMatch(normalizedContent, edit)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		matched = append(matched, *m)
	}

	newContent := normalizedContent
	for i := len(matched) - 1; i >= 0; i-- {
		m := matched[i]
		if m.replaceAll {
			newContent = strings.ReplaceAll(newContent, m.oldText, m.newText)
		} else {
			newContent = newContent[:m.start] + m.newText + newContent[m.end:]
		}
	}

	if err := os.WriteFile(absPath, []byte(newContent), 0o644); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write file: %v", err)), nil
	}

	diff := udiff.Unified(absPath, absPath, normalizedContent, newContent)

	fuzzyCount := 0
	for _, m := range matched {
		if m.usedFuzzyMatch {
			fuzzyCount++
		}
	}

	var msg string
	if len(matched) == 1 {
		if fuzzyCount > 0 {
			msg = fmt.Sprintf("Applied edit (fuzzy match) to %s\n%s", displayPath, diff)
		} else {
			msg = fmt.Sprintf("Applied edit to %s\n%s", displayPath, diff)
		}
	} else {
		if fuzzyCount > 0 {
			msg = fmt.Sprintf("Applied %d edits (%d fuzzy) to %s\n%s", len(matched), fuzzyCount, displayPath, diff)
		} else {
			msg = fmt.Sprintf("Applied %d edits to %s\n%s", len(matched), displayPath, diff)
		}
	}

	return fantasy.NewTextResponse(msg), nil
}

type replacement struct {
	oldText     string
	newText     string
	originalOld string
	originalNew string
	index       int
	replaceAll  bool
}

type matchedReplacement struct {
	replacement
	start          int
	end            int
	usedFuzzyMatch bool
}

func findMatch(content string, edit replacement) (*matchedReplacement, error) {
	if edit.replaceAll {
		count := strings.Count(content, edit.oldText)
		if count == 0 {
			idx, matchLen := fuzzyMatch(content, edit.oldText)
			if idx < 0 {
				return nil, fmt.Errorf("old_string not found in file")
			}
			matchedText := content[idx : idx+matchLen]
			return &matchedReplacement{
				replacement: replacement{
					oldText:     matchedText,
					newText:     edit.newText,
					originalOld: edit.originalOld,
					originalNew: edit.originalNew,
					index:       edit.index,
					replaceAll:  true,
				},
				start:          idx,
				end:            idx + matchLen,
				usedFuzzyMatch: true,
			}, nil
		}
		idx := strings.Index(content, edit.oldText)
		return &matchedReplacement{
			replacement: replacement{
				oldText:     edit.oldText,
				newText:     edit.newText,
				originalOld: edit.originalOld,
				originalNew: edit.originalNew,
				index:       edit.index,
				replaceAll:  true,
			},
			start:          idx,
			end:            idx + len(edit.oldText),
			usedFuzzyMatch: false,
		}, nil
	}

	count := strings.Count(content, edit.oldText)

	if count == 0 {
		idx, matchLen := fuzzyMatch(content, edit.oldText)
		if idx < 0 {
			return nil, fmt.Errorf("edits[%d]: could not find old_string in file", edit.index)
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

	if count > 1 && !edit.replaceAll {
		return nil, fmt.Errorf("found %d matches for old_string; provide more context or use replace_all", count)
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

		trimmed := strings.TrimRightFunc(line, isSpace)
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

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\r'
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
