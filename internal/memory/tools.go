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
		},
		Required: []string{"content"},
	}
}

func (t *memorySaveTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *memorySaveTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *memorySaveTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Content  string `json:"content"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}

	id := fmt.Sprintf("%d", time.Now().UnixNano())

	metadata := map[string]string{
		"category": args.Category,
	}

	if t.store == nil {
		return fantasy.NewTextErrorResponse("memory is not available"), nil
	}

	if err := t.store.Save(ctx, id, args.Content, metadata); err != nil {
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
		Description: "Search long-term memory for knowledge relevant to a query. Returns the most semantically similar memories. Use this to recall past discoveries, decisions, or knowledge before starting work.",
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
		},
		Required: []string{"query"},
	}
}

func (t *memoryQueryTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *memoryQueryTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *memoryQueryTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Query    string `json:"query"`
		N        int    `json:"n"`
		Category string `json:"category"`
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

	var filter map[string]string
	if args.Category != "" {
		filter = map[string]string{"category": args.Category}
	}

	if t.store == nil {
		return fantasy.NewTextErrorResponse("memory is not available"), nil
	}

	results, err := t.store.Query(ctx, args.Query, n, filter)
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
		if r.Metadata != nil && r.Metadata["category"] != "" {
			cat = fmt.Sprintf(" [%s]", r.Metadata["category"])
		}
		savedAt := ""
		if r.Metadata != nil && r.Metadata["saved_at"] != "" {
			savedAt = fmt.Sprintf(" (saved: %s)", r.Metadata["saved_at"])
		}
		b.WriteString(fmt.Sprintf("- [%.2f]%s %s%s", r.Similarity, cat, r.Content, savedAt))
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func NewMemorySaveTool(store *MemoryStore) fantasy.AgentTool {
	return &memorySaveTool{store: store}
}

func NewMemoryQueryTool(store *MemoryStore) fantasy.AgentTool {
	return &memoryQueryTool{store: store}
}
