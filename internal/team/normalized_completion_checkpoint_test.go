package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestTargetedRecoveryPreservesSealedNormalization(t *testing.T) {
	c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
		spec.Verify = "true"
	})
	stopBeforeDirectNormalizationReceipt(t, t.Context(), c, item, normalizationWire(approvedNormalizationDraft), false)
	forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
	reader, _ := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
	for _, action := range []TargetedRecoveryAction{TargetedRecoveryReconcile, TargetedRecoveryRetry} {
		report, err := reader.runTargetedRecovery(WithSessionResume(t.Context()), item.ID, action)
		if err == nil || !strings.Contains(err.Error(), "normalized result settlement") || report.Status != TaskInProgress || forbidden.Calls() != 0 {
			t.Fatalf("%s bypassed the original incomplete dispatch: report=%+v calls=%d error=%v", action, report, forbidden.Calls(), err)
		}
	}
	count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	if err != nil || count != 1 || todoByID(t, reader, item.ID).Status != TaskDone || forbidden.Calls() != 0 {
		t.Fatalf("ordinary resume did not settle original work: count=%d calls=%d error=%v", count, forbidden.Calls(), err)
	}
}

func TestNormalizedVerificationRejectsDivergentJournalResult(t *testing.T) {
	for _, field := range []string{"envelope", "normalized_payload_and_receipt", "normalizer_occurrence", "other_sealed_stage_after_correction"} {
		t.Run(field, func(t *testing.T) {
			c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
				spec.Verify = "test -d . && printf ran > verified.txt"
			})
			c.projectDir = t.TempDir()
			draft := approvedNormalizationDraft
			otherOccurrence := ""
			if field == "other_sealed_stage_after_correction" {
				other := admitDirectNormalizationTask(t, c, echoNormalization, nil)
				c.workerAgentOverride, _ = normalizationStreamedSubmission(c, other.ID, normalizationWire(draft), nil)
				if _, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(other), other.ID); err != nil {
					t.Fatal(err)
				}
				otherOccurrence = todoByID(t, c, other.ID).TypedResult.StructuredPayload.Normalization.OccurrenceSHA256
				c.repairAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizationWire(draft), nil)
				draft = rejectedNormalizationDraft
			}
			c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizationWire(draft), nil)
			stopBeforeObjectiveVerification(t, c, item)
			live := todoByID(t, c, item.ID)
			result := cloneTaskResult(live.TypedResult)
			receipt := cloneExecutionReceipt(live.ExecutionReceipt)
			switch field {
			case "envelope":
				result.Summary = "changed result summary"
			case "normalized_payload_and_receipt":
				_, compiled, err := c.boundResultContract(item.ID)
				if err != nil {
					t.Fatal(err)
				}
				replacement, err := validateStructuredResultPayload(compiled, *live.ResultContract, []byte(`{"verdict":"reject","findings":[{"summary":"different valid finding"}]}`))
				if err != nil {
					t.Fatalf("replacement must be schema valid: %v", err)
				}
				result.StructuredPayload.Value = replacement.Value
				result.StructuredPayload.SHA256 = replacement.SHA256
				receipt.ResultPayloadSHA256 = replacement.SHA256
				receipt.SubmittedResult = result
			case "normalizer_occurrence":
				// Overwrite the envelope in both projections, but leave all
				// payload hashes and the independently bound occurrence intact.
				result.StructuredPayload.Normalization.OccurrenceSHA256 = sha256HexBytes([]byte("different occurrence"))
				receipt.SubmittedResult = result
			case "other_sealed_stage_after_correction":
				if receipt.RepairProvenance == nil || !receipt.RepairProvenance.Success {
					t.Fatal("fixture did not checkpoint a successful correction")
				}
				result.StructuredPayload.Normalization.OccurrenceSHA256 = otherOccurrence
				receipt.ResultNormalizerOccurrenceSHA256 = otherOccurrence
				receipt.SubmittedResult, receipt.RepairProvenance.SubmittedResult = result, result
			}
			if err := c.emitEvent(string(EventTaskVerifying), live.Agent, item.ID, map[string]any{
				"id": item.ID, "status": TaskVerifying, "typed_result": result, "execution_receipt": &receipt,
			}); err != nil {
				t.Fatal(err)
			}
			forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
			reader, _ := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
			count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			blocked := todoByID(t, reader, item.ID)
			if count != 1 || localResultRejectionReason(err) != RepairFailureSource || blocked.Status != TaskBlocked ||
				blocked.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureSource || forbidden.Calls() != 0 {
				t.Fatalf("divergent verification result was admitted: count=%d status=%s calls=%d error=%v", count, blocked.Status, forbidden.Calls(), err)
			}
			if field == "normalized_payload_and_receipt" && !strings.Contains(err.Error(), "sealed verification publication") {
				t.Fatalf("verification did not consult the independently sealed publication: %v", err)
			}
			if _, err := os.Stat(filepath.Join(c.projectDir, "verified.txt")); !os.IsNotExist(err) {
				t.Fatalf("objective verifier ran with divergent result: %v", err)
			}
		})
	}
}

func TestAcceptedNormalizedSubmissionResumesAfterAcknowledgment(t *testing.T) {
	for _, name := range []string{"optional", "structured"} {
		t.Run(name, func(t *testing.T) {
			structured := name == "structured"
			c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
				spec.ResultContract.RequireStructured = structured
				spec.OutputMode = TaskOutputModeVerbatim
				spec.VerifySpec = &VerificationSpec{Type: VerifyToolCallAssert, ToolCallAssertions: []ToolCallAssertion{
					{Tool: submitResultToolName, ResultContains: submitResultSuccessText},
				}}
			})
			wire := `{"status":"success","summary":"optional result accepted"}`
			if structured {
				wire = normalizationWire(approvedNormalizationDraft)
			}
			c.workerAgentOverride = stopAfterNormalizedSubmission(t, c, item.ID, wire, nil)
			interruptAfterNormalizedSubmission(t, c, item)
			forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
			reader, _ := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
			persisted := todoByID(t, reader, item.ID)
			receipt, err := reader.readAcceptedNormalizedSubmission(persisted)
			if persisted.Status != TaskInProgress || err != nil || receipt == nil || (receipt.SubmittedResult.StructuredPayload != nil) != structured {
				t.Fatalf("acknowledged submission lost its in-progress handoff: status=%s error=%v", persisted.Status, err)
			}
			count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			done := todoByID(t, reader, item.ID)
			if err != nil || count != 1 || done.Status != TaskDone || done.VerifyResult == nil || forbidden.Calls() != 0 ||
				done.ExecutionReceipt.TranscriptRef != receipt.TranscriptRef || done.TypedResult.Summary != receipt.SubmittedResult.Summary {
				t.Fatalf("acknowledged submission failed to resume: count=%d status=%s calls=%d error=%v", count, done.Status, forbidden.Calls(), err)
			}
		})
	}
}

func TestAcceptedNormalizedNonSuccessResumesAfterAcknowledgment(t *testing.T) {
	for _, resultStatus := range []string{TaskResultStatusBlocked, TaskResultStatusPartial, TaskResultStatusFailed} {
		t.Run(resultStatus, func(t *testing.T) {
			c, journal, item := newDirectNormalizationRun(t, echoNormalization, nil)
			wire := strings.Replace(normalizationWire(approvedNormalizationDraft), `"status":"success"`, `"status":"`+resultStatus+`"`, 1)
			c.workerAgentOverride = stopAfterNormalizedSubmission(t, c, item.ID, wire, nil)
			interruptAfterNormalizedSubmission(t, c, item)
			forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
			reader, _ := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
			receipt, err := reader.readAcceptedNormalizedSubmission(todoByID(t, reader, item.ID))
			if err != nil || receipt == nil || receipt.SubmittedResult.Status != resultStatus {
				t.Fatalf("accepted status missing from original submission: %v", err)
			}
			want := TaskError
			if resultStatus == TaskResultStatusBlocked {
				want = TaskBlocked
			}
			count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			got := todoByID(t, reader, item.ID)
			if err != nil || count != 1 || got.Status != want || forbidden.Calls() != 0 || got.VerifyResult != nil ||
				got.ExecutionReceipt == nil || got.ExecutionReceipt.ExitCode == nil || *got.ExecutionReceipt.ExitCode != 1 ||
				got.ExecutionReceipt.TranscriptRef != receipt.TranscriptRef || got.ExecutionReceipt.ResultPayloadSHA256 != receipt.ResultPayloadSHA256 ||
				!sameCanonicalEvidence(got.TypedResult, receipt.SubmittedResult) {
				t.Fatalf("accepted result did not settle as %s: count=%d status=%s calls=%d error=%v", want, count, got.Status, forbidden.Calls(), err)
			}
		})
	}
}

func TestNormalizedPublicationsRedactModelTextBeforeRestart(t *testing.T) {
	for _, boundary := range []string{"acknowledgment", "verification"} {
		t.Run(boundary, func(t *testing.T) {
			c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
				spec.ResultContract.RequireStructured = false
				spec.Verify = "true"
			})
			secret := "sk-proj-" + strings.Repeat("Q", 40)
			wire := `{"status":"success","summary":"model summary ` + secret + `","details":"model details ` + secret + `"}`
			if boundary == "acknowledgment" {
				c.workerAgentOverride = stopAfterNormalizedSubmission(t, c, item.ID, wire, nil)
				interruptAfterNormalizedSubmission(t, c, item)
			} else {
				c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, wire, nil)
				stopBeforeObjectiveVerification(t, c, item)
			}
			got := todoByID(t, c, item.ID)
			identity := submitResultRuntimeIdentity{TaskID: item.ID, OccurrenceRevision: got.OccurrenceRevision, DispatchID: got.DispatchID}
			if boundary == "verification" {
				var ok bool
				identity, ok = normalizationWorkEvidenceIdentity(got)
				if !ok {
					t.Fatal("missing original verification identity")
				}
			}
			keys := []string{acceptedNormalizedSubmissionKey(c.activeBranchID(), identity)}
			if boundary == "verification" {
				keys = append(keys, c.normalizedVerificationPublicationKey(identity))
			}
			for _, key := range keys {
				body, found, err := workspaceDocs(c.session.Workspace).getDocument(key)
				if err != nil || !found || strings.Contains(string(body), secret) || !strings.Contains(string(body), "[REDACTED]") {
					t.Fatalf("publication did not preserve redacted model text: found=%v error=%v", found, err)
				}
			}
			forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
			reader, _ := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
			count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			done := todoByID(t, reader, item.ID)
			if err != nil || count != 1 || done.Status != TaskDone || done.VerifyResult == nil || forbidden.Calls() != 0 ||
				done.TypedResult.Summary != "model summary [REDACTED]" || done.TypedResult.Details != "model details [REDACTED]" {
				t.Fatalf("redacted publication is not replayable: count=%d status=%s calls=%d error=%v", count, done.Status, forbidden.Calls(), err)
			}
		})
	}
}

type interruptedAfterNormalizedSubmission struct{}

func stopAfterNormalizedSubmission(t *testing.T, c *Coordinator, taskID, wire string, prepare func(*isolatedAttempt) error) *scriptedWorker {
	t.Helper()
	worker, _ := normalizationStreamedSubmission(c, taskID, wire, nil)
	return scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		world := isolatedAttemptFromContext(ctx)
		if prepare != nil {
			if err := prepare(world); err != nil {
				return nil, err
			}
		}
		result, err := worker.Stream(ctx, call)
		if err != nil || c.GetTaskResult(taskID) == nil {
			return result, errors.Join(err, errors.New("worker did not receive an accepted normalized submission"))
		}
		retainIsolatedNormalization(world, true)
		panic(interruptedAfterNormalizedSubmission{})
	}})
}

func interruptAfterNormalizedSubmission(t *testing.T, c *Coordinator, item *TodoItem) {
	t.Helper()
	defer func() {
		if got := recover(); got != (interruptedAfterNormalizedSubmission{}) {
			t.Fatalf("expected interruption at the acknowledged result, got %v", got)
		}
	}()
	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(item), item.ID)
	t.Logf("execution returned before acknowledgment barrier: %v", err)
}

func TestNormalizedApplyRejectsMissingAcceptedReceipt(t *testing.T) {
	for _, applied := range []bool{false, true} {
		name := "applying"
		if applied {
			name = "applied"
		}
		t.Run(name, func(t *testing.T) {
			f := newIsolatedTeamFixture(t, nil)
			f.c.SetSessionData(&SessionData{})
			const path = "subject.txt"
			f.write(t, path, "original")
			item := admitDirectNormalizationTask(t, f.c, echoNormalization, func(spec *TodoSpec) {
				spec.SideEffect = SideEffectWorkspaceWrite
				policy, err := f.c.admittedWorkerWorkspace(TaskDef{SideEffect: spec.SideEffect}, f.c.session.Agents[spec.Agent])
				if err != nil {
					t.Fatal(err)
				}
				spec.WorkerWorkspace = policy
			})
			var delta *AttemptWorkspaceDelta
			f.c.workerAgentOverride = stopAfterNormalizedSubmission(t, f.c, item.ID, normalizationWire(approvedNormalizationDraft), func(world *isolatedAttempt) error {
				if err := os.WriteFile(filepath.Join(world.prepared.Root, path), []byte("proposed change"), 0o600); err != nil {
					return err
				}
				var err error
				delta, err = world.world.Delta(t.Context(), world.prepared)
				if err != nil {
					return err
				}
				return writeAttemptWorldDelta(world.worldDir(), delta)
			})
			interruptAfterNormalizedSubmission(t, f.c, item)
			forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
			reader, _ := restartNormalizedIsolatedReader(t, f.c, f.store, item, forbidden)
			persisted := todoByID(t, reader, item.ID)
			receipt, err := reader.readAcceptedNormalizedSubmission(persisted)
			if err != nil || receipt == nil || persisted.Status != TaskInProgress || persisted.ExecutionReceipt != nil || delta == nil {
				t.Fatalf("expected isolated work publication before lifecycle acceptance: %+v error=%v", persisted, err)
			}
			appendNormalizedApplyFixture(t, reader, persisted.ID, receipt.Attempt, EventAttemptWorkspaceApplyStarted, map[string]any{
				"world_id": receipt.ExecutionWorldID, "delta_digest": delta.Digest, "execution_receipt": receipt,
				"typed_result": receipt.SubmittedResult, "coordinator_output": receipt.SubmittedResult.Summary,
			})
			if applied {
				appendNormalizedApplyFixture(t, reader, persisted.ID, receipt.Attempt, EventAttemptWorkspaceApplied, map[string]any{
					"world_id": receipt.ExecutionWorldID, "delta_digest": delta.Digest,
				})
			}
			err = reader.reconcileAttemptWorlds(WithSessionResume(t.Context()))
			blocked := todoByID(t, reader, item.ID)
			if localResultRejectionReason(err) != RepairFailureSource || blocked.Status != TaskBlocked || forbidden.Calls() != 0 || f.read(t, path) != "original" {
				t.Fatalf("unproven apply was admitted: status=%s calls=%d error=%v", blocked.Status, forbidden.Calls(), err)
			}
			if _, err := os.Stat(reader.attemptWorldDirFor(receipt.ExecutionWorldID)); err != nil {
				t.Fatalf("unproven apply did not retain its world: %v", err)
			}
		})
	}
}

func appendNormalizedApplyFixture(t *testing.T, c *Coordinator, taskID string, attempt int, kind EventType, fields map[string]any) {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(kind), TaskID: taskID, Attempt: attempt,
		Actor: attemptWorldEventActor, IdempotencyKey: "fixture-alternate-" + string(kind), Payload: body}); err != nil {
		t.Fatal(err)
	}
}

func TestResultCollectionReconcilesIsolatedNormalizedApply(t *testing.T) {
	for _, name := range []string{"verified_apply", "missing_apply_projection", "before_apply_start", "prepared_terminal_failure", "workspace_conflict", "applying_conflict", "applying_io_failure", "substituted_applying_event", "substituted_applied_event", "substituted_delta_manifest", "missing_artifact", "transient_artifact_read"} {
		t.Run(name, func(t *testing.T) { testIsolatedNormalizedCollectionApply(t, name) })
	}
}

func testIsolatedNormalizedCollectionApply(t *testing.T, name string) {
	t.Helper()
	compiled, err := CompileResultContractDocument("findings.json", []byte(collectedFindingSchema))
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	var normalizations int
	c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
		spec.ResultContract = &ref
		mode := isolatedSpec()
		spec.WorkerWorkspace = &WorkerWorkspacePolicy{Mode: mode.Mode, EffectiveMode: mode.Mode, Integrate: mode.Integrate}
	}, func(c *Coordinator) {
		c.session.Scope = WorkspaceScope{Managed: true, ControlRoot: c.session.Workspace, SubjectRoot: c.projectDir}
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: func(_ context.Context, draft []byte) ([]byte, error) {
			normalizations++
			return draft, nil
		}})
	})
	task := taskDefFromTodoItem(item)
	world, err := c.prepareIsolatedAttempt(ctx, task, item.ID, receipt.Attempt)
	if err != nil || world == nil {
		t.Fatalf("prepare isolated collection work: %v", err)
	}
	defer c.closeIsolatedAttempt(ctx, world, "fixture cleanup")
	const path = "collected.txt"
	if err := os.WriteFile(filepath.Join(world.prepared.Root, path), []byte("verified collection"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt.ExecutionWorldID = world.worldID()
	const artifactPath = "collection-observation.txt"
	if err := os.WriteFile(filepath.Join(c.session.Workspace, artifactPath), []byte("accepted artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "original work report")
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.reserveResultCollection(ctx, cp, "collector", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":"success","summary":"collected result","structured_payload":{"findings":[]},"artifacts":[{"path":"`+artifactPath+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.prepareCollectionClaims(ctx, cp)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.commitPreparedCollection(ctx, cp); err != nil {
		t.Fatal(err)
	}
	accepted := todoByID(t, c, item.ID)
	cp = currentResultCollection(accepted)
	if cp == nil || cp.State != ResultCollectionCommitted || normalizations != 1 || len(accepted.TypedResult.Artifacts) != 1 {
		t.Fatalf("fixture did not commit a normalized collection: %+v calls=%d", cp, normalizations)
	}
	var restore func()
	if name == "verified_apply" {
		if err := c.integrateIsolatedAttempt(ctx, world, isolatedApplyEvidence{TypedResult: accepted.TypedResult,
			Receipt: accepted.ExecutionReceipt, CoordinatorOutput: accepted.TypedResult.Summary}); err != nil {
			t.Fatal(err)
		}
	} else {
		started := attemptApplyStartedPayload{WorldID: world.worldID(), TypedResult: accepted.TypedResult,
			ExecutionReceipt: accepted.ExecutionReceipt, CoordinatorOutput: accepted.TypedResult.Summary}
		_, err := c.prepareCollectedApply(ctx, world, accepted, cp, &started)
		if err != nil {
			t.Fatal(err)
		}
		if name == "substituted_delta_manifest" {
			substitutePreparedFinalDigest(t, world.worldDir())
		}
		if name == "substituted_applying_event" || name == "substituted_applied_event" {
			started.CoordinatorOutput = "unverified output"
		}
		if name == "missing_apply_projection" || name == "before_apply_start" || name == "workspace_conflict" || name == "prepared_terminal_failure" {
			c.terminalizeTaskErrorIfUnresolved(item.ID, rejectLocalResultPersistence(errors.New("fixture apply event write fault")))
			if status := todoByID(t, c, item.ID).Status; status != accepted.Status {
				t.Fatalf("scheduler terminalized a recoverable verified apply: %s", status)
			}
			if name == "workspace_conflict" {
				if err := os.WriteFile(filepath.Join(c.projectDir, path), []byte("new user work"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			appendNormalizedApplyFixture(t, c, item.ID, receipt.Attempt, EventAttemptWorkspaceApplyStarted, map[string]any{
				"world_id": started.WorldID, "delta_digest": started.DeltaDigest, "typed_result": started.TypedResult,
				"execution_receipt": started.ExecutionReceipt, "verify_result": started.VerifyResult, "coordinator_output": started.CoordinatorOutput,
			})
		}
		if name == "applying_conflict" {
			if err := os.WriteFile(filepath.Join(c.projectDir, path), []byte("new user work"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if name == "applying_io_failure" {
			restore = faultNormalizedEvidencePath(t, c.projectDir, true)
		}
		if name == "substituted_applied_event" {
			appendNormalizedApplyFixture(t, c, item.ID, receipt.Attempt, EventAttemptWorkspaceApplied, map[string]any{
				"world_id": started.WorldID, "delta_digest": started.DeltaDigest,
			})
		}
		if name == "missing_artifact" || name == "transient_artifact_read" {
			restore = faultNormalizedEvidencePath(t, normalizedArtifactData(c, accepted.TypedResult.Artifacts[0].ID), name == "transient_artifact_read")
		}
		if name == "missing_apply_projection" {
			removeCollectedApplyProjection(t, c, cp, world, path)
		}
		world.state = isolatedAttemptRetained
	}
	c.closeIsolatedAttempt(ctx, world, "fixture interruption before TaskDone")
	reader := reopenRecoveryCoordinator(t, c)
	resume := WithSessionResume(t.Context())
	if name == "prepared_terminal_failure" {
		exitCode := 7
		installAttemptTerminalEvidence(t, reader, TerminalSession{ID: "failed-original", RunID: receipt.RunID, OwnerTaskID: item.ID,
			ExitCode: &exitCode, State: TerminalSessionExited, StartedAt: receipt.StartedAt.Add(time.Millisecond)})
	}
	reconcileErr := reader.reconcileAttemptWorlds(resume)
	switch name {
	case "prepared_terminal_failure":
		assertTerminalBlocksPreparedCollectionApply(t, reader, item.ID, path, reconcileErr)
		return
	case "workspace_conflict", "applying_conflict":
		got := todoByID(t, reader, item.ID)
		data, err := os.ReadFile(filepath.Join(c.projectDir, path))
		if reconcileErr != nil || err != nil || string(data) != "new user work" || got.Status != TaskBlocked ||
			got.FailureEvent == nil || got.FailureEvent.FailureClass != FailureWorkspaceConflict || normalizations != 1 {
			t.Fatalf("restarted collection did not preserve conflicting work: status=%s calls=%d read=%v recovery=%v", got.Status, normalizations, err, reconcileErr)
		}
		return
	case "applying_io_failure":
		if localResultRejectionReason(reconcileErr) != RepairFailureCommit || todoByID(t, reader, item.ID).Status != accepted.Status {
			t.Fatalf("transient applying fault lost the committed world: %v", reconcileErr)
		}
		if err := os.Remove(c.projectDir); err != nil {
			t.Fatal(err)
		}
		restore()
		reader = reopenRecoveryCoordinator(t, reader)
		reconcileErr = reader.reconcileAttemptWorlds(resume)
	case "missing_apply_projection", "substituted_applying_event", "substituted_applied_event", "substituted_delta_manifest", "missing_artifact", "transient_artifact_read":
		wantStatus, wantReason := TaskBlocked, RepairFailureSource
		if name == "transient_artifact_read" {
			wantStatus, wantReason = accepted.Status, RepairFailureCommit
		}
		got := todoByID(t, reader, item.ID)
		if got.Status != wantStatus || localResultRejectionReason(reconcileErr) != wantReason || normalizations != 1 {
			t.Fatalf("unverified apply was not suspended: status=%s calls=%d error=%v", got.Status, normalizations, reconcileErr)
		}
		if _, err := os.Stat(filepath.Join(c.projectDir, path)); !os.IsNotExist(err) {
			t.Fatalf("recovery changed the subject before authenticating its evidence: %v", err)
		}
		if name != "transient_artifact_read" {
			return
		}
		restore()
		reader = reopenRecoveryCoordinator(t, reader)
		reconcileErr = reader.reconcileAttemptWorlds(resume)
	}
	if reconcileErr != nil {
		t.Fatal(reconcileErr)
	}
	done := todoByID(t, reader, item.ID)
	data, err := os.ReadFile(filepath.Join(c.projectDir, path))
	if err != nil || string(data) != "verified collection" || done.Status != TaskDone || normalizations != 1 ||
		!sameCanonicalEvidence(accepted.TypedResult, done.TypedResult) {
		t.Fatalf("committed collection did not resume its original apply: status=%s calls=%d error=%v", done.Status, normalizations, err)
	}
}

func TestNormalizedVerificationRevalidatesOriginalTranscriptBeforeCommand(t *testing.T) {
	for _, temporary := range []bool{false, true} {
		name := "missing"
		if temporary {
			name = "temporarily_unreadable"
		}
		t.Run(name, func(t *testing.T) {
			c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
				spec.Verify = "test -d . && printf verified > verified.txt"
			})
			c.projectDir = t.TempDir()
			c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizationWire(approvedNormalizationDraft), nil)
			stopBeforeObjectiveVerification(t, c, item)
			forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
			reader, readerJournal := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
			restore := removeNormalizedVerificationTranscript(t, reader, todoByID(t, reader, item.ID).ExecutionReceipt, temporary)
			ctx := WithSessionResume(t.Context())
			count, err := reader.ResumeInterruptedTasks(ctx)
			status, reason := TaskBlocked, RepairFailureSource
			if temporary {
				status, reason = TaskVerifying, RepairFailureCommit
			}
			if count != 1 || localResultRejectionReason(err) != reason || todoByID(t, reader, item.ID).Status != status || forbidden.Calls() != 0 {
				t.Fatalf("missing source admitted command: count=%d status=%s error=%v", count, todoByID(t, reader, item.ID).Status, err)
			}
			if _, err := os.Stat(filepath.Join(c.projectDir, "verified.txt")); !os.IsNotExist(err) {
				t.Fatalf("objective command ran before source validation: %v", err)
			}
			if temporary {
				restore()
				reader, _ = restartNormalizedIsolatedReader(t, reader, readerJournal, item, forbidden)
				if count, err := reader.ResumeInterruptedTasks(ctx); err != nil || count != 1 || todoByID(t, reader, item.ID).Status != TaskDone {
					t.Fatalf("verification did not finish after its source was restored: count=%d error=%v", count, err)
				}
			}
		})
	}
}

func TestNormalizedVerificationRevalidatesAcceptedArtifactBeforeCommand(t *testing.T) {
	for _, fault := range []string{"missing", "mismatched", "temporarily_unreadable"} {
		t.Run(fault, func(t *testing.T) {
			c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
				spec.Verify = "test -d . && printf verified > verified.txt"
			})
			c.projectDir = t.TempDir()
			const source = "source.txt"
			if err := os.WriteFile(filepath.Join(c.session.Workspace, source), []byte("accepted evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizedWireWithArtifact(approvedNormalizationDraft, source), nil)
			stopBeforeObjectiveVerification(t, c, item)
			forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
			reader, readerJournal := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
			accepted := todoByID(t, reader, item.ID).TypedResult
			if len(accepted.Artifacts) != 1 {
				t.Fatalf("expected one accepted artifact: %+v", accepted.Artifacts)
			}
			path := normalizedArtifactData(reader, accepted.Artifacts[0].ID)
			temporary := fault == "temporarily_unreadable"
			restore := faultNormalizedEvidencePath(t, path, temporary)
			if fault == "mismatched" {
				if err := os.WriteFile(path, []byte("different evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx := WithSessionResume(t.Context())
			count, err := reader.ResumeInterruptedTasks(ctx)
			status, reason := TaskBlocked, RepairFailureSource
			if temporary {
				status, reason = TaskVerifying, RepairFailureCommit
			}
			if count != 1 || localResultRejectionReason(err) != reason || todoByID(t, reader, item.ID).Status != status || forbidden.Calls() != 0 {
				t.Fatalf("invalid artifact admitted verification: count=%d status=%s error=%v", count, todoByID(t, reader, item.ID).Status, err)
			}
			if _, err := os.Stat(filepath.Join(c.projectDir, "verified.txt")); !os.IsNotExist(err) {
				t.Fatalf("objective command ran before artifact validation: %v", err)
			}
			if temporary {
				restore()
				reader, _ = restartNormalizedIsolatedReader(t, reader, readerJournal, item, forbidden)
				if count, err := reader.ResumeInterruptedTasks(ctx); err != nil || count != 1 || todoByID(t, reader, item.ID).Status != TaskDone {
					t.Fatalf("verification did not recover after the artifact was restored: count=%d error=%v", count, err)
				}
			}
		})
	}
}

func normalizedWireWithArtifact(draft, path string) string {
	return strings.TrimSuffix(normalizationWire(draft), "}") + `,"artifacts":[{"path":"` + path + `","description":"accepted fixture"}]}`
}

func normalizedArtifactData(c *Coordinator, id string) string {
	return filepath.Join(runtimestore.ArtifactRoot(c.artifactStoreRootPath()), "data", id)
}

func TestNormalizedToolVerificationUsesSameBoundedEvidenceAfterRestart(t *testing.T) {
	cases := []struct {
		name, observed, wantText string
		wantStatus               TaskStatus
	}{
		{"redacted", "sk-proj-" + strings.Repeat("Q", 32), "[REDACTED]", TaskDone},
		{"oversized", strings.Repeat("z", 1<<20) + "suffix", "suffix", TaskBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, restart := range []bool{false, true} {
				c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
					spec.VerifySpec = &VerificationSpec{Type: VerifyToolCallAssert, ToolCallAssertions: []ToolCallAssertion{
						{Tool: "fixture_observation", ResultContains: tc.wantText},
					}}
				})
				c.workerAgentOverride = normalizedSubmissionWithObservation(c, item.ID, tc.observed)
				var err error
				if restart {
					stopBeforeObjectiveVerification(t, c, item)
					forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
					c, _ = restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
					_, err = c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
					if forbidden.Calls() != 0 {
						t.Fatalf("verification called the model %d times", forbidden.Calls())
					}
				} else {
					_, err = c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(item), item.ID)
					if closeErr := journal.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
				}
				got := todoByID(t, c, item.ID)
				if got.Status != tc.wantStatus || (err == nil) != (tc.wantStatus == TaskDone) ||
					tc.wantStatus == TaskBlocked && localResultRejectionReason(err) != RepairFailureSource {
					t.Fatalf("restart=%v status=%s error=%v", restart, got.Status, err)
				}
			}
		})
	}
}

func TestNormalizedVerificationRevalidatesRequiredEvidenceBeforeCommand(t *testing.T) {
	c, journal, source := newDirectNormalizationRun(t, echoNormalization, nil)
	c.projectDir = t.TempDir()
	c.workerAgentOverride, _ = normalizationStreamedSubmission(c, source.ID, normalizationWire(approvedNormalizationDraft), nil)
	ctx := withTestProtocolRepairInvocationContext(t.Context())
	if _, err := c.executeTask(ctx, taskDefFromTodoItem(source), source.ID); err != nil {
		t.Fatalf("produce original evidence: %v", err)
	}
	item := admitDirectNormalizationTask(t, c, echoNormalization, func(spec *TodoSpec) {
		spec.Execution.RequiresEvidence = true
		spec.EvidenceFrom = []string{source.ID}
		spec.Verify = "test -d . && printf verified > verified.txt"
	})
	c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizationWire(approvedNormalizationDraft), nil)
	stopBeforeObjectiveVerification(t, c, item)
	invalidateNormalizedEvidenceDelivery(t, c, item.ID)
	forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
	reader, _ := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
	count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	if count != 1 || localResultRejectionReason(err) != RepairFailureSource || todoByID(t, reader, item.ID).Status != TaskBlocked || forbidden.Calls() != 0 {
		t.Fatalf("invalid required evidence resumed: count=%d status=%s error=%v", count, todoByID(t, reader, item.ID).Status, err)
	}
	if _, err := os.Stat(filepath.Join(c.projectDir, "verified.txt")); !os.IsNotExist(err) {
		t.Fatalf("objective command ran without required evidence: %v", err)
	}
}

func invalidateNormalizedEvidenceDelivery(t *testing.T, c *Coordinator, taskID string) {
	t.Helper()
	for _, original := range todoByID(t, c, taskID).ContextManifests {
		if !original.ModelCalled {
			continue
		}
		manifest := cloneContextInjectionManifest(&original)
		for i, entry := range manifest.Items {
			if entry.ID == "dependency_results" && entry.Included {
				manifest.Items[i].ContentHash = hashContentKey("different evidence")
				if err := c.persistContextManifest(manifest); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
	t.Fatal("original dispatch did not persist delivery of required evidence")
}

func normalizedSubmissionWithObservation(c *Coordinator, taskID, observed string) *scriptedWorker {
	worker, _ := normalizationStreamedSubmission(c, taskID, normalizationWire(approvedNormalizationDraft), nil)
	return scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		input := fantasy.ToolCallContent{ToolCallID: "observation", ToolName: "fixture_observation", Input: "{}"}
		output := fantasy.ToolResultContent{ToolCallID: input.ToolCallID, ToolName: input.ToolName, Result: fantasy.ToolResultOutputContentText{Text: observed}}
		if err := call.OnToolCall(input); err != nil {
			return nil, err
		}
		if err := call.OnToolResult(output); err != nil {
			return nil, err
		}
		result, err := worker.Stream(ctx, call)
		if err == nil {
			result.Steps = append([]fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{input, output}}}}, result.Steps...)
		}
		return result, err
	}})
}

func TestOptionalNormalizedResultResumesWithoutVerification(t *testing.T) {
	c, journal, item := newDirectNormalizationRun(t, func(context.Context, []byte) ([]byte, error) {
		t.Error("optional payload unexpectedly normalized")
		return nil, errors.New("unexpected normalizer call")
	}, func(spec *TodoSpec) {
		spec.ResultContract.RequireStructured = false
	})
	const summary = "reviewed without structured payload"
	c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, `{"status":"success","summary":"`+summary+`"}`, nil)
	execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_normalized_done BEFORE INSERT ON events WHEN NEW.type='task_completed'
		BEGIN SELECT RAISE(ABORT, 'fixture completion write failure'); END`)
	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(item), item.ID)
	if localResultRejectionReason(err) != RepairFailureCommit || !strings.Contains(err.Error(), "fixture completion write failure") {
		t.Fatalf("optional result lost its continuation: %v", err)
	}
	execJournal(t, c.session.Workspace, `DROP TRIGGER refuse_normalized_done`)
	forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
	reader, _ := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
	count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	done := todoByID(t, reader, item.ID)
	if err != nil || count != 1 || done.Status != TaskDone || done.TypedResult == nil || done.TypedResult.Summary != summary ||
		done.TypedResult.StructuredPayload != nil || done.VerifyResult != nil || forbidden.Calls() != 0 {
		t.Fatalf("optional completion diverged: count=%d status=%s calls=%d error=%v", count, done.Status, forbidden.Calls(), err)
	}
}

func TestNormalizedVerificationResumesRemainingAssumptionChecks(t *testing.T) {
	c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) {
		spec.VerifySpec = &VerificationSpec{
			Type: VerifyCommandExit, Command: `test -d . && printf 'verified\n' >> verifier.log`, AssumptionRefs: []string{"A1", "A2"},
		}
	})
	c.projectDir = t.TempDir()
	c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizationWire(approvedNormalizationDraft), nil)
	stopBeforeObjectiveVerification(t, c, item)
	forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
	reader, readerJournal := restartNormalizedIsolatedReader(t, c, journal, item, forbidden)
	armNormalizationAssumption(t, reader, item, true, "A1", "A2")
	execJournal(t, reader.session.Workspace, `CREATE TRIGGER refuse_second_check BEFORE INSERT ON events
		WHEN NEW.type='assumption_supported' AND json_extract(NEW.payload, '$.assumption_id')='A2'
		BEGIN SELECT RAISE(ABORT, 'fixture verification check failure'); END`)
	ctx := WithSessionResume(t.Context())
	count, err := reader.ResumeInterruptedTasks(ctx)
	if count != 1 || localResultRejectionReason(err) != RepairFailureCommit || !strings.Contains(err.Error(), "fixture verification check failure") ||
		todoByID(t, reader, item.ID).Status != TaskVerifying || forbidden.Calls() != 0 {
		t.Fatalf("partial assumption persistence settled verification: count=%d error=%v", count, err)
	}
	execJournal(t, reader.session.Workspace, `DROP TRIGGER refuse_second_check`)
	resumed, resumedJournal := restartNormalizedIsolatedReader(t, reader, readerJournal, item, forbidden)
	armNormalizationAssumption(t, resumed, item, false, "A1", "A2")
	count, err = resumed.ResumeInterruptedTasks(ctx)
	done := todoByID(t, resumed, item.ID)
	if err != nil || count != 1 || done.Status != TaskDone || done.VerifyResult == nil || forbidden.Calls() != 0 {
		t.Fatalf("assumption verification failed to settle: count=%d status=%s error=%v", count, done.Status, err)
	}
	data, err := os.ReadFile(filepath.Join(c.projectDir, "verifier.log"))
	if err != nil || string(data) != "verified\n" {
		t.Fatalf("objective verification did not execute exactly once: log=%q error=%v", data, err)
	}
	events, err := resumedJournal.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]int{}
	for _, event := range events {
		if event.Type != string(EventAssumptionSupported) {
			continue
		}
		var payload decisionEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Source == AssumptionSourceVerification {
			checks[payload.AssumptionID]++
		}
	}
	if checks["A1"] != 1 || checks["A2"] != 1 {
		t.Fatalf("verification checks were not preserved exactly once: %v", checks)
	}
}
