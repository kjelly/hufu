package team

// Memory tools exposed to workers: LTM save/update wrappers and stm_write.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"charm.land/fantasy"

	contextstore "github.com/anomalyco/hufu/internal/context"
	"github.com/anomalyco/hufu/internal/memory"
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
	var args struct {
		Content    string   `json:"content"`
		Category   string   `json:"category"`
		Supersedes []string `json:"supersedes"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil || args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}

	section := ClassifyLTMEntry(args.Content, "finding")
	if section == "" {
		section = ltmSectionPatterns
	}
	if t.coordinator.contextRepo != nil {
		if err := t.coordinator.appendCanonicalContext(ctx, contextstore.ContextPattern, args.Content, "memory_save", map[string]string{"legacy_section": section}); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to save canonical memory: %v", err)), nil
		}
		items, err := t.coordinator.contextRepo.Query(ctx, contextstore.RepositoryQuery{Scope: t.coordinator.contextScope(), Limit: 100000})
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("locating saved canonical memory: %v", err)), nil
		}
		var id string
		for _, item := range items {
			if item.Content == args.Content && item.Source.Ref == "memory_save" {
				id = item.ID
				break
			}
		}
		if id == "" {
			return fantasy.NewTextErrorResponse("saved canonical memory could not be located"), nil
		}
		if err := t.coordinator.contextRepo.MarkSuperseded(ctx, args.Supersedes, id); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("superseding canonical memory: %v", err)), nil
		}
		return fantasy.NewTextResponse(fmt.Sprintf("Saved to canonical memory (id: %s)", id)), nil
	}
	resp, err := t.original.Run(ctx, call)
	if err != nil || resp.IsError {
		return resp, err
	}

	// Legacy-mode fallback when no canonical repository is configured.
	t.coordinator.ltmWriteMu.Lock()
	defer t.coordinator.ltmWriteMu.Unlock()

	workspace := t.coordinator.session.Workspace
	existingLTM := LoadLTM(workspace, t.coordinator.session.Config.Name)
	entry := formatLTMEntry(args.Content)
	existingLTMSections := ParseSTMSections(existingLTM)
	if hasLTREntry(existingLTMSections, section, entry) {
		return resp, nil
	}

	// Record canonical context before attempting the compatibility projection.
	t.coordinator.shadowContextAppend(contextstore.ContextPattern, args.Content, "memory_save")
	newLTM := appendSTMEntry(existingLTM, entry, section)
	pruned := PruneLTM(newLTM)
	if err := SaveLTM(workspace, t.coordinator.session.Config.Name, TruncateLTM(pruned)); err != nil {
		log.Printf("warning: memory_save LTM write-back failed: %v", err)
	}

	return resp, nil
}

// canonicalMemoryQueryTool deliberately bypasses the legacy chromem memory
// store when SQLite context is available.  It keeps the public memory_query
// shape while retrieving only canonical, scope-filtered records.
type canonicalMemoryQueryTool struct {
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
}

func (t *canonicalMemoryQueryTool) Info() fantasy.ToolInfo {
	return memory.NewMemoryQueryTool(nil).Info()
}
func (t *canonicalMemoryQueryTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *canonicalMemoryQueryTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }
func (t *canonicalMemoryQueryTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if t.coordinator == nil || t.coordinator.contextRepo == nil {
		if t.coordinator == nil {
			return fantasy.NewTextErrorResponse("memory is not available"), nil
		}
		// The canonical store is intentionally best-effort during the phased
		// migration. Preserve the legacy query path when it could not open.
		return memory.NewMemoryQueryTool(t.coordinator.memoryStore).Run(ctx, call)
	}
	var args struct {
		Query string `json:"query"`
		N     int    `json:"n"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil || strings.TrimSpace(args.Query) == "" {
		return fantasy.NewTextErrorResponse("query is required"), nil
	}
	if args.N <= 0 {
		args.N = 5
	}
	if args.N > 20 {
		args.N = 20
	}
	results, _, err := contextstore.HybridRetrieve(ctx, t.coordinator.contextRepo, nil, contextstore.SearchRequest{Query: args.Query, Scope: t.coordinator.contextScope(), Limit: args.N})
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("canonical memory query failed: %v", err)), nil
	}
	if len(results) == 0 {
		return fantasy.NewTextResponse("No relevant memories found."), nil
	}
	var b strings.Builder
	for _, result := range results {
		fmt.Fprintf(&b, "- [%.2f] %s (id: %s)\n", result.Score, result.Item.Content, result.Item.ID)
	}
	return fantasy.NewTextResponse(strings.TrimSpace(b.String())), nil
}

type stmWriteTool struct {
	coordToolBase
	coordinator  *Coordinator
	allowReplace bool
}

func (t *stmWriteTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "stm_write",
		Description: "Write to short-term memory (stm.md), a shared workspace file visible to all agents in the current session. Use append mode to add new information. Whole-document replace is reserved for the coordinator or maintenance operations. This memory is session-scoped and will be archived when the session ends.",
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
	if mode != "append" && mode != "replace" {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid mode %q; must be append or replace", mode)), nil
	}
	if mode == "replace" && !t.allowReplace {
		return fantasy.NewTextErrorResponse("replace mode is restricted to the coordinator or maintenance operations; use append"), nil
	}
	if t.coordinator.contextRepo != nil {
		if err := t.coordinator.appendCanonicalContext(ctx, contextstore.ContextProgress, args.Content, "stm_write", map[string]string{"legacy_section": stmSectionProgress, "mode": mode}); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write canonical STM: %v", err)), nil
		}
		verb := "Appended to"
		if mode == "replace" {
			verb = "Replaced"
		}
		return fantasy.NewTextResponse(fmt.Sprintf("%s short-term memory (stm.md)", verb)), nil
	}

	// Legacy-mode fallback when no canonical repository is configured.
	t.coordinator.shadowContextAppend(contextstore.ContextProgress, args.Content, "stm_write")
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
	if t.coordinator.contextRepo != nil {
		if err := t.coordinator.appendCanonicalContext(ctx, contextstore.ContextPattern, args.Content, "ltm_update", map[string]string{"legacy_section": args.Section}); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write canonical LTM: %v", err)), nil
		}
		return fantasy.NewTextResponse(fmt.Sprintf("Appended to long-term memory section %q", args.Section)), nil
	}

	// Legacy-mode fallback when no canonical repository is configured.
	entry := formatLTMEntry(args.Content)
	workspace := t.coordinator.session.Workspace
	t.coordinator.ltmWriteMu.Lock()
	existing := LoadLTM(workspace, t.coordinator.session.Config.Name)
	if hasLTREntry(ParseSTMSections(existing), args.Section, entry) {
		t.coordinator.ltmWriteMu.Unlock()
		return fantasy.NewTextResponse(fmt.Sprintf("Already recorded in long-term memory section %q; skipped duplicate", args.Section)), nil
	}
	// Persist canonical knowledge before updating its legacy Markdown projection.
	t.coordinator.shadowContextAppend(contextstore.ContextPattern, args.Content, "ltm_update")
	newContent := TruncateLTM(PruneLTM(appendLTMEntry(existing, entry, args.Section)))
	err := SaveLTM(workspace, t.coordinator.session.Config.Name, newContent)
	t.coordinator.ltmWriteMu.Unlock()
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write ltm.md: %v", err)), nil
	}

	return fantasy.NewTextResponse(fmt.Sprintf("Appended to long-term memory section %q", args.Section)), nil
}
