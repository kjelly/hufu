package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/runtimestore"
)

func historicalBoundSpecs(t *testing.T, c *Coordinator, specs []TodoSpec, ids []string) []TodoSpec {
	t.Helper()
	if err := c.freezeHistoricalResultTaskBindings(t.Context(), specs, ids); err != nil {
		t.Fatal(err)
	}
	return specs
}

func TestHistoricalResultTaskBindingRecipientScope(t *testing.T) {
	c, _, _, grant := historicalGrantReplayFixture(t)
	ids := []string{"1", "2", "3", "4", "5"}
	specs := historicalBoundSpecs(t, c, []TodoSpec{
		{Agent: "Review Worker", Source: TaskSourceCoordinator},
		{Agent: "review-worker", Source: TaskSourceAgent, ParentID: "parent"},
		{Agent: "Review Worker", Source: TaskSourceCoordinator, ParentID: "workset-parent", WorksetBinding: &WorksetBinding{WorksetID: "workset"}},
		{Agent: "Review Worker", Source: TaskSourceSubagent, ParentID: "parent"},
		{Agent: "observer", Source: TaskSourceCoordinator},
	}, ids)
	for i, spec := range specs {
		binding := spec.HistoricalResultBinding
		if (binding != nil) != (i < 3) {
			t.Fatalf("task %d source %q grant = %+v", i, spec.Source, binding)
		}
		if binding == nil {
			continue
		}
		if binding.TaskID != ids[i] || binding.Recipient != "review worker" || binding.Grant.EventID != grant.ID || len(binding.ResultIDs) != 1 {
			t.Fatalf("task %d binding = %+v", i, binding)
		}
		if err := c.validateTaskHistoricalResultBinding(t.Context(), todoItemFromSpec(spec, binding.TaskID)); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatal("preparation created tasks for recipients")
	}
}

func TestHistoricalResultTaskBindingRejectsOverrides(t *testing.T) {
	for _, field := range []string{"missing", "task", "recipient", "branch", "cycle", "event", "hash", "result", "duplicate", "version", "subagent"} {
		t.Run(field, func(t *testing.T) {
			c, _, _, _ := historicalGrantReplayFixture(t)
			spec := historicalBoundSpecs(t, c, []TodoSpec{{Agent: "Review Worker"}}, []string{"1"})[0]
			item := todoItemFromSpec(spec, "1")
			switch field {
			case "missing":
				item.HistoricalResultBinding = nil
			case "task":
				item.HistoricalResultBinding.TaskID = "2"
			case "recipient":
				item.HistoricalResultBinding.Recipient = "observer"
			case "branch":
				item.HistoricalResultBinding.Grant.BranchID = "sibling"
			case "cycle":
				item.HistoricalResultBinding.Grant.CycleID = "new-cycle"
			case "event":
				item.HistoricalResultBinding.Grant.EventID = "new-event"
			case "hash":
				item.HistoricalResultBinding.Grant.EventHash = strings.Repeat("0", 64)
			case "result":
				item.HistoricalResultBinding.ResultIDs[0] = "res-" + strings.Repeat("0", 64)
			case "duplicate":
				item.HistoricalResultBinding.ResultIDs = append(item.HistoricalResultBinding.ResultIDs, item.HistoricalResultBinding.ResultIDs...)
			case "version":
				item.HistoricalResultBinding.SchemaVersion++
			case "subagent":
				item.Source = TaskSourceSubagent
			}
			if err := c.validateTaskHistoricalResultBinding(t.Context(), item); !errors.Is(err, ErrHistoricalResultTaskBinding) {
				t.Fatalf("override %s was accepted: %v", field, err)
			}
			if field != "missing" {
				spec.HistoricalResultBinding = item.HistoricalResultBinding
				spec.Source = item.Source
				if err := c.freezeHistoricalResultTaskBindings(t.Context(), []TodoSpec{spec}, []string{"1"}); !errors.Is(err, ErrHistoricalResultTaskBinding) {
					t.Fatalf("freeze replaced override %s: %v", field, err)
				}
			}
		})
	}
}

func TestHistoricalResultTaskBindingCreationAndReplay(t *testing.T) {
	c, store, _, grant := historicalGrantReplayFixture(t)
	spec := historicalBoundSpecs(t, c, []TodoSpec{{Agent: "Review Worker", Desc: "new objective", Source: TaskSourceCoordinator,
		ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: "test"}}}, []string{"1"})[0]
	projection, err := taskOccurrenceProjectionFromSpec(spec, "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitTaskOccurrence(t.Context(), projection, "1", 1); err != nil {
		t.Fatal(err)
	}
	// Removing a runtime binding after admission must not persist a task.
	missing := spec
	missing.HistoricalResultBinding = nil
	if items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{missing}, []string{"1"}); !errors.Is(err, ErrHistoricalResultTaskBinding) || len(items) != 0 {
		t.Fatalf("unbound occurrence persisted: %d, %v", len(items), err)
	}
	items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, []string{"1"})
	if err != nil || len(items) != 1 {
		t.Fatalf("bound creation = %d, %v", len(items), err)
	}
	item := items[0]
	if item.HistoricalResultBinding.Grant.EventID != grant.ID {
		t.Fatal("task replaced its grant event")
	}
	if _, found, err := c.validateTaskOccurrenceAdmission(t.Context(), taskDefFromTodoItem(item), item.ID, 1); err != nil || !found {
		t.Fatalf("binding differs between pre-creation and admitted occurrence: %v, %v", found, err)
	}
	// A lifecycle snapshot must not replace the task_created binding.
	forged := cloneTodoItem(item)
	forged.HistoricalResultBinding.ResultIDs[0] = "res-" + strings.Repeat("0", 64)
	payload, err := json.Marshal(c.taskTransitionPayloadWithCoordinator(forged))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{Type: string(EventTaskStarted), TaskID: item.ID, Actor: item.Agent, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(mustReadEvents(t, store))
	if err != nil || len(replayed) != 1 || !reflect.DeepEqual(replayed[0].HistoricalResultBinding, item.HistoricalResultBinding) {
		t.Fatalf("lifecycle event rewrote created binding: %+v, %v", replayed, err)
	}
	changed := cloneTodoItem(item)
	changed.HistoricalResultBinding.ResultIDs[0] = "res-" + strings.Repeat("0", 64)
	if reflect.DeepEqual(changed.HistoricalResultBinding, item.HistoricalResultBinding) {
		t.Fatal("clone retained the live ResultID slice")
	}
	retry, err := taskOccurrenceProjectionForRetry(item)
	if err != nil || !reflect.DeepEqual(retry.HistoricalResultBinding, item.HistoricalResultBinding) {
		t.Fatal("retry lost immutable binding")
	}
	if err := compareSingleTaskProjection(item, changed); err == nil {
		t.Fatal("canonical parity ignored a modified historical binding")
	}
}

func TestHistoricalResultTaskBindingRequiresCanonicalAuthority(t *testing.T) {
	for _, failure := range []string{"index", "checkpoint", "profile", "policy", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			c, store, _, _ := historicalGrantReplayFixture(t)
			ctx := t.Context()
			switch failure {
			case "index":
				if _, err := store.store.DB().ExecContext(ctx, `DELETE FROM historical_grant_cycles`); err != nil {
					t.Fatal(err)
				}
			case "checkpoint":
				session := ReduceToSessionData(mustReadEvents(t, store))
				session.HistoricalResultGrants.EventID = "forged-event"
				c.SetSessionData(session)
			case "profile":
				profile := c.ExecutionProfile()
				profile.DisableHistoricalTaskReuse = true
				c.SetExecutionProfile(profile)
			case "policy":
				c.maxConcurrent++
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := c.freezeHistoricalResultTaskBindings(ctx, []TodoSpec{{Agent: "Review Worker"}}, []string{"1"}); err == nil {
				t.Fatal("binding bypassed canonical authority or context cancellation")
			}
		})
	}
}

func TestHistoricalResultTaskBindingCycleAndForkScope(t *testing.T) {
	c, store, tree, grant := historicalGrantReplayFixture(t)
	before, err := tree.CreateBranch("before-grant", mustReadEvents(t, store)[0].ID, store)
	if err != nil {
		t.Fatal(err)
	}
	after, err := tree.CreateBranch("after-grant", grant.ID, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		branch string
		bound  bool
	}{{before.ID, false}, {after.ID, true}, {"main", true}} {
		c.recoveryBranch = test.branch
		spec := historicalBoundSpecs(t, c, []TodoSpec{{Agent: "Review Worker"}}, []string{"1"})[0]
		if (spec.HistoricalResultBinding != nil) != test.bound {
			t.Fatalf("binding on branch %s = %+v", test.branch, spec.HistoricalResultBinding)
		}
		if test.bound && spec.HistoricalResultBinding.Grant.BranchID != "main" {
			t.Fatal("fork manufactured a child grant")
		}
	}
	c.recoveryBranch = "main"
	if _, err := store.AppendPersisted(RunEvent{Type: string(EventWorkflowCycleStarted), RunID: "new-cycle", Actor: "coordinator",
		Payload: []byte(`{"request":"fresh objective","task_id_floor":0}`)}); err != nil {
		t.Fatal(err)
	}
	spec := historicalBoundSpecs(t, c, []TodoSpec{{Agent: "Review Worker"}}, []string{"1"})[0]
	if spec.HistoricalResultBinding != nil {
		t.Fatal("new cycle reused an earlier cycle's grants")
	}
}

func TestHistoricalResultTaskBindingCannotUseCheckpointOnly(t *testing.T) {
	c := newTestCoordinator(t)
	c.SetSessionData(&SessionData{HistoricalResultGrants: &runtimestore.HistoricalGrantCycleRef{EventID: "fake"}})
	if err := c.freezeHistoricalResultTaskBindings(t.Context(), []TodoSpec{{Agent: "worker"}}, []string{"1"}); !errors.Is(err, ErrHistoricalResultTaskBinding) {
		t.Fatalf("checkpoint created a task grant without a journal: %v", err)
	}
}

func TestHistoricalResultTaskBindingIncludesEveryGrantedResult(t *testing.T) {
	c, rows, _ := historicalGrantPreparationFixture(t)
	*rows = append(*rows, historicalGrantSourceFixture(t, c.session.Workspace, "2"))
	prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{
		"worker=" + (*rows)[1].ResultID, "worker=" + (*rows)[0].ResultID, "observer=" + (*rows)[0].ResultID,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, _, _, _ = historicalGrantPreparedReplayFixture(t, c, prepared)
	specs := historicalBoundSpecs(t, c, []TodoSpec{{Agent: "Review Worker"}, {Agent: "Observer"}}, []string{"1", "2"})
	if len(specs[0].HistoricalResultBinding.ResultIDs) != 2 || len(specs[1].HistoricalResultBinding.ResultIDs) != 1 {
		t.Fatal("binding omitted an authorized result or included another recipient's grant")
	}
	item := todoItemFromSpec(specs[0], "1")
	item.HistoricalResultBinding.ResultIDs = item.HistoricalResultBinding.ResultIDs[:1]
	if err := c.validateTaskHistoricalResultBinding(t.Context(), item); !errors.Is(err, ErrHistoricalResultTaskBinding) {
		t.Fatalf("partial result subset was accepted: %v", err)
	}
}

func TestHistoricalResultTaskBindingWorkerCreatedTodos(t *testing.T) {
	c, store, _, _ := historicalGrantReplayFixture(t)
	response, err := (&todoTool{coordinator: c}).handleCreate(t.Context(), "Review Worker", []string{"inspect inputs", "verify evidence"})
	if err != nil || response.IsError {
		t.Fatalf("worker todo creation = %+v, %v", response, err)
	}
	items, err := ReplayTodoList(mustReadEvents(t, store))
	if err != nil || len(items) != 2 {
		t.Fatalf("worker-created occurrences = %d, %v", len(items), err)
	}
	for _, item := range items {
		if item.Source != TaskSourceAgent || item.HistoricalResultBinding == nil || item.HistoricalResultBinding.TaskID != item.ID {
			t.Fatalf("worker-created todo did not bind before admission: %+v", item)
		}
		if _, found, err := c.validateTaskOccurrenceAdmission(t.Context(), taskDefFromTodoItem(item), item.ID, 1); err != nil || !found {
			t.Fatalf("worker-created binding did not survive admission: %v, %v", found, err)
		}
	}
}

func TestHistoricalResultTaskBindingEnvelopeFreezesBeforeDigest(t *testing.T) {
	c, _, _, _ := historicalGrantReplayFixture(t)
	def := c.session.Agents["review worker"]
	task, err := c.canonicalizeTaskOccurrence(TaskDef{Agent: "Review Worker", Goal: "new objective"}, def, "test")
	if err != nil {
		t.Fatal(err)
	}
	specs := []TodoSpec{{Agent: task.Agent, Goal: task.Goal, Desc: task.Goal, Model: task.Model,
		ExecutionTarget: task.ResolvedExecutionTarget, Source: TaskSourceCoordinator, SideEffect: task.SideEffect, Recovery: task.Recovery}}
	envelopes, err := c.prepareNewTaskExecutionEnvelopes(t.Context(), []TaskDef{task}, specs, []string{"1"}, true)
	if err != nil || len(envelopes) != 1 || specs[0].HistoricalResultBinding == nil {
		t.Fatalf("envelope did not freeze grant before occurrence digest: %v", err)
	}
	item := todoItemFromSpec(specs[0], "1")
	if err := validateTaskExecutionEnvelopeBinding(item, envelopes[0]); err != nil {
		t.Fatal(err)
	}
	item.HistoricalResultBinding = nil
	if err := validateTaskExecutionEnvelopeBinding(item, envelopes[0]); err == nil {
		t.Fatal("envelope digest did not bind historical grants")
	}
}

func TestHistoricalResultTaskBindingFanoutAndPrimaryScope(t *testing.T) {
	c, _, _, _ := historicalGrantReplayFixture(t)
	spec := historicalBoundSpecs(t, c, []TodoSpec{{Agent: "Review Worker"}}, []string{"1"})[0]
	item := todoItemFromSpec(spec, "1")
	leaf := cloneCoordinator(c, cloneSession(c.session, t.TempDir()))
	leaf.modelFanout = &modelFanoutBinding{parent: c, taskID: "1"}
	if err := leaf.validateTaskHistoricalResultBinding(t.Context(), item); err != nil {
		t.Fatalf("fanout used its disposable workspace as grant authority: %v", err)
	}
	primary, err := ProjectPrimaryDecisionOccurrence(universalDecisionLifecycleEvents(t)[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.validateTaskHistoricalResultBinding(t.Context(), primary); err != nil {
		t.Fatalf("primary decision tried to acquire a worker grant: %v", err)
	}
}

func TestHistoricalResultTaskBindingNeedsCreationProof(t *testing.T) {
	c, store, _, _ := historicalGrantReplayFixture(t)
	spec := historicalBoundSpecs(t, c, []TodoSpec{{Agent: "Review Worker"}}, []string{"1"})[0]
	item := todoItemFromSpec(spec, "1")
	if err := validateHistoricalResultTaskBindingReplay(mustReadEvents(t, store), item); !errors.Is(err, ErrHistoricalResultTaskBinding) {
		t.Fatalf("checkpoint-only binding had creation proof: %v", err)
	}
	for _, name := range []string{"missing_binding", "different_result", "grant_after_creation"} {
		t.Run(name, func(t *testing.T) {
			created := cloneTodoItem(item)
			if name == "missing_binding" {
				created.HistoricalResultBinding = nil
			} else if name == "different_result" {
				created.HistoricalResultBinding.ResultIDs[0] = "res-" + strings.Repeat("0", 64)
			}
			payload, err := json.Marshal(c.taskTransitionPayloadWithCoordinator(created))
			if err != nil {
				t.Fatal(err)
			}
			events := mustReadEvents(t, store)
			creation := RunEvent{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}
			if name == "grant_after_creation" {
				events = append([]RunEvent{creation}, events...)
			} else {
				events = append(events, creation)
			}
			if err := validateHistoricalResultTaskBindingReplay(events, item); !errors.Is(err, ErrHistoricalResultTaskBinding) {
				t.Fatalf("invalid creation proof accepted: %v", err)
			}
		})
	}
}

func TestHistoricalResultTaskBindingIsNotModelInput(t *testing.T) {
	for _, key := range []string{"historical_result_binding", "historical-result-binding", "HistoricalResultBinding", "HISTORICAL_RESULT_BINDING"} {
		input := `{"tasks":[{"agent":"worker","goal":"work","` + key + `":{"schema_version":1}}]}`
		if _, err := decodeModelTaskDefs([]byte(input)); err == nil {
			t.Fatalf("model supplied runtime binding through %s", key)
		}
	}
	raw, err := json.Marshal(TodoSpec{HistoricalResultBinding: &HistoricalResultTaskBinding{TaskID: "private-binding"}})
	if err != nil || strings.Contains(string(raw), "private-binding") {
		t.Fatal("runtime-only TodoSpec binding became serialized input")
	}
}
