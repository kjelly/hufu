package team

// Memory tools exposed to workers: LTM save/update wrappers and stm_write.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/tools"
)

type memorySaveLTMWrapper struct {
	original    fantasy.AgentTool
	coordinator *Coordinator
}

func (t *memorySaveLTMWrapper) Info() fantasy.ToolInfo {
	info := t.original.Info()
	if info.Parameters == nil {
		info.Parameters = make(map[string]any)
	}
	info.Parameters["visibility"] = map[string]any{
		"type": "string", "enum": []string{"shared", "private"},
		"description": "Memory visibility. Omit or use shared for the legacy team-visible behavior; private is visible only to the calling worker.",
	}
	info.Parameters["tier"] = map[string]any{
		"type": "string", "enum": []string{"session", "persistent"},
		"description": "Private-memory lifetime tier. Defaults to persistent when visibility is private.",
	}
	return info
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
		Visibility string   `json:"visibility"`
		Tier       string   `json:"tier"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil || args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}

	section := ClassifyLTMEntry(args.Content, "finding")
	if section == "" {
		section = ltmSectionPatterns
	}
	visibility := strings.ToLower(strings.TrimSpace(args.Visibility))
	if visibility == "" {
		visibility = "shared" // backwards-compatible default
	}
	if visibility != "shared" && visibility != "private" {
		return fantasy.NewTextErrorResponse("visibility must be shared or private"), nil
	}
	if visibility == "private" {
		item, err := t.savePrivateCandidate(ctx, args)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to save private memory candidate: %v", err)), nil
		}
		return fantasy.NewTextResponse(fmt.Sprintf("Saved private %s memory candidate (id: %s); it remains private and is confirmed only after accepted evidence", args.Tier, item.ID)), nil
	}
	if t.coordinator.contextRepo != nil {
		t.coordinator.persistKnowledgeCandidate(args.Content, section, "memory_save")
		return fantasy.NewTextResponse("Saved as candidate memory; it will be confirmed after acceptance"), nil
	}
	resp, err := t.original.Run(ctx, call)
	if err != nil || resp.IsError {
		return resp, err
	}

	// Legacy-mode fallback when no canonical repository is configured.
	t.coordinator.persistKnowledgeCandidate(args.Content, section, "memory_save")
	return resp, nil
}

// savePrivateCandidate resolves all scope fields from the execution context.
// The model supplies no agent, branch, task, run, or evidence selector, so it
// cannot write into another worker's memory namespace.
func (t *memorySaveLTMWrapper) savePrivateCandidate(ctx context.Context, args struct {
	Content    string   `json:"content"`
	Category   string   `json:"category"`
	Supersedes []string `json:"supersedes"`
	Visibility string   `json:"visibility"`
	Tier       string   `json:"tier"`
}) (contextstore.ContextItem, error) {
	if t.coordinator == nil || t.coordinator.workerMemorySvc == nil || t.coordinator.session == nil {
		return contextstore.ContextItem{}, fmt.Errorf("canonical worker memory is not available")
	}
	caller, _ := ctx.Value(tools.AgentNameKey).(string)
	def := t.coordinator.agentDefByName(caller)
	if def == nil || def.Memory.Mode == agent.WorkerMemoryOff || strings.TrimSpace(def.MemoryID) == "" {
		return contextstore.ContextItem{}, fmt.Errorf("private memory requires an enabled worker identity")
	}
	tier := strings.ToLower(strings.TrimSpace(args.Tier))
	if tier == "" {
		tier = "persistent"
	}
	if tier != "session" && tier != "persistent" {
		return contextstore.ContextItem{}, fmt.Errorf("tier must be session or persistent")
	}
	if tier == "persistent" && def.Memory.Mode != agent.WorkerMemoryPersistent {
		return contextstore.ContextItem{}, fmt.Errorf("worker memory policy does not allow persistent private memory")
	}
	taskID, _ := ctx.Value(todoIDKey{}).(string)
	if strings.TrimSpace(taskID) == "" {
		return contextstore.ContextItem{}, fmt.Errorf("private memory requires an active task")
	}
	runID := t.coordinator.executionRunID
	if runID == "" && t.coordinator.taskTracker != nil && t.coordinator.taskTracker.TodoList() != nil {
		runID = t.coordinator.taskTracker.TodoList().RunID()
	}
	if strings.TrimSpace(runID) == "" {
		return contextstore.ContextItem{}, fmt.Errorf("private memory requires an active run")
	}
	scope := resolveWorkerScope(t.coordinator.contextScope(), def, t.coordinator.activeBranchID())
	return t.coordinator.workerMemorySvc.SaveCandidate(ctx, WorkerMemoryCandidateRequest{
		WorkerID: def.MemoryID,
		Scope:    scope,
		Content:  args.Content,
		Category: args.Category,
		Tier:     tier,
		RunID:    runID,
		TaskID:   taskID,
		Source:   "memory_save",
	})
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
	t.coordinator.persistKnowledgeCandidate(args.Content, args.Section, "ltm_update")
	return fantasy.NewTextResponse(fmt.Sprintf("Saved to long-term memory section %q as a candidate; it will be confirmed after acceptance", args.Section)), nil
}
