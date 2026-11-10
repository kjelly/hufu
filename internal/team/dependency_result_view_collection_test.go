package team

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"
)

func dependencyViewCollectionFixture(t *testing.T) (*Coordinator, *TodoItem, context.Context, ExecutionReceipt, string) {
	t.Helper()
	var sourceID string
	var bound TaskDef
	c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
		spec.ContractID, spec.ContractHash, spec.ContractRevision = bound.ContractID, bound.ContractHash, bound.ContractRevision
		spec.DependencyResultView, spec.DependsOn = bound.DependencyResultView.clone(), []string{sourceID}
	}, func(c *Coordinator) {
		sourceID = dependencyViewAcceptedSource(t, c).ID
		c.session.Config.Delegation.BindTaskGoalContracts = true
		c.session.ContractTasks = []TaskDef{
			{ID: "produce", Agent: "worker"},
			{ID: "consume", Agent: "worker", DependsOnContracts: []string{"produce"}, DependencyResultView: &DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary"}}, Execution: ExecutionContract{RequiresResult: true}},
		}
		tasks, _, err := CompileTaskGoalContracts(c.session, []TaskDef{{Agent: "worker", ContractID: "produce", Goal: "produce"}, {Agent: "worker", ContractID: "consume", Goal: "consume"}})
		if err != nil {
			t.Fatal(err)
		}
		bound = tasks[1]
	})
	task := taskDefFromTodoItem(item)
	request := c.newTaskContextRequest(task, item.ID, receipt.Attempt, ContextTriggerTaskDispatch, item.Agent, "worker", nil)
	request.ModelExecutionID = receipt.ModelExecutionID
	request.AssignRequestID()
	input := buildWorkerContextInput(request, task, c.session.Agents[item.Agent], "", "", "", "", nil)
	input.DependencyResults = c.dependencyResultsForTask(item.ID)
	input.ModelContext = ModelContextSpec{ModelID: "test", ContextWindow: 16_384, MaxOutputTokens: 256, SafetyMarginTokens: 64}
	input.DisableMemory, input.DisableCanonicalMemory = true, true
	compiled, err := c.compileWorkerContext(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildContextInjectionManifest(request, compiled, nil, item.Agent, time.Now(), c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := markDependencyViewModelCall(c.bindDependencyViewCall(ctx, &manifest)); err != nil {
		t.Fatal(err)
	}
	receipt.ContextManifest = cloneContextInjectionManifest(&manifest)
	return c, c.todoItemByID(item.ID), ctx, receipt, sourceID
}

func TestDependencyResultViewCollectionResumePreservesWork(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "original source", true: "stale source"}[stale], func(t *testing.T) {
			c, item, ctx, receipt, sourceID := dependencyViewCollectionFixture(t)
			original, err := json.Marshal(receipt, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "complete work report")
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.reserveResultCollection(ctx, checkpoint, "collector-1", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, []byte(`{"status":"success","summary":"selected source consumed"}`))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.prepareCollectionClaims(ctx, checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			events, err := c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c.revokeTaskOccurrence(item.ID)
			replayed := ReduceToTodoList(events)
			for _, source := range replayed {
				if stale && source.ID == sourceID {
					source.OccurrenceRevision++
				}
			}
			c.taskTracker = NewTaskTracker()
			c.taskTracker.TodoList().AddReserved(replayed)
			worker := scriptWorker(textTurn("must never replay work"))
			c.workerAgentOverride = worker
			restored := c.todoItemByID(item.ID)
			_, err = c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(restored), item.ID)
			if (err != nil) != stale || worker.Calls() != 0 {
				t.Fatalf("resume stale=%t, worker calls=%d, error=%v", stale, worker.Calls(), err)
			}
			current := c.todoItemByID(item.ID)
			if (current.Status == TaskDone) == stale {
				t.Fatalf("resume completed with wrong source state: %s", current.Status)
			}
			// Read the immutable work artifact directly: the stale-source case
			// intentionally cannot pass the live-source recovery validator.
			body, err := c.readCollectionBytes(t.Context(), checkpoint.Work, resultCollectionWorkMaxBytes)
			if err != nil {
				t.Fatal(err)
			}
			var work resultCollectionWork
			if err := json.Unmarshal(body, &work, collectionDurationFormat); err != nil {
				t.Fatal(err)
			}
			preserved, err := json.Marshal(work.Receipt, json.Deterministic(true))
			if err != nil || string(original) != string(preserved) {
				t.Fatalf("result-only resume rewrote original work receipt: %v", err)
			}
		})
	}
}
