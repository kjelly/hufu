package inspect

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

var workerEpoch = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

// workerEvents builds an ordered canonical event stream.
type workerEvents struct {
	events []IndexedEvent
}

func (w *workerEvents) add(runID, taskID, eventType string, attempt, second int, payload map[string]any) string {
	id := fmt.Sprintf("evt-%d", len(w.events)+1)
	raw, _ := json.Marshal(payload)
	w.events = append(w.events, IndexedEvent{Ordinal: int64(len(w.events) + 1), Event: team.RunEvent{
		ID: id, RunID: runID, TaskID: taskID, Type: eventType, Attempt: attempt,
		Timestamp: workerEpoch.Add(time.Duration(second) * time.Second).Format(time.RFC3339Nano), Payload: raw,
	}})
	return id
}

func (w *workerEvents) start(runID, taskID string, dispatchAttempt, occurrenceAttempt, second int) string {
	return w.add(runID, taskID, string(team.EventTaskStarted), occurrenceAttempt, second, map[string]any{
		"status": "in_progress", "agent": "coder", "attempt": occurrenceAttempt, "dispatch_attempt": dispatchAttempt,
	})
}

func (w *workerEvents) status(runID, taskID string, status team.TaskStatus, second int, extra map[string]any) {
	eventType := map[team.TaskStatus]team.EventType{
		team.TaskInProgress: team.EventTaskStarted, team.TaskVerifying: team.EventTaskVerifying, team.TaskDone: team.EventTaskCompleted,
		team.TaskError: team.EventTaskFailed, team.TaskBlocked: team.EventTaskBlocked, team.TaskProtocolIncomplete: team.EventTaskProtocolIncomplete,
		team.TaskPaused: team.EventTaskPaused,
	}[status]
	payload := map[string]any{"status": string(status), "agent": "coder", "attempt": 1}
	for key, value := range extra {
		payload[key] = value
	}
	w.add(runID, taskID, string(eventType), 1, second, payload)
}

func primaryTarget() execution.ExecutionTarget {
	return execution.ExecutionTarget{Backend: "ollama", Model: "primary"}
}

func TestProjectWorkerAttemptsIsPureAndDeterministic(t *testing.T) {
	var w workerEvents
	w.add("run-1", "1", string(team.EventTaskCreated), 1, 0, map[string]any{"status": "pending"})
	w.start("run-1", "1", 1, 1, 1)
	w.status("run-1", "1", team.TaskDone, 5, nil)
	todos := []*team.TodoItem{{ID: "1", Agent: "coder", Status: team.TaskDone, ExecutionTarget: primaryTarget()}}
	hash := func() [32]byte {
		encoded, _ := json.Marshal(w.events)
		return sha256.Sum256(encoded)
	}
	before := hash()
	first := ProjectWorkerAttempts(todos, w.events, workerEpoch.Add(time.Hour))
	second := ProjectWorkerAttempts(todos, w.events, workerEpoch.Add(time.Hour))
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("projection is not deterministic:\n%+v\n%+v", first, second)
	}
	if hash() != before {
		t.Fatal("the projection modified its input events")
	}
	if len(first) != 1 || first[0].Activity != WorkerActivityTerminal || first[0].DurationMillis != 4000 || first[0].ExecutionTarget != "ollama/primary" {
		t.Fatalf("view = %+v", first)
	}
}

func TestProjectWorkerAttemptsIdentityAcrossResumeAndDuplicates(t *testing.T) {
	var w workerEvents
	w.add("run-1", "1", string(team.EventTaskCreated), 1, 0, nil)
	w.start("run-1", "1", 1, 1, 1)
	// A re-commit of the in-progress task is not a new attempt.
	w.add("run-1", "1", string(team.EventTaskStarted), 1, 2, map[string]any{"status": "in_progress", "agent": "coder", "attempt": 1})
	// The process crashed; a new invocation re-dispatched the occurrence.
	resumeStart := w.start("run-2", "1", 1, 1, 10)
	w.status("run-2", "1", team.TaskDone, 12, nil)
	// A duplicated event must not add an attempt.
	w.events = append(w.events, w.events[len(w.events)-2])
	todos := []*team.TodoItem{{ID: "1", Agent: "coder", Status: team.TaskDone, ExecutionTarget: primaryTarget(), ExecutionReceipts: []team.ExecutionReceipt{
		{RunID: "run-1", TaskID: "1", Attempt: 1, OccurrenceAttempt: 1, Usage: &team.ExecutionUsage{TotalTokens: 10}},
		{RunID: "run-2", TaskID: "1", Attempt: 1, OccurrenceAttempt: 1, Usage: &team.ExecutionUsage{TotalTokens: 20}},
	}}}
	views := ProjectWorkerAttempts(todos, w.events, workerEpoch)
	if len(views) != 2 {
		t.Fatalf("attempts = %d, want the crashed attempt and its re-dispatch (%+v)", len(views), views)
	}
	if views[0].AttemptKey == views[1].AttemptKey || views[1].StartedEventID != resumeStart {
		t.Fatalf("attempt keys %q/%q, second started by %q", views[0].AttemptKey, views[1].AttemptKey, views[1].StartedEventID)
	}
	if views[0].RunID != "run-1" || views[0].InvocationRunID != "run-1" || views[1].RunID != "run-1" || views[1].InvocationRunID != "run-2" {
		t.Fatalf("run identities = %+v", views)
	}
	if views[0].Usage == nil || views[0].Usage.TotalTokens != 10 || views[1].Usage == nil || views[1].Usage.TotalTokens != 20 {
		t.Fatal("the resumed invocation's receipt replaced the earlier attempt's")
	}
	if views[0].Activity != WorkerActivityTerminal || views[1].TaskStatus != string(team.TaskDone) {
		t.Fatalf("activities = %s/%s", views[0].Activity, views[1].TaskStatus)
	}
}

func TestProjectWorkerAttemptsFallbackAndWorld(t *testing.T) {
	fallback := execution.ExecutionTarget{Backend: "openai", Model: "fallback"}
	var w workerEvents
	w.start("run-1", "1", 1, 1, 1)
	w.add("run-1", "1", string(team.EventAttemptWorkspacePrepared), 1, 1, map[string]any{"world_id": "aw-1"})
	w.add("run-1", "1", string(team.EventAttemptWorkspaceDiscarded), 1, 2, map[string]any{"world_id": "aw-1"})
	w.add("run-1", "1", string(team.EventExecutionFallbackDecided), 1, 2, map[string]any{
		"from_attempt": 1, "to_target": fallback, "candidate_index": 1, "failure_class": "rate_limited",
	})
	route := &team.ExecutionRouteBinding{Name: "coding", Candidates: []execution.ExecutionTarget{primaryTarget(), fallback}}
	isolated := &team.WorkerWorkspacePolicy{Mode: agent.WorkerWorkspaceIsolated, EffectiveMode: agent.WorkerWorkspaceIsolated}
	todo := &team.TodoItem{ID: "1", Agent: "coder", Status: team.TaskInProgress, ExecutionTarget: primaryTarget(), ExecutionRoute: route, WorkerWorkspace: isolated}

	// Between the fallback decision and the next attempt's start.
	views := ProjectWorkerAttempts([]*team.TodoItem{todo}, w.events, workerEpoch.Add(3*time.Second))
	if len(views) != 1 || views[0].Activity != WorkerActivityFallback || views[0].AttemptWorldState != "discarded" || views[0].AttemptWorldID != "aw-1" {
		t.Fatalf("pending fallback view = %+v", views)
	}

	w.start("run-1", "1", 2, 1, 3)
	w.add("run-1", "1", string(team.EventAttemptWorkspacePrepared), 2, 3, map[string]any{"world_id": "aw-2"})
	w.add("run-1", "1", string(team.EventAttemptWorkspaceApplyStarted), 2, 6, map[string]any{"world_id": "aw-2"})
	views = ProjectWorkerAttempts([]*team.TodoItem{todo}, w.events, workerEpoch.Add(7*time.Second))
	if len(views) != 2 {
		t.Fatalf("views = %+v", views)
	}
	first, second := views[0], views[1]
	if first.AttemptKey == second.AttemptKey || second.FallbackCount != 1 || second.RetryCount != 0 {
		t.Fatalf("fallback attempt identity = %+v", second)
	}
	if second.ExecutionTarget != "openai/fallback" || second.CandidateIndex == nil || *second.CandidateIndex != 1 || second.RouteName != "coding" {
		t.Fatalf("fallback attempt target = %+v", second)
	}
	if first.CandidateIndex == nil || *first.CandidateIndex != 0 || first.ExecutionTarget != "ollama/primary" {
		t.Fatalf("primary attempt target = %+v", first)
	}
	if second.WorkspaceMode != "isolated" || second.AttemptWorldID != "aw-2" || second.AttemptWorldState != "applying" || second.Activity != WorkerActivityIntegrating {
		t.Fatalf("world correlation = %+v", second)
	}
	if second.DurationMillis != 4000 || second.Usage != nil {
		t.Fatalf("running attempt duration %d usage %+v, want now-injected duration and unknown usage", second.DurationMillis, second.Usage)
	}
}

func TestProjectWorkerAttemptsStatesAndValidation(t *testing.T) {
	tests := []struct {
		name         string
		status       team.TaskStatus
		failureClass team.TaskFailureClass
		receipt      *team.ExecutionReceipt
		wantActivity string
	}{
		{name: "running without a receipt", status: team.TaskInProgress, wantActivity: WorkerActivityRunning},
		{name: "in progress with a receipt is unknown", status: team.TaskInProgress, receipt: &team.ExecutionReceipt{RunID: "run-1", Attempt: 1, OccurrenceAttempt: 1}, wantActivity: WorkerActivityUnknown},
		{name: "verifying", status: team.TaskVerifying, wantActivity: WorkerActivityVerifying},
		{name: "result repair", status: team.TaskProtocolIncomplete, wantActivity: WorkerActivityResultRepair},
		{name: "blocked", status: team.TaskBlocked, failureClass: team.FailureEnvironment, wantActivity: WorkerActivityTerminal},
		{name: "error", status: team.TaskError, failureClass: team.FailureWorkspaceConflict, wantActivity: WorkerActivityTerminal},
		{name: "paused", status: team.TaskPaused, wantActivity: WorkerActivityTerminal},
		{name: "invalid structured payload", status: team.TaskError, receipt: &team.ExecutionReceipt{RunID: "run-1", Attempt: 1, OccurrenceAttempt: 1, ResultContractID: "review-v1", ResultValidation: team.ResultValidationInvalid}, wantActivity: WorkerActivityTerminal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w workerEvents
			w.start("run-1", "1", 1, 1, 1)
			extra := map[string]any{}
			if tt.failureClass != "" {
				extra["failure_event"] = map[string]any{"failure_class": string(tt.failureClass)}
			}
			if tt.status != team.TaskInProgress {
				w.status("run-1", "1", tt.status, 2, extra)
			}
			todo := &team.TodoItem{ID: "1", Agent: "coder", Status: tt.status}
			if tt.receipt != nil {
				todo.ExecutionReceipts = []team.ExecutionReceipt{*tt.receipt}
			}
			views := ProjectWorkerAttempts([]*team.TodoItem{todo}, w.events, workerEpoch.Add(time.Minute))
			if len(views) != 1 {
				t.Fatalf("views = %+v", views)
			}
			view := views[0]
			if view.Activity != tt.wantActivity || view.TaskStatus != string(tt.status) || view.FailureClass != string(tt.failureClass) {
				t.Fatalf("view = %+v, want activity %s class %s", view, tt.wantActivity, tt.failureClass)
			}
			if tt.receipt != nil && tt.receipt.ResultValidation != "" && (view.ResultValidation != string(tt.receipt.ResultValidation) || view.ResultContractID != tt.receipt.ResultContractID) {
				t.Fatalf("result validation = %q %q", view.ResultContractID, view.ResultValidation)
			}
			if view.ExecutionTarget != "" {
				t.Fatalf("a missing target was guessed as %q", view.ExecutionTarget)
			}
		})
	}
}

func TestProjectWorkerAttemptsNeverCarriesContent(t *testing.T) {
	const secret = "api_key=SECRET-PROJECTION-XYZ"
	var w workerEvents
	w.add("run-1", "1", string(team.EventTaskStarted), 1, 1, map[string]any{
		"status": "in_progress", "agent": "coder", "attempt": 1, "dispatch_attempt": 1,
		"output": secret, "detail": secret, "goal": secret, "summary": secret,
	})
	w.status("run-1", "1", team.TaskError, 3, map[string]any{
		"output": secret, "failure_event": map[string]any{"failure_class": "execution", "error": secret},
		"execution_receipt": map[string]any{"attempt": 1, "repair_provenance": map[string]any{"prompt": secret}},
	})
	todo := &team.TodoItem{
		ID: "1", Agent: "coder", Status: team.TaskError, Goal: secret, Output: secret, Detail: secret,
		ExecutionReceipts: []team.ExecutionReceipt{{
			RunID: "run-1", Attempt: 1, OccurrenceAttempt: 1,
			RepairProvenance: &team.RepairProvenance{Prompt: secret},
			ToolDispositions: []team.ToolExecutionDisposition{{ToolName: secret}},
		}},
	}
	encoded, err := json.Marshal(ProjectWorkerAttempts([]*team.TodoItem{todo}, w.events, workerEpoch))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET-PROJECTION") {
		t.Fatalf("the projection leaked content: %s", encoded)
	}
}

func TestSummarizeWorkerAttemptsProjectsRunMetrics(t *testing.T) {
	views := []WorkerAttemptView{{TaskID: "1"}, {TaskID: "1"}}
	summary := SummarizeWorkerAttempts(views, &team.RunMetrics{
		StructuredResultValidationFailures: 1, IsolatedAttemptsTotal: 2, AttemptWorkspaceConflictsTotal: 1,
		AttemptWorldsOrphanRemoved: 3, WorkerFallbacksTotal: 1, WorkerFallbacksByClass: map[team.ProviderFailureClass]int{team.ProviderRateLimited: 1},
	})
	want := WorkerHubSummary{
		Attempts: 2, StructuredResultValidationFailures: 1, IsolatedAttemptsTotal: 2, AttemptWorkspaceConflictsTotal: 1,
		AttemptWorldsOrphanRemoved: 3, WorkerFallbacksTotal: 1, WorkerFallbacksByClass: map[string]int{"rate_limited": 1},
	}
	if !reflect.DeepEqual(summary, want) {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if got := SummarizeWorkerAttempts(views, nil); got.Attempts != 2 || got.WorkerFallbacksTotal != 0 {
		t.Fatalf("summary without metrics = %+v", got)
	}
}
