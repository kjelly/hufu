package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// dynamicCatalogSession is a team without phases. withCatalog adds a
// one-entry action catalog, which is what opens the action runtime.
func dynamicCatalogSession(t *testing.T, provider ActionProvider, withCatalog bool) *TeamSession {
	t.Helper()
	registry := NewProviderRegistry()
	registry.Register("diagnostics", provider)
	session := &TeamSession{
		Workspace:        t.TempDir(),
		Config:           agent.TeamConfig{Name: "dynamic-catalog"},
		Agents:           map[string]*agent.AgentDef{"runtime-engineer": {Name: "runtime-engineer", Role: "worker"}},
		ProviderRegistry: registry,
	}
	if withCatalog {
		session.ActionCatalog = &ActionCatalogSnapshot{Version: actionCatalogSnapshotVersion, Hash: "sha256:catalog", Entries: []ActionCatalogEntry{{
			ID: "collect", Capability: "diagnostics", Type: "collect", Agent: "runtime-engineer", SideEffect: SideEffectNone,
			Recovery: RecoveryRetry, MaxInvocations: 1, Hash: "sha256:entry",
		}}}
	}
	return session
}

func newDynamicCatalogCoordinator(t *testing.T, session *TeamSession) (*Coordinator, *EventStore) {
	t.Helper()
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewEventStore(session.Workspace, "run-dynamic-catalog", "session-dynamic-catalog")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	c := &Coordinator{session: session, taskTracker: NewTaskTracker(), phaseWorkflow: w, eventStore: events, executionRunID: "run-dynamic-catalog"}
	c.SetEventJournal(eventStoreJournal{store: events})
	return c, events
}

func catalogActionTask(binding *CatalogActionBinding) TaskDef {
	return TaskDef{
		Agent: "runtime-engineer", Goal: "collect diagnostics", SideEffect: SideEffectNone, Recovery: RecoveryRetry,
		Action:        &Action{Capability: "diagnostics", Type: "collect", Payload: `{"service":"api"}`},
		CatalogAction: binding,
	}
}

func addCatalogActionTodo(c *Coordinator, task TaskDef) (TaskDef, *TodoItem) {
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: task.Agent, Desc: task.Goal, Action: task.Action, SideEffect: task.SideEffect, Recovery: task.Recovery,
	}})[0]
	return task, item
}

func actionLifecycleEvents(t *testing.T, events *EventStore) []RunEvent {
	t.Helper()
	stored, err := events.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var result []RunEvent
	for _, event := range stored {
		if strings.HasPrefix(event.Type, "action_") {
			result = append(result, event)
		}
	}
	return result
}

func TestDynamicTeamRunsCatalogActionWithoutPhases(t *testing.T) {
	provider := &recordingActionProvider{result: ActionResult{Outputs: map[string]any{"summary": "ok"}}}
	session := dynamicCatalogSession(t, provider, true)
	c, events := newDynamicCatalogCoordinator(t, session)
	if c.phaseWorkflow.Enabled() || !c.phaseWorkflow.ActionsEnabled() {
		t.Fatalf("catalog-only workflow Enabled=%v ActionsEnabled=%v, want phase dispatch off and actions on", c.phaseWorkflow.Enabled(), c.phaseWorkflow.ActionsEnabled())
	}
	binding := &CatalogActionBinding{ActionID: "collect", EntryHash: "sha256:entry", CatalogHash: "sha256:catalog", ArgumentsHash: runInputHash([]byte(`{"service":"api"}`)), InvocationID: "tai_test"}
	// The binding is not durable until WP-5, so the test enters the action
	// runtime directly with the TaskDef the compiler will produce.
	task, item := addCatalogActionTodo(c, catalogActionTask(binding))
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
		t.Fatalf("catalog action failed: %v", err)
	}
	if provider.executed != 1 {
		t.Fatalf("provider executions = %d, want 1", provider.executed)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskDone || got.TypedResult == nil || got.TypedResult.Source != "runtime" {
		t.Fatalf("catalog task = status %s result %#v", got.Status, got.TypedResult)
	}
	lifecycle := actionLifecycleEvents(t, events)
	if len(lifecycle) != 2 || lifecycle[0].Type != "action_started" || lifecycle[1].Type != "action_completed" {
		t.Fatalf("action events = %v, want started then completed", lifecycle)
	}
	var payload LifecycleEventPayload
	if err := json.Unmarshal(lifecycle[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Phase != "" || payload.ActionStatus != "success" || len(payload.Artifacts) == 0 || payload.Artifacts[0].Type != "runtime_action_receipt" {
		t.Fatalf("action_completed payload = %#v, want empty phase and a receipt", payload)
	}
}

func TestDynamicTeamRejectsNonCatalogActions(t *testing.T) {
	tests := []struct {
		name        string
		withCatalog bool
		binding     *CatalogActionBinding
	}{
		{name: "static action in a catalog team", withCatalog: true},
		{name: "static action without a catalog"},
		{name: "catalog binding without a catalog", binding: &CatalogActionBinding{ActionID: "collect", InvocationID: "tai_test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &recordingActionProvider{result: "ok"}
			c, events := newDynamicCatalogCoordinator(t, dynamicCatalogSession(t, provider, tt.withCatalog))
			task, item := addCatalogActionTodo(c, catalogActionTask(tt.binding))
			_, err := c.executeRuntimeAction(context.Background(), task, item.ID)
			if err == nil || !strings.Contains(err.Error(), "action invocation requires an enabled runtime workflow") {
				t.Fatalf("executeRuntimeAction error = %v, want the workflow requirement", err)
			}
			if provider.executed != 0 {
				t.Fatalf("provider executions = %d, want 0", provider.executed)
			}
			if lifecycle := actionLifecycleEvents(t, events); len(lifecycle) != 0 {
				t.Fatalf("rejected action emitted lifecycle events %v", lifecycle)
			}
		})
	}
}

func TestCatalogActionBindingCloneNormalizesProposals(t *testing.T) {
	var nilBinding *CatalogActionBinding
	if nilBinding.clone() != nil {
		t.Fatal("clone of nil binding is not nil")
	}
	empty := (&CatalogActionBinding{ActionID: "collect", ProposalIDs: []string{}}).clone()
	if empty.ProposalIDs != nil {
		t.Fatalf("empty proposal IDs = %#v, want nil", empty.ProposalIDs)
	}
	original := &CatalogActionBinding{ActionID: "collect", ProposalIDs: []string{"tap_a"}}
	clone := original.clone()
	clone.ProposalIDs[0] = "tap_b"
	if original.ProposalIDs[0] != "tap_a" {
		t.Fatal("clone shares proposal IDs with the original")
	}
}
