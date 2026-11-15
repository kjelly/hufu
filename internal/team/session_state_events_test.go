package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkflowRetryCountersSurviveSecretLikeFailureDetails(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "legacy"}[legacy], func(t *testing.T) {
			c := newTestCoordinator(t)
			store, err := NewEventStore(c.session.Workspace, "run-retry-redaction", "session-retry-redaction")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			c.eventStore = store
			failure := ExecutionError{Phase: PhasePrepare, Component: "prepare", Category: CategoryProviderFailure, Source: "adapter", Cause: "max_review_input_tokens=200000 api_key=private-fixture-value", Retryable: true}
			retry := NewRetryState()
			if legacy {
				retry.Attempts[failure.Signature().String()] = 1
			} else if !retry.RecordFailure(failure, 1) {
				t.Fatal("first retry denied")
			}
			if err := c.emitWorkflowStateChange(newWorkflowStateChangedPayload(PhasePrepare, nil, retry)); err != nil {
				t.Fatal(err)
			}
			events := mustReadEvents(t, store)
			for _, event := range events {
				if strings.Contains(string(event.Payload), "private-fixture-value") {
					t.Fatal("retry state persisted credential text")
				}
			}
			replayed := ReduceToSessionData(events)
			if replayed.RetryState == nil || len(replayed.RetryState.Attempts) != 1 {
				t.Fatalf("retry counters unavailable after replay: %#v", replayed.RetryState)
			}
			if replayed.RetryState.RecordFailure(failure, 1) {
				t.Fatal("event replay reset the exhausted retry allowance")
			}
		})
	}
}

func newSessionStateTestCoordinator(t *testing.T, session *TeamSession) *Coordinator {
	t.Helper()
	store, err := NewEventStore(session.Workspace, "run-session-state", "session-state")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &Coordinator{session: session, sessionData: NewSession(), taskTracker: NewTaskTracker(), eventStore: store}
}

func countEvents(t *testing.T, store *EventStore, eventType EventType) int {
	t.Helper()
	count := 0
	for _, event := range mustReadEvents(t, store) {
		if event.Type == string(eventType) {
			count++
		}
	}
	return count
}

// sameJSON compares values by their persisted JSON, the form the session
// checkpoint and replay share.
func sameJSON(t *testing.T, got, want any) bool {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	return string(g) == string(w)
}

func TestCheckpointedWorkflowStateReplaysFromEvents(t *testing.T) {
	session := workflowTestSession(t)
	workflow, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	c := newSessionStateTestCoordinator(t, session)
	c.phaseWorkflow = workflow

	c.saveCheckpoint()
	if err := workflow.Start(); err != nil {
		t.Fatal(err)
	}
	items := []*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}
	if err := workflow.observe(items); err != nil {
		t.Fatal(err)
	}
	workflow.retryState.Attempts["signature"] = 2
	c.saveCheckpoint()
	// An unchanged snapshot is not appended again.
	c.saveCheckpoint()

	if got := countEvents(t, c.eventStore, EventWorkflowStateChanged); got != 2 {
		t.Fatalf("workflow_state_changed events = %d, want 2 (INIT, then AUDIT)", got)
	}
	replayed := ReduceToSessionData(mustReadEvents(t, c.eventStore))
	live := c.sessionData
	if replayed.WorkflowState != PhaseAudit || replayed.WorkflowState != live.WorkflowState {
		t.Fatalf("replayed workflow state = %s, live = %s, want AUDIT", replayed.WorkflowState, live.WorkflowState)
	}
	if !sameJSON(t, replayed.PhaseResults, live.PhaseResults) || !sameJSON(t, replayed.RetryState, live.RetryState) {
		t.Fatalf("replayed results=%+v retry=%+v, live results=%+v retry=%+v", replayed.PhaseResults, replayed.RetryState, live.PhaseResults, live.RetryState)
	}
	restored, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.restore(replayed.WorkflowState, replayed.PhaseResults, live.RuntimeWorkspace, replayed.RetryState); err != nil {
		t.Fatalf("restore replayed workflow: %v", err)
	}
}

func TestWorkflowReplayKeepsTheLastSnapshot(t *testing.T) {
	event := func(state Phase) RunEvent {
		payload, err := json.Marshal(WorkflowStateChangedPayload{State: state})
		if err != nil {
			t.Fatal(err)
		}
		return RunEvent{Type: string(EventWorkflowStateChanged), Payload: payload}
	}
	// A return to an earlier state must win; an idempotency key would have
	// deduplicated the second PREPARE away.
	replayed := ReduceToSessionData([]RunEvent{event(PhasePrepare), event(PhaseAudit), event(PhasePrepare)})
	if replayed.WorkflowState != PhasePrepare {
		t.Fatalf("replayed workflow state = %s, want PREPARE", replayed.WorkflowState)
	}
}

func TestAcceptanceRevisionsReplayFromEvents(t *testing.T) {
	session := &TeamSession{Workspace: t.TempDir()}
	c := &Coordinator{session: session, taskTracker: NewTaskTracker()}
	// The configured contract is set while the coordinator is built, before
	// the session or the event store exists, so neither records it.
	if err := c.SetAcceptanceSpec(AcceptanceSpec{Commands: []string{"test -f configured"}}); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(session.Workspace, "run-acceptance", "session-acceptance")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c.eventStore = store
	c.sessionData = NewSession()
	if err := c.SetAcceptanceSpecWithReason(AcceptanceSpec{Commands: []string{"test -f modified"}}, "tightened"); err != nil {
		t.Fatal(err)
	}

	live := c.sessionData.AcceptanceContractRevisions
	replayed := ReduceToSessionData(mustReadEvents(t, store)).AcceptanceContractRevisions
	if len(live) != 1 || len(replayed) != 1 {
		t.Fatalf("revisions live=%+v replayed=%+v, want one runtime revision each", live, replayed)
	}
	// The event envelope, not the session write, timestamps the replayed
	// revision; everything else must match.
	replayed[0].Timestamp = live[0].Timestamp
	if !sameJSON(t, replayed, live) {
		t.Fatalf("replayed revisions = %+v, want %+v", replayed, live)
	}
}

func TestContinuationCheckpointReplaysFromEvents(t *testing.T) {
	session := &TeamSession{Workspace: t.TempDir()}
	c := newSessionStateTestCoordinator(t, session)
	tests := []struct {
		name string
		step func()
	}{
		{name: "pending checkpoint", step: func() { c.saveContinuationCheckpoint(3, 10, "step_limit", "pending") }},
		{name: "resumed checkpoint", step: func() { c.ResumeContinuationCheckpoint() }},
		{name: "aborted checkpoint", step: func() { c.saveContinuationCheckpoint(4, 10, "operator", "aborted") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.step()
			replayed := ReduceToSessionData(mustReadEvents(t, c.eventStore)).ContinuationCheckpoint
			if replayed == nil || !sameJSON(t, replayed, c.sessionData.ContinuationCheckpoint) {
				t.Fatalf("replayed checkpoint = %+v, want %+v", replayed, c.sessionData.ContinuationCheckpoint)
			}
		})
	}
}
