package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

func InspectOverview(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindOverview); err != nil {
		return nil, err
	}
	workspace, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{
		RequestedPath: query.Workspace,
		Mode:          "exact",
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	bound, err := BindReadTarget(ctx, operatorpkg.BindingRequest{
		Workspace: workspace,
		SessionID: query.SessionID,
		RunID:     query.RunID,
		BranchID:  query.BranchID,
	})
	if err != nil {
		return nil, err
	}
	resolvedQuery := query
	resolvedQuery.Workspace = workspace.WorkspaceExact
	resolvedQuery.RunID = bound.Scope.RunID
	resolvedQuery.SessionID = bound.Scope.SessionID
	resolvedQuery.BranchID = bound.Scope.BranchID
	runData, selected, err := projectRunData(ctx, resolvedQuery, bound.Lineage)
	if err != nil {
		return nil, err
	}
	tasks, err := team.ReplayTodoList(selected.runEvents)
	if err != nil {
		return nil, fmt.Errorf("%w: replay overview tasks for run %q: %v", ErrIntegrity, resolvedQuery.RunID, err)
	}

	states, reasons, blockers := overviewTaskFacts(tasks)
	if runData.StopReason != "" {
		reasons = append(reasons, runData.StopReason)
	}
	if bound.Session != nil && bound.Session.RecoveryReason != "" {
		reasons = append(reasons, bound.Session.RecoveryReason)
	}
	activity := operatorpkg.DeriveActivity(operatorpkg.ActivityFacts{
		BindingStatus:      bound.Scope.BindingStatus,
		EventChain:         "verified",
		RequiredProjection: "consistent",
		HasTerminalRun:     selected.terminal != nil,
		DurablyInterrupted: overviewInterrupted(selected.terminal != nil, bound.Session, states),
		TaskStates:         states,
		RawReasonCodes:     reasons,
	})
	outcome := operatorpkg.OutcomeView{
		RunOutcome:      runData.Outcome,
		StopReason:      runData.StopReason,
		AcceptanceState: runData.Acceptance,
		CompletionState: runData.Completion,
	}
	if selected.terminal != nil {
		outcome.GoalSatisfied = new(runData.GoalSatisfied)
	}
	anchor := latestRunEvent(bound.Lineage.Events, bound.Scope.RunID)
	learning := learningView(ctx, workspace.WorkspaceExact, bound.Scope.ProjectID, bound.Scope.TeamName)
	integrity := operatorpkg.IntegrityView{
		Status: "valid", EventChain: "verified", Projection: "consistent", ReasonCodes: []string{},
	}
	if learning.Status == "unknown" || learning.Status == "unavailable" {
		integrity.Status = "degraded"
		integrity.ReasonCodes = []string{"learning_projection_unavailable"}
	}
	snapshot := operatorpkg.OperatorSnapshot{
		SchemaVersion: operatorpkg.SchemaVersion,
		Scope:         bound.Scope,
		Activity:      activity,
		Outcome:       outcome,
		Integrity:     integrity,
		Freshness: operatorpkg.FreshnessView{
			QueriedAt: time.Now().UTC().Format(time.RFC3339Nano), EventID: anchor.Event.ID,
			EventHash: anchor.Event.Hash, EventOrdinal: anchor.Ordinal, LiveState: "not_applicable",
		},
		Attention:        operatorpkg.DeriveAttention(activity.State, runData.Outcome, "valid", ""),
		Blockers:         blockers,
		LatestChanges:    latestChanges(bound.Lineage.Events, bound.Scope.RunID, 3),
		RoleTargets:      roleTargetsFromTasks(tasks),
		Learning:         learning,
		SecondaryActions: []operatorpkg.ActionSuggestion{},
	}
	recovery := RecoveryEligibilityForTasks(tasks, bound.Lineage.Events, bound.Scope.RunID, activity.State == operatorpkg.ActivityInterrupted)
	if recovery != nil {
		snapshot.Attention = operatorpkg.DeriveAttention(activity.State, runData.Outcome, "valid", recovery.ExternalEffectState)
	}
	sessionRevision, sessionRefs := SessionRecoveryRevision(bound.Session, bound.Scope.BranchID)
	snapshot.PrimaryAction, snapshot.SecondaryActions, err = operatorpkg.SelectActions(operatorpkg.ActionSelectionFacts{
		Snapshot: snapshot, Recovery: recovery, SessionExpectedRevision: sessionRevision, SessionSourceRefs: sessionRefs,
		MutationFacadeAvailable: true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: select operator action: %v", ErrIntegrity, err)
	}
	snapshot, err = operatorpkg.FinalizeSnapshot(snapshot)
	if err != nil {
		return nil, fmt.Errorf("%w: finalize operator snapshot: %v", ErrIntegrity, err)
	}
	data := OverviewData{
		Operation: OperationView{Status: "succeeded"},
		Snapshot:  &snapshot,
		Warnings:  []operatorpkg.DiagnosticView{},
	}
	return envelope(KindOverview, resolvedQuery, bound.Scope.BranchID, data), nil
}

func OverviewFailure(query InspectQuery, err error) *Envelope {
	errorView := classifyOverviewError(err)
	integrity := Integrity{EventChain: "unavailable", Projection: "unavailable"}
	if errors.Is(err, ErrIntegrity) {
		integrity.EventChain = "invalid"
	}
	query.Workspace = ""
	return &Envelope{
		SchemaVersion: SchemaVersion,
		Kind:          KindOverview,
		Query:         query,
		Integrity:     integrity,
		Data: OverviewData{
			Operation: OperationView{Status: "failed"},
			Error:     &errorView,
			Warnings:  []operatorpkg.DiagnosticView{},
		},
		Diagnostics: []Diagnostic{},
	}
}

func classifyOverviewError(err error) OperatorErrorView {
	switch {
	case errors.Is(err, ErrInvalidQuery):
		return OperatorErrorView{Kind: "usage", Code: "invalid_query", Message: "operator overview query is invalid"}
	case errors.Is(err, ErrScopeConflict):
		return OperatorErrorView{Kind: "scope", Code: "workspace_scope_conflict", Message: "requested scope conflicts with persisted binding"}
	case errors.Is(err, ErrAmbiguous):
		return OperatorErrorView{Kind: "scope", Code: "target_ambiguous", Message: "operator target is ambiguous"}
	case errors.Is(err, ErrNotFound):
		return OperatorErrorView{Kind: "scope", Code: "target_not_found", Message: "operator target was not found"}
	case errors.Is(err, ErrUnauthorized):
		return OperatorErrorView{Kind: "policy", Code: "target_unauthorized", Message: "operator target is not authorized"}
	case errors.Is(err, ErrIntegrity):
		return OperatorErrorView{Kind: "integrity", Code: "integrity_failure", Message: "canonical operator evidence failed integrity validation"}
	default:
		return OperatorErrorView{Kind: "runtime", Code: "inspect_failed", Message: "operator overview could not be produced"}
	}
}

func overviewTaskFacts(tasks []*team.TodoItem) ([]string, []string, []operatorpkg.DiagnosticView) {
	states := make([]string, 0, len(tasks))
	var reasons []string
	var blockers []operatorpkg.DiagnosticView
	for _, item := range tasks {
		if item == nil {
			continue
		}
		states = append(states, string(item.Status))
		if item.FailureEvent != nil && item.FailureEvent.FailureType != "" {
			reasons = append(reasons, item.FailureEvent.FailureType)
		}
		switch item.Status {
		case team.TaskError, team.TaskBlocked, team.TaskProtocolIncomplete:
			blockers = append(blockers, operatorpkg.DiagnosticView{
				Code: "task_" + string(item.Status), Severity: "error", Message: "task requires operator review", Ref: item.ID,
			})
		}
	}
	if reasons == nil {
		reasons = []string{}
	}
	if blockers == nil {
		blockers = []operatorpkg.DiagnosticView{}
	}
	return states, reasons, blockers
}

func overviewInterrupted(terminal bool, session *team.SessionData, states []string) bool {
	if terminal || session == nil {
		return false
	}
	if session.RecoveryRequired || session.PendingTerminalCommit != nil {
		return true
	}
	return slices.Contains(states, string(team.TaskPaused))
}

func latestRunEvent(events []IndexedEvent, runID string) IndexedEvent {
	var latest IndexedEvent
	for _, indexed := range events {
		if indexed.Event.RunID == runID && indexed.Ordinal >= latest.Ordinal {
			latest = indexed
		}
	}
	return latest
}

func latestChanges(events []IndexedEvent, runID string, limit int) []operatorpkg.ChangeView {
	var selected []IndexedEvent
	for _, indexed := range events {
		if indexed.Event.RunID == runID && isOverviewChange(indexed.Event.Type) {
			selected = append(selected, indexed)
		}
	}
	if len(selected) > limit {
		selected = selected[len(selected)-limit:]
	}
	changes := make([]operatorpkg.ChangeView, 0, len(selected))
	for _, indexed := range selected {
		var payload struct {
			Status     string `json:"status"`
			ReasonCode string `json:"reason_code"`
			StopReason string `json:"stop_reason"`
			Failure    struct {
				FailureType string `json:"failure_type"`
			} `json:"failure_event"`
		}
		_ = json.Unmarshal(indexed.Event.Payload, &payload)
		reasonCode := firstNonEmpty(payload.ReasonCode, payload.Failure.FailureType, payload.StopReason)
		refs := []string{}
		if indexed.Event.TaskID != "" {
			refs = append(refs, indexed.Event.TaskID)
		}
		changes = append(changes, operatorpkg.ChangeView{
			EventID: indexed.Event.ID, EventOrdinal: indexed.Ordinal, Kind: indexed.Event.Type,
			Status: payload.Status, ReasonCode: reasonCode, Refs: refs,
		})
	}
	return changes
}

func isOverviewChange(eventType string) bool {
	switch team.EventType(eventType) {
	case team.EventRunStarted, team.EventRunFinished,
		team.EventTaskCreated, team.EventTaskPlanned, team.EventTaskStarted,
		team.EventTaskVerifying, team.EventTaskPaused, team.EventTaskCompleted,
		team.EventTaskFailed, team.EventTaskBlocked, team.EventTaskSkipped,
		team.EventTaskProtocolIncomplete, team.EventTaskCancelled,
		team.EventTaskRemoved, team.EventTaskResolution,
		team.EventPolicyDecision, team.EventRecoveryDecision, team.EventWorkflowStateChanged,
		team.EventCriterionReevaluated, team.EventCriterionCheckpoint:
		return true
	default:
		return false
	}
}

func roleTargetsFromTasks(tasks []*team.TodoItem) []operatorpkg.RoleTargetView {
	roles := []string{"coordinator", "guard", "judge", "plan_reviewer", "sidecar", "worker"}
	result := make([]operatorpkg.RoleTargetView, 0, len(roles))
	for _, role := range roles {
		view := operatorpkg.RoleTargetView{Role: role, Availability: "unavailable", ReasonCode: "not_persisted_for_role"}
		if role == "worker" {
			var targets []string
			backends := make(map[string]string)
			for _, item := range tasks {
				if item == nil {
					continue
				}
				target := item.ExecutionTarget.String()
				if target == "" {
					target = strings.TrimSpace(item.Model)
				}
				if target == "" {
					continue
				}
				targets = append(targets, target)
				backends[target] = item.ExecutionTarget.Backend
			}
			slices.Sort(targets)
			targets = slices.Compact(targets)
			switch len(targets) {
			case 1:
				view.Effective = targets[0]
				view.BackendKind = backends[targets[0]]
				view.Source = "durable_task"
				view.Availability = "verified"
				view.ReasonCode = "persisted_execution_target"
			default:
				if len(targets) > 1 {
					view.Source = "durable_task"
					view.ReasonCode = "multiple_persisted_targets"
				}
			}
		}
		result = append(result, view)
	}
	return result
}
