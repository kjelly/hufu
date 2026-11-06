package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

func TestNativeMediaExecutionRetainsOriginalOwnerAcrossInvocations(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx, end, err := c.beginPublicInvocationExecutionRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.checkRunAdmission(); err != nil {
		end()
		t.Fatal(err)
	}
	item := createMediaOccurrence(t, c, ctx, nativeInvocationTask(t, c, ctx, media.InvocationSingleTask))
	originalRun, originalHash := item.MediaBinding.RunID, item.MediaBinding.Hash
	c.finalizePublicInvocationFailure(context.Canceled)
	end()
	if err := os.Remove(filepath.Join(c.session.Workspace, "input.png")); err != nil {
		t.Fatal(err)
	}
	ctx = openMediaInvocation(t, c)
	if c.executionRunID == originalRun {
		t.Fatal("resume fixture reused its original invocation identity")
	}
	item = c.todoItemByID(item.ID)
	if err := c.validateNativeMediaTaskExecution(ctx, taskDefFromTodoItem(item), item.ID); err != nil {
		t.Fatal("current invocation could not validate its original canonical image owner", err)
	}
	if item.MediaBinding.RunID != originalRun || item.MediaBinding.Hash != originalHash {
		t.Fatal("resume rebound its input instead of retaining the original owner")
	}
}

func TestNativeMediaCancelledPreflightKeepsCanonicalCancellation(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	item := createMediaOccurrence(t, c, ctx, nativeInvocationTask(t, c, ctx, media.InvocationSingleTask))
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := c.executeTask(ctx, taskDefFromTodoItem(item), item.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled native preflight lost its cause", err)
	}
	item = c.todoItemByID(item.ID)
	if item.Status != TaskError || item.FailureEvent == nil || item.FailureEvent.FailureClass != FailureCancelled || len(item.FailureFingerprints) != 0 {
		t.Fatalf("cancelled native preflight was reported as a policy block: status=%s failure=%+v", item.Status, item.FailureEvent)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cancellations := 0
	for _, event := range events {
		if event.TaskID == item.ID && event.Type == string(EventTaskCancelled) {
			cancellations++
		}
	}
	if cancellations != 1 {
		t.Fatal("cancelled native preflight lost its canonical cancellation event")
	}
}

func TestNativeMediaResumeResetRetainsCanonicalBinding(t *testing.T) {
	for _, restartInvocation := range []bool{false, true} {
		name := "same invocation"
		if restartInvocation {
			name = "new invocation"
		}
		t.Run(name, func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			ctx, end, err := c.beginPublicInvocationExecutionRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(end)
			if err := c.checkRunAdmission(); err != nil {
				t.Fatal(err)
			}
			item := createMediaOccurrence(t, c, ctx, nativeInvocationTask(t, c, ctx, media.InvocationSingleTask))
			bindingHash, inputRun := item.MediaBinding.Hash, item.MediaBinding.RunID
			if err := c.CommitTaskTransition(ctx, item.ID, TaskPending, TaskInProgress, "interrupted work", "", nil); err != nil {
				t.Fatal(err)
			}
			if restartInvocation {
				c.finalizePublicInvocationFailure(context.Canceled)
				end()
				ctx = openMediaInvocation(t, c)
			}
			if err := c.AdmitExecutionPolicy(); err != nil {
				t.Fatal(err)
			}
			if err := c.prepareInterruptedTaskForResume(ctx, item.ID, "resume canonical image work"); err != nil {
				t.Fatal("native reset could not retain the admitted owner", err)
			}
			item = c.todoItemByID(item.ID)
			if item.Status != TaskPending || item.MediaBinding.Hash != bindingHash || item.MediaBinding.RunID != inputRun {
				t.Fatal("native reset recreated or changed its input binding")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
				t.Fatal("canonical resume reset failed independent image audit", err)
			}
			replayed := todoItemByID(ReduceToTodoList(events), item.ID)
			if replayed.Status != item.Status || !replayed.StartedAt.Equal(item.StartedAt) || !replayed.EndedAt.Equal(item.EndedAt) || replayed.ModelTime != item.ModelTime || replayed.ToolTime != item.ToolTime {
				t.Fatal("resume reset replay retained the interrupted attempt's lifecycle projection")
			}
		})
	}
}

func nativeInvocationTask(t *testing.T, c *Coordinator, ctx context.Context, mode media.InvocationMode) TaskDef {
	t.Helper()
	task := admittedMediaTask(t, c, ctx, "native-invocation")
	binding := *task.MediaBinding.Clone()
	binding.InvocationMode = mode
	binding, err := binding.Seal()
	if err != nil {
		t.Fatal(err)
	}
	task.MediaBinding, task.MediaInputDigest = &binding, binding.InputDigest
	return task
}

func TestNativeMediaInvocationOwnsOneCompleteOrderedTurn(t *testing.T) {
	for _, mode := range []media.InvocationMode{media.InvocationSingleTask, media.InvocationDirectAgent} {
		t.Run(string(mode), func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			ctx := openMediaInvocation(t, c)
			task := nativeInvocationTask(t, c, ctx, mode)
			item := createMediaOccurrence(t, c, ctx, task)
			events, err := c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, events); err != nil {
				t.Fatal(err)
			}
			for _, downgrade := range []bool{false, true} {
				binding := task.MediaBinding.Clone()
				if downgrade {
					binding.InvocationMode = ""
					sealed, err := binding.Seal()
					if err != nil {
						t.Fatal(err)
					}
					binding = &sealed
				}
				spec := TodoSpec{Agent: item.Agent, Goal: item.Goal, Model: item.Model, ExecutionTarget: item.ExecutionTarget, MediaBinding: binding, MediaInputDigest: binding.InputDigest}
				id := c.taskTracker.TodoList().ReserveIDs(1)[0]
				requireMediaCode(t, c.finalizeMediaTaskSpec(ctx, &spec, id), media.AccessDenied)
			}
			if len(c.taskTracker.TodoList().Items()) != 1 || c.todoItemByID(item.ID).MediaBinding.InvocationMode != mode {
				t.Fatal("input ownership was copied or downgraded")
			}
		})
	}
}

func TestNativeMediaInvocationRejectsSubsetOrReorderedTurn(t *testing.T) {
	for _, scenario := range []string{"subset", "reordered"} {
		t.Run(scenario, func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			ctx := openMediaInvocation(t, c)
			task := nativeInvocationTask(t, c, ctx, media.InvocationSingleTask)
			binding := task.MediaBinding.Clone()
			if scenario == "subset" {
				binding.Selections, binding.Refs = binding.Selections[:1], binding.Refs[:1]
			} else {
				binding.Selections[0], binding.Selections[1] = binding.Selections[1], binding.Selections[0]
			}
			sealed, err := binding.Seal()
			if err != nil {
				t.Fatal(err)
			}
			spec := TodoSpec{Agent: "worker", Goal: task.Goal, Model: "fixture/vision", ExecutionTarget: execution.ExecutionTarget{Backend: "fixture", Model: "vision"}, MediaBinding: &sealed, MediaInputDigest: sealed.InputDigest}
			id := c.taskTracker.TodoList().ReserveIDs(1)[0]
			requireMediaCode(t, c.finalizeMediaTaskSpec(ctx, &spec, id), media.AccessDenied)
		})
	}
}

func TestNativeMediaInvocationRejectsGraphsBeforeDispatch(t *testing.T) {
	for _, scenario := range []string{"siblings", "pipeline", "fanout", "named edge"} {
		t.Run(scenario, func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			ctx := openMediaInvocation(t, c)
			task := nativeInvocationTask(t, c, ctx, media.InvocationSingleTask)
			tasks := []TaskDef{task}
			switch scenario {
			case "siblings":
				tasks = append(tasks, TaskDef{Agent: "worker", Goal: "other"})
			case "pipeline":
				tasks[0].Pipeline = true
			case "fanout":
				tasks[0].FanOut = new(FanOutSpec)
			case "named edge":
				tasks[0].DependsOnContracts = []string{"other"}
			}
			_, err := c.ExecuteTasks(ctx, tasks)
			requireMediaCode(t, err, media.Unsupported)
			if len(c.taskTracker.TodoList().Items()) != 0 {
				t.Fatal("unsupported graph created executable task occurrences")
			}
		})
	}
}

func TestNativeMediaInvocationChecksCanonicalExecutionShape(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	canonical := createMediaOccurrence(t, c, ctx, nativeInvocationTask(t, c, ctx, media.InvocationSingleTask))
	for _, test := range []struct {
		name   string
		mutate func(*TodoItem)
	}{
		{"legacy mode", func(item *TodoItem) {
			item.MediaBinding = item.MediaBinding.Clone()
			item.MediaBinding.InvocationMode = ""
		}},
		{"parent", func(item *TodoItem) { item.ParentID = "other" }},
		{"phase", func(item *TodoItem) { item.Phase = Phase("investigate") }},
		{"sidecar", func(item *TodoItem) { item.Sidecar = true }},
		{"action", func(item *TodoItem) { item.Action = new(Action) }},
		{"dependency", func(item *TodoItem) { item.DependsOn = []string{"other"} }},
		{"ordering", func(item *TodoItem) { item.OrderAfter = []string{"other"} }},
		{"failure edge", func(item *TodoItem) { item.OnFailure = "other" }},
		{"escalation", func(item *TodoItem) { item.Escalate = true }},
		{"untyped model fanout", func(item *TodoItem) { item.ModelTopology = []string{"fixture/vision", "fixture/other"} }},
		{"unfrozen target fanout", func(item *TodoItem) {
			item.ExecutionTopology = []execution.ExecutionTarget{item.ExecutionTarget, {Backend: "unfrozen", Model: "other"}}
		}},
		{"fallback", func(item *TodoItem) {
			item.ExecutionRoute = &ExecutionRouteBinding{Candidates: []execution.ExecutionTarget{item.ExecutionTarget, item.ExecutionTarget}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := *canonical
			test.mutate(&item)
			requireMediaCode(t, validateNativeMediaInvocation(&item, c.executionPolicy.snapshot), media.Unsupported)
		})
	}
}
