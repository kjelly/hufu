package team

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
)

const maxCoordinatorActionProposals = 50

// teamActionProposalCounts counts an action's proposals by assessment.
type teamActionProposalCounts struct {
	Recommended int `json:"recommended"`
	Candidate   int `json:"candidate"`
	Defer       int `json:"defer"`
	Reject      int `json:"reject"`
}

// teamActionProposalView is one proposal as the coordinator sees it.
type teamActionProposalView struct {
	ProposalID      string                  `json:"proposal_id"`
	Agent           string                  `json:"agent"`
	TaskID          string                  `json:"task_id"`
	Assessment      string                  `json:"assessment"`
	Arguments       any                     `json:"arguments"`
	ArgumentsHash   string                  `json:"arguments_hash"`
	EntryHash       string                  `json:"entry_hash"`
	Rationale       string                  `json:"rationale"`
	ExpectedOutcome string                  `json:"expected_outcome,omitempty"`
	EvidenceRefs    []TeamActionEvidenceRef `json:"evidence_refs,omitempty"`
}

// TeamActionEvidenceRef is an artifact a proposal cites as evidence.
type TeamActionEvidenceRef struct {
	ID             string `json:"id"`
	SHA256         string `json:"sha256"`
	ProducerTaskID string `json:"producer_task_id"`
}

type coordinatorTeamActionSummary struct {
	ID              string                   `json:"id"`
	Description     string                   `json:"description"`
	Agent           string                   `json:"agent"`
	SideEffect      SideEffectClass          `json:"side_effect"`
	Recovery        RecoveryPolicy           `json:"recovery"`
	RequireProposal bool                     `json:"require_proposal"`
	AllowUnattended bool                     `json:"allow_unattended"`
	MaxInvocations  int                      `json:"max_invocations"`
	InvocationsUsed int                      `json:"invocations_used"`
	ProposalCounts  teamActionProposalCounts `json:"proposal_counts"`
}

type coordinatorTeamActionDetail struct {
	coordinatorTeamActionSummary
	InputSchema   RunInputSchema           `json:"input_schema"`
	OutputSchema  *RunInputSchema          `json:"output_schema,omitempty"`
	SchemaDialect string                   `json:"schema_dialect"`
	Proposals     []teamActionProposalView `json:"proposals"`
}

// coordinatorTeamActionTools returns the coordinator's catalog tools, or nil
// when the team declares no catalog.
func (c *Coordinator) coordinatorTeamActionTools() []fantasy.AgentTool {
	if c == nil || c.session == nil || c.session.ActionCatalog == nil {
		return nil
	}
	return []fantasy.AgentTool{&coordinatorTeamActionListTool{coordinator: c}, &coordinatorTeamActionGetTool{coordinator: c}}
}

func (c *Coordinator) coordinatorTeamActionSummary(entry ActionCatalogEntry) coordinatorTeamActionSummary {
	return coordinatorTeamActionSummary{
		ID: entry.ID, Description: entry.Description, Agent: entry.Agent, SideEffect: entry.SideEffect, Recovery: entry.Recovery,
		RequireProposal: entry.RequireProposal, AllowUnattended: entry.AllowUnattended, MaxInvocations: entry.MaxInvocations,
		InvocationsUsed: c.teamActionInvocationsUsed(entry.ID), ProposalCounts: c.teamActionProposalCounts(entry.ID),
	}
}

// teamActionInvocationsUsed counts the admitted catalog tasks of an action.
// Invocation accounting is added with catalog dispatch.
func (c *Coordinator) teamActionInvocationsUsed(string) int { return 0 }

// teamActionProposalCounts counts an action's recorded proposals. Proposals
// are added with team_action_propose.
func (c *Coordinator) teamActionProposalCounts(string) teamActionProposalCounts {
	return teamActionProposalCounts{}
}

// teamActionProposalViews returns an action's newest proposals first.
func (c *Coordinator) teamActionProposalViews(string) []teamActionProposalView {
	return []teamActionProposalView{}
}

type coordinatorTeamActionListTool struct{ coordinator *Coordinator }

func (*coordinatorTeamActionListTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (*coordinatorTeamActionListTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (*coordinatorTeamActionListTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: teamActionListToolName,
		Description: "List this team's predefined runtime actions with the agent that runs each one, its side effect, " +
			"how many invocations remain, and how many worker proposals it has.",
		Parameters: map[string]any{
			"query": map[string]any{"type": "string", "description": "Optional case-insensitive text to match in action IDs and descriptions."},
		},
	}
}

func (t *coordinatorTeamActionListTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var input struct {
		Query string `json:"query"`
	}
	if err := decodeTeamActionInput(call.Input, &input); err != nil {
		return teamActionErrorResponse(teamActionInvalidToolRequest, err.Error()), nil
	}
	if err := validTeamActionQuery(input.Query); err != nil {
		return teamActionErrorResponse(teamActionInvalidToolRequest, err.Error()), nil
	}
	actions := make([]coordinatorTeamActionSummary, 0)
	truncated := false
	for _, entry := range t.coordinator.session.ActionCatalog.Entries {
		if !matchTeamActionQuery(entry, input.Query) {
			continue
		}
		if len(actions) == maxActionListResults {
			truncated = true
			break
		}
		actions = append(actions, t.coordinator.coordinatorTeamActionSummary(entry))
	}
	return teamActionJSONResponse(struct {
		Actions   []coordinatorTeamActionSummary `json:"actions"`
		Truncated bool                           `json:"truncated"`
	}{actions, truncated})
}

type coordinatorTeamActionGetTool struct{ coordinator *Coordinator }

func (*coordinatorTeamActionGetTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (*coordinatorTeamActionGetTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *coordinatorTeamActionGetTool) Info() fantasy.ToolInfo {
	ids := make([]string, 0)
	if t.coordinator != nil && t.coordinator.session != nil && t.coordinator.session.ActionCatalog != nil {
		for _, entry := range t.coordinator.session.ActionCatalog.Entries {
			ids = append(ids, entry.ID)
		}
	}
	sort.Strings(ids)
	return fantasy.ToolInfo{
		Name: teamActionGetToolName,
		Description: "Show one predefined runtime action: its input and output schemas and the latest worker proposals. " +
			"Dispatch it with the agent tool's catalog_action.",
		Parameters: map[string]any{
			"action": map[string]any{"type": "string", "enum": ids, "description": "Action ID from team_action_list."},
		},
		Required: []string{"action"},
	}
}

func (t *coordinatorTeamActionGetTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var input struct {
		Action string `json:"action"`
	}
	if err := decodeTeamActionInput(call.Input, &input); err != nil {
		return teamActionErrorResponse(teamActionInvalidToolRequest, err.Error()), nil
	}
	entry, ok := t.coordinator.session.ActionCatalog.Lookup(input.Action)
	if !ok {
		return teamActionErrorResponse(teamActionNotDiscoverable, fmt.Sprintf("no action %q is defined", input.Action)), nil
	}
	proposals := t.coordinator.teamActionProposalViews(entry.ID)
	if len(proposals) > maxCoordinatorActionProposals {
		proposals = proposals[:maxCoordinatorActionProposals]
	}
	return teamActionJSONResponse(coordinatorTeamActionDetail{
		coordinatorTeamActionSummary: t.coordinator.coordinatorTeamActionSummary(entry),
		InputSchema:                  entry.InputSchema, OutputSchema: entry.OutputSchema,
		SchemaDialect: teamActionSchemaDialect, Proposals: proposals,
	})
}

// teamActionCatalogPrompt is the fixed coordinator guidance for a team with a
// catalog; the catalog itself is read through team_action_list/get.
const teamActionCatalogPrompt = `This team defines predefined runtime actions. Use team_action_list and team_action_get to inspect them
and the workers' proposals. To run one, add a task to the agent tool with the entry's agent, a short goal,
and catalog_action {"id": ..., "arguments": {...}}. Arguments must match the entry's input schema.
Workers can only recommend actions; they cannot run them.`

func (c *Coordinator) appendActionCatalogPrompt(b *strings.Builder) {
	if c == nil || c.session == nil || c.session.ActionCatalog == nil {
		return
	}
	b.WriteString("\n## Team actions\n")
	b.WriteString(teamActionCatalogPrompt)
	b.WriteString("\n")
}
