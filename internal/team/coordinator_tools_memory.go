package team

// Memory tools exposed to workers: LTM save/update wrappers and stm_write.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"charm.land/fantasy"
)

type memorySaveLTMWrapper struct {
	original    fantasy.AgentTool
	coordinator *Coordinator
}

func (t *memorySaveLTMWrapper) Info() fantasy.ToolInfo {
	return t.original.Info()
}

func (t *memorySaveLTMWrapper) ProviderOptions() fantasy.ProviderOptions {
	return t.original.ProviderOptions()
}

func (t *memorySaveLTMWrapper) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.original.SetProviderOptions(opts)
}

func (t *memorySaveLTMWrapper) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := t.original.Run(ctx, call)
	if err != nil || resp.IsError {
		return resp, err
	}

	var args struct {
		Content  string `json:"content"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil || args.Content == "" {
		return resp, nil
	}

	section := ClassifyLTMEntry(args.Content, "finding")
	if section == "" {
		section = ltmSectionPatterns
	}

	t.coordinator.ltmWriteMu.Lock()
	defer t.coordinator.ltmWriteMu.Unlock()

	workspace := t.coordinator.session.Workspace
	existingLTM := LoadLTM(workspace, t.coordinator.session.Config.Name)
	entry := formatLTMEntry(args.Content)
	existingLTMSections := ParseSTMSections(existingLTM)
	if hasLTREntry(existingLTMSections, section, entry) {
		return resp, nil
	}

	newLTM := appendSTMEntry(existingLTM, entry, section)
	pruned := PruneLTM(newLTM)
	if err := SaveLTM(workspace, t.coordinator.session.Config.Name, TruncateLTM(pruned)); err != nil {
		log.Printf("warning: memory_save LTM write-back failed: %v", err)
	}

	return resp, nil
}

type stmWriteTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *stmWriteTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "stm_write",
		Description: "Write to short-term memory (stm.md), a shared workspace file visible to all agents in the current session. Use append mode to add new information, or replace mode to overwrite. This memory is session-scoped and will be archived when the session ends.",
		Parameters: map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The content to write to short-term memory",
			},
			"mode": map[string]any{
				"type":        "string",
				"description": "Write mode: \"append\" (add to end, default) or \"replace\" (overwrite entire file)",
				"enum":        []string{"append", "replace"},
			},
		},
		Required: []string{"content"},
	}
}

func (t *stmWriteTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Content string `json:"content"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}

	mode := args.Mode
	if mode == "" {
		mode = "append"
	}

	err := t.coordinator.updateSTM(func(existing string) string {
		if mode == "replace" {
			return TruncateSTM(args.Content)
		}
		if existing == "" {
			return TruncateSTM(args.Content)
		}
		return TruncateSTM(existing + "\n" + args.Content)
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write stm.md: %v", err)), nil
	}

	verb := "Appended to"
	if mode == "replace" {
		verb = "Replaced"
	}
	return fantasy.NewTextResponse(fmt.Sprintf("%s short-term memory (stm.md)", verb)), nil
}

type ltmUpdateTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *ltmUpdateTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "ltm_update",
		Description: "Update long-term memory (ltm.md), a persistent file shared across sessions for this team. Each entry is appended to the specified section so it can be retrieved in future sessions.",
		Parameters: map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The knowledge to record (one concise fact, decision, or pattern per call)",
			},
			"section": map[string]any{
				"type":        "string",
				"description": "Which long-term memory section to append to",
				"enum": []string{
					ltmSectionConventions,
					ltmSectionArchitecture,
					ltmSectionPatterns,
					ltmSectionIssues,
					ltmSectionFiles,
					ltmSectionTools,
				},
			},
		},
		Required: []string{"content", "section"},
	}
}

func (t *ltmUpdateTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Content string `json:"content"`
		Section string `json:"section"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}
	if args.Section == "" {
		return fantasy.NewTextErrorResponse("section is required"), nil
	}

	// Validate section against the enum defined in Info()
	validSections := map[string]bool{
		ltmSectionConventions:  true,
		ltmSectionArchitecture: true,
		ltmSectionPatterns:     true,
		ltmSectionIssues:       true,
		ltmSectionFiles:        true,
		ltmSectionTools:        true,
	}
	if !validSections[args.Section] {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid section %q; must be one of: %s, %s, %s, %s, %s, %s",
			args.Section,
			ltmSectionConventions, ltmSectionArchitecture, ltmSectionPatterns,
			ltmSectionIssues, ltmSectionFiles, ltmSectionTools)), nil
	}

	entry := formatLTMEntry(args.Content)
	workspace := t.coordinator.session.Workspace
	t.coordinator.ltmWriteMu.Lock()
	existing := LoadLTM(workspace, t.coordinator.session.Config.Name)
	if hasLTREntry(ParseSTMSections(existing), args.Section, entry) {
		t.coordinator.ltmWriteMu.Unlock()
		return fantasy.NewTextResponse(fmt.Sprintf("Already recorded in long-term memory section %q; skipped duplicate", args.Section)), nil
	}
	newContent := TruncateLTM(PruneLTM(appendLTMEntry(existing, entry, args.Section)))
	err := SaveLTM(workspace, t.coordinator.session.Config.Name, newContent)
	t.coordinator.ltmWriteMu.Unlock()
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write ltm.md: %v", err)), nil
	}

	return fantasy.NewTextResponse(fmt.Sprintf("Appended to long-term memory section %q", args.Section)), nil
}
