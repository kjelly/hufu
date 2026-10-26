package team

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func completedCycleCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	session := workflowTestSession(t)
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	results := map[Phase]PhaseResult{PhaseVerify: {Status: PhaseStatusSuccess}}
	if err := w.restore(PhaseDone, results, "", NewRetryState()); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(session.Workspace, "run-prior", "session-cycles")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	c := &Coordinator{
		session: session, sessionData: NewSession(), phaseWorkflow: w,
		taskTracker: NewTaskTracker(), eventStore: es, executionRunID: "run-followup",
		reportStatus: func(StatusEvent) {}, projectDir: session.Workspace,
	}
	c.SetEventJournal(eventStoreJournal{store: es})
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{ID: "1", Agent: "preparer", Desc: "original research", Status: TaskDone, Output: "previous research"},
		{ID: "2", Agent: "verifier", Desc: "original audit", Status: TaskDone, Output: "previous report"},
	})
	for _, message := range []struct{ role, text string }{{"user", "original question"}, {"assistant", "previous report"}} {
		if err := c.recordSessionMessage(message.text, message.role); err != nil {
			t.Fatal(err)
		}
	}
	c.saveCheckpoint()
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	es, err = NewEventStore(session.Workspace, "run-followup", "session-cycles")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	c.eventStore = es
	c.SetEventJournal(eventStoreJournal{store: es})
	return c
}

func TestWorkflowCyclePreservesConversationButRequiresFreshEvidence(t *testing.T) {
	c := completedCycleCoordinator(t)
	c.TaskCache().Restore([]TaskCacheSeed{{AgentKey: "preparer", Task: "new research", Output: "stale cached answer"}})
	c.sessionData.CriterionResults = []CriterionResult{{}}
	c.sessionData.ContinuationCheckpoint = &ContinuationCheckpoint{TurnCount: 5}
	started, err := c.beginFollowUpWorkflow(t.Context(), "investigate that report further")
	if err != nil || !started {
		t.Fatalf("begin follow-up = %t, %v", started, err)
	}
	if c.phaseWorkflow.State() != PhasePrepare || len(c.phaseWorkflow.results) != 0 {
		t.Fatalf("workflow reused completed phase evidence: %s / %#v", c.phaseWorkflow.State(), c.phaseWorkflow.results)
	}
	if len(c.taskTracker.TodoList().Items()) != 0 || c.allTasksCompletedSuccessfully() {
		t.Fatal("old task results count as follow-up completion")
	}
	if c.initialPrompt != "investigate that report further" || len(c.sessionData.Entries) != 3 || c.sessionData.Entries[1].Content != "previous report" {
		t.Fatalf("conversation or current request lost: %#v", c.sessionData)
	}
	if c.sessionData.ContinuationCheckpoint != nil || len(c.sessionData.CriterionResults) != 0 {
		t.Fatal("follow-up inherited continuation or criterion completion")
	}
	if cache := c.TaskCache().(*defaultTaskCache); len(cache.entries) != 0 {
		t.Fatal("follow-up inherited the previous cycle's result cache")
	}
	request := c.newCoordinatorContextRequest("follow-up", true, 1)
	if err := request.Validate(); err != nil {
		t.Fatalf("follow-up context still fails preflight: %v", err)
	}
	if err := c.phaseWorkflow.validateTasks([]TaskDef{{Agent: "verifier", Phase: PhaseVerify}}); err == nil {
		t.Fatal("old verification permits skipping fresh research")
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "preparer", Desc: "new research"}})
	if items[0].ID != "3" {
		t.Fatalf("follow-up reused a historical task ID: %s", items[0].ID)
	}
	c.saveCheckpoint()
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToSessionData(events)
	if err := CompareCanonicalProjection(c.sessionData, events); err != nil {
		t.Fatalf("live/replay disagreement: %v", err)
	}
	if len(replayed.Tasks) != 1 || replayed.Tasks[0].ID != "3" || len(replayed.Entries) != 3 {
		t.Fatalf("cycle replay = %#v", replayed)
	}
	// Replay up to the boundary still exposes the original task results.
	for i, event := range events {
		if event.Type == string(EventWorkflowCycleStarted) {
			if old := ReduceToSessionData(events[:i]); len(old.Tasks) != 2 || old.WorkflowState != PhaseDone {
				t.Fatalf("historical cycle was overwritten: %#v", old)
			}
			break
		}
	}
}

func TestWorkflowCycleRecoveryAfterDurableBoundary(t *testing.T) {
	c := completedCycleCoordinator(t)
	before := ReduceToSessionData(mustCycleEvents(t, c))
	boundary := RunEvent{
		Type: string(EventWorkflowCycleStarted), RunID: "run-followup", Actor: "coordinator",
		Payload: []byte(`{"task_id_floor":2,"request":"investigate further"}`),
	}
	if _, err := c.eventStore.AppendPersisted(boundary); err != nil {
		t.Fatal(err)
	}
	// Simulate process death before updating the mutable session or starting
	// PREPARE. The event includes the request needed to resume the new cycle.
	c.sessionData = before
	c.checkCanonicalProjectionShadow(nil, "main")
	if c.sessionData.RecoveryRequired || c.phaseWorkflow.State() != PhaseInit || len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatalf("boundary recovery failed: %s / %#v", c.phaseWorkflow.State(), c.sessionData)
	}
	if entries := c.sessionData.Entries; entries[len(entries)-1].Content != "investigate further" {
		t.Fatal("recovery lost the follow-up request")
	}
	if id := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "preparer", Desc: "recovered research"}})[0].ID; id != "3" {
		t.Fatalf("restart reused historical task ID %s", id)
	}
}

func TestWorkflowCycleCompletesOnlyAfterFreshContractsAndCanContinueAgain(t *testing.T) {
	c := completedCycleCoordinator(t)
	for cycle := range 2 {
		if _, err := c.beginFollowUpWorkflow(t.Context(), "investigate further"); err != nil {
			t.Fatal(err)
		}
		for _, contract := range c.session.ContractTasks {
			if err := c.phaseWorkflow.requireFinished(); err == nil {
				t.Fatal("workflow finished before all fresh contracts completed")
			}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: contract.Agent, Phase: contract.Phase, ContractID: runtimeContractID(contract), Desc: "fresh evidence"}})[0]
			if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskDone, "verified", "fresh evidence"); err != nil {
				t.Fatal(err)
			}
			if err := c.phaseWorkflow.observe(c.taskTracker.TodoList().Items()); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.phaseWorkflow.requireFinished(); err != nil {
			t.Fatalf("fresh cycle cannot finish: %v", err)
		}
		c.saveCheckpoint()
		if cycle == 0 {
			// A second follow-up is a separate public run in the same branch.
			_ = c.eventStore.Close()
			store, err := NewEventStore(c.session.Workspace, "run-third", "session-cycles")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			c.eventStore, c.executionRunID = store, "run-third"
			c.SetEventJournal(eventStoreJournal{store: store})
		}
	}
	items, err := ReplayTodoList(mustCycleEvents(t, c))
	if err != nil || len(items) != 4 || items[0].ID != "7" || items[3].ID != "10" {
		t.Fatalf("multiple cycles lost their task ownership: %v, %#v", err, items)
	}
}

func TestWorkflowCycleReservesRemovedTaskIDsAndDoesNotRestoreOldJournalCache(t *testing.T) {
	c := completedCycleCoordinator(t)
	appendBranchEvent(t, c.eventStore, RunEvent{Type: "task_created", TaskID: "9", Payload: []byte(`{"id":"9","status":"pending","agent":"preparer"}`)})
	appendBranchEvent(t, c.eventStore, RunEvent{Type: "task_removed", TaskID: "9", Payload: []byte(`{"id":"9","status":"skipped"}`)})
	j, err := openTaskJournal(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.append(journalRecord{Op: "put", Agent: "preparer", Desc: "same follow-up", Output: "stale result", TS: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	loaded, err := loadTaskJournal(c.session.Workspace, time.Now(), taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil || len(loaded["preparer"]) != 1 {
		t.Fatalf("journal fixture is not reusable before the boundary: %v, %#v", err, loaded)
	}
	if _, err := c.beginFollowUpWorkflow(t.Context(), "same follow-up"); err != nil {
		t.Fatal(err)
	}
	c.initTaskJournal()
	t.Cleanup(func() { _ = c.journal.Close() })
	if cache := c.TaskCache().(*defaultTaskCache); len(cache.entries) != 0 {
		t.Fatal("old task journal resurrected a previous cycle's result")
	}
	if id := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "preparer", Desc: "same follow-up"}})[0].ID; id != "10" {
		t.Fatalf("removed task ID was reused: %s", id)
	}
}

func TestWorkflowCycleRejectsInvalidReplayBoundaries(t *testing.T) {
	c := completedCycleCoordinator(t)
	prior := mustCycleEvents(t, c)
	for _, input := range []string{
		`{"task_id_floor":1,"request":"follow-up"}`,
		`{"task_id_floor":2,"previous_cycle_id":"wrong","request":"follow-up"}`,
		`{"task_id_floor":2,"request":""}`,
	} {
		boundary := RunEvent{Type: string(EventWorkflowCycleStarted), RunID: "run-next", Payload: []byte(input)}
		events := append(append([]RunEvent(nil), prior...), boundary)
		if _, err := ReplayTodoList(events); err == nil {
			t.Fatalf("invalid boundary accepted: %s", input)
		}
	}
}

func mustCycleEvents(t *testing.T, c *Coordinator) []RunEvent {
	t.Helper()
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestWorkflowCycleDoesNotRestartRecoveryOrReplayFailedWork(t *testing.T) {
	for _, phase := range []Phase{PhasePrepare, PhaseVerify, PhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			c := completedCycleCoordinator(t)
			results := map[Phase]PhaseResult{}
			for _, p := range c.phaseWorkflow.phases {
				results[p] = PhaseResult{Status: PhaseStatusSuccess}
			}
			if err := c.phaseWorkflow.restore(phase, results, "", NewRetryState()); err != nil {
				t.Fatal(err)
			}
			started, err := c.beginFollowUpWorkflow(t.Context(), "continue")
			if (err != nil) != (phase == PhaseFailed) || started || c.phaseWorkflow.State() != phase || len(c.taskTracker.TodoList().Items()) != 2 {
				t.Fatalf("recovery workflow changed: started=%t err=%v state=%s", started, err, c.phaseWorkflow.State())
			}
		})
	}
	for _, mode := range []string{"resume", "cancelled", "append_failure", "unfinished", "empty"} {
		t.Run(mode, func(t *testing.T) {
			c := completedCycleCoordinator(t)
			ctx := t.Context()
			prompt := "follow up"
			switch mode {
			case "resume":
				ctx = WithSessionResume(ctx)
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			case "append_failure":
				_ = c.eventStore.Close()
			case "unfinished":
				items := c.taskTracker.TodoList().Items()
				items[0].Status = TaskPending
				c.taskTracker.TodoList().Restore(items)
			case "empty":
				prompt = " "
			}
			started, err := c.beginFollowUpWorkflow(ctx, prompt)
			if started || err == nil || c.phaseWorkflow.State() != PhaseDone || len(c.taskTracker.TodoList().Items()) != 2 || c.workflowCycleID() != "" {
				t.Fatalf("refusal mutated state: started=%t err=%v phase=%s", started, err, c.phaseWorkflow.State())
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

func TestWorkflowCycleKeepsConversationDuringExactInitialDelegation(t *testing.T) {
	c := completedCycleCoordinator(t)
	c.session.Config.Delegation = agent.DelegationPolicy{RequireExactInitialBatch: true, InitialBatch: []string{"preparer"}}
	if _, err := c.beginFollowUpWorkflow(t.Context(), "follow up"); err != nil {
		t.Fatal(err)
	}
	if !c.initialDelegationPending() || c.initialContextWithheld() {
		t.Fatal("follow-up must require the initial batch and retain conversation")
	}
	if err := c.validateDelegationPolicy([]TaskDef{{Agent: "verifier", Goal: "skip research"}}); err == nil || !strings.Contains(err.Error(), "initial") {
		t.Fatalf("follow-up bypassed initial delegation: %v", err)
	}
}

func TestWorkflowCyclePublicEntryPointsResetBeforeProviderPreparation(t *testing.T) {
	for _, entry := range []string{"run", "chat"} {
		t.Run(entry, func(t *testing.T) {
			c := completedCycleCoordinator(t)
			// A provider boundary failure stops the test before model execution.
			// Observe the production entry point at that boundary rather than
			// manually calling the cycle helper.
			pm, err := agent.NewProviderManager("", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			c.providerManager = pm
			stop := errors.New("test provider boundary")
			called := false
			c.providerBoundaryStart = func(context.Context, string) error {
				called = true
				if c.phaseWorkflow.State() != PhasePrepare || len(c.taskTracker.TodoList().Items()) != 0 {
					t.Fatalf("provider preparation inherited the completed workflow: %s", c.phaseWorkflow.State())
				}
				request := c.newCoordinatorContextRequest("follow up", entry == "chat", 1)
				if err := request.Validate(); err != nil {
					t.Fatalf("provider preparation has invalid context: %v", err)
				}
				if len(c.sessionData.Entries) != 3 || c.sessionData.Entries[1].Content != "previous report" {
					t.Fatal("public entry point lost the conversation")
				}
				return stop
			}
			if entry == "run" {
				_, err = c.Run(t.Context(), "follow up")
			} else {
				_, err = c.ContinueWithPrompt(t.Context(), "follow up")
			}
			if !called || !errors.Is(err, stop) {
				t.Fatalf("entry point did not reach provider boundary: called=%t err=%v", called, err)
			}
		})
	}
}
