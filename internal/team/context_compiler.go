package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/memory"
)

type ContextScope string

const (
	ScopeGlobal  ContextScope = "global"
	ScopeTask    ContextScope = "task"
	ScopeAgent   ContextScope = "agent"
	ScopeSession ContextScope = "session"
)

const (
	PriorityUserGoal              = 1
	PriorityHardConstraints       = 2
	PriorityApprovedPlan          = 3
	PriorityAgentCoreInstructions = 4
	PriorityProjectInstructions   = 5
	PriorityDependencyTaskResults = 6
	PriorityVerificationCriteria  = 7
	PriorityRecentSTM             = 8
	PriorityRelevantLTM           = 9
	PriorityConcurrentTaskSummary = 10
	PriorityGeneralHistory        = 11
)

type ContextItem struct {
	ID           string
	Kind         string
	Content      string
	Source       string
	Scope        ContextScope
	Priority     int
	TokenCount   int
	Confidence   float64
	Freshness    time.Time
	Provenance   []string
	DedupKey     string
	Compressible bool
	Required     bool
}

type CoordinatorContextInput struct {
	Goal             string
	SessionContext   string
	RawSTM           string
	RawLTM           string
	MemoryStore      *memory.MemoryStore
	SidecarCompacter SidecarCompacter
	PrevSummary      *StructuredSummary
	ModelContext     ModelContextSpec
	SystemTokens     int
	ToolsTokens      int
	Role             string
	IsContinuation   bool
	DisableMemory    bool
	ProjectContext   string
}

type WorkerContextInput struct {
	TaskGoal          string
	TaskDef           TaskDef
	AgentDef          *agent.AgentDef
	RawSTM            string
	RawLTM            string
	ContextFiles      map[string]string // filename -> content
	ConcurrentTasks   string
	DependencyResults []TaskResult
	MemoryStore       *memory.MemoryStore
	ModelContext      ModelContextSpec
	SystemTokens      int
	ToolsTokens       int
	MaxAuxChars       int
	DisableMemory     bool
}

type CompiledContext struct {
	Prompt           string
	CompactedSummary *StructuredSummary
	IncludedItems    []ContextItem
	OmittedItems     []ContextItem
	UsedTokens       int
	OverBudget       bool
	Fingerprint      string
}

func hashContentKey(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(hash[:16])
}

// NormalizedDedupKey computes a canonical key for deduplication.
func (item *ContextItem) NormalizedDedupKey() string {
	if item.DedupKey != "" {
		return item.DedupKey
	}
	return hashContentKey(item.Content)
}

// ValidateRequiredItems checks if all required context items are present in the context.
func ValidateRequiredItems(items []ContextItem) error {
	for _, item := range items {
		if item.Required && strings.TrimSpace(item.Content) == "" {
			return fmt.Errorf("required context item %q (kind: %s) is missing or empty", item.ID, item.Kind)
		}
	}
	return nil
}

// DeduplicateContextItems removes duplicate context items across sources
// (Session Context, STM, LTM, Vector Memory, Conversation History, TypedResult).
// Higher priority (lower numerical Priority value) or fresher items win.
func DeduplicateContextItems(items []ContextItem) []ContextItem {
	seen := make(map[string]ContextItem)
	var order []string

	for _, item := range items {
		if strings.TrimSpace(item.Content) == "" {
			continue
		}
		key := item.NormalizedDedupKey()
		if key == "" {
			continue
		}

		existing, exists := seen[key]
		if !exists {
			seen[key] = item
			order = append(order, key)
			continue
		}

		// Resolution logic: lower Priority wins; if equal, higher Confidence; if equal, fresher timestamp
		if item.Priority < existing.Priority ||
			(item.Priority == existing.Priority && item.Confidence > existing.Confidence) ||
			(item.Priority == existing.Priority && item.Confidence == existing.Confidence && item.Freshness.After(existing.Freshness)) {
			seen[key] = item
		}
	}

	result := make([]ContextItem, 0, len(order))
	for _, key := range order {
		result = append(result, seen[key])
	}
	return result
}

// RankContextItems sorts items by Priority ascending (1 highest to 11 lowest), then by Freshness descending.
func RankContextItems(items []ContextItem) []ContextItem {
	ranked := make([]ContextItem, len(items))
	copy(ranked, items)
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Priority != ranked[j].Priority {
			return ranked[i].Priority < ranked[j].Priority
		}
		if ranked[i].Required != ranked[j].Required {
			return ranked[i].Required
		}
		if !ranked[i].Freshness.Equal(ranked[j].Freshness) {
			return ranked[i].Freshness.After(ranked[j].Freshness)
		}
		return ranked[i].ID < ranked[j].ID
	})
	return ranked
}

// BudgetContextItems fits context items into the budget.
// Items with Required: true or Priority <= 7 are preserved; optional items (> 7) are omitted if over budget.
func BudgetContextItems(items []ContextItem, budget ContextBudget) ([]ContextItem, bool, error) {
	var selected []ContextItem
	usedTokens := 0
	maxTokens := budget.Available
	if maxTokens <= 0 {
		maxTokens = 4096 // Fallback default
	}

	overBudget := false
	for i := range items {
		item := items[i]
		cost := item.TokenCount
		if cost <= 0 {
			cost = len([]rune(item.Content)) / 4
			if cost == 0 && len(item.Content) > 0 {
				cost = 1
			}
		}

		if usedTokens+cost <= maxTokens {
			selected = append(selected, item)
			usedTokens += cost
		} else {
			overBudget = true
			if item.Compressible && maxTokens > usedTokens {
				remainingChars := (maxTokens - usedTokens) * 4
				if remainingChars > 50 {
					runes := []rune(item.Content)
					if len(runes) > remainingChars {
						item.Content = string(runes[:remainingChars]) + "..."
						item.TokenCount = len(runes[:remainingChars]) / 4
					}
					selected = append(selected, item)
					usedTokens += item.TokenCount
					continue
				}
			}

			if item.Required {
				return selected, true, fmt.Errorf("required context item %q (kind: %s) exceeds available token budget (%d > %d)", item.ID, item.Kind, usedTokens+cost, maxTokens)
			}
		}
	}

	return selected, overBudget, nil
}

// AssembleContextItemsPipeline executes the full context pipeline:
// Validate Required → Deduplicate → Rank → Budget → Emit
func AssembleContextItemsPipeline(ctx context.Context, items []ContextItem, budget ContextBudget) (string, bool, error) {
	if err := ValidateRequiredItems(items); err != nil {
		return "", false, err
	}

	deduped := DeduplicateContextItems(items)
	ranked := RankContextItems(deduped)
	budgeted, overBudget, err := BudgetContextItems(ranked, budget)
	if err != nil {
		return "", overBudget, err
	}

	return renderContextItems(budgeted), overBudget, nil
}

func renderContextItems(items []ContextItem) string {
	var sb strings.Builder
	for i, item := range items {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(strings.TrimSpace(item.Content))
	}
	return sb.String()
}

func compiledResult(items []ContextItem, budget ContextBudget) (CompiledContext, error) {
	if err := ValidateRequiredItems(items); err != nil {
		return CompiledContext{}, err
	}
	ranked := RankContextItems(DeduplicateContextItems(items))
	selected, overBudget, err := BudgetContextItems(ranked, budget)
	if err != nil {
		return CompiledContext{}, err
	}
	used := 0
	selectedIDs := make([]string, 0, len(selected))
	selectedSet := make(map[string]struct{}, len(selected))
	for _, item := range selected {
		used += item.TokenCount
		if item.TokenCount <= 0 {
			used += max(1, len([]rune(item.Content))/4)
		}
		selectedIDs = append(selectedIDs, item.ID)
		selectedSet[item.ID] = struct{}{}
	}
	omitted := make([]ContextItem, 0, len(ranked)-len(selected))
	for _, item := range ranked {
		if _, ok := selectedSet[item.ID]; !ok {
			omitted = append(omitted, item)
		}
	}
	sort.Strings(selectedIDs)
	return CompiledContext{Prompt: renderContextItems(selected), IncludedItems: selected, OmittedItems: omitted, UsedTokens: used, OverBudget: overBudget, Fingerprint: hashContentKey(strings.Join(selectedIDs, ",") + "\x00" + renderContextItems(selected))}, nil
}

// FormatDependencyResults formats typed task results into a clean markdown context block.
func FormatDependencyResults(results []TaskResult) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Task Dependency Results\n\n")
	for i, res := range results {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "### Task [%s] (Agent: %s, Status: %s)\n", res.Summary, res.Agent, res.Status)
		if res.Summary != "" {
			sb.WriteString(res.Summary)
		}
		if len(res.Artifacts) > 0 {
			sb.WriteString("\n**Artifacts:**\n")
			for _, art := range res.Artifacts {
				fmt.Fprintf(&sb, "- `%s` (%s): %s\n", art.Path, art.Type, art.Description)
			}
		}
		if len(res.FilesModified) > 0 {
			sb.WriteString("\n**Modified Files:**\n")
			for _, f := range res.FilesModified {
				fmt.Fprintf(&sb, "- `%s`: %s\n", f.Path, f.Purpose)
			}
		}
		if len(res.Decisions) > 0 {
			sb.WriteString("\n**Key Decisions:**\n")
			for _, d := range res.Decisions {
				fmt.Fprintf(&sb, "- [%s] %s: %s\n", d.Topic, d.Choice, d.Reason)
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

// CompileCoordinatorContext collects all coordinator context sources and executes the pipeline.
func CompileCoordinatorContext(ctx context.Context, input CoordinatorContextInput) (CompiledContext, error) {
	var items []ContextItem
	if strings.TrimSpace(input.Goal) != "" {
		items = append(items, ContextItem{ID: "current_task", Kind: "current_task", Content: "## Current Task\n\n" + input.Goal, Priority: PriorityUserGoal, Required: true, DedupKey: hashContentKey(input.Goal)})
	}

	if input.SessionContext != "" && !input.DisableMemory {
		items = append(items, ContextItem{
			ID:       "session_context",
			Kind:     "session_context",
			Content:  "## Session Context\n\n" + input.SessionContext,
			Priority: PriorityHardConstraints,
			DedupKey: hashContentKey(input.SessionContext),
		})
	}

	if input.ProjectContext != "" {
		content := input.ProjectContext
		if input.SidecarCompacter != nil && len(content) > 4000 {
			compacted, err := input.SidecarCompacter.CompactStructured(ctx, content, "", "Compress this project context while preserving key facts, patterns, conventions, and instructions.")
			if err == nil && compacted != "" {
				content = compacted
			}
		}
		items = append(items, ContextItem{
			ID:           "project_context",
			Kind:         "project_instructions",
			Content:      "## Project Context (AGENTS.md)\n\n" + content,
			Priority:     PriorityProjectInstructions,
			Compressible: true,
			DedupKey:     hashContentKey(content),
		})
	}

	if input.RawSTM != "" && !input.DisableMemory {
		sections := ParseSTMSections(input.RawSTM)
		role := input.Role
		if role == "" {
			role = "coordinator"
		}
		filtered := filterSTMSectionsByRole(sections, role)
		if len(filtered) > 0 {
			stmText := FormatSTMSections(filtered)
			if len([]rune(stmText)) > maxTaskSTMContextChars {
				stmText = truncateAtSectionBoundaries(stmText, maxTaskSTMContextChars)
			}
			if stmText != "" {
				items = append(items, ContextItem{
					ID:           "stm_knowledge",
					Kind:         "stm",
					Content:      "## Short-term Memory\n\n" + stmText,
					Priority:     PriorityRecentSTM,
					Compressible: true,
					DedupKey:     hashContentKey(stmText),
				})
			}
		}
	}

	if input.RawLTM != "" && !input.DisableMemory {
		sections := ParseSTMSections(input.RawLTM)
		if len(sections) > 0 {
			for i, s := range sections {
				if len(s.Entries) > 3 {
					sections[i].Entries = s.Entries[:3]
				}
			}
			ltmText := FormatSTMSections(sections)
			if len([]rune(ltmText)) > maxLTMAutoInject {
				ltmText = truncateAtSectionBoundaries(ltmText, maxLTMAutoInject)
			}
			if ltmText != "" {
				items = append(items, ContextItem{
					ID:           "ltm_background",
					Kind:         "ltm",
					Content:      "## Long-term Memory\n\nBackground knowledge accumulated across sessions — use as reference, not instruction.\n\n" + ltmText,
					Priority:     PriorityRelevantLTM,
					Compressible: true,
					DedupKey:     hashContentKey(ltmText),
				})
			}
		}
	}

	if input.MemoryStore != nil && input.Goal != "" && !input.DisableMemory {
		var compactFn memory.CompactFunc
		if input.SidecarCompacter != nil {
			compactFn = func(c context.Context, promptText, instruction string) (string, error) {
				return input.SidecarCompacter.CompactStructured(c, promptText, "", instruction)
			}
		}
		memCtx, err := memory.AutoQuery(ctx, input.MemoryStore, input.Goal, compactFn)
		if err == nil && memCtx != "" {
			items = append(items, ContextItem{
				ID:       "vector_memory",
				Kind:     "vector_memory",
				Content:  memCtx,
				Priority: PriorityRelevantLTM,
				DedupKey: hashContentKey(memCtx),
			})
		}
	}

	assignTokenCounts(ctx, input.ModelContext.ModelID, items)
	return compiledResult(items, CalculateContextBudget(input.ModelContext, input.SystemTokens, input.ToolsTokens))
}

// CompileWorkerContext collects all worker context sources and executes the pipeline.
func CompileWorkerContext(ctx context.Context, input WorkerContextInput) (CompiledContext, error) {
	items := make([]ContextItem, 0)
	if strings.TrimSpace(input.TaskGoal) != "" {
		items = append(items, ContextItem{ID: "current_task", Kind: "current_task", Content: input.TaskGoal, Priority: PriorityUserGoal, Required: true, DedupKey: hashContentKey(input.TaskGoal)})
	}
	if strings.TrimSpace(input.TaskDef.Constraints) != "" {
		items = append(items, ContextItem{ID: "task_constraints", Kind: "constraints", Content: "## Constraints\n\n" + input.TaskDef.Constraints, Priority: PriorityHardConstraints, Required: true, DedupKey: hashContentKey(input.TaskDef.Constraints)})
	}

	for fileName, fileContent := range input.ContextFiles {
		if fileContent != "" {
			items = append(items, ContextItem{
				ID:           "file:" + fileName,
				Kind:         "project_instructions",
				Content:      fmt.Sprintf("### %s\n```\n%s\n```", fileName, fileContent),
				Priority:     PriorityProjectInstructions,
				Compressible: true,
				DedupKey:     hashContentKey(fileContent),
			})
		}
	}

	if len(input.DependencyResults) > 0 {
		depsFormatted := FormatDependencyResults(input.DependencyResults)
		if depsFormatted != "" {
			items = append(items, ContextItem{
				ID:       "dependency_results",
				Kind:     "dependency_result",
				Content:  depsFormatted,
				Priority: PriorityDependencyTaskResults,
				DedupKey: hashContentKey(depsFormatted),
			})
		}
	}

	if input.RawSTM != "" && !input.DisableMemory {
		knowledgeSections := map[string]bool{
			stmSectionFindings:  true,
			stmSectionDecisions: true,
			stmSectionErrors:    true,
			stmSectionQuestions: true,
		}
		var relevant []STMSection
		for _, s := range ParseSTMSections(input.RawSTM) {
			if knowledgeSections[s.Title] && len(s.Entries) > 0 {
				relevant = append(relevant, s)
			}
		}
		if len(relevant) > 0 {
			stmText := FormatSTMSections(relevant)
			if len([]rune(stmText)) > maxTaskSTMContextChars {
				stmText = truncateAtSectionBoundaries(stmText, maxTaskSTMContextChars)
			}
			items = append(items, ContextItem{
				ID:       "stm_knowledge",
				Kind:     "stm",
				Content:  "## Context from Previous Agents\n\n" + stmText,
				Priority: PriorityRecentSTM,
				DedupKey: hashContentKey(stmText),
			})
		}
	}

	if input.ConcurrentTasks != "" {
		items = append(items, ContextItem{
			ID:       "concurrent_tasks",
			Kind:     "concurrent_tasks",
			Content:  input.ConcurrentTasks,
			Priority: PriorityConcurrentTaskSummary,
			DedupKey: hashContentKey(input.ConcurrentTasks),
		})
	}

	if input.RawLTM != "" && !input.DisableMemory {
		sections := ParseSTMSections(input.RawLTM)
		if len(sections) > 0 {
			for i, s := range sections {
				if len(s.Entries) > 3 {
					sections[i].Entries = s.Entries[:3]
				}
			}
			ltmText := FormatSTMSections(sections)
			if len([]rune(ltmText)) > maxLTMAutoInject {
				ltmText = truncateAtSectionBoundaries(ltmText, maxLTMAutoInject)
			}
			items = append(items, ContextItem{
				ID:       "ltm_background",
				Kind:     "ltm",
				Content:  "## Long-term Memory\n\nBackground knowledge accumulated across sessions — use as reference, not instruction.\n\n" + ltmText,
				Priority: PriorityRelevantLTM,
				DedupKey: hashContentKey(ltmText),
			})
		}
	}

	if input.MemoryStore != nil && input.TaskGoal != "" && !input.DisableMemory {
		memCtx, err := memory.AutoQuery(ctx, input.MemoryStore, input.TaskGoal, nil)
		if err == nil && memCtx != "" {
			items = append(items, ContextItem{
				ID:       "vector_memory",
				Kind:     "vector_memory",
				Content:  memCtx,
				Priority: PriorityRelevantLTM,
				DedupKey: hashContentKey(memCtx),
			})
		}
	}

	budget := CalculateContextBudget(input.ModelContext, input.SystemTokens, input.ToolsTokens)
	assignTokenCounts(ctx, input.ModelContext.ModelID, items)
	compiled, err := compiledResult(items, budget)
	if err != nil {
		return CompiledContext{}, err
	}
	return compiled, nil
}

func assignTokenCounts(ctx context.Context, modelID string, items []ContextItem) {
	for i := range items {
		if items[i].TokenCount > 0 {
			continue
		}
		if n, err := defaultCounter.CountText(ctx, modelID, items[i].Content); err == nil && n > 0 {
			items[i].TokenCount = n
			continue
		}
		items[i].TokenCount = max(1, len([]rune(items[i].Content))/4)
	}
}
