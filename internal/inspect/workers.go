package inspect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

// Worker attempt activities, derived only from lifecycle state.
const (
	WorkerActivityQueued       = "queued"
	WorkerActivityFallback     = "fallback"
	WorkerActivityIntegrating  = "integrating"
	WorkerActivityResultRepair = "result_repair"
	WorkerActivityVerifying    = "verifying"
	WorkerActivityRunning      = "running"
	WorkerActivityTerminal     = "terminal"
	WorkerActivityUnknown      = "unknown"
)

// WorkerAttemptView is one worker attempt as recorded by canonical runtime
// state. It carries identities, enums, targets, and counters only: never
// output, prompts, tool arguments or results, or transcript content.
type WorkerAttemptView struct {
	AttemptKey        string `json:"attempt_key"`
	RunID             string `json:"run_id"`
	InvocationRunID   string `json:"invocation_run_id,omitempty"`
	TaskID            string `json:"task_id"`
	Agent             string `json:"agent"`
	StartedEventID    string `json:"started_event_id"`
	Attempt           int    `json:"attempt"`
	OccurrenceAttempt int    `json:"occurrence_attempt"`

	TaskStatus string `json:"task_status"`
	Activity   string `json:"activity"`

	ExecutionTarget string `json:"execution_target,omitempty"`
	CandidateIndex  *int   `json:"candidate_index,omitempty"`
	RouteName       string `json:"route_name,omitempty"`

	WorkspaceMode     string `json:"workspace_mode"`
	AttemptWorldID    string `json:"attempt_world_id,omitempty"`
	AttemptWorldState string `json:"attempt_world_state,omitempty"`

	StartedAt      time.Time `json:"started_at,omitzero"`
	FinishedAt     time.Time `json:"finished_at,omitzero"`
	DurationMillis int64     `json:"duration_ms,omitempty"`

	// Usage is nil when unknown (a running attempt has no receipt yet).
	Usage *team.ExecutionUsage `json:"usage,omitempty"`

	ResultContractID string `json:"result_contract_id,omitempty"`
	ResultValidation string `json:"result_validation,omitempty"`

	FailureClass  string `json:"failure_class,omitempty"`
	RetryCount    int    `json:"retry_count"`
	FallbackCount int    `json:"fallback_count"`
}

// attemptBuilder accumulates one attempt's events between its start and the
// next attempt of the same task.
type attemptBuilder struct {
	view          WorkerAttemptView
	ordinal       int64
	lastEventType string
	lastStatus    team.TaskStatus
	worldState    string
	finished      time.Time
	eventReceipt  *team.ExecutionReceipt
	fallbackTo    *execution.ExecutionTarget
	fallbackIndex *int
}

type pendingFallback struct {
	fromAttempt int
	to          *execution.ExecutionTarget
	index       *int
}

type lifecyclePayload struct {
	Status           team.TaskStatus        `json:"status"`
	Agent            string                 `json:"agent"`
	Attempt          int                    `json:"attempt"`
	DispatchAttempt  *int                   `json:"dispatch_attempt"`
	ExecutionReceipt *team.ExecutionReceipt `json:"execution_receipt"`
	FailureEvent     *struct {
		FailureClass team.TaskFailureClass `json:"failure_class"`
	} `json:"failure_event"`
}

type attemptWorldPayload struct {
	WorldID string `json:"world_id"`
}

type fallbackPayload struct {
	FromAttempt    int                        `json:"from_attempt"`
	ToTarget       *execution.ExecutionTarget `json:"to_target"`
	CandidateIndex *int                       `json:"candidate_index"`
}

var attemptWorldEventStates = map[string]string{
	string(team.EventAttemptWorkspacePrepared):        "prepared",
	string(team.EventAttemptWorkspaceApplyStarted):    "applying",
	string(team.EventAttemptWorkspaceApplied):         "applied",
	string(team.EventAttemptWorkspaceApplyConflicted): "conflicted",
	string(team.EventAttemptWorkspaceDiscarded):       "discarded",
	string(team.EventAttemptWorkspaceOrphanRemoved):   "removed",
}

var taskLifecycleEvents = map[string]bool{
	string(team.EventTaskStarted): true, string(team.EventTaskVerifying): true, string(team.EventTaskPaused): true,
	string(team.EventTaskCompleted): true, string(team.EventTaskFailed): true, string(team.EventTaskBlocked): true,
	string(team.EventTaskSkipped): true, string(team.EventTaskProtocolIncomplete): true, string(team.EventTaskCancelled): true,
	string(team.EventTaskPlanned): true,
}

// ProjectWorkerAttempts rebuilds every worker attempt from the canonical
// events and the current todos. It is pure: it never writes a store or the
// session, and now is injected, so equal inputs give equal output.
//
// An attempt starts at a task_started event carrying dispatch_attempt; other
// task_started events (re-commits of an in-progress task) belong to the
// attempt already open. Its key derives from that event's durable ID.
func ProjectWorkerAttempts(todos []*team.TodoItem, events []IndexedEvent, now time.Time) []WorkerAttemptView {
	byTask := make(map[string]*team.TodoItem, len(todos))
	for _, item := range todos {
		if item != nil {
			byTask[item.ID] = item
		}
	}
	ordered := append([]IndexedEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Ordinal < ordered[j].Ordinal })

	seen := make(map[string]bool)
	createdRun := make(map[string]string)
	current := make(map[string]*attemptBuilder)
	dispatchFallbacks := make(map[string]int)
	pending := make(map[string]pendingFallback)
	var attempts []*attemptBuilder
	for _, indexed := range ordered {
		event := indexed.Event
		identity := event.ID
		if identity == "" && event.IdempotencyKey != "" {
			identity = event.BranchID + "\x00" + event.IdempotencyKey
		}
		if identity != "" {
			if seen[identity] {
				continue
			}
			seen[identity] = true
		}
		switch {
		case event.Type == string(team.EventTaskCreated):
			if _, ok := createdRun[event.TaskID]; !ok {
				createdRun[event.TaskID] = event.RunID
			}
		case taskLifecycleEvents[event.Type]:
			var payload lifecyclePayload
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if event.Type == string(team.EventTaskStarted) && payload.DispatchAttempt != nil && *payload.DispatchAttempt > 0 {
				dispatchAttempt := *payload.DispatchAttempt
				if dispatchAttempt == 1 {
					dispatchFallbacks[event.TaskID] = 0
					delete(pending, event.TaskID)
				}
				runID := createdRun[event.TaskID]
				if runID == "" {
					runID = event.RunID
				}
				b := &attemptBuilder{ordinal: indexed.Ordinal, view: WorkerAttemptView{
					AttemptKey: workerAttemptKey(event.RunID, event.TaskID, event.ID),
					RunID:      runID, InvocationRunID: event.RunID, TaskID: event.TaskID, Agent: payload.Agent,
					StartedEventID: event.ID, Attempt: dispatchAttempt, OccurrenceAttempt: payload.Attempt,
					StartedAt: parseEventTime(event.Timestamp),
				}}
				if fallback, ok := pending[event.TaskID]; ok && fallback.fromAttempt+1 == dispatchAttempt {
					b.fallbackTo, b.fallbackIndex = fallback.to, fallback.index
					delete(pending, event.TaskID)
				}
				b.view.FallbackCount = dispatchFallbacks[event.TaskID]
				b.view.RetryCount = max(dispatchAttempt-1-b.view.FallbackCount, 0)
				current[event.TaskID] = b
				attempts = append(attempts, b)
			}
			b := current[event.TaskID]
			if b == nil {
				continue
			}
			if b.view.Agent == "" {
				b.view.Agent = payload.Agent
			}
			b.lastEventType, b.lastStatus = event.Type, payload.Status
			if isTerminalWorkerStatus(payload.Status) {
				b.finished = parseEventTime(event.Timestamp)
			}
			if payload.FailureEvent != nil && payload.FailureEvent.FailureClass != "" {
				b.view.FailureClass = string(payload.FailureEvent.FailureClass)
			}
			if receipt := payload.ExecutionReceipt; receipt != nil && receipt.Attempt == b.view.Attempt {
				copied := *receipt
				b.eventReceipt = &copied
			}
		case attemptWorldEventStates[event.Type] != "":
			b := current[event.TaskID]
			var payload attemptWorldPayload
			if b == nil || event.Attempt != b.view.Attempt || json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			b.view.AttemptWorldID = payload.WorldID
			b.worldState = attemptWorldEventStates[event.Type]
			b.lastEventType = event.Type
		case event.Type == string(team.EventExecutionFallbackDecided):
			var payload fallbackPayload
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if b := current[event.TaskID]; b != nil && payload.FromAttempt == b.view.Attempt {
				b.lastEventType = event.Type
			}
			dispatchFallbacks[event.TaskID]++
			pending[event.TaskID] = pendingFallback{fromAttempt: payload.FromAttempt, to: payload.ToTarget, index: payload.CandidateIndex}
		}
	}

	views := make([]WorkerAttemptView, 0, len(attempts))
	for _, b := range attempts {
		views = append(views, b.finish(byTask[b.view.TaskID], current[b.view.TaskID] == b, now))
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].TaskID != views[j].TaskID {
			return taskIDLess(views[i].TaskID, views[j].TaskID)
		}
		if views[i].OccurrenceAttempt != views[j].OccurrenceAttempt {
			return views[i].OccurrenceAttempt < views[j].OccurrenceAttempt
		}
		return false // keep event order for attempts of the same occurrence
	})
	return views
}

func (b *attemptBuilder) finish(todo *team.TodoItem, latest bool, now time.Time) WorkerAttemptView {
	view := b.view
	receipt := b.receiptFrom(todo)
	status := b.lastStatus
	if latest && todo != nil {
		status = todo.Status
	}
	view.TaskStatus = string(status)
	view.AttemptWorldState = b.worldState
	view.WorkspaceMode = string(agent.WorkerWorkspaceShared)
	if todo != nil {
		if todo.WorkerWorkspace != nil && todo.WorkerWorkspace.EffectiveMode == agent.WorkerWorkspaceIsolated {
			view.WorkspaceMode = string(agent.WorkerWorkspaceIsolated)
		}
		if todo.ExecutionRoute != nil {
			view.RouteName = todo.ExecutionRoute.Name
		}
		if todo.ResultContract != nil {
			view.ResultContractID = todo.ResultContract.ID
		}
	}
	switch {
	case receipt != nil && !receipt.ExecutionTarget.IsZero():
		view.ExecutionTarget = receipt.ExecutionTarget.String()
	case b.fallbackTo != nil:
		view.ExecutionTarget = b.fallbackTo.String()
	case todo != nil && !todo.ExecutionTarget.IsZero():
		view.ExecutionTarget = todo.ExecutionTarget.String()
	}
	switch {
	case receipt != nil && receipt.CandidateIndex != nil:
		index := *receipt.CandidateIndex
		view.CandidateIndex = &index
	case b.fallbackIndex != nil:
		index := *b.fallbackIndex
		view.CandidateIndex = &index
	case todo != nil && todo.ExecutionRoute != nil && b.fallbackTo == nil:
		index := 0
		view.CandidateIndex = &index
	}
	if receipt != nil {
		if receipt.Usage != nil {
			usage := *receipt.Usage
			view.Usage = &usage
		}
		if receipt.ResultContractID != "" {
			view.ResultContractID = receipt.ResultContractID
		}
		view.ResultValidation = string(receipt.ResultValidation)
	}
	// The attempt ends at its terminal lifecycle event, or, for an attempt a
	// retry superseded, when its receipt was written.
	finished := b.finished
	if finished.IsZero() && receipt != nil {
		finished = receipt.FinishedAt
	}
	view.FinishedAt = finished
	switch {
	case !view.StartedAt.IsZero() && !finished.IsZero() && !finished.Before(view.StartedAt):
		view.DurationMillis = finished.Sub(view.StartedAt).Milliseconds()
	case latest && !view.StartedAt.IsZero() && !isTerminalWorkerStatus(status) && now.After(view.StartedAt):
		view.DurationMillis = now.Sub(view.StartedAt).Milliseconds()
	}
	if latest && view.FailureClass == "" && todo != nil && todo.FailureEvent != nil && isFailedWorkerStatus(status) {
		view.FailureClass = string(todo.FailureEvent.FailureClass)
	}
	view.Activity = b.activity(status, latest, receipt != nil)
	return view
}

// receiptFrom finds the attempt's receipt on the current todo, matched by
// its identity, falling back to the receipt its lifecycle events carried.
func (b *attemptBuilder) receiptFrom(todo *team.TodoItem) *team.ExecutionReceipt {
	if todo != nil {
		var candidates []*team.ExecutionReceipt
		for i := range todo.ExecutionReceipts {
			receipt := &todo.ExecutionReceipts[i]
			if receipt.Attempt != b.view.Attempt || (receipt.OccurrenceAttempt != 0 && receipt.OccurrenceAttempt != b.view.OccurrenceAttempt) {
				continue
			}
			if receipt.RunID == b.view.InvocationRunID {
				return receipt
			}
			candidates = append(candidates, receipt)
		}
		// Without a run match, only an unambiguous receipt is used.
		if len(candidates) == 1 && candidates[0].RunID == "" {
			return candidates[0]
		}
	}
	return b.eventReceipt
}

func (b *attemptBuilder) activity(status team.TaskStatus, latest, hasReceipt bool) string {
	if !latest {
		return WorkerActivityTerminal
	}
	switch {
	case status == team.TaskPending || status == team.TaskPlanned:
		return WorkerActivityQueued
	case b.lastEventType == string(team.EventExecutionFallbackDecided):
		return WorkerActivityFallback
	case b.worldState == "applying":
		return WorkerActivityIntegrating
	case status == team.TaskProtocolIncomplete:
		return WorkerActivityResultRepair
	case status == team.TaskVerifying:
		return WorkerActivityVerifying
	case status == team.TaskInProgress && !hasReceipt:
		return WorkerActivityRunning
	case isTerminalWorkerStatus(status) || status == team.TaskPaused:
		return WorkerActivityTerminal
	}
	return WorkerActivityUnknown
}

func isTerminalWorkerStatus(status team.TaskStatus) bool {
	switch status {
	case team.TaskDone, team.TaskError, team.TaskBlocked, team.TaskSkipped:
		return true
	}
	return false
}

func isFailedWorkerStatus(status team.TaskStatus) bool {
	return status == team.TaskError || status == team.TaskBlocked
}

func workerAttemptKey(runID, taskID, eventID string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + taskID + "\x00" + eventID))
	return hex.EncodeToString(sum[:])[:16]
}

func parseEventTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// taskIDLess orders numeric task IDs numerically and everything else
// lexically after them.
func taskIDLess(a, b string) bool {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return an < bn
	case aerr == nil:
		return true
	case berr == nil:
		return false
	}
	return a < b
}

// WorkerHubSummary is the run-level worker counters shown beside the
// attempts. It projects RunMetrics fields owned by the phases that count
// them and defines no counter of its own; Attempts counts the views.
type WorkerHubSummary struct {
	Attempts                           int            `json:"attempts"`
	StructuredResultValidationFailures int            `json:"structured_result_validation_failures"`
	IsolatedAttemptsTotal              int            `json:"isolated_attempts_total"`
	AttemptWorkspaceConflictsTotal     int            `json:"attempt_workspace_conflicts_total"`
	AttemptWorldsOrphanRemoved         int            `json:"attempt_worlds_orphan_removed"`
	WorkerFallbacksTotal               int            `json:"worker_fallbacks_total"`
	WorkerFallbacksByClass             map[string]int `json:"worker_fallbacks_by_class,omitempty"`
}

// SummarizeWorkerAttempts builds the hub summary. A nil metrics leaves the
// run counters at zero.
func SummarizeWorkerAttempts(views []WorkerAttemptView, metrics *team.RunMetrics) WorkerHubSummary {
	summary := WorkerHubSummary{Attempts: len(views)}
	if metrics == nil {
		return summary
	}
	summary.StructuredResultValidationFailures = metrics.StructuredResultValidationFailures
	summary.IsolatedAttemptsTotal = metrics.IsolatedAttemptsTotal
	summary.AttemptWorkspaceConflictsTotal = metrics.AttemptWorkspaceConflictsTotal
	summary.AttemptWorldsOrphanRemoved = metrics.AttemptWorldsOrphanRemoved
	summary.WorkerFallbacksTotal = metrics.WorkerFallbacksTotal
	for class, count := range metrics.WorkerFallbacksByClass {
		if summary.WorkerFallbacksByClass == nil {
			summary.WorkerFallbacksByClass = make(map[string]int)
		}
		summary.WorkerFallbacksByClass[string(class)] = count
	}
	return summary
}

// WorkerAttempts is the hub's data: the projected attempts and the summary.
type WorkerAttempts struct {
	Attempts []WorkerAttemptView `json:"attempts"`
	Summary  WorkerHubSummary    `json:"summary"`
}

// LoadWorkerAttempts reads the active branch lineage of a workspace and
// projects its worker attempts, with todos replayed from the same events.
// It is read-only. A workspace without events has no attempts.
func LoadWorkerAttempts(ctx context.Context, workspace string, metrics *team.RunMetrics, now time.Time) (WorkerAttempts, error) {
	lineage, err := LoadLineage(ctx, InspectQuery{Workspace: workspace})
	if err != nil {
		return WorkerAttempts{}, err
	}
	events := make([]team.RunEvent, 0, len(lineage.Events))
	for _, indexed := range lineage.Events {
		events = append(events, indexed.Event)
	}
	todos, err := team.ReplayTodoList(events)
	if err != nil {
		return WorkerAttempts{}, fmt.Errorf("%w: replay worker tasks: %v", ErrIntegrity, err)
	}
	return ProjectWorkerHub(todos, lineage.Events, metrics, now), nil
}

// ProjectWorkerHub is the single projection every hub surface (CLI, TUI,
// report) renders: the attempts and their summary.
func ProjectWorkerHub(todos []*team.TodoItem, events []IndexedEvent, metrics *team.RunMetrics, now time.Time) WorkerAttempts {
	attempts := ProjectWorkerAttempts(todos, events, now)
	return WorkerAttempts{Attempts: attempts, Summary: SummarizeWorkerAttempts(attempts, metrics)}
}
