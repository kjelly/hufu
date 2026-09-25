package team

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

const (
	maxActionListResults         = 50
	maxActionQueryBytes          = 128
	teamActionSchemaDialect      = "hufu-run-input-schema/v1"
	teamActionCallerInvalid      = "team_action_caller_invalid"
	teamActionNotDiscoverable    = "team_action_not_discoverable"
	teamActionInvalidToolRequest = "team_action_request_invalid"
)

// staticTeamActionToolNames returns the catalog protocol tools def may use:
// list and get for any entry it can discover. It depends only on the session
// and the agent, so offline lint resolves the same surface as runtime.
func staticTeamActionToolNames(session *TeamSession, def *agent.AgentDef) []string {
	if session == nil || session.ActionCatalog == nil || def == nil {
		return nil
	}
	name := normalizedName(def.Name)
	for _, entry := range session.ActionCatalog.Entries {
		if slices.Contains(entry.Discover, name) {
			return []string{teamActionListToolName, teamActionGetToolName}
		}
	}
	return nil
}

// discoverableEntries returns the entries agentName may discover, by ID.
func (s *ActionCatalogSnapshot) discoverableEntries(agentName string) []ActionCatalogEntry {
	if s == nil {
		return nil
	}
	agentName = normalizedName(agentName)
	var entries []ActionCatalogEntry
	for _, entry := range s.Entries {
		if slices.Contains(entry.Discover, agentName) {
			entries = append(entries, entry)
		}
	}
	return entries
}

type teamActionDirectInvocationKey struct{}

// withDirectAgentInvocation marks a direct-agent run, which has no
// coordinator to dispatch catalog actions and so gets no catalog tools.
func withDirectAgentInvocation(ctx context.Context) context.Context {
	return context.WithValue(ctx, teamActionDirectInvocationKey{}, true)
}

func directAgentInvocation(ctx context.Context) bool {
	value, _ := ctx.Value(teamActionDirectInvocationKey{}).(bool)
	return value
}

// workerTeamActionTools builds the concrete catalog tools for a resolved
// surface. They need a real task occurrence and are never built for a leaf
// extra-model execution or a direct-agent run.
func (c *Coordinator) workerTeamActionTools(ctx context.Context, def *agent.AgentDef, names []string, req WorkerToolResolutionRequest) []fantasy.AgentTool {
	if c == nil || c.session == nil || c.session.ActionCatalog == nil || def == nil || strings.TrimSpace(req.TodoID) == "" {
		return nil
	}
	if ctx.Value(leafExecutionKey{}) != nil || directAgentInvocation(ctx) || taskToolResolutionTodo(c, req) == nil {
		return nil
	}
	agentName := normalizedName(def.Name)
	var built []fantasy.AgentTool
	if slices.Contains(names, teamActionListToolName) {
		built = append(built, &teamActionListTool{coordinator: c, todoID: req.TodoID, agent: agentName})
	}
	if slices.Contains(names, teamActionGetToolName) {
		built = append(built, &teamActionGetTool{coordinator: c, todoID: req.TodoID, agent: agentName})
	}
	return built
}

// teamActionCallerValid confirms a worker catalog tool runs for the task and
// agent it was built for, outside any leaf or protocol-repair execution.
func teamActionCallerValid(ctx context.Context, todoID, agentName string) bool {
	if current, _ := ctx.Value(todoIDKey{}).(string); current != todoID {
		return false
	}
	if current, _ := ctx.Value(tools.AgentNameKey).(string); !strings.EqualFold(strings.TrimSpace(current), agentName) {
		return false
	}
	return ctx.Value(leafExecutionKey{}) == nil && !protocolRepairExecution(ctx)
}

func teamActionErrorResponse(code, detail string) fantasy.ToolResponse {
	return fantasy.NewTextErrorResponse(code + ": " + detail)
}

func teamActionJSONResponse(value any) (fantasy.ToolResponse, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("encode team action response: %w", err)
	}
	return fantasy.NewTextResponse(string(encoded)), nil
}

// decodeTeamActionInput strictly decodes a catalog tool's arguments.
func decodeTeamActionInput(raw string, target any) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	decoded, err := decodeUniqueJSON([]byte(raw))
	if err != nil {
		return err
	}
	if _, ok := decoded.(map[string]any); !ok {
		return fmt.Errorf("arguments must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

type teamActionSummary struct {
	ID              string          `json:"id"`
	Description     string          `json:"description"`
	SideEffect      SideEffectClass `json:"side_effect"`
	ProposalAllowed bool            `json:"proposal_allowed"`
}

type teamActionListResult struct {
	Actions   []teamActionSummary `json:"actions"`
	Truncated bool                `json:"truncated"`
}

type teamActionContract struct {
	ID              string          `json:"id"`
	Description     string          `json:"description"`
	SideEffect      SideEffectClass `json:"side_effect"`
	Recovery        RecoveryPolicy  `json:"recovery"`
	InputSchema     RunInputSchema  `json:"input_schema"`
	OutputSchema    *RunInputSchema `json:"output_schema,omitempty"`
	ProposalAllowed bool            `json:"proposal_allowed"`
	RequireProposal bool            `json:"require_proposal"`
	SchemaDialect   string          `json:"schema_dialect"`
}

// matchTeamActionQuery reports whether a case-insensitive query appears in an
// entry's ID or description. An empty query matches every entry.
func matchTeamActionQuery(entry ActionCatalogEntry, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	return query == "" || strings.Contains(entry.ID, query) || strings.Contains(strings.ToLower(entry.Description), query)
}

func validTeamActionQuery(query string) error {
	if len(query) > maxActionQueryBytes || !utf8.ValidString(query) {
		return fmt.Errorf("query must be valid UTF-8 of at most %d bytes", maxActionQueryBytes)
	}
	return nil
}

type teamActionListTool struct {
	coordinator *Coordinator
	todoID      string
	agent       string
}

func (*teamActionListTool) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{}
}
func (*teamActionListTool) boundArtifactPolicyTool() {}
func (*teamActionListTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (*teamActionListTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (*teamActionListTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: teamActionListToolName,
		Description: "List the predefined runtime actions this team lets you inspect. Workers cannot run actions; " +
			"the coordinator decides whether to dispatch one. Use team_action_get for an action's input schema.",
		Parameters: map[string]any{
			"query": map[string]any{"type": "string", "description": "Optional case-insensitive text to match in action IDs and descriptions."},
		},
	}
}

func (t *teamActionListTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if !teamActionCallerValid(ctx, t.todoID, t.agent) {
		return teamActionErrorResponse(teamActionCallerInvalid, "this tool belongs to a different task or agent"), nil
	}
	var input struct {
		Query string `json:"query"`
	}
	if err := decodeTeamActionInput(call.Input, &input); err != nil {
		return teamActionErrorResponse(teamActionInvalidToolRequest, err.Error()), nil
	}
	if err := validTeamActionQuery(input.Query); err != nil {
		return teamActionErrorResponse(teamActionInvalidToolRequest, err.Error()), nil
	}
	result := teamActionListResult{Actions: []teamActionSummary{}}
	for _, entry := range t.coordinator.session.ActionCatalog.discoverableEntries(t.agent) {
		if !matchTeamActionQuery(entry, input.Query) {
			continue
		}
		if len(result.Actions) == maxActionListResults {
			result.Truncated = true
			break
		}
		result.Actions = append(result.Actions, teamActionSummary{
			ID: entry.ID, Description: entry.Description, SideEffect: entry.SideEffect,
			ProposalAllowed: slices.Contains(entry.Propose, t.agent),
		})
	}
	return teamActionJSONResponse(result)
}

type teamActionGetTool struct {
	coordinator *Coordinator
	todoID      string
	agent       string
}

func (*teamActionGetTool) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{}
}
func (*teamActionGetTool) boundArtifactPolicyTool()                   {}
func (*teamActionGetTool) ProviderOptions() fantasy.ProviderOptions   { return fantasy.ProviderOptions{} }
func (*teamActionGetTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *teamActionGetTool) Info() fantasy.ToolInfo {
	ids := make([]string, 0)
	if t.coordinator != nil && t.coordinator.session != nil {
		for _, entry := range t.coordinator.session.ActionCatalog.discoverableEntries(t.agent) {
			ids = append(ids, entry.ID)
		}
	}
	sort.Strings(ids)
	return fantasy.ToolInfo{
		Name:        teamActionGetToolName,
		Description: "Show one predefined runtime action: what it does, its side effect, and the JSON schema its arguments must match.",
		Parameters: map[string]any{
			"action": map[string]any{"type": "string", "enum": ids, "description": "Action ID from team_action_list."},
		},
		Required: []string{"action"},
	}
}

func (t *teamActionGetTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if !teamActionCallerValid(ctx, t.todoID, t.agent) {
		return teamActionErrorResponse(teamActionCallerInvalid, "this tool belongs to a different task or agent"), nil
	}
	var input struct {
		Action string `json:"action"`
	}
	if err := decodeTeamActionInput(call.Input, &input); err != nil {
		return teamActionErrorResponse(teamActionInvalidToolRequest, err.Error()), nil
	}
	entry, ok := t.coordinator.session.ActionCatalog.Lookup(input.Action)
	if !ok || !slices.Contains(entry.Discover, t.agent) {
		return teamActionErrorResponse(teamActionNotDiscoverable, fmt.Sprintf("no action %q is available to you", input.Action)), nil
	}
	return teamActionJSONResponse(teamActionContract{
		ID: entry.ID, Description: entry.Description, SideEffect: entry.SideEffect, Recovery: entry.Recovery,
		InputSchema: entry.InputSchema, OutputSchema: entry.OutputSchema,
		ProposalAllowed: slices.Contains(entry.Propose, t.agent), RequireProposal: entry.RequireProposal,
		SchemaDialect: teamActionSchemaDialect,
	})
}

// teamActionToolCollision rejects a concrete handler that claims a catalog
// tool name in a team with a catalog, so an MCP or custom tool cannot pose
// as a runtime-owned catalog tool. Teams without a catalog are unaffected.
func (c *Coordinator) teamActionToolCollision(concrete []fantasy.AgentTool) error {
	if c == nil || c.session == nil || c.session.ActionCatalog == nil {
		return nil
	}
	for _, name := range agentToolNames(concrete) {
		if slices.Contains(teamActionToolNames, name) {
			return fmt.Errorf("protocol tool name %q collides with a concrete handler", name)
		}
	}
	return nil
}
