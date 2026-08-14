package team

// Memory tools exposed to workers: LTM save/update wrappers and stm_write.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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
	info.Parameters["category"] = map[string]any{
		"type": "string", "enum": []string{"decision", "convention", "architecture", "issue", "error", "lesson", "pattern", "observation", "finding", "verification", "artifact", "requirement", "instruction", "summary"},
		"description": "Optional canonical knowledge category. If omitted, the compatibility section classifier chooses the kind.",
	}
	info.Parameters["supersedes"] = map[string]any{
		"type": "array", "items": map[string]any{"type": "string"},
		"description": "Current canonical memory IDs to replace after this candidate is accepted. Targets must be visible in the caller's own memory identity.",
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
		Confidence *float64 `json:"confidence"`
		Supersedes []string `json:"supersedes"`
		FilePaths  []string `json:"file_paths"`
		Visibility string   `json:"visibility"`
		Tier       string   `json:"tier"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil || args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}
	args.Supersedes = normalizeMemoryIDs(args.Supersedes)
	if args.Confidence != nil && (*args.Confidence < 0 || *args.Confidence > 1) {
		return fantasy.NewTextErrorResponse("confidence must be between 0 and 1"), nil
	}
	paths, err := normalizeMemoryFilePaths(args.FilePaths)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
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
		item, err := t.savePrivateCandidate(ctx, args, paths)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to save private memory candidate: %v", err)), nil
		}
		return fantasy.NewTextResponse(fmt.Sprintf("Saved private %s memory candidate (id: %s); it remains private and is confirmed only after accepted evidence", args.Tier, item.ID)), nil
	}
	if t.coordinator.contextRepo != nil {
		runID := t.coordinator.executionRunID
		if runID == "" && t.coordinator.taskTracker != nil && t.coordinator.taskTracker.TodoList() != nil {
			runID = t.coordinator.taskTracker.TodoList().RunID()
		}
		taskID, _ := ctx.Value(todoIDKey{}).(string)
		item, err := NewSharedMemoryService(t.coordinator.contextRepo).Propose(ctx, SharedMemoryProposal{
			Scope: t.coordinator.contextScope(), Content: args.Content, Section: section, Category: args.Category,
			Source: "memory_save", RunID: runID, TaskID: taskID, Confidence: args.Confidence, FilePaths: paths, Supersedes: args.Supersedes,
		})
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to save shared memory candidate: %v", err)), nil
		}
		_ = t.coordinator.emitEvent("shared_memory_candidate_saved", "coordinator", taskID, map[string]interface{}{
			"item_id": item.ID, "run_id": runID, "kind": item.Kind, "lifecycle": item.Lifecycle,
		})
		if err := t.coordinator.rebuildLegacyContextProjections(ctx); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("saved canonical candidate but projection rebuild failed: %v", err)), nil
		}
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
	Confidence *float64 `json:"confidence"`
	Supersedes []string `json:"supersedes"`
	FilePaths  []string `json:"file_paths"`
	Visibility string   `json:"visibility"`
	Tier       string   `json:"tier"`
}, paths []string) (contextstore.ContextItem, error) {
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
		WorkerID:   def.MemoryID,
		Scope:      scope,
		Content:    args.Content,
		Category:   args.Category,
		Tier:       tier,
		RunID:      runID,
		TaskID:     taskID,
		Source:     "memory_save",
		Confidence: args.Confidence,
		FilePaths:  paths,
		Supersedes: args.Supersedes,
	})
}

func normalizeMemoryIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
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
		Query         string   `json:"query"`
		N             int      `json:"n"`
		Category      string   `json:"category"`
		MinConfidence *float64 `json:"min_confidence"`
		FilePaths     []string `json:"file_paths"`
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
	if args.MinConfidence != nil && (*args.MinConfidence < 0 || *args.MinConfidence > 1) {
		return fantasy.NewTextErrorResponse("min_confidence must be between 0 and 1"), nil
	}
	paths, err := normalizeMemoryFilePaths(args.FilePaths)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	categoryKind, err := memoryCategoryKind(args.Category)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	req := contextstore.SearchRequest{Query: args.Query, Scope: t.coordinator.contextScope(), Limit: 100, MinConfidence: args.MinConfidence, FilePaths: paths}
	if categoryKind != "" {
		req.Kinds = []contextstore.ContextKind{categoryKind}
	}
	results, trace, err := contextstore.HybridRetrieve(ctx, t.coordinator.contextRepo, nil, req)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("canonical memory query failed: %v", err)), nil
	}
	if len(results) > args.N {
		results = results[:args.N]
	}
	if len(results) == 0 {
		return fantasy.NewTextResponse("No relevant memories found."), nil
	}
	var b strings.Builder
	for _, result := range results {
		fmt.Fprintf(&b, "- [%.2f] %s (id: %s)\n", result.Score, result.Item.Content, result.Item.ID)
	}
	if len(trace.FilePathBoosted) > 0 {
		fmt.Fprintf(&b, "\n(retrieval trace: file-path boost applied to %s)", strings.Join(trace.FilePathBoosted, ", "))
	}
	return fantasy.NewTextResponse(strings.TrimSpace(b.String())), nil
}

func normalizeMemoryFilePaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	unique := make(map[string]struct{}, len(paths))
	for _, raw := range paths {
		path := filepath.ToSlash(filepath.Clean(strings.TrimSpace(raw)))
		if path == "." || path == "" || filepath.IsAbs(raw) || path == ".." || strings.HasPrefix(path, "../") {
			return nil, fmt.Errorf("file_paths must be workspace-relative paths")
		}
		unique[path] = struct{}{}
	}
	out := make([]string, 0, len(unique))
	for path := range unique {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

type stmWriteTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *stmWriteTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "stm_write",
		Description: "Deprecated compatibility tool. Append one explicitly typed shared session-memory item; task results are captured automatically. It never replaces stm.md.",
		Parameters: map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The content to write to short-term memory",
			},
			"kind": map[string]any{
				"type":        "string",
				"description": "Explicit canonical context kind.",
				"enum":        []string{"observation", "decision", "open_question", "error", "verification", "artifact", "progress", "summary"},
			},
		},
		Required: []string{"content", "kind"},
	}
}

func (t *stmWriteTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Content string `json:"content"`
		Kind    string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}

	kind, err := stmContextKind(args.Kind)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if t.coordinator.contextRepo != nil {
		if err := t.coordinator.appendCanonicalContext(ctx, kind, args.Content, "stm_write", map[string]string{"legacy_section": stmSectionForKind(kind), "deprecated_tool": "true"}); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write canonical STM: %v", err)), nil
		}
		return fantasy.NewTextResponse("Appended typed short-term memory"), nil
	}

	// Legacy-mode fallback when no canonical repository is configured.
	t.coordinator.shadowContextAppend(kind, args.Content, "stm_write")
	err = t.coordinator.updateSTM(func(existing string) string {
		if existing == "" {
			return TruncateSTM(args.Content)
		}
		return TruncateSTM(existing + "\n" + args.Content)
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write stm.md: %v", err)), nil
	}

	return fantasy.NewTextResponse("Appended typed short-term memory"), nil
}

func stmContextKind(raw string) (contextstore.ContextKind, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "observation":
		return contextstore.ContextObservation, nil
	case "decision":
		return contextstore.ContextDecision, nil
	case "open_question":
		return contextstore.ContextOpenQuestion, nil
	case "error":
		return contextstore.ContextError, nil
	case "verification":
		return contextstore.ContextVerification, nil
	case "artifact":
		return contextstore.ContextArtifact, nil
	case "progress":
		return contextstore.ContextProgress, nil
	case "summary":
		return contextstore.ContextSummary, nil
	default:
		return "", fmt.Errorf("kind must be one of observation, decision, open_question, error, verification, artifact, progress, or summary")
	}
}

func stmSectionForKind(kind contextstore.ContextKind) string {
	switch kind {
	case contextstore.ContextObservation:
		return stmSectionFindings
	case contextstore.ContextDecision:
		return stmSectionDecisions
	case contextstore.ContextOpenQuestion:
		return stmSectionQuestions
	case contextstore.ContextError:
		return stmSectionErrors
	default:
		return stmSectionProgress
	}
}

type ltmUpdateTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *ltmUpdateTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "ltm_update",
		Description: "Deprecated compatibility alias for proposing persistent shared memory. The candidate becomes visible only after accepted evidence.",
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
