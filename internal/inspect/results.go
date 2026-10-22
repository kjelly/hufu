package inspect

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

// HistoricalResult is an event-backed snapshot, not the latest TodoList entry.
// Content belongs to this explicitly content-bearing API, not InspectTask.
type HistoricalResult struct {
	ResultID              string           `json:"result_id"`
	Kind                  string           `json:"kind"`
	Team                  string           `json:"team,omitempty"`
	RunID                 string           `json:"run_id"`
	OriginRunID           string           `json:"origin_run_id"`
	InvocationID          string           `json:"invocation_id,omitempty"`
	SessionID             string           `json:"session_id"`
	TaskID                string           `json:"task_id,omitempty"`
	Agent                 string           `json:"agent,omitempty"`
	Attempt               int              `json:"attempt"`
	Status                string           `json:"status"`
	CurrentStatus         string           `json:"current_status"`
	Validity              string           `json:"validity"`
	CompletedAt           time.Time        `json:"completed_at,omitzero"`
	Description           string           `json:"description,omitempty"`
	OriginalPrompt        string           `json:"original_prompt,omitempty"`
	OriginalPromptEventID string           `json:"original_prompt_event_id,omitempty"`
	Summary               string           `json:"summary,omitempty"`
	Output                string           `json:"output,omitempty"`
	TypedResult           *team.TaskResult `json:"typed_result,omitempty"`
	SourceEventID         string           `json:"source_event_id"`
	SourceHash            string           `json:"source_hash"`
	Branches              []string         `json:"branches"`
	Workspace             string           `json:"-"`
	Ordinal               int64            `json:"-"`
}

type HistoricalResultScope struct {
	Workspace   string `json:"workspace"`
	Team        string `json:"team,omitempty"`
	Branch      string `json:"branch,omitempty"`
	AllBranches bool   `json:"all_branches,omitzero"`
}

type HistoricalCoverage struct {
	Scope          HistoricalResultScope `json:"scope"`
	EventCount     int                   `json:"event_count"`
	ChainHead      string                `json:"chain_head,omitempty"`
	ResultCount    int                   `json:"result_count"`
	MissingContent int                   `json:"missing_content"`
	UnknownTeam    int                   `json:"unknown_team"`
	IndexState     string                `json:"index_state"`
}

type HistoricalSnapshot struct {
	Results  []HistoricalResult `json:"results"`
	Coverage HistoricalCoverage `json:"coverage"`
}

// LoadHistoricalResults never repairs, migrates, or opens a writable store.
// All branches are selected explicitly; global chain validation still runs
// before any result content can leave this function.
func LoadHistoricalResults(ctx context.Context, scope HistoricalResultScope) (HistoricalSnapshot, error) {
	if scope.AllBranches && scope.Branch != "" {
		return HistoricalSnapshot{}, fmt.Errorf("%w: --branch and --all-branches are mutually exclusive", ErrInvalidQuery)
	}
	lineage, tree, knownTeams, err := historicalLineage(ctx, scope, 1_000_000, 256<<20)
	if err != nil {
		return HistoricalSnapshot{}, err
	}
	snapshot := HistoricalSnapshot{Results: []HistoricalResult{}, Coverage: HistoricalCoverage{Scope: scope, EventCount: lineage.GlobalEventCount, IndexState: "event_replay"}}
	if !scope.AllBranches {
		snapshot.Coverage.Scope.Branch = lineage.BranchID
	}
	if len(lineage.GlobalEvents) > 0 {
		snapshot.Coverage.ChainHead = lineage.GlobalEvents[len(lineage.GlobalEvents)-1].Event.ID
	}
	lineages := map[string][]IndexedEvent{lineage.BranchID: lineage.Events}
	if scope.AllBranches {
		lineages, err = historicalBranchLineages(tree, lineage.GlobalEvents)
		if err != nil {
			return HistoricalSnapshot{}, err
		}
	}
	byID := make(map[string]int)
	for _, branch := range slices.Sorted(maps.Keys(lineages)) {
		rows, teams, projectErr := projectHistoricalResults(ctx, lineages[branch], branch, scope)
		if projectErr != nil {
			return HistoricalSnapshot{}, projectErr
		}
		for _, name := range teams {
			knownTeams[name] = true
		}
		for _, row := range rows {
			if index, exists := byID[row.ResultID]; exists {
				existing := &snapshot.Results[index]
				existing.Branches = append(existing.Branches, branch)
				if existing.Validity != row.Validity || existing.CurrentStatus != row.CurrentStatus {
					existing.Validity = "branch_dependent"
					existing.CurrentStatus = "branch_dependent"
				}
				continue
			}
			byID[row.ResultID] = len(snapshot.Results)
			snapshot.Results = append(snapshot.Results, row)
		}
	}
	if scope.Team != "" && !knownTeams[scope.Team] && lineage.GlobalEventCount != 0 {
		return HistoricalSnapshot{}, fmt.Errorf("%w: team %q has no canonical identity in the selected workspace/branches", ErrScopeConflict, scope.Team)
	}
	for _, row := range snapshot.Results {
		if row.Summary == "" && row.Output == "" && row.TypedResult == nil {
			snapshot.Coverage.MissingContent++
		}
		if row.Team == "" {
			snapshot.Coverage.UnknownTeam++
		}
	}
	snapshot.Coverage.ResultCount = len(snapshot.Results)
	SortHistoricalResults(snapshot.Results, "newest")
	return snapshot, nil
}

func historicalBranchLineages(tree *team.SessionTree, indexed []IndexedEvent) (map[string][]IndexedEvent, error) {
	raw := make([]team.RunEvent, len(indexed))
	ordinals := make(map[string]int64, len(indexed))
	for i, event := range indexed {
		raw[i] = event.Event
		ordinals[event.Event.ID] = event.Ordinal
	}
	result := make(map[string][]IndexedEvent, len(tree.Branches))
	for branch := range tree.Branches {
		events, projectErr := team.ProjectValidatedEventsForBranch(raw, tree, branch)
		if projectErr != nil {
			return nil, fmt.Errorf("%w: project history branch: %v", ErrIntegrity, projectErr)
		}
		for _, event := range events {
			result[branch] = append(result[branch], IndexedEvent{Event: event, Ordinal: ordinals[event.ID]})
		}
	}
	return result, nil
}

type historicalTaskState struct {
	originRun string
	events    []team.RunEvent
	rows      []int
}

type historicalPromptKey struct{ session, run string }
type historicalPromptSource struct{ content, eventID string }

// Keep the first user message of each originating run, independently of
// later continuations and resume runs. Only the selected lineage is read.
func historicalOriginalPrompts(ctx context.Context, events []IndexedEvent) (map[historicalPromptKey]historicalPromptSource, error) {
	prompts := make(map[historicalPromptKey]historicalPromptSource)
	for _, indexed := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		event := indexed.Event
		if event.Type != string(team.EventUserMessageAdded) && event.Type != string(team.EventWorkflowCycleStarted) {
			continue
		}
		key := historicalPromptKey{event.SessionID, event.RunID}
		if _, exists := prompts[key]; exists {
			continue
		}
		var payload team.SessionMessageEventPayload
		if err := unmarshalHistoricalJSON(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("%w: decode historical request: %v", ErrIntegrity, err)
		}
		if event.Type == string(team.EventWorkflowCycleStarted) {
			var cycle team.WorkflowCycleStartedPayload
			if err := unmarshalHistoricalJSON(event.Payload, &cycle); err != nil {
				return nil, fmt.Errorf("%w: decode follow-up request: %v", ErrIntegrity, err)
			}
			payload.Content = cycle.Request
		}
		if strings.TrimSpace(payload.Content) != "" {
			prompts[key] = historicalPromptSource{utils.RedactSecrets(payload.Content), event.ID}
		}
	}
	return prompts, nil
}

func projectHistoricalResults(ctx context.Context, events []IndexedEvent, branch string, scope HistoricalResultScope) ([]HistoricalResult, []string, error) {
	prompts, err := historicalOriginalPrompts(ctx, events)
	if err != nil {
		return nil, nil, err
	}
	rows := []HistoricalResult{}
	teams := map[string]string{}
	states := make(map[string]*historicalTaskState)
	known := make(map[string]bool)
	for _, indexed := range events {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		event := indexed.Event
		var metadata struct {
			Team           string `json:"team"`
			Status         string `json:"status"`
			ResetForRetry  bool   `json:"reset_for_retry"`
			ResetForResume bool   `json:"reset_for_resume"`
		}
		if len(event.Payload) != 0 {
			if err := json.Unmarshal(event.Payload, &metadata); err != nil {
				return nil, nil, fmt.Errorf("%w: decode history event %s: %v", ErrIntegrity, event.ID, err)
			}
		}
		if metadata.Team != "" {
			metadata.Team = strings.ToLower(strings.TrimSpace(metadata.Team))
			if prior := teams[event.RunID]; prior != "" && prior != metadata.Team {
				return nil, nil, fmt.Errorf("%w: conflicting team identity for run %s", ErrIntegrity, event.RunID)
			}
			teams[event.RunID] = metadata.Team
			known[metadata.Team] = true
		}
		if event.Type == string(team.EventRunFinished) && (scope.Team == "" || teams[event.RunID] == scope.Team) {
			row, err := historicalRunResponse(indexed, branch, scope.Workspace, teams[event.RunID])
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, row)
			continue
		}
		if !strings.HasPrefix(event.Type, "task_") || event.TaskID == "" {
			continue
		}
		teamName := teams[event.RunID]
		// Filter before replaying or decoding result content; a team selector
		// must not send another team's content to either the CLI or an LLM.
		if scope.Team != "" && teamName != scope.Team {
			continue
		}
		key := teamName + "\x00" + event.SessionID + "\x00" + event.TaskID
		state := states[key]
		newOccurrence := event.Type == string(team.EventTaskCreated) && !metadata.ResetForRetry && !metadata.ResetForResume && state != nil && state.originRun != event.RunID
		if state == nil || newOccurrence {
			state = &historicalTaskState{originRun: event.RunID}
			states[key] = state
		}
		state.events = append(state.events, event)
		if metadata.Status != "" {
			for _, index := range state.rows {
				rows[index].CurrentStatus = metadata.Status
			}
		}
		if event.Type == string(team.EventTaskRemoved) || metadata.ResetForRetry || metadata.ResetForResume {
			for _, index := range state.rows {
				rows[index].Validity = "invalidated"
				rows[index].CurrentStatus = "pending"
				if event.Type == string(team.EventTaskRemoved) {
					rows[index].CurrentStatus = "removed"
				}
			}
		}
		if !team.IsTerminalEvent(event.Type) {
			continue
		}
		todos, err := team.ReplayTodoList(state.events)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: replay historical result: %v", ErrIntegrity, err)
		}
		for _, item := range todos {
			if item.ID != event.TaskID {
				continue
			}
			for _, index := range state.rows {
				rows[index].CurrentStatus = string(item.Status)
				if rows[index].Validity == "accepted" {
					rows[index].Validity = "superseded"
				}
			}
			row, err := historicalResultFromItem(indexed, item, branch, scope.Workspace, teamName, state.originRun)
			if err != nil {
				return nil, nil, err
			}
			state.rows = append(state.rows, len(rows))
			rows = append(rows, row)
		}
	}
	for index := range rows {
		row := &rows[index]
		prompt := prompts[historicalPromptKey{row.SessionID, row.OriginRunID}]
		row.OriginalPrompt, row.OriginalPromptEventID = prompt.content, prompt.eventID
	}
	return rows, slices.Sorted(maps.Keys(known)), nil
}

func historicalRunResponse(indexed IndexedEvent, branch, workspace, teamName string) (HistoricalResult, error) {
	event := indexed.Event
	var payload team.RunFinishedEventPayload
	if err := unmarshalHistoricalJSON(event.Payload, &payload); err != nil {
		return HistoricalResult{}, fmt.Errorf("%w: decode historical run response: %v", ErrIntegrity, err)
	}
	completed, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
	return HistoricalResult{
		ResultID: team.HistoricalResultID(event), Kind: "run_response", Team: teamName,
		RunID: event.RunID, OriginRunID: event.RunID, InvocationID: event.InvocationID,
		SessionID: event.SessionID, Status: string(payload.Outcome), CurrentStatus: string(payload.Outcome), Validity: "accepted",
		CompletedAt: completed, Output: utils.RedactSecrets(payload.Response), Summary: utils.RedactSecrets(payload.Reason),
		SourceEventID: event.ID, SourceHash: event.Hash, Branches: []string{branch}, Workspace: workspace, Ordinal: indexed.Ordinal,
	}, nil
}

func historicalResultFromItem(indexed IndexedEvent, item *team.TodoItem, branch, workspace, teamName, originRun string) (HistoricalResult, error) {
	event := indexed.Event
	completed, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
	row := HistoricalResult{
		ResultID: team.HistoricalResultID(event), Kind: "task", Team: teamName,
		RunID: event.RunID, OriginRunID: originRun, InvocationID: event.InvocationID,
		SessionID: event.SessionID, TaskID: event.TaskID, Agent: item.Agent,
		Attempt: max(event.Attempt, item.Retries+1), Status: string(item.Status), CurrentStatus: string(item.Status),
		Validity: "accepted", CompletedAt: completed, Description: utils.RedactSecrets(item.Desc),
		Summary: utils.RedactSecrets(item.Detail), Output: utils.RedactSecrets(item.Output),
		SourceEventID: event.ID, SourceHash: event.Hash, Branches: []string{branch}, Workspace: workspace, Ordinal: indexed.Ordinal,
	}
	var content struct {
		TypedResult *team.TaskResult `json:"typed_result"`
	}
	if err := unmarshalHistoricalJSON(event.Payload, &content); err != nil {
		return HistoricalResult{}, fmt.Errorf("%w: decode terminal result content: %v", ErrIntegrity, err)
	}
	if content.TypedResult != nil {
		// Canonical result bytes are never changed: redact a detached view.
		encoded, err := marshalHistoricalJSON(content.TypedResult)
		if err != nil {
			return HistoricalResult{}, fmt.Errorf("%w: encode historical content: %v", ErrIntegrity, err)
		}
		redacted, err := utils.RedactJSON(encoded)
		if err != nil {
			return HistoricalResult{}, fmt.Errorf("%w: redact historical content: %v", ErrIntegrity, err)
		}
		var detached team.TaskResult
		if err := unmarshalHistoricalJSON(redacted, &detached); err != nil {
			return HistoricalResult{}, fmt.Errorf("%w: decode historical content: %v", ErrIntegrity, err)
		}
		row.TypedResult = &detached
		row.Summary = detached.Summary
	}
	return row, nil
}

// SortHistoricalResults provides stable ordering across workspaces and ties.
func SortHistoricalResults(rows []HistoricalResult, order string) {
	slices.SortFunc(rows, func(a, b HistoricalResult) int {
		value := a.CompletedAt.Compare(b.CompletedAt)
		ordinal := cmp.Compare(a.Ordinal, b.Ordinal)
		if order != "oldest" {
			value = -value
			ordinal = -ordinal
		}
		return cmp.Or(value, strings.Compare(a.Workspace, b.Workspace), strings.Compare(a.Team, b.Team), ordinal, strings.Compare(a.ResultID, b.ResultID))
	})
}
