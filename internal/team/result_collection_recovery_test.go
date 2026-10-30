package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func blockedRecoveryFixture(t *testing.T, body string, prepared bool, reason RepairFailureReason) (*Coordinator, string, *ResultCollectionCheckpoint) {
	t.Helper()
	c, item, ctx, cp := beginCollectionFixture(t)
	var err error
	if body != "" {
		cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		cp, err = c.recordCollectionResponse(ctx, cp, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if prepared {
			cp, err = c.prepareCollectionClaims(ctx, cp)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	cp, err = c.blockResultCollection(ctx, cp, reason)
	if err != nil {
		t.Fatal(err)
	}
	c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: reason, Checkpoint: cp, Cause: errors.New("historical collection rejection")})
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "historical collection rejection", NeedsHuman, FailureProtocol, TaskBlocked, "original report with limitations")
	return c, item.ID, cp
}

func TestResultCollectionRecoveryAssessmentPreservesEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		prepared   bool
		reason     RepairFailureReason
		want       ResultCollectionRecoveryDisposition
		additional int
	}{
		{"valid prepared previously source invalid", `{"status":"success","summary":"limitations retained in original report"}`, true, RepairFailureSource, RecoveryCommitLocal, 0},
		{"valid received", `{"status":"success","summary":"complete"}`, false, RepairFailureSource, RecoveryCommitLocal, 0},
		{"malformed response", `{"status":`, false, RepairFailureSource, RecoveryRepairClaims, 2},
		{"missing response unavailable", "", false, repairFailureCollectionUnavailable, RecoveryResumeCollection, 2},
		{"missing source claims", "", false, RepairFailureSource, RecoveryBlocked, 0},
		{"storage only", "", false, RepairFailureCommit, RecoveryResumeCollection, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, id, cp := blockedRecoveryFixture(t, tc.body, tc.prepared, tc.reason)
			before, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			taskBefore := c.todoItemByID(id)
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if a.Disposition != tc.want || a.AdditionalTurns != tc.additional {
				t.Fatalf("assessment=%+v", a)
			}
			again, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil || again.Fingerprint != a.Fingerprint {
				t.Fatalf("unstable assessment: %v", err)
			}
			after, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(taskBefore, c.todoItemByID(id)) || len(c.resultValidationFailures) > 0 {
				t.Fatal("assessment changed durable evidence, status, or counters")
			}
			if a.CheckpointSHA256 == "" || a.Checkpoint.Work != cp.Work || a.OriginalReason != tc.reason {
				t.Fatal("assessment lost historical binding")
			}
			if tc.want != RecoveryBlocked && validateRecoveryAssessment(a) != nil {
				t.Fatalf("invalid assessment: %v", validateRecoveryAssessment(a))
			}
		})
	}
}

func TestResultCollectionRecoveryRefusesHistoricalInputRebinding(t *testing.T) {
	c, id, cp := blockedRecoveryFixture(t, `{"status":"success","summary":"original limitations"}`, true, RepairFailureSource)
	snapshot := setTaskInputSnapshot(t, c, true)
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(EventRunInputsResolved), RunID: snapshot.RunID, BranchID: cp.BranchID, Actor: "runtime", Payload: body}); err != nil {
		t.Fatal(err)
	}
	before, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.AssessResultCollectionRecovery(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Disposition != RecoveryBlocked || a.AdditionalTurns != 0 || a.Checkpoint.InputsID != "" || a.Checkpoint.Work != cp.Work {
		t.Fatalf("historical work was rebound: %+v", a)
	}
	if !slices.ContainsFunc(a.Checks, func(check ResultCollectionRecoveryCheck) bool {
		return check.Code == "frozen_inputs" && !check.Passed && strings.Contains(check.Diagnostic, "unsupported")
	}) {
		t.Fatalf("missing historical binding diagnostic: %+v", a.Checks)
	}
	after, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) || currentResultCollection(c.todoItemByID(id)).InputsID != "" {
		t.Fatal("assessment altered historical evidence")
	}
}

func TestResultCollectionRecoveryGrantReplaysAndCommitsLocally(t *testing.T) {
	c, id, cp := blockedRecoveryFixture(t, `{"status":"success","summary":"original limitations remain recorded"}`, true, RepairFailureSource)
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Reopen the task projection from historical events before authorizing.
	tasks, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().AddReserved(tasks)
	c.executionRunID = "operator-recovery"
	worker := scriptWorker(textTurn("must never run"))
	c.workerAgentOverride = worker
	ctx := WithResultOnlyRepair(t.Context(), id)
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	granted := c.todoItemByID(id)
	grant := granted.ExecutionReceipt.RepairProvenance.OperatorGrants[0]
	if grant.Recovery == nil || grant.Recovery.Disposition != RecoveryCommitLocal || grant.AdditionalAttempts != 0 || grant.Recovery.Checkpoint.Work != cp.Work {
		t.Fatalf("grant=%+v", grant)
	}
	if err := c.authorizeOperatorResultRepair(ctx); err == nil {
		t.Fatal("duplicate grant accepted")
	}
	events, err = c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tasks, err = ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().AddReserved(tasks)
	c.revokeTaskOccurrence(id)
	if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id); err != nil {
		t.Fatal(err)
	}
	done := c.todoItemByID(id)
	if done.Status != TaskDone || worker.Calls() != 0 || done.ExecutionReceipt.ResultCollection.TurnsUsed != cp.TurnsUsed || done.ExecutionReceipt.ResultCollection.Work != cp.Work {
		t.Fatal("local recovery replayed work or lost progress")
	}
	if done.TypedResult == nil || !strings.Contains(done.TypedResult.Summary, "limitations") {
		t.Fatal("lost original result summary")
	}
}

func TestResultCollectionRecoveryRejectsStaleOrForgedGrant(t *testing.T) {
	for _, kind := range []string{"fingerprint", "scope", "budget", "prior_checkpoint", "policy"} {
		t.Run(kind, func(t *testing.T) {
			c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"complete"}`, true, RepairFailureSource)
			c.executionRunID = "operator-recovery"
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			grant := &ResultRepairGrant{RunID: c.executionRunID, BranchID: c.activeBranchID(), AfterAttempts: a.TurnsUsed, AdditionalAttempts: a.AdditionalTurns, TranscriptRef: a.TranscriptRef, Recovery: cloneRecoveryAssessment(a)}
			switch kind {
			case "fingerprint":
				grant.Recovery.Fingerprint = "invalid"
			case "scope":
				grant.Recovery.Checkpoint.TaskID = "other"
			case "budget":
				grant.AdditionalAttempts = 2
			case "prior_checkpoint":
				grant.Recovery.Checkpoint.Revision++
			case "policy":
				c.executionPolicy.snapshot.ConfigurationHash = strings.Repeat("a", 64)
			}
			if err := c.CommitTaskTransition(t.Context(), id, TaskBlocked, TaskProtocolIncomplete, "forged grant", "", map[string]any{"result_repair_grant": grant}); err == nil {
				t.Fatal("invalid grant accepted")
			}
			if c.todoItemByID(id).Status != TaskBlocked {
				t.Fatal("rejected grant changed task")
			}
		})
	}
}

func TestResultCollectionRecoveryConcurrentAuthorization(t *testing.T) {
	c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"complete"}`, true, RepairFailureSource)
	c.executionRunID = "concurrent-recovery"
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { results <- c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)) })
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 || len(c.todoItemByID(id).ExecutionReceipt.RepairProvenance.OperatorGrants) != 1 {
		t.Fatalf("successful grants=%d", success)
	}
}

func TestResultCollectionRecoveryPreviewUsesReadOnlySource(t *testing.T) {
	c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"complete"}`, true, RepairFailureSource)
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := NewDryRunCoordinator(DryRunCoordinatorParams{Session: cloneSession(c.session, c.session.Workspace), DefaultProviderURL: "http://127.0.0.1:1/v1", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	source := &ResumeSource{BranchID: "main", Session: &SessionData{Tasks: tasks}}
	before, _ := json.Marshal(tasks, collectionDurationFormat)
	a, err := preview.AssessResumeResultCollection(context.Background(), source, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Disposition != RecoveryBlocked {
		t.Fatalf("preview must match public resume refusal without frozen inputs: %+v", a)
	}
	after, _ := json.Marshal(tasks, collectionDurationFormat)
	current, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || !reflect.DeepEqual(events, current) || preview.eventStore != nil || preview.recoveryBranch != "" {
		t.Fatal("preview changed source or retained a mutable runtime")
	}
}

func TestResultCollectionRecoveryKeepsReservedTurnsAfterInterruption(t *testing.T) {
	c, id, cp := blockedRecoveryFixture(t, `{"status":`, false, RepairFailureSource)
	c.executionRunID = "first-recovery"
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
		t.Fatal(err)
	}
	granted := c.todoItemByID(id).ExecutionReceipt.ResultCollection
	ctx, err := c.restoreCollectionContext(WithSessionResume(t.Context()), granted)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := c.reserveResultCollection(ctx, granted, "interrupted-collector", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	// A crash after reserve has no response. It must not refund the consumed turn.
	blocked, err := c.blockResultCollection(ctx, reserved, repairFailureCollectionUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	c.recordCollectionStopReceipt(id, &resultCollectionFailure{Reason: repairFailureCollectionUnavailable, Checkpoint: blocked, Cause: context.Canceled})
	item := c.todoItemByID(id)
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, id, "collector interrupted", NeedsHuman, FailureProtocol, TaskBlocked, "")
	c.executionRunID = "second-recovery"
	a, err := c.AssessResultCollectionRecovery(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Disposition != RecoveryResumeCollection || a.TurnsUsed != cp.TurnsUsed+1 {
		t.Fatalf("assessment refunded reserved turn: %+v", a)
	}
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
		t.Fatal(err)
	}
	current := c.todoItemByID(id).ExecutionReceipt.ResultCollection
	if current.TurnsUsed != reserved.TurnsUsed || current.TurnLimit != reserved.TurnsUsed+2 {
		t.Fatal("grant reset or over-extended the budget")
	}
}

func TestResultCollectionRecoveryHeadFenceRejectsAnotherWriter(t *testing.T) {
	c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"complete"}`, true, RepairFailureSource)
	a, err := c.AssessResultCollectionRecovery(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewEventStore(c.session.Workspace, "other-run", "other-session")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err = other.AppendPersisted(RunEvent{Type: "other_writer", Actor: "operator", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	_, err = c.eventStore.AppendPersistedAtHead(t.Context(), RunEvent{Type: "stale_recovery", Actor: "operator", Payload: []byte(`{}`)}, a.JournalHeadID)
	if !errors.Is(err, ErrEventHeadChanged) {
		t.Fatalf("stale writer error=%v", err)
	}
}

type recoveryAppendFailureJournal struct{ ConditionalEventJournal }

func (j recoveryAppendFailureJournal) AppendAtHead(context.Context, RunEvent, string) (RunEvent, error) {
	return RunEvent{}, errors.New("injected append failure")
}

func TestResultCollectionRecoveryFailedAuthorizationIsAtomic(t *testing.T) {
	for _, kind := range []string{"cancelled", "storage_failure"} {
		t.Run(kind, func(t *testing.T) {
			c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"complete"}`, true, RepairFailureSource)
			before := c.todoItemByID(id)
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			c.executionRunID = "failed-authorization"
			ctx := WithResultOnlyRepair(t.Context(), id)
			if kind == "cancelled" {
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
			} else {
				c.SetEventJournal(recoveryAppendFailureJournal{c.EventJournal().(ConditionalEventJournal)})
			}
			if err := c.authorizeOperatorResultRepair(ctx); err == nil {
				t.Fatal("failed authorization succeeded")
			}
			after, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(events, after) || !reflect.DeepEqual(before, c.todoItemByID(id)) {
				t.Fatal("failed authorization changed grant, status or checkpoint")
			}
		})
	}
}

func TestResultCollectionRecoveryReplayRejectsChangedAssessment(t *testing.T) {
	c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"complete"}`, true, RepairFailureSource)
	c.executionRunID = "grant-before-crash"
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
		t.Fatal(err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := range events {
		if events[i].Type != string(EventTaskProtocolIncomplete) {
			continue
		}
		var payload map[string]jsontext.Value
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		var grant ResultRepairGrant
		if err := json.Unmarshal(payload["result_repair_grant"], &grant); err != nil {
			t.Fatal(err)
		}
		grant.Recovery.CheckpointSHA256 = strings.Repeat("a", 64)
		payload["result_repair_grant"], err = json.Marshal(grant, collectionDurationFormat)
		if err != nil {
			t.Fatal(err)
		}
		events[i].Payload, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReplayTodoList(events); err == nil {
		t.Fatal("replay trusted a changed grant")
	}
}
