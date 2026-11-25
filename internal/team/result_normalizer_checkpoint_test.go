package team

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizerRetainsDirectCommitInterruptionReceipt(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		abrupt, correction, verbatim bool
	}{
		{name: "returned_commit_failure"},
		{name: "returned_correction_commit_failure", correction: true},
		{name: "before_attempt_receipt", abrupt: true, verbatim: true},
		{name: "before_correction_receipt", abrupt: true, correction: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDirectNormalizationInterruption(t, tc.abrupt, tc.correction, tc.verbatim)
		})
	}
}

func TestNormalizerResumesAfterOutputProjectionFailure(t *testing.T) {
	calls := 0
	const draft = `{"findings":[],"verdict":"APPROVE"}`
	c, journal, item := newDirectNormalizationRun(t, func(context.Context, []byte) ([]byte, error) {
		calls++
		return []byte(approvedNormalizationDraft), nil
	}, nil)
	worker, model := normalizationStreamedSubmission(c, item.ID, normalizationWire(draft), nil)
	c.workerAgentOverride = worker
	execJournal(t, c.session.Workspace, `CREATE TRIGGER reject_normalizer_output BEFORE UPDATE ON runtime_documents
		WHEN NEW.name LIKE 'result-normalization/%' AND NEW.body != 'null'
		BEGIN SELECT RAISE(ABORT, 'fixture normalizer projection fault'); END`)
	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(item), item.ID)
	c.terminalizeTaskErrorIfUnresolved(item.ID, err)
	interrupted := todoByID(t, c, item.ID)
	if localResultRejectionReason(err) != RepairFailureCommit || !strings.Contains(err.Error(), "fixture normalizer projection fault") ||
		!canResumeDirectNormalization(interrupted) || model.calls != 1 || calls != 1 {
		t.Fatalf("original output did not remain available for recovery: status=%s model=%d normalizer=%d error=%v", interrupted.Status, model.calls, calls, err)
	}
	execJournal(t, c.session.Workspace, "DROP TRIGGER reject_normalizer_output")
	forbidden := scriptWorker(failTurn(errors.New("unexpected worker or repair call")))
	reader, _ := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
	count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	done := todoByID(t, reader, item.ID)
	if err != nil || count != 1 || done.Status != TaskDone || forbidden.Calls() != 0 {
		t.Fatalf("sealed output did not settle after restart: count=%d status=%s model=%d error=%v", count, done.Status, forbidden.Calls(), err)
	}
	if done.ExecutionReceipt.ResultRawPayloadSHA256 != sha256HexBytes([]byte(draft)) ||
		done.ExecutionReceipt.ResultNormalizerOutputSHA256 != sha256HexBytes([]byte(approvedNormalizationDraft)) ||
		!receiptMatchesResultPayload(done.ExecutionReceipt, done.TypedResult.StructuredPayload) {
		t.Fatalf("resumed receipt differs from the original normalizer action: %+v", done.ExecutionReceipt)
	}
}

func TestDirectNormalizationResumeRejectsLostDispatchCheckpoint(t *testing.T) {
	calls := 0
	c, events, item := newDirectNormalizationRun(t, func(context.Context, []byte) ([]byte, error) {
		calls++
		return []byte(`{"findings":[],"verdict":"approve"}`), nil
	}, func(spec *TodoSpec) {
		spec.SideEffect, spec.Recovery = SideEffectNone, RecoveryRetry
		spec.Execution.AllowsReplay = new(true)
	})
	stopBeforeDirectNormalizationReceipt(t, withTestProtocolRepairInvocationContext(t.Context()), c, item,
		normalizationWire(`{"findings":[],"verdict":"APPROVE"}`), false)
	c.saveCheckpoint()
	saved := LoadSession(c.session.Workspace)
	if saved == nil {
		t.Fatal("interrupted dispatch was not persisted")
	}
	active := todoItemByID(saved.Tasks, item.ID)
	if active == nil || active.Status != TaskInProgress || !IsTaskReplayable(taskDefFromTodoItem(active)) || calls != 1 {
		t.Fatalf("expected replayable in-progress dispatch and one normalizer invocation, got %v and %d", active, calls)
	}
	key := directNormalizationCheckpointDocument(c.activeBranchID(), active.ID, active.OccurrenceRevision, active.DispatchID)
	if _, found, err := workspaceDocs(c.session.Workspace).getDocument(key); err != nil || !found {
		t.Fatalf("original direct checkpoint missing: %t, %v", found, err)
	}
	execJournal(t, c.session.Workspace, "DELETE FROM runtime_documents WHERE name=?", key)
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}
	reader, _ := restartDirectNormalizationRun(t, c, saved, item, func(context.Context, []byte) ([]byte, error) {
		t.Error("resume invoked the normalizer")
		return nil, errors.New("unexpected normalizer call")
	})
	forbidden := scriptWorker(failTurn(errors.New("unexpected worker or repair call")))
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
	count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	settled := todoByID(t, reader, item.ID)
	if count != 1 || err == nil || !strings.Contains(err.Error(), "checkpoint is missing for the started dispatch") ||
		settled.Status != TaskBlocked || forbidden.Calls() != 0 {
		t.Fatalf("lost started dispatch must block: count=%d status=%s model calls=%d err=%v", count, settled.Status, forbidden.Calls(), err)
	}
}
