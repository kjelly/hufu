package team

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

// TokenCounter measures token counts for text, messages, and tool definitions.
type TokenCounter interface {
	CountText(ctx context.Context, modelID, text string) (int, error)
	CountMessages(ctx context.Context, modelID string, messages []fantasy.Message) (int, error)
	CountTools(ctx context.Context, modelID string, tools []fantasy.AgentTool) (int, error)
}

// ModelContextSpec defines context limits and estimation parameters for a model.
type ModelContextSpec struct {
	ModelID             string `json:"model_id"`
	ContextWindow       int    `json:"context_window"`
	ContextWindowSource string `json:"context_window_source,omitempty"`
	MaxOutputTokens     int    `json:"max_output_tokens"`
	SafetyMarginTokens  int    `json:"safety_margin_tokens"`
	Estimator           string `json:"estimator"` // e.g. "tiktoken", "claude", "qwen", "llama", "estimated"
	IsEstimated         bool   `json:"is_estimated"`
}

// WithEffectiveMaxOutputTokens returns a copy of spec whose MaxOutputTokens
// reflects the caller's actually-configured generation max-tokens
// (agent.md / team.yaml / CLI) instead of the static per-model-family
// registry default. The context budget must reserve however much output the
// request will really allow (spec.md item 2); a non-positive effective
// value leaves spec unchanged, falling back to the registry's guess.
func (spec ModelContextSpec) WithEffectiveMaxOutputTokens(effective int) ModelContextSpec {
	if effective > 0 {
		spec.MaxOutputTokens = effective
	}
	return spec
}

// ContextBudget breaks down token allocation across prompt sections and output reply.
type ContextBudget struct {
	Window        int `json:"window"`
	System        int `json:"system"`
	Tools         int `json:"tools"`
	ReservedReply int `json:"reserved_reply"`
	SafetyMargin  int `json:"safety_margin"`
	Available     int `json:"available"`
}

// ContextUsageBreakdown captures token usage across context subsystems for reports.
type ContextUsageBreakdown struct {
	SystemInstructions    int `json:"system_instructions"`
	ToolSchemas           int `json:"tool_schemas"`
	RecentConversation    int `json:"recent_conversation"`
	CompactedHistory      int `json:"compacted_history"`
	ProjectContext        int `json:"project_context"`
	StmLtmRag             int `json:"stm_ltm_rag"`
	TaskDependencyResults int `json:"task_dependency_results"`
	ReplyReserve          int `json:"reply_reserve"`
}

// BreakdownReport formats a context usage breakdown table matching §5.4.
func (b ContextBudget) BreakdownReport(usage ContextUsageBreakdown) string {
	total := usage.SystemInstructions + usage.ToolSchemas + usage.RecentConversation +
		usage.CompactedHistory + usage.ProjectContext + usage.StmLtmRag +
		usage.TaskDependencyResults + usage.ReplyReserve

	var sb strings.Builder
	fmt.Fprintf(&sb, "Context usage: %s / %s\n\n", formatNumber(total), formatNumber(b.Window))
	fmt.Fprintf(&sb, "%-25s %8s\n", "System instructions", formatNumber(usage.SystemInstructions))
	fmt.Fprintf(&sb, "%-25s %8s\n", "Tool schemas", formatNumber(usage.ToolSchemas))
	fmt.Fprintf(&sb, "%-25s %8s\n", "Recent conversation", formatNumber(usage.RecentConversation))
	fmt.Fprintf(&sb, "%-25s %8s\n", "Compacted history", formatNumber(usage.CompactedHistory))
	fmt.Fprintf(&sb, "%-25s %8s\n", "Project context", formatNumber(usage.ProjectContext))
	fmt.Fprintf(&sb, "%-25s %8s\n", "STM/LTM/RAG", formatNumber(usage.StmLtmRag))
	fmt.Fprintf(&sb, "%-25s %8s\n", "Task dependency results", formatNumber(usage.TaskDependencyResults))
	fmt.Fprintf(&sb, "%-25s %8s\n", "Reply reserve", formatNumber(usage.ReplyReserve))
	return sb.String()
}

func formatNumber(n int) string {
	in := fmt.Sprintf("%d", n)
	out := make([]byte, 0, len(in)+len(in)/3)
	for i, c := range in {
		if i > 0 && (len(in)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// ModelSpecRegistry maintains model specs and estimators.
type ModelSpecRegistry struct {
	mu    sync.RWMutex
	specs map[string]ModelContextSpec
}

var globalRegistry = NewModelSpecRegistry()

var observedContextCapacityRE = regexp.MustCompile(`(?i)(?:available context size|context window|context length|context size|max(?:imum)? context(?: size)?)\D+(\d+)`)

// estimatedModelLogger is invoked once per process for each model whose spec is
// derived from a fallback estimator rather than a known registry entry (§5.3:
// "log 標記 estimated"). It defaults to a stderr warning via the stdlib log
// package and is swappable for tests.
var estimatedModelLogger = func(modelID, estimator string) {
	log.Printf("warning: no exact tokenizer for model %q; using %q estimator fallback (token counts are estimated)", modelID, estimator)
}

// estimatedWarnSeen dedups estimated-model warnings so each model is logged once.
var estimatedWarnSeen sync.Map

func warnEstimatedOnce(modelID, estimator string) {
	if modelID == "" {
		return
	}
	if _, loaded := estimatedWarnSeen.LoadOrStore(modelID, struct{}{}); loaded {
		return
	}
	estimatedModelLogger(modelID, estimator)
}

func NewModelSpecRegistry() *ModelSpecRegistry {
	r := &ModelSpecRegistry{
		specs: make(map[string]ModelContextSpec),
	}
	r.initDefaults()
	return r
}

func (r *ModelSpecRegistry) initDefaults() {
	// Default model specs for known families
	defaults := []ModelContextSpec{
		{ModelID: "gpt-4o", ContextWindow: 128000, MaxOutputTokens: 16384, SafetyMarginTokens: 2000, Estimator: "tiktoken"},
		{ModelID: "gpt-4", ContextWindow: 8192, MaxOutputTokens: 4096, SafetyMarginTokens: 1000, Estimator: "tiktoken"},
		{ModelID: "claude-3-5-sonnet", ContextWindow: 200000, MaxOutputTokens: 8192, SafetyMarginTokens: 4000, Estimator: "claude"},
		{ModelID: "claude-3-7-sonnet", ContextWindow: 200000, MaxOutputTokens: 8192, SafetyMarginTokens: 4000, Estimator: "claude"},
		{ModelID: "qwen2.5", ContextWindow: 128000, MaxOutputTokens: 8192, SafetyMarginTokens: 2000, Estimator: "qwen"},
		{ModelID: "qwen3", ContextWindow: 128000, MaxOutputTokens: 8192, SafetyMarginTokens: 2000, Estimator: "qwen"},
		{ModelID: "llama3.1", ContextWindow: 128000, MaxOutputTokens: 8192, SafetyMarginTokens: 2000, Estimator: "llama"},
		{ModelID: "llama3.2", ContextWindow: 128000, MaxOutputTokens: 8192, SafetyMarginTokens: 2000, Estimator: "llama"},
	}
	for _, spec := range defaults {
		r.specs[spec.ModelID] = spec
	}
}

// GlobalModelSpecRegistry returns the process-wide model spec registry used
// by CalculateContextBudget's callers throughout this package. Exposed so
// callers outside internal/team (team setup in cmd/hufu) can register
// runtime-detected specs from a provider's model metadata.
func GlobalModelSpecRegistry() *ModelSpecRegistry {
	return globalRegistry
}

// RegisterConfiguredContextWindow applies an operator-declared context
// capacity to the models participating in the active team. It changes only
// admission capacity; model-specific output and safety budgets remain intact.
// An omitted value intentionally leaves estimated models fail-closed.
func RegisterConfiguredContextWindow(modelIDs []string, window int) {
	if window <= 0 {
		return
	}
	seen := make(map[string]struct{}, len(modelIDs))
	for _, modelID := range modelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		key := strings.ToLower(modelID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		spec := globalRegistry.GetSpec(modelID)
		spec.ModelID = modelID
		spec.ContextWindow = window
		spec.ContextWindowSource = "operator"
		spec.IsEstimated = false
		globalRegistry.RegisterSpec(spec)
	}
}

// DetectAndCacheProviderContextLengths probes baseURL's OpenAI-compatible
// /models endpoint for each model in modelIDs and registers its advertised
// context length as an override in the global model spec registry, so
// context-budget accounting reflects the model actually being talked to
// instead of Hufu's static per-family fallback (spec.md item 2). Only
// models whose current spec is already flagged estimated are probed —
// models with an exact hardcoded entry (e.g. "gpt-4o", "claude-3-5-sonnet")
// are skipped, since those specs are already accurate and probing them
// would just be a wasted round-trip to the provider.
//
// Best-effort and bounded: each probe races against its own timeout, probes
// run concurrently, and a failed or unreachable endpoint is silently
// skipped per model rather than treated as an error.
func DetectAndCacheProviderContextLengths(ctx context.Context, baseURL, apiKey string, modelIDs []string) {
	seen := make(map[string]bool, len(modelIDs))
	var wg sync.WaitGroup
	for _, modelID := range modelIDs {
		if modelID == "" || seen[modelID] {
			continue
		}
		seen[modelID] = true
		if !globalRegistry.GetSpec(modelID).IsEstimated {
			continue
		}
		wg.Add(1)
		go func(modelID string) {
			defer wg.Done()
			_, name := agent.ParseModelProvider(modelID)
			probeCtx, cancel := context.WithTimeout(ctx, agent.ProviderContextProbeTimeout)
			defer cancel()
			capacity, err := agent.DetectProviderContextCapacity(probeCtx, baseURL, apiKey, name)
			length := capacity.ContextWindow
			if err != nil || length <= 0 {
				return
			}
			spec := globalRegistry.GetSpec(modelID)
			spec.ModelID = strings.ToLower(modelID)
			spec.ContextWindow = length
			spec.ContextWindowSource = capacity.Source
			spec.IsEstimated = false
			globalRegistry.RegisterSpec(spec)
		}(modelID)
	}
	wg.Wait()
}

// DetectAndCacheOllamaContextLengths is retained for source compatibility.
func DetectAndCacheOllamaContextLengths(ctx context.Context, baseURL, apiKey string, modelIDs []string) {
	DetectAndCacheProviderContextLengths(ctx, baseURL, apiKey, modelIDs)
}

func (r *ModelSpecRegistry) RegisterSpec(spec ModelContextSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs[strings.ToLower(spec.ModelID)] = spec
}

// RegisterObservedContextWindow records a runtime limit reported by a
// provider. Runtime observations are authoritative for the current process,
// but never persisted as model metadata: another local server may expose a
// different hardware-dependent window on the next run.
func (r *ModelSpecRegistry) RegisterObservedContextWindow(modelID string, window int) {
	if r == nil || window <= 0 || strings.TrimSpace(modelID) == "" {
		return
	}
	spec := r.GetSpec(modelID)
	if spec.ContextWindow > 0 && spec.ContextWindow <= window && spec.ContextWindowSource == agent.ContextCapacitySourceObserved {
		return
	}
	spec.ModelID = strings.ToLower(modelID)
	spec.ContextWindow = window
	spec.ContextWindowSource = agent.ContextCapacitySourceObserved
	spec.IsEstimated = false
	r.RegisterSpec(spec)
}

// ParseObservedContextWindow extracts a provider-reported effective context
// size from a context overflow error. The parser is intentionally generic and
// does not name Lemonade, llama.cpp, Ollama, or any other vendor.
func ParseObservedContextWindow(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	matches := observedContextCapacityRE.FindStringSubmatch(err.Error())
	if len(matches) != 2 {
		return 0, false
	}
	var window int
	if _, scanErr := fmt.Sscanf(matches[1], "%d", &window); scanErr != nil || window <= 0 {
		return 0, false
	}
	return window, true
}

func (r *ModelSpecRegistry) GetSpec(modelID string) ModelContextSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	lower := strings.ToLower(modelID)
	// Strip provider prefix if present e.g. "ollama/qwen3:8b" -> "qwen3:8b"
	parts := strings.Split(lower, "/")
	name := parts[len(parts)-1]
	nameClean := strings.Split(name, ":")[0]

	if spec, ok := r.specs[lower]; ok {
		return spec
	}
	if spec, ok := r.specs[name]; ok {
		return spec
	}
	if spec, ok := r.specs[nameClean]; ok {
		return spec
	}

	// Fallback estimation matching model families
	estimator := "estimated"
	window := 128000
	maxOutput := 4096
	margin := 2000

	if strings.Contains(name, "claude") {
		estimator = "claude"
		window = 200000
		maxOutput = 8192
		margin = 4000
	} else if strings.Contains(name, "qwen") {
		estimator = "qwen"
		window = 128000
		maxOutput = 8192
	} else if strings.Contains(name, "llama") {
		estimator = "llama"
		window = 128000
		maxOutput = 8192
	} else if strings.Contains(name, "gpt-4") || strings.Contains(name, "o1") || strings.Contains(name, "o3") {
		estimator = "tiktoken"
		window = 128000
		maxOutput = 16384
	}

	spec := ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      window,
		MaxOutputTokens:    maxOutput,
		SafetyMarginTokens: margin,
		Estimator:          estimator,
		IsEstimated:        true,
	}
	// §5.3: record/log that this model's token counts are estimated, not exact.
	warnEstimatedOnce(modelID, estimator)
	return spec
}

// DefaultTokenCounter provides model-aware and family-based token counting with conservative margin.
type DefaultTokenCounter struct {
	registry *ModelSpecRegistry
}

func NewDefaultTokenCounter(r *ModelSpecRegistry) *DefaultTokenCounter {
	if r == nil {
		r = globalRegistry
	}
	return &DefaultTokenCounter{registry: r}
}

var defaultCounter = NewDefaultTokenCounter(globalRegistry)

func (tc *DefaultTokenCounter) CountText(ctx context.Context, modelID, text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	spec := tc.registry.GetSpec(modelID)
	return estimateTextTokens(text, spec.Estimator), nil
}

func (tc *DefaultTokenCounter) CountMessages(ctx context.Context, modelID string, messages []fantasy.Message) (int, error) {
	if len(messages) == 0 {
		return 0, nil
	}
	spec := tc.registry.GetSpec(modelID)
	total := 0
	for _, msg := range messages {
		// Message role overhead (approx 4 tokens per message for formatting tags)
		total += 4
		for _, part := range msg.Content {
			switch part.GetType() {
			case fantasy.ContentTypeText:
				if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
					total += estimateTextTokens(p.Text, spec.Estimator)
				}
			case fantasy.ContentTypeReasoning:
				if p, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
					total += estimateTextTokens(p.Text, spec.Estimator)
				}
			case fantasy.ContentTypeToolCall:
				if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
					total += estimateTextTokens(p.ToolName+" "+p.Input, spec.Estimator)
				}
			case fantasy.ContentTypeToolResult:
				if p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
					txt, _ := toolResultOutputText(p.Output)
					total += estimateTextTokens(txt, spec.Estimator)
				}
			}
		}
	}
	return total, nil
}

func (tc *DefaultTokenCounter) CountTools(ctx context.Context, modelID string, tools []fantasy.AgentTool) (int, error) {
	if len(tools) == 0 {
		return 0, nil
	}
	spec := tc.registry.GetSpec(modelID)
	total := 0
	for _, tool := range tools {
		toolDef := tool.Info()
		text := fmt.Sprintf("Tool: %s Description: %s Parameters: %v", toolDef.Name, toolDef.Description, toolDef.Parameters)
		total += estimateTextTokens(text, spec.Estimator)
	}
	return total, nil
}

// estimateTextTokens calculates tokens using character density and model-family heuristics with conservative safety margin.
func estimateTextTokens(text string, estimator string) int {
	runes := []rune(text)
	if len(runes) == 0 {
		return 0
	}

	var cjkCount, codeCount, asciiCount int
	for _, r := range runes {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			cjkCount++
		} else if r == '{' || r == '}' || r == '[' || r == ']' || r == ';' || r == ':' || r == '=' || r == '<' || r == '>' || r == '/' || r == '\\' {
			codeCount++
		} else {
			asciiCount++
		}
	}

	// Density heuristics:
	// CJK: ~0.67 tokens/char (1 token per 1.5 chars)
	// Code/JSON/YAML: ~0.35 tokens/char (1 token per 2.8 chars)
	// ASCII/English: ~0.25 tokens/char (1 token per 4 chars)
	rawTokens := float64(cjkCount)*0.67 + float64(codeCount)*0.35 + float64(asciiCount)*0.25

	// Conservative safety margin of 15% (1.15) for estimated fallbacks as specified in §5.3
	multiplier := 1.15
	switch estimator {
	case "tiktoken", "claude":
		multiplier = 1.05
	case "qwen":
		multiplier = 1.10
	}

	estimated := int(rawTokens * multiplier)
	if estimated == 0 && len(runes) > 0 {
		return 1
	}
	return estimated
}

// CalculateContextBudget computes the available context budget given model spec and system/tools usage.
func CalculateContextBudget(spec ModelContextSpec, systemTokens, toolsTokens int) ContextBudget {
	window := spec.ContextWindow
	if window <= 0 {
		window = 128000
	}
	reserved := spec.MaxOutputTokens
	if reserved <= 0 {
		reserved = 4096
	}
	margin := spec.SafetyMarginTokens
	if margin <= 0 {
		margin = 2000
	}

	avail := window - systemTokens - toolsTokens - reserved - margin
	if avail < 0 {
		avail = 0
	}
	return ContextBudget{
		Window:        window,
		System:        systemTokens,
		Tools:         toolsTokens,
		ReservedReply: reserved,
		SafetyMargin:  margin,
		Available:     avail,
	}
}

// TruncateToTokenBudget returns a prefix of text whose estimated token count
// (via estimator) is at most maxTokens, appending a truncation marker when
// text had to be cut. Unlike CapStepMessagesWithCounter (which shapes chat
// messages), this operates on a single raw string — e.g. project context
// injected once per session (spec.md item 7: worker-context-size must be a
// token budget, not a character count, to stay consistent with the rest of
// Hufu's token-aware context accounting).
//
// estimateTextTokens's weighted character-category sum only grows as text
// grows, so a binary search over the rune-length cutoff finds the longest
// prefix within budget.
func TruncateToTokenBudget(text, estimator string, maxTokens int) string {
	if maxTokens <= 0 || estimateTextTokens(text, estimator) <= maxTokens {
		return text
	}
	runes := []rune(text)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if estimateTextTokens(string(runes[:mid]), estimator) <= maxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]) + "\n\n... [truncated]"
}

// IsContextOverflowError checks if an error indicates a model context length overflow.
func IsContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context length") ||
		strings.Contains(msg, "context_length_exceeded") ||
		strings.Contains(msg, "maximum context length") ||
		strings.Contains(msg, "too many tokens") ||
		strings.Contains(msg, "token limit") ||
		strings.Contains(msg, "context overflow") ||
		strings.Contains(msg, "prompt is too long") ||
		strings.Contains(msg, "exceeds context") ||
		strings.Contains(msg, "context window")
}
