package team

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// The worker prompt marks injected memory as id=context:<id> and never shows
// the runtime-owned retrieval ID, so a claim must be accepted in that form.
func TestMemoryUseBindsRuntimeRetrievalForPromptID(t *testing.T) {
	tests := []struct {
		name          string
		contextItemID string
		retrievalID   string
		wantErr       string
	}{
		{name: "prompt marker spelling without retrieval", contextItemID: "context:memory-1"},
		{name: "bare id without retrieval", contextItemID: " memory-1 "},
		{name: "explicit matching retrieval", contextItemID: "memory-1", retrievalID: "retrieval-1"},
		{name: "wrong retrieval fails closed", contextItemID: "memory-1", retrievalID: "retrieval-other", wantErr: "retrieval_id is not valid"},
		{name: "uninjected id fails closed", contextItemID: "context:forged", wantErr: "was not injected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, manifest := memoryValidationCoordinator(t)
			result := &TaskResult{TaskID: manifest.TaskID, Source: "submitted", MemoryUses: []MemoryUseRef{{RetrievalID: tt.retrievalID, ContextItemID: tt.contextItemID, Disposition: MemoryUseApplied, Confidence: 1}}}
			err := c.validateMemoryUseClaims(context.Background(), manifest.TaskID, result)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validation error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := result.MemoryUses[0]; got.RetrievalID != manifest.RetrievalID || got.ContextItemID != "memory-1" {
				t.Fatalf("bound use = %+v, want retrieval %q and item memory-1", got, manifest.RetrievalID)
			}
		})
	}
}

func TestMemoryUsesAreDroppedWhenLearningIsOff(t *testing.T) {
	c, manifest := memoryValidationCoordinator(t)
	c.session = &TeamSession{Config: agent.TeamConfig{MemoryLearning: agent.DefaultMemoryLearningPolicy()}}
	result := &TaskResult{TaskID: manifest.TaskID, Source: "submitted", MemoryUses: []MemoryUseRef{{ContextItemID: "context:forged", Disposition: MemoryUseApplied, Confidence: 1}}}
	if err := c.validateMemoryUseClaims(context.Background(), manifest.TaskID, result); err != nil {
		t.Fatalf("learning-off claim rejected the result: %v", err)
	}
	if result.MemoryUses != nil {
		t.Fatalf("learning-off claim kept memory uses %+v", result.MemoryUses)
	}
}

func TestSubmitResultMemoryUseRetrievalIsOptional(t *testing.T) {
	info := submitResultToolInfo(taskResultSubmissionContract{})
	schema, _ := info.Parameters["memory_uses"].(map[string]any)
	item, _ := schema["items"].(map[string]any)
	required, _ := item["required"].([]string)
	if slices.Contains(required, "retrieval_id") || !slices.Contains(required, "context_item_id") {
		t.Fatalf("memory_uses required fields = %v", required)
	}
}

func TestMemoryOccurrenceIDSurvivesRetryAndResume(t *testing.T) {
	tests := []struct {
		name string
		item *TodoItem
		want string
	}{
		{name: "nil todo", item: nil, want: ""},
		{name: "no manifest", item: &TodoItem{ID: "1"}, want: ""},
		{name: "first run owns the occurrence", item: &TodoItem{ID: "1", MemoryManifests: []MemoryInjectionManifest{
			{RunID: "run-a", Attempt: 1},
			{RunID: "run-a", Attempt: 2},
			{RunID: "run-resumed", Attempt: 3},
		}}, want: "run-a/1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := memoryOccurrenceID(tt.item); got != tt.want {
				t.Fatalf("occurrence = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMemoryObservationSupportAndOccurrence(t *testing.T) {
	outcome := func(effective float64, occurrence string) []byte {
		raw, _ := json.Marshal(memoryOutcomePayload{
			memoryEventPayload: memoryEventPayload{SchemaVersion: memoryEventSchemaVersion, RetrievalID: "retrieval-1", ContextItemID: "memory-1", PolicyVersion: "memory-policy-v1", OccurrenceID: occurrence},
			Signal:             "verification_passed", Disposition: MemoryUseApplied, EffectiveWeight: effective, Direction: "positive",
		})
		return raw
	}
	tests := []struct {
		name         string
		payload      []byte
		wantSupport  int
		wantTaskKey  string
		wantPositive float64
	}{
		{name: "credited pass is support", payload: outcome(0.5, "run-a/1"), wantSupport: 1, wantTaskKey: "run-a/1", wantPositive: 0.5},
		{name: "capped pass is not support", payload: outcome(0, "run-a/1"), wantSupport: 0, wantTaskKey: "run-a/1"},
		{name: "event without occurrence keeps todo id", payload: outcome(1, ""), wantSupport: 1, wantTaskKey: "1", wantPositive: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := RunEvent{Type: "memory_outcome_recorded", TaskID: "1", Timestamp: time.Now().UTC().Format(time.RFC3339Nano), IdempotencyKey: "key", Payload: tt.payload}
			observation, ok := memoryObservationFromEvent(event, agent.DefaultMemoryLearningPolicy())
			if !ok {
				t.Fatal("outcome event was not reduced")
			}
			if observation.VerifiedSupportDelta != tt.wantSupport || observation.TaskID != tt.wantTaskKey || observation.PositiveWeight != tt.wantPositive {
				t.Fatalf("observation = %+v", observation)
			}
		})
	}
}

// Todo IDs restart in every fresh run while the workspace event log spans runs.
// A later run's task with the same ID must earn its own credit and count as a
// separate task, while a retry of one occurrence stays within that cap.
func TestOutcomeCreditIsScopedToTaskOccurrence(t *testing.T) {
	c, repo := outcomeTestCoordinator(t, "memory-1")
	uses := func(retrieval string) []MemoryUseRef {
		return []MemoryUseRef{{RetrievalID: retrieval, ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}
	}
	first := outcomeTestItem(uses("retrieval-1"), &VerificationResult{ExitCode: 0})
	c.recordMemoryOutcomeForTask(first, "task_completed")

	retry := first.MemoryManifests[0]
	retry.RetrievalID, retry.Attempt = "retrieval-1-retry", 2
	first.MemoryManifests = append(first.MemoryManifests, retry)
	first.TypedResult = &TaskResult{TaskID: first.ID, Attempt: 2, MemoryUses: uses("retrieval-1-retry")}
	c.recordMemoryOutcomeForTask(first, "task_completed")

	second := outcomeTestItem(uses("retrieval-2"), &VerificationResult{ExitCode: 0})
	second.MemoryManifests[0].RunID, second.MemoryManifests[0].RetrievalID = "run-2", "retrieval-2"
	c.recordMemoryOutcomeForTask(second, "task_completed")

	aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", "memory-policy-v1")
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.PositiveWeight != 2 || aggregate.VerifiedSupportCount != 2 || aggregate.IndependentTaskCount != 2 {
		t.Fatalf("aggregate = %+v, want weight 2, verified 2, independent tasks 2", aggregate)
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	reduced, err := contextstore.ReduceExperienceAggregates(ExperienceObservationsFromEvents(events, c.session.Config.MemoryLearning))
	if err != nil {
		t.Fatal(err)
	}
	if len(reduced) != 1 || reduced[0].PositiveWeight != aggregate.PositiveWeight || reduced[0].VerifiedSupportCount != aggregate.VerifiedSupportCount || reduced[0].IndependentTaskCount != aggregate.IndependentTaskCount {
		t.Fatalf("pure reducer = %+v, live = %+v", reduced, aggregate)
	}
}

// Terminal tasks commit through CommitTaskTransition, which the checkpoint
// emitter only revisits, so the commit itself must record outcome credit.
func TestTerminalTransitionRecordsMemoryOutcome(t *testing.T) {
	tests := []struct {
		name         string
		mode         agent.MemoryLearningMode
		wantOutcomes int
	}{
		{name: "learning on records credit at commit", mode: agent.MemoryLearningObserve, wantOutcomes: 1},
		{name: "learning off records nothing", mode: agent.MemoryLearningOff, wantOutcomes: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			workspace := t.TempDir()
			repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			if err := repo.Append(ctx, contextstore.ContextItem{ID: "memory-1", Kind: contextstore.ContextPattern, Content: "procedure", Scope: contextstore.Scope{ProjectID: "project"}, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
				t.Fatal(err)
			}
			policy := agent.DefaultMemoryLearningPolicy()
			policy.Mode = tt.mode
			coord := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{MemoryLearning: policy}}, sessionData: NewSession(), taskTracker: NewTaskTracker(), contextRepo: repo}
			coord.initEventStore()
			t.Cleanup(func() { _ = coord.EventStore().Close() })
			items, err := coord.CommitTaskCreation(ctx, []TodoSpec{{Agent: "worker", Desc: "apply memory"}})
			if err != nil {
				t.Fatal(err)
			}
			id := items[0].ID
			if err := coord.CommitTaskTransition(ctx, id, TaskPending, TaskInProgress, "started", "", nil); err != nil {
				t.Fatal(err)
			}
			list := coord.taskTracker.TodoList()
			if err := list.SetMemoryManifest(id, &MemoryInjectionManifest{RetrievalID: "retrieval-1", RunID: "run-1", TaskID: id, Attempt: 1, Agent: "worker", PolicyVersion: policy.PolicyVersion, Items: []MemoryInjectionItem{{ContextItemID: "memory-1"}}}); err != nil {
				t.Fatal(err)
			}
			if err := list.SetTypedResult(id, &TaskResult{TaskID: id, Attempt: 1, MemoryUses: []MemoryUseRef{{RetrievalID: "retrieval-1", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}}); err != nil {
				t.Fatal(err)
			}
			if err := list.SetVerificationResult(id, &VerificationResult{ExitCode: 0}); err != nil {
				t.Fatal(err)
			}
			if err := coord.CommitTaskTransition(ctx, id, TaskInProgress, TaskDone, "completed", "done", nil); err != nil {
				t.Fatal(err)
			}
			events, err := coord.EventStore().ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			outcomes := 0
			for _, event := range events {
				if event.Type == "memory_outcome_recorded" {
					outcomes++
				}
			}
			if outcomes != tt.wantOutcomes {
				t.Fatalf("memory_outcome_recorded events = %d, want %d", outcomes, tt.wantOutcomes)
			}
		})
	}
}
