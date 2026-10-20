package inspect

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/cost"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

// RoutesData contains observations from one immutable input head. Completion
// evidence is an analytics qualification, never a replacement for RunResult.
type RoutesData struct {
	SchemaVersion int            `json:"schema_version"`
	SourceHead    cost.Freshness `json:"source_head"`
	RunOutcome    string         `json:"run_outcome"`
	Acceptance    string         `json:"acceptance_state"`
	Tasks         []RouteTask    `json:"tasks"`
	Attempts      []RouteAttempt `json:"attempts"`
	AttemptCount  int            `json:"attempt_count"`
	Groups        []RouteGroup   `json:"groups"`
	Coverage      RouteCoverage  `json:"evidence_coverage"`
	Diagnostics   []Diagnostic   `json:"diagnostics"`
}

type RouteTask struct {
	TaskID             string `json:"task_id"`
	CreatedEventID     string `json:"created_event_id"`
	OriginRunID        string `json:"origin_run_id"`
	SourceBranchID     string `json:"source_branch_id"`
	TaskStatus         string `json:"task_status"`
	Agent              string `json:"agent"`
	RouteKind          string `json:"route_kind"`
	RouteName          string `json:"route_name,omitempty"`
	RouteDigest        string `json:"route_digest,omitempty"`
	ChangedInRun       bool   `json:"changed_in_run"`
	CrossRunScope      bool   `json:"cross_run_scope"`
	CompletionEvidence string `json:"completion_evidence"`
	Reason             string `json:"reason,omitempty"`
	WinningReceiptHash string `json:"winning_receipt_hash,omitempty"`
}

// RouteAttempt enriches, rather than reimplements, the lifecycle projector.
// Multiple model executions within a dispatch retain the same AttemptKey but
// have distinct ExecutionKey and receipt hashes. Missing starts remain explicit.
type RouteAttempt struct {
	WorkerAttemptView
	ExecutionKey     string `json:"execution_key"`
	ModelExecutionID string `json:"model_execution_id,omitempty"`
	ReceiptHash      string `json:"receipt_hash,omitempty"`
	SourceBranchID   string `json:"source_branch_id"`
	RouteKind        string `json:"route_kind"`
	RouteDigest      string `json:"route_digest,omitempty"`
	AttemptClass     string `json:"attempt_class"`
	Winning          bool   `json:"winning"`
	// Uncounted receipts cannot be assigned to one of several dispatches
	// already represented by lifecycle rows. They add no execution or credit.
	Counted bool `json:"counted"`
}

type RouteGroup struct {
	RouteKind       string `json:"route_kind"`
	RouteName       string `json:"route_name,omitempty"`
	RouteDigest     string `json:"route_digest,omitempty"`
	ExecutionTarget string `json:"execution_target"`
	Attempts        int    `json:"attempts"`
	VerifiedTasks   int    `json:"verified_tasks"`
}

type RouteCoverage struct {
	Tasks       int `json:"tasks"`
	Verified    int `json:"verified"`
	NotVerified int `json:"not_verified"`
	Unavailable int `json:"unavailable"`
	Reused      int `json:"reused"`
}

func InspectRoutes(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindRoutes); err != nil {
		return nil, err
	}
	resolved, bound, err := bindObservationTarget(ctx, query)
	if err != nil {
		return nil, err
	}
	data, err := projectRoutes(ctx, resolved, bound.Lineage)
	if err != nil {
		return nil, err
	}
	return envelope(KindRoutes, resolved, bound.Scope.BranchID, data), nil
}

func bindObservationTarget(ctx context.Context, query InspectQuery) (InspectQuery, BoundReadTarget, error) {
	workspace, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{RequestedPath: query.Workspace, Mode: "exact"})
	if err != nil {
		return InspectQuery{}, BoundReadTarget{}, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	bound, err := BindReadTarget(ctx, operatorpkg.BindingRequest{
		Workspace: workspace, RunID: query.RunID, SessionID: query.SessionID, BranchID: query.BranchID,
	})
	if err != nil {
		return InspectQuery{}, BoundReadTarget{}, err
	}
	query.Workspace, query.RunID = workspace.WorkspaceExact, bound.Scope.RunID
	query.SessionID, query.BranchID = bound.Scope.SessionID, bound.Scope.BranchID
	return query, bound, nil
}

// observationEvents includes ancestors needed to resolve origin identity, but
// stops at the queried run's last event. Later runs cannot rewrite history.
func observationEvents(lineage Lineage, query InspectQuery) ([]IndexedEvent, error) {
	last := -1
	for i, indexed := range lineage.Events {
		if indexed.Event.RunID == query.RunID && (query.SessionID == "" || indexed.Event.SessionID == query.SessionID) {
			last = i
		}
	}
	if last < 0 {
		return nil, fmt.Errorf("%w: run has no events in selected scope", ErrNotFound)
	}
	var events []IndexedEvent
	for _, indexed := range lineage.Events[:last+1] {
		if query.SessionID == "" || indexed.Event.SessionID == query.SessionID {
			events = append(events, indexed)
		}
	}
	return events, nil
}

func projectRoutes(ctx context.Context, query InspectQuery, lineage Lineage) (RoutesData, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	events, err := observationEvents(lineage, query)
	if err != nil {
		return RoutesData{}, err
	}
	raw := make([]team.RunEvent, len(events))
	for i, event := range events {
		raw[i] = event.Event
	}
	todos, err := team.ReplayTodoList(raw)
	if err != nil {
		return RoutesData{}, fmt.Errorf("%w: replay route tasks: %v", ErrIntegrity, err)
	}
	explanation, err := auditverify.ExplainLineageRun(ctx, query.Workspace, query.RunID, raw)
	if err != nil {
		return RoutesData{}, classifyProjectionError(query.RunID, err)
	}
	if err := observationIntegrity(explanation); err != nil {
		return RoutesData{}, err
	}
	data := RoutesData{
		SchemaVersion: 1, Tasks: []RouteTask{}, Attempts: []RouteAttempt{}, Groups: []RouteGroup{},
		RunOutcome: "unavailable", Acceptance: "unavailable", Diagnostics: []Diagnostic{},
	}
	head := events[len(events)-1].Event
	data.SourceHead = cost.Freshness{EventID: head.ID, EventHash: head.Hash}
	session := team.ReduceToSessionData(raw)
	var result *team.RunResult
	if session != nil && session.RunResult != nil && session.RunResult.RunID == query.RunID {
		result = session.RunResult
		data.RunOutcome = string(result.Outcome)
		data.Acceptance = string(team.AcceptanceNotConfigured)
		if result.Acceptance != nil {
			data.Acceptance = string(result.Acceptance.EffectiveState())
		}
	}
	rows, err := routeTaskIdentities(events, query.RunID)
	if err != nil {
		return RoutesData{}, err
	}
	// Lifecycle projection owns dispatch identity and retry/fallback semantics.
	views, err := projectWorkerAttemptsContext(ctx, todos, events, time.Time{})
	if err != nil {
		return RoutesData{}, err
	}
	found := query.TaskID == ""
	for _, todo := range todos {
		if err := ctx.Err(); err != nil {
			return RoutesData{}, err
		}
		if todo == nil || query.TaskID != "" && todo.ID != query.TaskID {
			continue
		}
		row, exists := rows[todo.ID]
		if !exists {
			continue
		}
		found = true
		row.TaskStatus, row.Agent = string(todo.Status), todo.Agent
		row.RouteKind, row.RouteName, row.RouteDigest = routeIdentity(todo)
		if err := routePayloadIntegrity(todo, query.RunID); err != nil {
			return RoutesData{}, err
		}
		row.CompletionEvidence, row.Reason, row.WinningReceiptHash = routeEvidence(todo, result, explanation)
		// Unchanged ancestor tasks are visible as reuse, never run success credit.
		if !row.ChangedInRun {
			row.WinningReceiptHash = ""
		}
		data.Tasks = append(data.Tasks, row)
		if row.Reason != "" && row.ChangedInRun {
			data.Diagnostics = append(data.Diagnostics, Diagnostic{
				Code: row.Reason, Severity: "info", Message: "Completion evidence is not fully verified.", Ref: safeOpaqueRef(row.TaskID),
			})
		}
		attempts, err := routeAttemptsContext(ctx, todo, row, views, events, query.RunID)
		if err != nil {
			return RoutesData{}, err
		}
		data.Attempts = append(data.Attempts, attempts...)
		for _, attempt := range attempts {
			if attempt.Counted {
				data.AttemptCount++
			} else {
				data.Diagnostics = append(data.Diagnostics, Diagnostic{
					Code: "attribution_unavailable", Severity: "info",
					Message: "Receipt could not be assigned to a unique dispatch; it adds no attempt or completion credit.",
					Ref:     attempt.ReceiptHash,
				})
			}
		}
	}
	if !found {
		return RoutesData{}, fmt.Errorf("%w: task is outside selected run lineage", ErrNotFound)
	}
	sortRouteRows(&data, events)
	data.Groups, data.Coverage = summarizeRoutes(data.Tasks, data.Attempts)
	return data, nil
}

func routeTaskIdentities(events []IndexedEvent, runID string) (map[string]RouteTask, error) {
	rows := make(map[string]RouteTask)
	statuses := make(map[string]team.TaskStatus)
	for _, indexed := range events {
		event := indexed.Event
		if event.TaskID == "" || event.Type != string(team.EventTaskCreated) && !taskLifecycleEvents[event.Type] {
			continue
		}
		row, exists := rows[event.TaskID]
		if !exists {
			row = RouteTask{TaskID: event.TaskID, SourceBranchID: event.BranchID}
			if event.Type == string(team.EventTaskCreated) {
				row.CreatedEventID = event.ID
				row.OriginRunID = event.RunID
			}
		}
		var payload lifecyclePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("%w: invalid task lifecycle payload", ErrIntegrity)
		}
		changed := !exists || payload.Status != "" && payload.Status != statuses[event.TaskID] ||
			event.Type == string(team.EventTaskStarted) && payload.DispatchAttempt != nil
		if event.RunID == runID && changed {
			row.ChangedInRun = true
		}
		if payload.Status != "" {
			statuses[event.TaskID] = payload.Status
		}
		row.CrossRunScope = row.ChangedInRun && row.OriginRunID != runID
		rows[event.TaskID] = row
	}
	return rows, nil
}

func routeIdentity(todo *team.TodoItem) (kind, name, digest string) {
	if todo.ExecutionRoute != nil {
		if todo.ExecutionRoute.Name == "" || todo.ExecutionRoute.Digest == "" {
			return "unavailable", "", ""
		}
		return "named", todo.ExecutionRoute.Name, todo.ExecutionRoute.Digest
	}
	if !todo.ExecutionTarget.IsZero() {
		return "unbound", "", ""
	}
	return "unavailable", "", ""
}

func sortRouteRows(data *RoutesData, events []IndexedEvent) {
	ordinals := make(map[string]int64, len(events))
	for _, event := range events {
		ordinals[event.Event.ID] = event.Ordinal
	}
	slices.SortStableFunc(data.Tasks, func(a, b RouteTask) int {
		if ordinals[a.CreatedEventID] != ordinals[b.CreatedEventID] {
			if ordinals[a.CreatedEventID] < ordinals[b.CreatedEventID] {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.TaskID, b.TaskID)
	})
	slices.SortStableFunc(data.Attempts, func(a, b RouteAttempt) int {
		if ordinals[a.StartedEventID] != ordinals[b.StartedEventID] {
			if ordinals[a.StartedEventID] < ordinals[b.StartedEventID] {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.ExecutionKey, b.ExecutionKey)
	})
}

func summarizeRoutes(tasks []RouteTask, attempts []RouteAttempt) ([]RouteGroup, RouteCoverage) {
	coverage := RouteCoverage{}
	for _, task := range tasks {
		if !task.ChangedInRun {
			if task.TaskStatus == string(team.TaskDone) {
				coverage.Reused++
			}
			continue
		}
		coverage.Tasks++
		switch task.CompletionEvidence {
		case "verified":
			coverage.Verified++
		case "not_verified":
			coverage.NotVerified++
		default:
			coverage.Unavailable++
		}
	}
	type key struct{ kind, name, digest, target string }
	indices := make(map[key]int)
	groups := []RouteGroup{}
	for _, attempt := range attempts {
		if !attempt.Counted {
			continue
		}
		k := key{attempt.RouteKind, attempt.RouteName, attempt.RouteDigest, attempt.ExecutionTarget}
		index, exists := indices[k]
		if !exists {
			index = len(groups)
			indices[k] = index
			groups = append(groups, RouteGroup{RouteKind: k.kind, RouteName: k.name, RouteDigest: k.digest, ExecutionTarget: k.target})
		}
		groups[index].Attempts++
		if attempt.Winning {
			groups[index].VerifiedTasks++
		}
	}
	return groups, coverage
}
