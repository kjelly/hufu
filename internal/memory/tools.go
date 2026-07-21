package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

type memorySaveTool struct {
	store *MemoryStore
	pOpts fantasy.ProviderOptions
}

func (t *memorySaveTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "memory_save",
		Description: "Save a piece of knowledge to long-term memory so it can be retrieved later by you or other agents. Use this to persist important discoveries, decisions, API details, or other reusable knowledge across sessions.",
		Parameters: map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The knowledge or information to save. Be specific and self-contained.",
			},
			"category": map[string]any{
				"type":        "string",
				"description": "Optional category for organizing memories (e.g. 'api-discovery', 'decision', 'architecture', 'bug')",
			},
			"confidence": map[string]any{
				"type":        "number",
				"description": "Optional confidence score between 0.0 and 1.0 (default: 1.0 for verified/confirmed facts, 0.8 for initial findings)",
			},
			"supersedes": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional list of memory IDs that this new memory supersedes/replaces",
			},
			"file_paths": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional file paths associated with this memory",
			},
		},
		Required: []string{"content"},
	}
}

func (t *memorySaveTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *memorySaveTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *memorySaveTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Content    string   `json:"content"`
		Category   string   `json:"category"`
		Confidence float64  `json:"confidence"`
		Supersedes []string `json:"supersedes"`
		FilePaths  []string `json:"file_paths"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}

	if t.store == nil {
		return fantasy.NewTextErrorResponse("memory is not available"), nil
	}

	id := fmt.Sprintf("%d", time.Now().UnixNano())
	rec := MemoryRecord{
		ID:         id,
		Content:    args.Content,
		Category:   args.Category,
		Confidence: args.Confidence,
		FilePaths:  args.FilePaths,
		Status:     StatusConfirmed,
		CreatedAt:  time.Now(),
	}

	var err error
	if len(args.Supersedes) > 0 {
		err = t.store.SupersedeRecord(ctx, rec, args.Supersedes)
	} else {
		err = t.store.SaveRecord(ctx, rec)
	}

	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to save memory: %v", err)), nil
	}

	catMsg := ""
	if args.Category != "" {
		catMsg = fmt.Sprintf(" in category '%s'", args.Category)
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Saved to memory%s (id: %s)", catMsg, id)), nil
}

type memoryQueryTool struct {
	store *MemoryStore
	pOpts fantasy.ProviderOptions
}

func (t *memoryQueryTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "memory_query",
		Description: "Search long-term memory for knowledge relevant to a query. Returns the most semantically and contextually relevant memories. Use this to recall past discoveries, decisions, or knowledge before starting work.",
		Parameters: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query describing what knowledge you're looking for",
			},
			"n": map[string]any{
				"type":        "number",
				"description": "Number of results to return (default: 5, max: 20)",
			},
			"category": map[string]any{
				"type":        "string",
				"description": "Optional category filter to narrow results",
			},
			"min_confidence": map[string]any{
				"type":        "number",
				"description": "Optional minimum confidence score threshold (e.g., 0.5)",
			},
			"file_paths": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional file paths to boost relevance for matching memories",
			},
		},
		Required: []string{"query"},
	}
}

func (t *memoryQueryTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *memoryQueryTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *memoryQueryTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Query         string   `json:"query"`
		N             int      `json:"n"`
		Category      string   `json:"category"`
		MinConfidence float64  `json:"min_confidence"`
		FilePaths     []string `json:"file_paths"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Query == "" {
		return fantasy.NewTextErrorResponse("query is required"), nil
	}

	n := args.N
	if n <= 0 {
		n = 5
	}
	if n > 20 {
		n = 20
	}

	if t.store == nil {
		return fantasy.NewTextErrorResponse("memory is not available"), nil
	}

	results, err := t.store.QueryRecords(ctx, QueryOptions{
		Query:         args.Query,
		N:             n,
		Category:      args.Category,
		MinConfidence: args.MinConfidence,
		FilePaths:     args.FilePaths,
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("memory query failed: %v", err)), nil
	}

	if len(results) == 0 {
		return fantasy.NewTextResponse("No relevant memories found."), nil
	}

	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		cat := ""
		if r.Record.Category != "" {
			cat = fmt.Sprintf(" [%s]", r.Record.Category)
		}
		savedAt := ""
		if !r.Record.CreatedAt.IsZero() {
			savedAt = fmt.Sprintf(" (saved: %s)", r.Record.CreatedAt.Format(time.RFC3339))
		}
		statusTag := ""
		if eff := r.Record.EffectiveStatus(); eff != StatusConfirmed {
			statusTag = fmt.Sprintf(" [%s]", eff)
		}
		fmt.Fprintf(&b, "- [%.2f]%s%s %s%s (id: %s, confidence: %.2f)", r.Score, cat, statusTag, r.Record.Content, savedAt, r.Record.ID, r.Record.Confidence)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func NewMemorySaveTool(store *MemoryStore) fantasy.AgentTool {
	return &memorySaveTool{store: store}
}

func NewMemoryQueryTool(store *MemoryStore) fantasy.AgentTool {
	return &memoryQueryTool{store: store}
}
