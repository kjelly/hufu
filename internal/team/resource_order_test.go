package team

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

// scriptedResultAgent is a fake worker that submits, through the real
// submit_result tool, the result its script assigns to the task's goal (success
// by default). It records how many workers ran at once.
type scriptedResultAgent struct {
	c      *Coordinator
	script map[string]string

	mu      sync.Mutex
	active  int
	peak    int
	started []string
}

func (a *scriptedResultAgent) Generate(ctx context.Context, _ fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func (a *scriptedResultAgent) Stream(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func (a *scriptedResultAgent) run(ctx context.Context) (*fantasy.AgentResult, error) {
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	goal := ""
	if item := a.c.todoItemByID(todoID); item != nil {
		goal = item.Goal
	}
	a.mu.Lock()
	a.active++
	a.peak = max(a.peak, a.active)
	a.started = append(a.started, goal)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.active--
		a.mu.Unlock()
	}()
	input, ok := a.script[goal]
	if !ok {
		input = `{"status":"success","summary":"done"}`
	}
	if _, err := (&submitResultTool{coordinator: a.c, todoID: todoID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input}); err != nil {
		return nil, err
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "reported"}}}}, nil
}

func newResourceOrderCoordinator(t *testing.T, maxConcurrent, retries int) *Coordinator {
	t.Helper()
	session := &TeamSession{
		Workspace: t.TempDir(),
		Config:    agent.TeamConfig{Name: "resource-order", AllowFreeTextResults: true, MaxRetries: retries},
		Agents: map[string]*agent.AgentDef{
			"worker": {Name: "worker", Role: "worker", MaxRetries: retries, Generation: agent.GenerationParams{Model: "test-model"}},
		},
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, maxConcurrent, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(session.Workspace, "run-resource-order", "session-resource-order")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c.eventStore = store
	c.SetEventJournal(eventStoreJournal{store: store})
	c.executionRunID = "run-resource-order"
	c.SetSessionData(NewSession())
	c.phaseWorkflow = nil
	return c
}

const (
	failedResult  = `{"status":"failed","summary":"build failed"}`
	blockedResult = `{"status":"blocked","summary":"BLOCKED: step-risk=system-change; needs explicit user confirmation"}`
)

// TestSerializedMutationsDoNotInheritFailure reproduces the operator run in
// which three independent workspace writers were serialized on the whole
// workspace and one blocked step blocked every later step.
func TestSerializedMutationsDoNotInheritFailure(t *testing.T) {
	tests := []struct {
		name        string
		retries     int
		script      map[string]string
		dependsOn   []int // declared dependencies of the second task
		wantStatus  []TaskStatus
		wantWrapUp  bool
		wantStarted []string
	}{
		{
			name: "failed first writer", script: map[string]string{"first": failedResult},
			wantStatus:  []TaskStatus{TaskError, TaskDone, TaskDone},
			wantStarted: []string{"first", "second", "third"},
		},
		{
			name: "blocked first writer", retries: 1, script: map[string]string{"first": blockedResult},
			wantStatus:  []TaskStatus{TaskBlocked, TaskDone, TaskDone},
			wantWrapUp:  true,
			wantStarted: []string{"first", "second", "third"},
		},
		{
			name: "declared dependency still blocks", script: map[string]string{"first": failedResult}, dependsOn: []int{0},
			wantStatus:  []TaskStatus{TaskError, TaskBlocked, TaskDone},
			wantStarted: []string{"first", "third"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newResourceOrderCoordinator(t, 2, test.retries)
			worker := &scriptedResultAgent{c: c, script: test.script}
			c.workerAgentOverride = worker
			_, _ = c.ExecuteTasks(context.Background(), []TaskDef{
				{Agent: "worker", Goal: "first", SideEffect: SideEffectWorkspaceWrite},
				{Agent: "worker", Goal: "second", SideEffect: SideEffectWorkspaceWrite, DependsOn: test.dependsOn},
				{Agent: "worker", Goal: "third", SideEffect: SideEffectWorkspaceWrite},
			})

			items := c.taskTracker.TodoList().Items()
			if len(items) != 3 {
				t.Fatalf("tasks = %d, want 3", len(items))
			}
			for i, want := range test.wantStatus {
				if items[i].Status != want {
					t.Errorf("task %s status = %s, want %s (%q)", items[i].ID, items[i].Status, want, items[i].Detail)
				}
			}
			// The conflicting writers keep their batch order through
			// durable ordering edges instead of declared dependencies.
			if !slices.Equal(items[1].OrderAfter, []string{items[0].ID}) && len(test.dependsOn) == 0 {
				t.Errorf("second order_after = %v, want [%s]", items[1].OrderAfter, items[0].ID)
			}
			if !slices.Equal(items[2].OrderAfter, []string{items[0].ID, items[1].ID}) || len(items[2].DependsOn) != 0 {
				t.Errorf("third order_after = %v depends_on = %v, want ordering on both earlier writers only", items[2].OrderAfter, items[2].DependsOn)
			}
			// Replaying the durable events alone restores the ordering edges.
			events, err := c.EventJournal().ReadEvents(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if replayed := ReduceToTodoList(events); len(replayed) != 3 || !slices.Equal(replayed[2].OrderAfter, items[2].OrderAfter) {
				t.Errorf("replayed order_after = %v, want %v", replayed[len(replayed)-1].OrderAfter, items[2].OrderAfter)
			}
			if test.dependsOn != nil && !strings.Contains(items[1].Detail, "blocked by failed dependency") {
				t.Errorf("second detail = %q, want the declared dependency failure", items[1].Detail)
			}
			if worker.peak != 1 || !slices.Equal(worker.started, test.wantStarted) {
				t.Errorf("workers started %v with peak concurrency %d, want %v one at a time", worker.started, worker.peak, test.wantStarted)
			}
			if c.IsWrapUp() != test.wantWrapUp {
				t.Errorf("wrap-up = %v, want %v", c.IsWrapUp(), test.wantWrapUp)
			}
		})
	}
}

// TestIndependentTaskStartsAfterASiblingFails covers a task that waits only
// for a free slot: a failed sibling must release the slot instead of leaving
// it to be stranded as if a dependency had failed.
func TestIndependentTaskStartsAfterASiblingFails(t *testing.T) {
	c := newResourceOrderCoordinator(t, 1, 0)
	worker := &scriptedResultAgent{c: c, script: map[string]string{"first": failedResult}}
	c.workerAgentOverride = worker
	_, _ = c.ExecuteTasks(context.Background(), []TaskDef{
		{Agent: "worker", Goal: "first", SideEffect: SideEffectNone},
		{Agent: "worker", Goal: "second", SideEffect: SideEffectNone},
	})
	items := c.taskTracker.TodoList().Items()
	if len(items) != 2 || items[0].Status != TaskError || items[1].Status != TaskDone {
		t.Fatalf("tasks = %s/%s, want error then done", items[0].Status, items[1].Status)
	}
}

// TestOrderPredecessorSettlement checks when a task ordered after another may
// start: once that task has finished with any outcome, or once it can no
// longer start because a declared dependency of it did not complete. The
// resource lease alone cannot enforce this while the predecessor waits.
func TestOrderPredecessorSettlement(t *testing.T) {
	tests := []struct {
		name        string
		predecessor TaskStatus
		dependency  TaskStatus // state of the predecessor's declared dependency
		want        bool
	}{
		{name: "pending predecessor", predecessor: TaskPending, dependency: TaskDone, want: false},
		{name: "running predecessor", predecessor: TaskInProgress, dependency: TaskDone, want: false},
		{name: "predecessor waiting on a running dependency", predecessor: TaskPending, dependency: TaskInProgress, want: false},
		{name: "done predecessor", predecessor: TaskDone, dependency: TaskDone, want: true},
		{name: "failed predecessor", predecessor: TaskError, dependency: TaskDone, want: true},
		{name: "blocked predecessor", predecessor: TaskBlocked, dependency: TaskDone, want: true},
		{name: "predecessor whose dependency failed", predecessor: TaskPending, dependency: TaskError, want: true},
		{name: "predecessor whose dependency was blocked", predecessor: TaskPending, dependency: TaskBlocked, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coord := &Coordinator{taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}}
			tasks := []TaskDef{
				{Agent: "reader"},
				{Agent: "writer", SideEffect: SideEffectWorkspaceWrite, DependsOn: []int{0}},
				{Agent: "writer", SideEffect: SideEffectWorkspaceWrite, OrderAfter: []int{1}},
			}
			s := mustNewDAGScheduler(t, coord, tasks, nil, nil)
			s.states[0], s.states[1] = test.dependency, test.predecessor
			if got := s.readyToLaunch(2); got != test.want {
				t.Fatalf("ready = %v, want %v", got, test.want)
			}
		})
	}
}

// TestExplicitEmptyListsMatchTheDurableOccurrence covers a coordinator that
// writes "depends_on": [] for an independent step. JSON decodes that into an
// empty slice while the durable occurrence rebuilds nil, and the scheduler
// contract check rejected the whole batch.
func TestExplicitEmptyListsMatchTheDurableOccurrence(t *testing.T) {
	c := newResourceOrderCoordinator(t, 2, 0)
	worker := &scriptedResultAgent{c: c}
	c.workerAgentOverride = worker
	_, err := c.ExecuteTasks(context.Background(), []TaskDef{
		{Agent: "worker", Goal: "first", SideEffect: SideEffectNone, DependsOn: []int{}, ContextFiles: []string{}},
		{Agent: "worker", Goal: "second", SideEffect: SideEffectWorkspaceWrite, DependsOn: []int{}},
	})
	if err != nil {
		t.Fatalf("ExecuteTasks: %v", err)
	}
	if items := c.taskTracker.TodoList().Items(); len(items) != 2 || items[0].Status != TaskDone || items[1].Status != TaskDone {
		t.Fatalf("tasks = %v, want both done", todoStatuses(items))
	}
}

// TestFailedBatchDoesNotBlockRedispatch covers a batch that fails after its
// tasks were created but before any worker started. Its pending occurrences
// and round counts used to stay behind, so dispatching the same task again
// was refused as a duplicate.
func TestFailedBatchDoesNotBlockRedispatch(t *testing.T) {
	c := newResourceOrderCoordinator(t, 2, 0)
	worker := &scriptedResultAgent{c: c}
	c.workerAgentOverride = worker
	c.SetStepConfirmFn(func(context.Context, []TaskDef) (bool, error) {
		return false, errors.New("confirmation channel closed")
	})
	task := TaskDef{Agent: "worker", Goal: "count the lines in README.md", SideEffect: SideEffectNone}
	if _, err := c.ExecuteTasks(context.Background(), []TaskDef{task}); err == nil {
		t.Fatal("the failing batch reported success")
	}
	if items := c.taskTracker.TodoList().Items(); len(items) != 0 || len(worker.started) != 0 {
		t.Fatalf("after the failed batch: tasks %v, workers %v; want nothing left and nothing run", todoStatuses(items), worker.started)
	}
	c.SetStepConfirmFn(nil)
	if _, err := c.ExecuteTasks(context.Background(), []TaskDef{task}); err != nil {
		t.Fatalf("redispatch: %v", err)
	}
	if items := c.taskTracker.TodoList().Items(); len(items) != 1 || items[0].Status != TaskDone || len(worker.started) != 1 {
		t.Fatalf("after redispatch: tasks %v, workers %v; want the task done once", todoStatuses(items), worker.started)
	}
}
