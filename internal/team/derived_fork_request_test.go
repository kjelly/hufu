package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func derivedForkRequestFixture(t *testing.T) (*Coordinator, *versionFixture) {
	t.Helper()
	f, ops := derivedForkPreparationFixture(t)
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventTaskCreated), TaskID: "9", RunID: "source-run",
		Payload: []byte(`{"id":"9","agent":"worker","status":"error","side_effect":"none"}`)})
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventWorkflowStateChanged), RunID: "source-run", Payload: []byte(`{"state":"FAILED"}`)})
	if _, err := ops.ForkNewTask(t.Context(), "investigation", "main"); err != nil {
		t.Fatal(err)
	}
	c := newTestCoordinator(t, withTeamConfig(func(config *agent.TeamConfig) {
		config.Name = "review"
		config.Delegation = agent.DelegationPolicy{RequireExactInitialBatch: true, InitialBatch: []string{"worker"}}
		config.Workflow = agent.WorkflowConfig{Phases: []string{"prepare", "audit", "execute", "verify"}}
	}))
	c.session.Workspace, c.session.Dir, c.projectDir = f.control, f.subject, f.subject
	c.session.WorkspaceVersion = f.version
	c.session.Scope = WorkspaceScope{Managed: true, ProjectID: "project", ContextScopeID: f.subject, SubjectRoot: f.subject, ProjectRoot: f.subject, ControlRoot: f.control}
	c.eventStore = f.events
	f.events.SetBranchID(f.tree.ActiveBranch)
	c.SetEventJournal(eventStoreJournal{store: f.events})
	var err error
	c.phaseWorkflow, err = newRuntimeWorkflow(c.session)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessionData(LoadSession(f.control))
	return c, f
}

func TestDerivedForkFirstRequestStartsFreshVersionedCycle(t *testing.T) {
	c, f := derivedForkRequestFixture(t)
	before := mustReadEvents(t, f.events)
	end := c.beginExecutionRun()
	defer end()
	started, err := c.beginFollowUpWorkflow(t.Context(), "new investigation")
	if err != nil || !started {
		t.Fatalf("derived cycle = %t, %v", started, err)
	}
	if c.AwaitingDerivedForkRequest() || c.phaseWorkflow.State() != PhasePrepare || len(c.taskTracker.TodoList().Items()) != 0 ||
		len(c.SessionData().Entries) != 1 || c.SessionData().Entries[0].Content != "new investigation" || !c.initialContextWithheld() {
		t.Fatalf("first request projection: pending=%t phase=%s tasks=%d entries=%+v withheld=%t", c.AwaitingDerivedForkRequest(), c.phaseWorkflow.State(), len(c.taskTracker.TodoList().Items()), c.SessionData().Entries, c.initialContextWithheld())
	}
	events := mustReadEvents(t, c.EventStore())
	var boundaries int
	for _, event := range events[len(before):] {
		if event.Type != string(EventWorkflowCycleStarted) {
			continue
		}
		boundaries++
		payload, err := decodeWorkflowCycleStarted(event)
		if err != nil || payload.Kind != "derived_fork" || payload.SchemaVersion != 1 || payload.TaskIDFloor != 9 ||
			event.RunID != c.executionRunID || payload.DerivedFork == nil || payload.DerivedFork.EventID != before[len(before)-1].ID {
			t.Fatalf("derived boundary lost its invocation or native source: %v", err)
		}
	}
	if boundaries != 1 {
		t.Fatalf("got %d derived boundaries", boundaries)
	}
	c.session.Config.Delegation.ShareRequestWith = []string{"worker"}
	if prior, err := c.previousResponseForWorker(t.Context(), "worker"); err != nil || prior != nil {
		t.Fatalf("first derived cycle requires or imports a parent response: %v", err)
	}
	if _, err := ProjectValidatedEventsForBranch(events, f.tree, f.tree.ActiveBranch); err != nil {
		t.Fatal(err)
	}
	if err := CompareCanonicalProjection(c.SessionData(), FilterEventsForBranch(events, f.tree, f.tree.ActiveBranch)); err != nil {
		t.Fatal(err)
	}
	if started, err := c.beginFollowUpWorkflow(t.Context(), "new investigation"); err != nil || started {
		t.Fatalf("repeated text created a second boundary: %t, %v", started, err)
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "fresh work"}})
	if items[0].ID != "10" {
		t.Fatalf("new work reused a parent task identity: %s", items[0].ID)
	}
}

func TestDerivedForkPendingEntryRejectsBeforeInvocation(t *testing.T) {
	c, f := derivedForkRequestFixture(t)
	before := len(mustReadEvents(t, f.events))
	for _, run := range []func() error{
		func() error { _, err := c.Run(WithSessionResume(t.Context()), "resume"); return err },
		func() error { _, err := c.Run(t.Context(), " "); return err },
		func() error { _, err := c.RunDirectAgent(t.Context(), "worker", "old work"); return err },
		func() error { _, err := c.ContinueWithPrompt(t.Context(), "old work"); return err },
		func() error { _, err := c.DryRun(t.Context(), "old work"); return err },
		func() error {
			workflow := c.phaseWorkflow
			c.phaseWorkflow = nil
			defer func() { c.phaseWorkflow = workflow }()
			return c.ValidateDerivedForkRequest(t.Context(), "new work")
		},
		func() error {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return c.ValidateDerivedForkRequest(ctx, "new work")
		},
	} {
		if err := run(); err == nil {
			t.Fatal("unsupported pending entry was admitted")
		}
	}
	if !c.AwaitingDerivedForkRequest() || len(mustReadEvents(t, f.events)) != before {
		t.Fatal("rejected entry consumed initialization or appended a run")
	}
}

func TestDerivedForkFirstCycleCommitsExplicitGrant(t *testing.T) {
	c, _ := derivedForkRequestFixture(t)
	manager, err := agent.NewProviderManager("http://127.0.0.1:1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = manager
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.FreezeExecutionPolicyAtStartup(); err != nil {
		t.Fatal(err)
	}
	row := historicalGrantSourceFixture(t, c.session.Workspace, "1")
	row.Team = "review"
	c.SetHistoricalTaskReader(historicalTaskReaderFunc(func(_ context.Context, scope HistoricalTaskScope) ([]HistoricalTaskResult, error) {
		if scope.Team != "review" || scope.Branch != "investigation" || scope.Workspace != c.session.Workspace {
			t.Fatal("derived intake broadened source scope")
		}
		return []HistoricalTaskResult{row}, nil
	}))
	request, err := c.PrepareHistoricalResultRequest(t.Context(), "new investigation", []string{"worker=" + row.ResultID})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithHistoricalResultRequest(t.Context(), request)
	if _, err := c.claimHistoricalResultRequest(ctx, "new investigation"); err != nil {
		t.Fatal(err)
	}
	end := c.beginExecutionRun()
	defer end()
	if started, err := c.beginFollowUpWorkflow(ctx, "new investigation"); err != nil || !started {
		t.Fatalf("derived intake = %t, %v", started, err)
	}
	events := mustReadEvents(t, c.EventStore())
	for index, event := range events {
		if event.Type != string(EventHistoricalResultsGranted) {
			continue
		}
		if index == 0 || events[index-1].Type != string(EventWorkflowCycleStarted) || events[index-1].RunID != event.RunID {
			t.Fatal("derived cycle and grant are not adjacent")
		}
	}
	if c.SessionData().HistoricalResultGrants == nil || c.AwaitingDerivedForkRequest() {
		t.Fatal("derived cycle did not commit its grant")
	}
	if err := CompareCanonicalProjection(c.SessionData(), events); err != nil {
		t.Fatal(err)
	}
}
