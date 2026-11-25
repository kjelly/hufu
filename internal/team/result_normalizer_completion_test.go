package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/sidecar"
)

const approvedNormalizationDraft = `{"verdict":"approve","findings":[]}`

type interruptedVerification struct{}

func TestOptionalNormalizedHandoffPreservesFinalizerRejection(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want RepairFailureReason
	}{
		{name: "unavailable_source", want: RepairFailureSource},
		{name: "failed_action", want: RepairFailureAction},
		{name: "failed_commit", want: RepairFailureCommit},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var normalizations int
			c, journal, item := newDirectNormalizationRun(t, func(_ context.Context, draft []byte) ([]byte, error) {
				normalizations++
				if scenario.want == RepairFailureAction {
					return nil, errors.New("fixture normalizer action failed")
				}
				return draft, nil
			}, func(spec *TodoSpec) {
				spec.ResultContract.RequireStructured = false
				spec.SideEffect = SideEffectNone
				spec.Verify = "true"
			})
			worker := scriptWorker(textTurn("## Findings\n\nThe reviewed code has no findings."))
			c.workerAgentOverride = worker
			wire := normalizationWire(approvedNormalizationDraft)
			var sink TaskResultSink
			switch scenario.want {
			case RepairFailureSource:
				wire = normalizedWireWithArtifact(approvedNormalizationDraft, "missing-artifact.txt")
			case RepairFailureCommit:
				sink = interruptedResultSink{}
			}
			var model *rejectedResultStreamModel
			c.repairAgentOverride, model = normalizationStreamedSubmission(c, item.ID, wire, sink)
			_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(item), item.ID)
			c.terminalizeTaskErrorIfUnresolved(item.ID, err)
			got := todoByID(t, c, item.ID)
			expected := TaskBlocked
			if scenario.want == RepairFailureCommit {
				expected = TaskProtocolIncomplete
			}
			if localResultRejectionReason(err) != scenario.want || got.Status != expected || got.VerifyResult != nil ||
				got.ExecutionReceipt.RepairProvenance.FailureReason != scenario.want || model.calls != 1 || worker.Calls() != 1 {
				t.Fatalf("finalizer rejection was bypassed: status=%s worker=%d finalizer=%d error=%v", got.Status, worker.Calls(), model.calls, err)
			}
			if scenario.want != RepairFailureCommit {
				return
			}
			forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
			c, _ = restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
			count, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			if count != 1 || err != nil || todoByID(t, c, item.ID).Status != TaskDone || forbidden.Calls() != 0 || normalizations != 1 {
				t.Fatalf("failed finalizer commit did not resume the sealed output: count=%d error=%v", count, err)
			}
		})
	}
}

func TestOrdinaryNormalizedVerificationResumes(t *testing.T) {
	for _, tc := range []struct {
		name, mode, resultContains string
		want                       TaskStatus
	}{
		{"success", "success", "submitted successfully", TaskDone},
		{"expected_failure_with_match", "expected_failure", "submitted successfully", TaskBlocked},
		{"expected_failure_without_match", "expected_failure", "unexpected observation", TaskDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
				spec.OutputMode = TaskOutputModeVerbatim
				spec.VerifySpec = &VerificationSpec{Type: VerifyToolCallAssert, Mode: tc.mode, ToolCallAssertions: []ToolCallAssertion{
					{Tool: submitResultToolName, InputContains: approvedNormalizationDraft, ResultContains: tc.resultContains},
				}}
			})
			c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizationWire(approvedNormalizationDraft), nil)
			stopBeforeObjectiveVerification(t, c, item)
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			checkpoint := normalizationResumeProjection(t, c)
			persisted := todoItemByID(checkpoint.Tasks, item.ID)
			if persisted == nil || persisted.ExecutionReceipt == nil {
				t.Fatal("verifying event is missing its receipt")
			}
			if persisted.Status != TaskVerifying || persisted.ExecutionReceipt.RepairProvenance != nil ||
				persisted.TypedResult == nil || persisted.Output == "" {
				t.Fatalf("verifying event status=%s provenance=%v has_result=%v output_bytes=%d", persisted.Status,
					persisted.ExecutionReceipt.RepairProvenance != nil, persisted.TypedResult != nil, len(persisted.Output))
			}
			reader, _ := restartDirectNormalizationRun(t, c, checkpoint, item, func(context.Context, []byte) ([]byte, error) {
				t.Error("verification repeated normalization")
				return nil, errors.New("unexpected normalizer call")
			})
			forbidden := scriptWorker(failTurn(errors.New("unexpected model call")))
			reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
			count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			done := todoByID(t, reader, item.ID)
			if count != 1 || done.Status != tc.want || done.VerifyResult == nil || (err == nil) != (tc.want == TaskDone) || forbidden.Calls() != 0 {
				t.Fatalf("ordinary verification count=%d status=%s verification=%+v model calls=%d error=%v", count, done.Status, done.VerifyResult, forbidden.Calls(), err)
			}
			if tc.want == TaskDone {
				assertRecoveredNormalizationTranscript(t, reader, item.ID, persisted.ExecutionReceipt)
			}
		})
	}
}

func stopBeforeObjectiveVerification(t *testing.T, c *Coordinator, item *TodoItem) {
	t.Helper()
	c.reportStatus = func(event StatusEvent) {
		if event.Type == "verify_start" {
			panic(interruptedVerification{})
		}
	}
	defer func() {
		if got := recover(); got != (interruptedVerification{}) {
			t.Fatalf("expected interruption immediately before objective verification, got %v", got)
		}
	}()
	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(item), item.ID)
	t.Logf("execution returned before verification barrier: %v", err)
}

func echoNormalization(_ context.Context, draft []byte) ([]byte, error) {
	return draft, nil
}

func TestNormalizedCorrectionRetainsAllowanceOnTemporaryReadFault(t *testing.T) {
	c, journal, item := newDirectNormalizationRun(t, echoNormalization, nil)
	wire := normalizationWire(rejectedNormalizationDraft)
	interruptBeforeNormalizationValidation(t, c, item, wire, rejectedNormalizationDraft, true, false)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	actions := 0
	normalize := func(_ context.Context, draft []byte) ([]byte, error) {
		actions++
		return draft, nil
	}
	ctx := WithSessionResume(withTestProtocolRepairInvocationContext(t.Context()))
	reader, journal := restartDirectNormalizationRun(t, c, normalizationResumeProjection(t, c), item, normalize)
	checkpointRejectedNormalization(t, ctx, reader, item.ID)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reader, journal = restartDirectNormalizationRun(t, c, normalizationResumeProjection(t, reader), item, normalize)
	interrupted := todoByID(t, reader, item.ID)
	receipt := interrupted.ExecutionReceipt
	restore := removeNormalizedVerificationTranscript(t, reader, receipt, true)
	forbidden := scriptWorker(failTurn(errors.New("unexpected model call")))
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
	count, err := reader.ResumeInterruptedTasks(ctx)
	active := todoByID(t, reader, item.ID)
	if count != 1 || localResultRejectionReason(err) != RepairFailureCommit || active.Status != TaskProtocolIncomplete ||
		active.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureInvalidSchema || active.ExecutionReceipt.RepairProvenance.RepairAttempts != 0 ||
		forbidden.Calls() != 0 || actions != 0 {
		t.Fatalf("temporary read spent the correction: count=%d status=%s error=%v", count, active.Status, err)
	}
	restore()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reader, _ = restartDirectNormalizationRun(t, c, normalizationResumeProjection(t, reader), item, normalize)
	repair, model := normalizationStreamedSubmission(reader, item.ID, normalizationWire(approvedNormalizationDraft), nil)
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, repair
	count, err = reader.ResumeInterruptedTasks(ctx)
	done := todoByID(t, reader, item.ID)
	if err != nil || count != 1 || done.Status != TaskDone || forbidden.Calls() != 0 || model.calls != 1 || actions != 1 ||
		done.ExecutionReceipt.TranscriptRef != receipt.TranscriptRef || done.ExecutionReceipt.RepairProvenance.RepairAttempts != 1 {
		t.Fatalf("replayed correction repeated work or changed evidence: count=%d status=%s error=%v", count, done.Status, err)
	}
}

func checkpointRejectedNormalization(t *testing.T, ctx context.Context, reader *Coordinator, taskID string) {
	t.Helper()
	item, err := reader.restoreInterruptedDirectNormalization(ctx, todoByID(t, reader, taskID))
	if err != nil {
		t.Fatalf("restore interrupted rejected submission: %v", err)
	}
	recovery, err := reader.loadDirectNormalizationRecovery(ctx, item)
	if err != nil {
		t.Fatalf("reopen rejected submission after %s checkpoint (dispatch %s): %v", item.Status, item.DispatchID, err)
	}
	localCtx := context.WithValue(ctx, directNormalizationRecoveryKey{}, recovery)
	response, err := reader.runLocalResultContinuation(localCtx, item, item.ExecutionReceipt.Attempt, "normalized-"+recovery.digest, string(recovery.submission.Wire))
	if err != nil || !response.IsError || submissionFailureMetadata(response.Metadata) != RepairFailureInvalidSchema {
		t.Fatalf("local replay did not reproduce the rejected schema: response=%v error=%v", response, err)
	}
	if repair, err := reader.checkpointDirectNormalizationRejection(item, taskDefFromTodoItem(item), response); err != nil || !repair {
		t.Fatalf("failed to checkpoint authentic invalid submission before correction: %v", err)
	}
}

func normalizationResumeProjection(t *testing.T, c *Coordinator) *SessionData {
	t.Helper()
	source, err := ReadResumeSource(c.session.Workspace, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return source.Session
}

func TestNormalizedVerificationResumesFromCommittedResult(t *testing.T) {
	c, journal, item := newDirectNormalizationRun(t, func(_ context.Context, raw []byte) ([]byte, error) {
		return raw, nil
	}, func(spec *TodoSpec) {
		spec.OutputMode = TaskOutputModeVerbatim
		spec.Verify = "true"
	})
	stopBeforeDirectNormalizationReceipt(t, t.Context(), c, item, normalizationWire(approvedNormalizationDraft), false)
	item, err := c.restoreInterruptedDirectNormalization(t.Context(), todoByID(t, c, item.ID))
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := c.loadDirectNormalizationRecovery(t.Context(), item)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), directNormalizationRecoveryKey{}, recovery)
	response, err := c.runLocalResultContinuation(ctx, item, item.ExecutionReceipt.Attempt, "normalized-"+recovery.digest, string(recovery.submission.Wire))
	if err != nil || response.IsError {
		t.Fatalf("local continuation failed: %v %s", err, response.Content)
	}
	result := c.GetTaskResult(item.ID)
	if err := c.materializeDirectNormalizationReceipt(item, result); err != nil {
		t.Fatal(err)
	}
	item = todoByID(t, c, item.ID)
	_, manifest, err := c.finalizeRecoveredNormalizedResult(ctx, item, taskDefFromTodoItem(item), result, result.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, item.ID, TaskVerifying, "running objective verification", manifest, nil); err != nil {
		t.Fatal(err)
	}
	c.saveCheckpoint()
	checkpoint := LoadSession(c.session.Workspace)
	if err := journal.Close(); err != nil || checkpoint == nil {
		t.Fatalf("verifying checkpoint unavailable: %v", err)
	}
	reader, _ := restartDirectNormalizationRun(t, c, checkpoint, item, func(context.Context, []byte) ([]byte, error) {
		t.Error("verification repeated normalization")
		return nil, errors.New("unexpected normalizer call")
	})
	forbidden := scriptWorker(failTurn(errors.New("unexpected model call")))
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
	count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	done := todoByID(t, reader, item.ID)
	if err != nil || count != 1 || done.Status != TaskDone || done.VerifyResult == nil || forbidden.Calls() != 0 {
		t.Fatalf("verification continuation count=%d status=%s verification=%+v model calls=%d error=%v", count, done.Status, done.VerifyResult, forbidden.Calls(), err)
	}
	assertRecoveredNormalizationTranscript(t, reader, item.ID, item.ExecutionReceipt)
}

func TestNormalizedResultRecoveryAppliesInvariantGate(t *testing.T) {
	c, _, item := newDirectNormalizationRun(t, nil, func(spec *TodoSpec) {
		spec.InvariantVerification = InvariantVerificationGate
	})
	result := &TaskResult{Status: TaskResultStatusSuccess, Summary: "reviewed", InvariantVerification: &InvariantVerificationResult{
		Assessments: []InvariantAssessment{completionAssessment("required", InvariantSeverityError, InvariantViolated)},
	}}
	if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskInProgress, "worker ran", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskProtocolIncomplete, "resuming sealed normalization", "", map[string]any{"protocol_checkpoint": true}); err != nil {
		t.Fatal(err)
	}
	item = todoByID(t, c, item.ID)
	_, err := c.finishProtocolRepair(t.Context(), item, taskDefFromTodoItem(item), item.Agent, item.Model, result, result.Summary)
	blocked := todoByID(t, c, item.ID)
	if err == nil || blocked.Status != TaskBlocked || blocked.FailureEvent == nil || blocked.FailureEvent.FailureClass != FailureExecution {
		t.Fatalf("recovered result escaped its invariant gate: status=%s failure=%+v error=%v", blocked.Status, blocked.FailureEvent, err)
	}
}

func TestNormalizedResumeRetainsOriginalIsolatedWorld(t *testing.T) {
	for _, failure := range []string{"returned_submission", "returned_correction", "interrupted_work_submission", "interrupted_ordinary_verification", "verifying_write_failure", "corrected_verification_write_failure", "resume_commit_failure", "resume_verification_read_failure", "missing_verification_evidence", "resume_world_read_failure", "optional_payload", "optional_acknowledgment", "structured_acknowledgment", "apply_start_write_failure", "apply_projection_write_failure", "missing_pending_apply_manifest", "missing_apply_projection", "apply_start_conflict", "apply_finish_write_failure", "empty_apply_done_write_failure", "forged_apply_started", "forged_already_applied", "forged_delta_manifest", "missing_apply_artifact", "verification_projection_write_failure", "changed_world_after_verification_fault"} {
		t.Run(failure, func(t *testing.T) { testNormalizedIsolatedContinuation(t, failure) })
	}
}

func testNormalizedIsolatedContinuation(t *testing.T, failure string) {
	t.Helper()
	abrupt := failure == "interrupted_work_submission"
	corrected := failure == "returned_correction" || failure == "corrected_verification_write_failure"
	optionalAck := failure == "optional_acknowledgment"
	acknowledged := optionalAck || failure == "structured_acknowledgment"
	optionalPayload := failure == "optional_payload" || optionalAck
	writeFault, applyPending := isolatedNormalizationWriteFault(failure)
	projectionWriteFault := failure == "apply_projection_write_failure" || failure == "missing_pending_apply_manifest"
	verificationWriteFault := failure == "verification_projection_write_failure" || failure == "changed_world_after_verification_fault"
	countVerification := projectionWriteFault || verificationWriteFault || failure == "apply_start_write_failure" || failure == "missing_apply_projection"
	abruptVerification := failure == "interrupted_ordinary_verification" || failure == "resume_verification_read_failure" ||
		failure == "missing_verification_evidence" || failure == "resume_world_read_failure" || failure == "optional_payload"
	committed := abruptVerification || writeFault != "" || projectionWriteFault || verificationWriteFault || acknowledged
	f := newIsolatedTeamFixture(t, nil)
	f.c.SetSessionData(&SessionData{})
	var skepticCalls atomic.Int32
	if countVerification {
		installCompletionSkeptic(f.c, &skepticCalls)
	}
	const relPath = "reviewed.txt"
	const original = "original"
	const edited = "verified edit"
	f.write(t, relPath, original)
	if verificationWriteFault {
		f.write(t, verifierFixtureSecretFile, verificationFixtureSecretContent(t))
	}
	actionCalls := 0
	item := admitDirectNormalizationTask(t, f.c, func(_ context.Context, raw []byte) ([]byte, error) {
		actionCalls++
		return raw, nil
	}, func(spec *TodoSpec) {
		spec.SideEffect = SideEffectWorkspaceWrite
		spec.Verify = `test "$(cat reviewed.txt)" = 'verified edit'`
		if countVerification {
			spec.Verify += ` && printf 'verified\n' >> verifier.log`
			spec.AdversarialVerify = 1
		}
		if verificationWriteFault {
			spec.Verify += verifierFixtureSecretOutput
		}
		if failure == "empty_apply_done_write_failure" {
			spec.Verify = ""
		}
		if optionalPayload {
			spec.ResultContract.RequireStructured = false
		}
		if failure == "resume_verification_read_failure" || failure == "missing_verification_evidence" {
			spec.Verify = ""
			spec.VerifySpec = &VerificationSpec{Type: VerifyToolCallAssert, ToolCallAssertions: []ToolCallAssertion{
				{Tool: submitResultToolName, InputContains: approvedNormalizationDraft, ResultContains: "submitted successfully"},
			}}
		}
		policy, err := f.c.admittedWorkerWorkspace(TaskDef{SideEffect: spec.SideEffect}, f.c.session.Agents[spec.Agent])
		if err != nil {
			t.Fatal(err)
		}
		spec.WorkerWorkspace = policy
	})
	draft := approvedNormalizationDraft
	if corrected {
		draft = rejectedNormalizationDraft
	}
	var sink TaskResultSink
	if failure == "returned_submission" || failure == "resume_commit_failure" {
		sink = interruptedResultSink{}
	}
	wire := normalizationWire(draft)
	if failure == "missing_apply_artifact" {
		const source = "source.txt"
		if err := os.WriteFile(filepath.Join(f.c.session.Workspace, source), []byte("accepted evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
		wire = normalizedWireWithArtifact(draft, source)
	}
	if optionalPayload {
		wire = `{"status":"success","summary":"reviewed without structured payload"}`
	}
	if failure == "resume_commit_failure" {
		wire = `{"status":"success","summary":"reviewed","assumption_checks":[{"assumption_id":"A1","status":"supported"}],"structured_payload":` + draft + `}`
	}
	worker, _ := normalizationStreamedSubmission(f.c, item.ID, wire, sink)
	tool := f.c.gatePolicyTools([]fantasy.AgentTool{&submitResultTool{coordinator: f.c, todoID: item.ID, sink: interruptedProcessSink{}}})[0]
	scripted := scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		attempt := isolatedAttemptFromContext(ctx)
		if attempt == nil {
			return nil, errors.New("missing isolated attempt")
		}
		if failure != "empty_apply_done_write_failure" {
			if err := os.WriteFile(filepath.Join(attempt.prepared.Root, relPath), []byte(edited), 0o600); err != nil {
				return nil, err
			}
		}
		if abruptVerification {
			// The process will stop after entering verification. Unwinding
			// this test's injected interruption must keep the prepared world.
			retainIsolatedNormalization(attempt, true)
		}
		if abrupt {
			wire := normalizationWire(approvedNormalizationDraft)
			const callID = "interrupted-isolated-submission"
			if err := call.OnToolCall(fantasy.ToolCallContent{ToolCallID: callID, ToolName: submitResultToolName, Input: wire}); err != nil {
				return nil, err
			}
			// Simulate process death, when the task's cleanup does not run.
			defer retainIsolatedNormalization(attempt, true)
			_, err := tool.Run(ctx, fantasy.ToolCall{ID: callID, Name: submitResultToolName, Input: wire})
			return nil, err
		}
		return worker.Stream(ctx, call)
	}})
	f.c.workerAgentOverride = scripted
	if acknowledged {
		scripted = stopAfterNormalizedSubmission(t, f.c, item.ID, wire, func(attempt *isolatedAttempt) error {
			if attempt == nil {
				return errors.New("missing isolated attempt")
			}
			return os.WriteFile(filepath.Join(attempt.prepared.Root, relPath), []byte(edited), 0o600)
		})
		f.c.workerAgentOverride = scripted
	}
	var correctionSink TaskResultSink
	if failure == "returned_correction" {
		correctionSink = interruptedResultSink{}
	}
	repair, correctionModel := normalizationStreamedSubmission(f.c, item.ID, normalizationWire(approvedNormalizationDraft), correctionSink)
	f.c.repairAgentOverride = repair
	runCtx := withTestProtocolRepairInvocationContext(t.Context())
	if writeFault != "" || projectionWriteFault || verificationWriteFault {
		trigger := "CREATE TRIGGER refuse_normalized_checkpoint BEFORE INSERT ON events WHEN NEW.type='" + writeFault + "'"
		if projectionWriteFault {
			trigger = "CREATE TRIGGER refuse_normalized_checkpoint BEFORE UPDATE ON runtime_documents WHEN NEW.name LIKE 'normalized-completion/%' AND json_extract(NEW.body, '$.delta_sha256') IS NOT NULL"
		}
		if verificationWriteFault {
			trigger = verificationProjectionFaultTrigger
		}
		execJournal(t, f.c.session.Workspace, trigger+" BEGIN SELECT RAISE(ABORT, 'fixture checkpoint write failure'); END")
		_, err := f.c.executeTask(runCtx, taskDefFromTodoItem(item), item.ID)
		if err == nil || !strings.Contains(err.Error(), "fixture checkpoint write failure") || !isNormalizedVerificationPending(todoByID(t, f.c, item.ID)) {
			t.Fatalf("checkpoint write did not leave its committed result active: %v", err)
		}
		if !projectionWriteFault && failure != "apply_finish_write_failure" {
			execJournal(t, f.c.session.Workspace, `DROP TRIGGER refuse_normalized_checkpoint`)
		}
	} else if acknowledged {
		interruptAfterNormalizedSubmission(t, f.c, item)
	} else if abruptVerification {
		stopBeforeObjectiveVerification(t, f.c, item)
	} else if abrupt {
		func() {
			defer func() {
				if got := recover(); got != (interruptedProcessSink{}) {
					t.Fatalf("expected interruption at isolated result commit, got %v", got)
				}
			}()
			_, _ = f.c.executeTask(runCtx, taskDefFromTodoItem(item), item.ID)
		}()
	} else {
		_, err := f.c.executeTask(runCtx, taskDefFromTodoItem(item), item.ID)
		interrupted := todoByID(t, f.c, item.ID)
		if localResultRejectionReason(err) != RepairFailureCommit || interrupted.Status != TaskProtocolIncomplete || !canResumeDirectNormalization(interrupted) {
			t.Fatalf("returned submission lost its interrupted receipt: %v", err)
		}
	}
	beforeResume := original
	if failure == "apply_finish_write_failure" || failure == "forged_already_applied" {
		beforeResume = edited
	}
	if f.read(t, relPath) != beforeResume {
		t.Fatal("isolated changes reached the canonical project at the wrong checkpoint")
	}
	if failure == "apply_start_conflict" {
		f.write(t, relPath, "independent project edit")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	checkpoint := normalizationResumeProjection(t, f.c)
	if checkpoint == nil {
		t.Fatal("isolated repair checkpoint unavailable")
	}
	reader, journal := restartDirectNormalizationRun(t, f.c, checkpoint, item, func(context.Context, []byte) ([]byte, error) {
		t.Error("isolated continuation repeated normalization")
		return nil, errors.New("unexpected normalizer call")
	})
	forbidden := scriptWorker(failTurn(errors.New("unexpected model call")))
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
	if countVerification {
		installCompletionSkeptic(reader, &skepticCalls)
	}
	ctx := WithSessionResume(t.Context())
	persisted := todoByID(t, reader, item.ID)
	receipt, corrections := persisted.ExecutionReceipt, 0
	if acknowledged {
		var err error
		receipt, err = reader.readAcceptedNormalizedSubmission(persisted)
		if err != nil || receipt == nil || persisted.Status != TaskInProgress {
			t.Fatalf("isolated acknowledgment did not preserve its original dispatch: status=%s error=%v", persisted.Status, err)
		}
	}
	if corrected {
		corrections = 1
	}
	if abrupt {
		var err error
		receipt, err = reader.loadInterruptedNormalizationReceipt(ctx, persisted)
		if err != nil || receipt == nil {
			live := todoByID(t, f.c, item.ID)
			t.Fatalf("the isolated dispatch lost its sealed checkpoint: replay=%s/%d/%s live=%s/%d/%s error=%v", persisted.Status, persisted.OccurrenceRevision, persisted.DispatchID, live.Status, live.OccurrenceRevision, live.DispatchID, err)
		}
	}
	if failure == "missing_apply_projection" {
		assertDirectApplyRequiresOriginalIntent(t, reader, journal, persisted, forbidden, &skepticCalls, f, relPath, original)
		return
	}
	if failure == "missing_pending_apply_manifest" {
		assertMissingPendingDirectApply(t, reader, journal, persisted, forbidden, &skepticCalls, f, relPath, original)
		return
	}
	if verificationWriteFault {
		assertDirectVerificationWorkspaceResume(t, reader, persisted, failure == "changed_world_after_verification_fault", forbidden, &skepticCalls, f, relPath, original, edited)
		return
	}
	if projectionWriteFault {
		_, err := reader.ResumeInterruptedTasks(ctx)
		if localResultRejectionReason(err) != RepairFailureCommit || !strings.Contains(err.Error(), "fixture checkpoint write failure") ||
			skepticCalls.Load() != 1 || forbidden.Calls() != 0 || f.read(t, relPath) != original {
			t.Fatalf("second projection fault did not retain the first prepared intent: %v", err)
		}
		execJournal(t, reader.session.Workspace, "DROP TRIGGER refuse_normalized_checkpoint")
		reader, journal = restartNormalizedIsolatedReader(t, reader, journal, item, forbidden)
		installCompletionSkeptic(reader, &skepticCalls)
	}
	if strings.HasPrefix(failure, "forged_") || failure == "missing_apply_artifact" {
		appendDivergentNormalizedApply(t, reader, persisted, failure)
		reader, _ = restartNormalizedIsolatedReader(t, reader, journal, item, forbidden)
		err := reader.reconcileAttemptWorlds(ctx)
		blocked := todoByID(t, reader, item.ID)
		if localResultRejectionReason(err) != RepairFailureSource || blocked.Status != TaskBlocked || forbidden.Calls() != 0 ||
			f.read(t, relPath) != beforeResume || blocked.TypedResult.Summary != persisted.TypedResult.Summary {
			t.Fatalf("divergent apply changed the project or accepted result: status=%s error=%v", blocked.Status, err)
		}
		if _, err := os.Stat(reader.attemptWorldDirFor(receipt.ExecutionWorldID)); err != nil {
			t.Fatalf("divergent apply lost its original world: %v", err)
		}
		return
	}
	reconcileErr := reader.reconcileAttemptWorlds(ctx)
	if failure == "apply_finish_write_failure" {
		if localResultRejectionReason(reconcileErr) != RepairFailureCommit || !isNormalizedVerificationPending(todoByID(t, reader, item.ID)) {
			t.Fatalf("recovery blocked after another apply-finish write fault: %v", reconcileErr)
		}
		execJournal(t, reader.session.Workspace, `DROP TRIGGER refuse_normalized_checkpoint`)
		reader, journal = restartNormalizedIsolatedReader(t, reader, journal, item, forbidden)
		if err := reader.reconcileAttemptWorlds(ctx); err != nil {
			t.Fatal(err)
		}
	} else if reconcileErr != nil {
		t.Fatal(reconcileErr)
	}
	if failure == "missing_verification_evidence" {
		removeNormalizedVerificationTranscript(t, reader, receipt, false)
		count, err := reader.ResumeInterruptedTasks(ctx)
		blocked := todoByID(t, reader, item.ID)
		if count != 1 || localResultRejectionReason(err) != RepairFailureSource || blocked.Status != TaskBlocked ||
			blocked.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureSource || f.read(t, relPath) != original || forbidden.Calls() != 0 {
			t.Fatalf("missing verification evidence did not durably block: count=%d receipt=%+v error=%v", count, blocked.ExecutionReceipt, err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		replayed := todoItemByID(normalizationResumeProjection(t, reader).Tasks, item.ID)
		if replayed == nil || replayed.Status != TaskBlocked || replayed.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureSource {
			t.Fatalf("replay lost source rejection: %+v", replayed)
		}
		return
	}
	if failure == "resume_commit_failure" || failure == "resume_verification_read_failure" {
		reader, journal = interruptNormalizedLocalCompletion(t, ctx, reader, journal, persisted, forbidden, failure == "resume_commit_failure")
		if f.read(t, relPath) != original {
			t.Fatal("local storage failure applied the isolated edit")
		}
	}
	if failure == "resume_world_read_failure" {
		world := reader.attemptWorldDirFor(receipt.ExecutionWorldID)
		restore := faultNormalizedEvidencePath(t, filepath.Join(world, attemptWorldBaselineFile), true)
		count, err := reader.ResumeInterruptedTasks(ctx)
		if count != 1 || localResultRejectionReason(err) != RepairFailureCommit || !isNormalizedVerificationPending(todoByID(t, reader, item.ID)) {
			t.Fatalf("temporary world fault settled the task: count=%d error=%v", count, err)
		}
		restore()
		reader, journal = restartNormalizedIsolatedReader(t, reader, journal, item, forbidden)
	}
	count, err := reader.ResumeInterruptedTasks(ctx)
	wantCount := 1
	if applyPending {
		wantCount = 0
	}
	if failure == "apply_start_conflict" {
		done := todoByID(t, reader, item.ID)
		if err == nil || count != wantCount || done.Status != TaskBlocked || f.read(t, relPath) != "independent project edit" || forbidden.Calls() != 0 {
			t.Fatalf("resumed apply overwrote or retried a genuine conflict: count=%d status=%s error=%v", count, done.Status, err)
		}
		return
	}
	wantFile := edited
	if failure == "empty_apply_done_write_failure" {
		wantFile = original
	}
	if err != nil || count != wantCount || scripted.Calls() != 1 || correctionModel.calls != corrections || f.read(t, relPath) != wantFile {
		t.Fatalf("isolated repair continuation: count=%d worker calls=%d error=%v", count, scripted.Calls(), err)
	}
	if countVerification {
		if got := f.read(t, "verifier.log"); got != "verified\n" {
			t.Fatalf("prepared delta contains repeated objective effects: %q", got)
		}
		if got := skepticCalls.Load(); got != 1 {
			t.Fatalf("adversarial verification ran %d times", got)
		}
	}
	if committed {
		done := todoByID(t, reader, item.ID)
		wantActions := 1 + corrections
		if optionalPayload {
			wantActions = 0
		}
		if receipt == nil || actionCalls != wantActions || done.Status != TaskDone || done.ExecutionReceipt == nil || done.ExecutionReceipt.ExecutionWorldID == "" ||
			done.ExecutionReceipt.ExecutionWorldID != receipt.ExecutionWorldID || forbidden.Calls() != 0 ||
			corrected != (done.ExecutionReceipt.RepairProvenance != nil && done.ExecutionReceipt.RepairProvenance.Success) {
			t.Fatalf("ordinary verification lost original workspace receipt: %+v, normalizer=%d, model=%d", done.ExecutionReceipt, actionCalls, forbidden.Calls())
		}
		return
	}
	assertDirectNormalizationContinuation(t, reader, journal, item.ID, forbidden, actionCalls, receipt.InterruptedResultNormalization, "", corrections, false)
}

func installCompletionSkeptic(c *Coordinator, calls *atomic.Int32) {
	s := &sidecar.Sidecar{}
	s.SetDecisionExecutor(func(context.Context, sidecar.DecisionInvocation) (decisionrt.Result, error) {
		calls.Add(1)
		return decisionrt.Result{Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "not_refuted"}}, nil
	})
	c.SetAgentPool(&mockAgentPool{sidecar: s, resolveDef: c.session.Agents["worker"], resolveKey: "worker"})
}

func isolatedNormalizationWriteFault(failure string) (eventType string, pendingApply bool) {
	switch failure {
	case "verifying_write_failure", "corrected_verification_write_failure":
		return string(EventTaskVerifying), false
	case "apply_start_write_failure", "missing_apply_projection", "apply_start_conflict", "forged_apply_started", "forged_delta_manifest", "missing_apply_artifact":
		return string(EventAttemptWorkspaceApplyStarted), false
	case "apply_finish_write_failure":
		return string(EventAttemptWorkspaceApplied), true
	case "empty_apply_done_write_failure", "forged_already_applied":
		return string(EventTaskCompleted), true
	default:
		return "", false
	}
}

func removeNormalizedVerificationTranscript(t *testing.T, c *Coordinator, receipt *ExecutionReceipt, temporary bool) func() {
	t.Helper()
	path := filepath.Join(runtimestore.ArtifactRoot(c.artifactStoreRootPath()), "data", receipt.TranscriptRef)
	return faultNormalizedEvidencePath(t, path, temporary)
}

func faultNormalizedEvidencePath(t *testing.T, path string, temporary bool) func() {
	t.Helper()
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if temporary {
		if err := os.Symlink(filepath.Base(path), path); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		if err := os.Rename(path+".saved", path); err != nil {
			t.Fatal(err)
		}
	}
}

func restartNormalizedIsolatedReader(t *testing.T, reader *Coordinator, journal *EventStore, item *TodoItem, forbidden *scriptedWorker) (*Coordinator, *EventStore) {
	t.Helper()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	checkpoint := normalizationResumeProjection(t, reader)
	second, secondJournal := restartDirectNormalizationRun(t, reader, checkpoint, item, func(context.Context, []byte) ([]byte, error) {
		t.Error("second continuation repeated normalization")
		return nil, errors.New("unexpected normalizer call")
	})
	second.workerAgentOverride, second.repairAgentOverride = forbidden, forbidden
	return second, secondJournal
}

func interruptNormalizedLocalCompletion(t *testing.T, ctx context.Context, reader *Coordinator, journal *EventStore, item *TodoItem, forbidden *scriptedWorker, commit bool) (*Coordinator, *EventStore) {
	t.Helper()
	var restore func()
	if commit {
		armNormalizationAssumption(t, reader, item, true)
		execJournal(t, reader.session.Workspace, `CREATE TRIGGER refuse_assumption BEFORE INSERT ON events WHEN NEW.type='assumption_supported' BEGIN SELECT RAISE(ABORT, 'fixture commit write failure'); END`)
		restore = func() { execJournal(t, reader.session.Workspace, `DROP TRIGGER refuse_assumption`) }
	} else {
		restore = removeNormalizedVerificationTranscript(t, reader, item.ExecutionReceipt, true)
	}
	count, err := reader.ResumeInterruptedTasks(ctx)
	active := todoByID(t, reader, item.ID)
	if count != 1 || localResultRejectionReason(err) != RepairFailureCommit || active.Status != item.Status || forbidden.Calls() != 0 ||
		commit && !strings.Contains(err.Error(), "fixture commit write failure") {
		t.Fatalf("temporary local completion failed to suspend: count=%d status=%s error=%v", count, active.Status, err)
	}
	restore()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	checkpoint := normalizationResumeProjection(t, reader)
	replayed := todoItemByID(checkpoint.Tasks, item.ID)
	if replayed == nil || replayed.Status != item.Status || replayed.ExecutionReceipt == nil ||
		replayed.ExecutionReceipt.InterruptedResultNormalization != item.ExecutionReceipt.InterruptedResultNormalization ||
		replayed.ExecutionReceipt.ExecutionWorldID != item.ExecutionReceipt.ExecutionWorldID ||
		replayed.ExecutionReceipt.TranscriptRef != item.ExecutionReceipt.TranscriptRef {
		t.Fatalf("temporary local failure changed the original proof: %+v", replayed)
	}
	second, secondJournal := restartDirectNormalizationRun(t, reader, checkpoint, item, func(context.Context, []byte) ([]byte, error) {
		t.Error("second continuation repeated normalization")
		return nil, errors.New("unexpected normalizer call")
	})
	second.workerAgentOverride, second.repairAgentOverride = forbidden, forbidden
	if commit {
		armNormalizationAssumption(t, second, replayed, false)
	}
	if err := second.reconcileAttemptWorlds(ctx); err != nil {
		t.Fatal(err)
	}
	return second, secondJournal
}

func armNormalizationAssumption(t *testing.T, c *Coordinator, item *TodoItem, publish bool, ids ...string) {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{"A1"}
	}
	record := &DecisionRecord{SchemaVersion: 1, ID: "normalization-commit-decision", RunID: c.executionRunID,
		TaskID: item.ID, Profile: "standard", FinalOption: "approve"}
	for _, id := range ids {
		record.Assumptions = append(record.Assumptions, DecisionAssumption{ID: id, Statement: "declared check"})
	}
	if publish {
		journal, err := c.decisionJournalFor()
		if err != nil {
			t.Fatal(err)
		}
		if err := appendDecisionEvent(t.Context(), journal, agent.EventDecisionFinalized, decisionEvent{
			DecisionID: record.ID, RunID: record.RunID, TaskID: item.ID, Attempt: 1, Profile: record.Profile,
			Record: record, Question: "Accept the normalized result?",
		}); err != nil {
			t.Fatal(err)
		}
	}
	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{CheckpointEvery: 1}, ReplanPolicy{})
	if err := c.armDiscipline(t.Context(), item.ID, taskDefFromTodoItem(item), policy, record); err != nil {
		t.Fatal(err)
	}
}
