package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func normalizedCollectionCompletionFixture(t *testing.T, isolated, verify bool, extraVerify ...string) (*Coordinator, string, *isolatedAttempt, *int) {
	t.Helper()
	compiled, err := CompileResultContractDocument("findings.json", []byte(collectedFindingSchema))
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	var normalizations int
	var snapshot *RunInputSnapshot
	c, item, ctx, receipt := resultCollectionIdentityFixture(t, "http://127.0.0.1:1/v1", true, "", func(spec *TodoSpec) {
		spec.ResultContract = &ref
		if isolated {
			policy := isolatedSpec()
			spec.WorkerWorkspace = &WorkerWorkspacePolicy{Mode: policy.Mode, EffectiveMode: policy.Mode, Integrate: policy.Integrate}
		}
		if verify {
			spec.VerifySpec = &VerificationSpec{Type: VerifyCommandExit, Command: "printf 'verified\\n' >> verifier.log" + strings.Join(extraVerify, "")}
			spec.AdversarialVerify = 1
		}
	}, func(c *Coordinator, spec *TodoSpec, _ []string) {
		if err := c.resolveRunInputsForInvocation(t.Context(), "review original evidence"); err != nil {
			t.Fatal(err)
		}
		snapshot = c.RunInputSnapshot()
		if snapshot == nil {
			t.Fatal("fixture did not persist a run input snapshot")
		}
		spec.RunInputSnapshotID, spec.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	}, func(c *Coordinator) {
		c.session.Scope = WorkspaceScope{Managed: true, ControlRoot: c.session.Workspace, SubjectRoot: c.projectDir}
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: func(_ context.Context, draft []byte) ([]byte, error) {
			normalizations++
			return draft, nil
		}})
	})
	var world *isolatedAttempt
	receipt.RunInputSnapshotID, receipt.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	if isolated {
		if len(extraVerify) != 0 {
			if err := os.WriteFile(filepath.Join(c.projectDir, verifierFixtureSecretFile), []byte(verificationFixtureSecretContent(t)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		world, err = c.prepareIsolatedAttempt(ctx, taskDefFromTodoItem(item), item.ID, receipt.Attempt)
		if err != nil || world == nil {
			t.Fatalf("prepare collection workspace: %v", err)
		}
		t.Cleanup(func() { c.closeIsolatedAttempt(ctx, world, "fixture cleanup") })
		receipt.ExecutionWorldID = world.worldID()
	}
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "original work report")
	if err != nil {
		t.Fatal(err)
	}
	commitNormalizedCollectionFixture(t, c, ctx, cp, `{"findings":[]}`)
	if world != nil {
		world.state = isolatedAttemptRetained
		c.closeIsolatedAttempt(ctx, world, "restart after collection acceptance")
	}
	return c, item.ID, world, &normalizations
}

func TestResultCollectionResumesVerifiedIsolatedWorldAfterProjectionFault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed bool
	}{{"original workspace", false}, {"changed workspace", true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, id, world, normalizations := normalizedCollectionCompletionFixture(t, true, true, verifierFixtureSecretOutput)
			execJournal(t, c.session.Workspace, verificationProjectionFaultTrigger+` BEGIN SELECT RAISE(ABORT, 'fixture verifier projection fault'); END`)
			c = reopenRecoveryCoordinator(t, c)
			forbidden := scriptWorker(failTurn(errors.New("accepted collection dispatched a worker or repair")))
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			var skepticCalls atomic.Int32
			installCompletionSkeptic(c, &skepticCalls)
			ctx := WithSessionResume(t.Context())
			_, err := c.executeTask(ctx, taskDefFromTodoItem(todoByID(t, c, id)), id)
			if localResultRejectionReason(err) != RepairFailureCommit || !strings.Contains(err.Error(), "fixture verifier projection fault") || skepticCalls.Load() != 0 {
				t.Fatalf("fixture did not interrupt publication of objective verification: %v", err)
			}
			item := todoByID(t, c, id)
			cp := currentResultCollection(item)
			key := normalizedCompletionKey(cp.BranchID, collectionWorkIdentity(cp))
			assertPendingVerificationWorld(t, c, world.worldDir(), key)
			if tc.changed {
				writeUnverifiedApplyFixture(t, filepath.Join(world.prepared.Root, afterPreparedApplyPath))
			}
			execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_normalized_checkpoint")
			c = reopenRecoveryCoordinator(t, c)
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			installCompletionSkeptic(c, &skepticCalls)
			_, err = c.executeTask(ctx, taskDefFromTodoItem(todoByID(t, c, id)), id)
			assertVerificationWorkspaceResume(t, c, todoByID(t, c, id), world.worldDir(), key, err, tc.changed, skepticCalls.Load())
			if forbidden.Calls() != 0 || *normalizations != 1 {
				t.Fatalf("recovery repeated model work: model calls=%d normalizations=%d", forbidden.Calls(), *normalizations)
			}
		})
	}
}

func TestResultCollectionCompletionChecksOriginalTerminal(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		originalExit int
		resumeExit   int
		want         TaskStatus
	}{
		{name: "original_failure", originalExit: 7, want: TaskBlocked},
		{name: "unrelated_failure", resumeExit: 9, want: TaskDone},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c, id, _, normalizations := normalizedCollectionCompletionFixture(t, false, true)
			receipt := todoByID(t, c, id).ExecutionReceipt
			const resumeRun = "later-collection-invocation"
			body, err := json.Marshal([]TerminalSession{
				{ID: "original-session", RunID: receipt.RunID, OwnerTaskID: id, ExitCode: &scenario.originalExit, State: TerminalSessionExited, StartedAt: receipt.StartedAt.Add(time.Millisecond)},
				{ID: "other-attempt", RunID: receipt.RunID, OwnerTaskID: id, ExitCode: &scenario.resumeExit, State: TerminalSessionExited, StartedAt: receipt.FinishedAt.Add(time.Second)},
				{ID: "resume-session", RunID: resumeRun, OwnerTaskID: id, ExitCode: &scenario.resumeExit, State: TerminalSessionExited, StartedAt: receipt.StartedAt.Add(time.Millisecond)},
			})
			if err != nil {
				t.Fatal(err)
			}
			putRuntimeDocument(t, c.session.Workspace, runtimestore.DocumentTerminalSessions, body)
			source, err := ReadResumeSource(c.session.Workspace, "", "")
			if err != nil {
				t.Fatal(err)
			}
			c = reopenRecoveryCoordinator(t, c)
			c.SetSessionData(source.Session)
			c.executionRunID = resumeRun
			frozen := c.RunInputSnapshot()
			if frozen == nil {
				t.Fatal("original run snapshot absent after replay")
			}
			if err := c.selectRecoveryRunInputSnapshot(frozen); err != nil {
				t.Fatal(err)
			}
			projected := cloneExecutionReceipt(todoByID(t, c, id).ExecutionReceipt)
			projected.StartedAt = receipt.FinishedAt.Add(time.Second)
			projected.FinishedAt = receipt.FinishedAt.Add(2 * time.Second)
			if err := c.setAttemptReceipt(id, &projected); err != nil {
				t.Fatal(err)
			}
			c.terminalSessionMgr, err = NewTerminalSessionManager(c.session.Workspace, nil)
			if err != nil {
				t.Fatal(err)
			}
			forbidden := scriptWorker(failTurn(errors.New("accepted collection dispatched a worker or repair")))
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			var skepticCalls atomic.Int32
			installCompletionSkeptic(c, &skepticCalls)
			count, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			got := todoByID(t, c, id)
			if count != 1 || got.Status != scenario.want || *normalizations != 1 || forbidden.Calls() != 0 {
				t.Fatalf("original terminal verdict not preserved: status=%s resumed=%d error=%v", got.Status, count, err)
			}
			if scenario.want == TaskDone {
				if err != nil || got.VerifyResult == nil || skepticCalls.Load() != 1 {
					t.Fatalf("unrelated terminal blocked original work: error=%v", err)
				}
			} else if err == nil || ClassifyTaskFailureStructured(FailureClassificationInput{Err: err}) != FailureVerify || got.VerifyResult != nil || skepticCalls.Load() != 0 {
				t.Fatalf("original terminal failure reached verification: error=%v", err)
			}
		})
	}
}

func assertTerminalBlocksPreparedCollectionApply(t *testing.T, c *Coordinator, id, path string, err error) {
	t.Helper()
	item := todoByID(t, c, id)
	if ClassifyTaskFailureStructured(FailureClassificationInput{Err: err}) != FailureVerify || item.Status != TaskBlocked {
		t.Fatalf("prepared apply accepted a failed terminal: status=%s error=%v", item.Status, err)
	}
	if _, err := os.Stat(filepath.Join(c.projectDir, path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed terminal reached workspace apply: %v", err)
	}
}

func TestResultCollectionReplayRejectsMismatchedNormalizationOccurrence(t *testing.T) {
	c, id, _, _ := normalizedCollectionCompletionFixture(t, false, false)
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplayTodoList(events); err != nil {
		t.Fatal(err)
	}
	for i, event := range events {
		if event.TaskID != id || event.Type != string(EventTaskResultCollectionCheckpointed) {
			continue
		}
		var payload resultCollectionEvent
		if err := json.Unmarshal(event.Payload, &payload, collectionDurationFormat); err != nil {
			t.Fatal(err)
		}
		if payload.Result == nil {
			continue
		}
		if err := validateResultCollectionEvent(event); err != nil {
			t.Fatal(err)
		}
		payload.Receipt.ResultNormalizerOccurrenceSHA256 = strings.Repeat("0", 64)
		event.Payload, err = json.Marshal(payload, collectionDurationFormat)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateResultCollectionEvent(event); err == nil {
			t.Fatal("collection accepted a substituted normalization receipt")
		}
		events[i] = event
		if _, err := ReplayTodoList(events); err == nil {
			t.Fatal("collection replay accepted a substituted normalization receipt")
		}
		return
	}
	t.Fatal("accepted collection event missing")
}

func TestResultCollectionPublicResumeClassifiesSourceFailure(t *testing.T) {
	for _, temporary := range []bool{false, true} {
		name, want := "missing", RepairFailureSource
		if temporary {
			name, want = "transient", RepairFailureCommit
		}
		t.Run(name, func(t *testing.T) {
			c, id, _, normalizations := normalizedCollectionCompletionFixture(t, false, false)
			original := todoByID(t, c, id)
			path := normalizedArtifactData(c, currentResultCollection(original).Accepted.ArtifactID)
			restore := faultNormalizedEvidencePath(t, path, temporary)
			source, err := ReadResumeSource(c.session.Workspace, "", "")
			if err != nil {
				t.Fatal(err)
			}
			preview := newTestCoordinator(t)
			preview.executionRunID = ""
			preview.session, preview.providerManager, preview.projectDir, preview.maxConcurrent = c.session, c.providerManager, c.projectDir, c.maxConcurrent
			if _, err := preview.AssessResumeSource(t.Context(), source); err == nil || !strings.Contains(err.Error(), "accepted collection") {
				t.Fatalf("preview did not detect unreadable accepted collection: %v", err)
			}
			after, err := ReadResumeSource(c.session.Workspace, "", "")
			if err != nil || after.sessionSHA256 != source.sessionSHA256 {
				t.Fatalf("source preview changed the canonical branch: %v", err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			c = newRecoveryCoordinator(t, c)
			c.executionRunID = ""
			if err := c.AuthorizeSessionRestore(source); err != nil {
				t.Fatal(err)
			}
			c.SetSessionData(source.Session)
			forbidden := scriptWorker(failTurn(errors.New("accepted collection dispatched a worker or repair")))
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			ctx := WithSessionResume(t.Context())
			_, err = c.Run(ctx, "resume original evidence")
			if localResultRejectionReason(err) != want {
				t.Fatalf("public resume lost source classification %s: %v", want, err)
			}
			after, err = ReadResumeSource(c.session.Workspace, "", "")
			if err != nil {
				t.Fatal(err)
			}
			got := todoItemByID(after.Session.Tasks, id)
			if !temporary {
				if got.Status != TaskBlocked || got.ExecutionReceipt.RepairProvenance.FailureReason != want {
					t.Fatalf("public permanent rejection was not durable: %+v", got)
				}
			} else {
				if got.Status != original.Status || !sameCanonicalEvidence(got.ExecutionReceipt, original.ExecutionReceipt) {
					t.Fatalf("public transient rejection changed collection: %+v", got)
				}
				restore()
				c = reopenRecoveryCoordinator(t, c)
				c.SetSessionData(after.Session)
				frozen := c.RunInputSnapshot()
				if frozen == nil {
					t.Fatal("original run snapshot absent after replay")
				}
				if err := c.selectRecoveryRunInputSnapshot(frozen); err != nil {
					t.Fatal(err)
				}
				c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
				if count, err := c.ResumeInterruptedTasks(ctx); count != 1 || err != nil || todoByID(t, c, id).Status != TaskDone {
					t.Fatalf("original collection did not resume after restoring the source: count=%d error=%v", count, err)
				}
			}
			if forbidden.Calls() != 0 || *normalizations != 1 {
				t.Fatal("public resume repeated the original work or normalization")
			}
		})
	}
}

func commitNormalizedCollectionFixture(t *testing.T, c *Coordinator, ctx context.Context, cp *ResultCollectionCheckpoint, payload string) {
	t.Helper()
	cp, err := c.reserveResultCollection(ctx, cp, "collector", []byte("{\"tools\":[]}"))
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":"success","summary":"collected result","structured_payload":`+payload+`}`))
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
}

func TestResultCollectionResumesAfterTransientHandoffFailure(t *testing.T) {
	for _, state := range []ResultCollectionState{ResultCollectionInFlight, ResultCollectionReceived, ResultCollectionPrepared} {
		t.Run(string(state), func(t *testing.T) {
			c, item, ctx, cp, normalizations := collectionHandoffFixture(t)
			cp, err := c.reserveResultCollection(ctx, cp, "collector-return", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			const returned = `{"status":"success","summary":"saved return","structured_payload":{"findings":[]}}`
			usage := ExecutionUsage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}
			switch state {
			case ResultCollectionInFlight:
				execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_received BEFORE INSERT ON events WHEN NEW.type='task_result_collection_checkpointed' AND json_extract(NEW.payload, '$.execution_receipt.result_collection.state')='received' BEGIN SELECT RAISE(ABORT, 'fixture response publication fault'); END`)
				if _, err := c.recordCollectionResponseUsage(ctx, cp, []byte(returned), usage, nil); localResultRejectionReason(err) != RepairFailureCommit {
					t.Fatalf("response publication = %v", err)
				}
				execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_received")
			default:
				cp, err = c.recordCollectionResponseUsage(ctx, cp, []byte(returned), usage, nil)
				if err != nil {
					t.Fatal(err)
				}
				blob := cp.Response
				if state == ResultCollectionPrepared {
					cp, err = c.prepareCollectionClaims(ctx, cp)
					if err != nil {
						t.Fatal(err)
					}
					blob = cp.Prepared
				}
				restore := faultNormalizedEvidencePath(t, normalizedArtifactData(c, blob.ArtifactID), true)
				err = c.collectWorkResult(ctx, c.session.Agents["worker"], cp)
				if localResultRejectionReason(err) != RepairFailureCommit {
					t.Fatalf("%s I/O = %v", state, err)
				}
				c.terminalizeTaskErrorIfUnresolved(item.ID, err)
				restore()
			}
			c = reopenRecoveryCoordinator(t, c)
			c.executionRunID = "resume-saved-collector-return"
			before := todoByID(t, c, item.ID)
			if before.Status != TaskInProgress || !sameCanonicalEvidence(currentResultCollection(before), cp) {
				t.Fatalf("storage failure changed %s checkpoint: %+v", state, before)
			}
			forbidden := scriptWorker(failTurn(errors.New("worker or repair ran again")))
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			count, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			after := todoByID(t, c, item.ID)
			accepted := currentResultCollection(after)
			if count != 1 || err != nil || after.Status != TaskDone || after.TypedResult == nil || after.TypedResult.Summary != "saved return" ||
				accepted == nil || accepted.Work != cp.Work || accepted.TurnsUsed != 1 || accepted.ObservedUsage == nil || *accepted.ObservedUsage != usage ||
				forbidden.Calls() != 0 || *normalizations != 1 {
				t.Fatalf("%s recovery count=%d status=%s normalizations=%d checkpoint=%+v err=%v", state, count, after.Status, *normalizations, accepted, err)
			}
		})
	}
}

func collectionHandoffFixture(t *testing.T) (*Coordinator, *TodoItem, context.Context, *ResultCollectionCheckpoint, *int) {
	t.Helper()
	compiled, err := CompileResultContractDocument("findings.json", []byte(collectedFindingSchema))
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	var normalizations int
	c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
		spec.ResultContract = &ref
	}, func(c *Coordinator) {
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: func(_ context.Context, draft []byte) ([]byte, error) {
			normalizations++
			return draft, nil
		}})
	})
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "original complete work")
	if err != nil {
		t.Fatal(err)
	}
	return c, item, ctx, cp, &normalizations
}

func TestResultCollectionResumesNormalizedCompletionWithoutRepeatingChecks(t *testing.T) {
	for _, tc := range []struct {
		name         string
		isolated     bool
		lostManifest bool
	}{
		{name: "shared_completion_append"},
		{name: "isolated_apply_checkpoint", isolated: true},
		{name: "isolated_pending_apply_record_lost", isolated: true, lostManifest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolated := tc.isolated
			c, id, world, normalizations := normalizedCollectionCompletionFixture(t, isolated, true)
			forbidden := scriptWorker(failTurn(errors.New("accepted collection dispatched a worker or repair")))
			var skepticCalls atomic.Int32
			trigger := "CREATE TRIGGER reject_collection_settlement BEFORE INSERT ON events WHEN NEW.type='task_completed'"
			if isolated {
				trigger = "CREATE TRIGGER reject_collection_settlement BEFORE INSERT ON runtime_documents WHEN NEW.name LIKE 'collected-apply/%'"
			}
			execJournal(t, c.session.Workspace, trigger+" BEGIN SELECT RAISE(ABORT, 'fixture collection settlement write failure'); END")
			ctx := WithSessionResume(t.Context())
			var verified *VerificationResult
			for retry := range 2 {
				c = reopenRecoveryCoordinator(t, c)
				c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
				installCompletionSkeptic(c, &skepticCalls)
				item := todoByID(t, c, id)
				_, err := c.executeTask(ctx, taskDefFromTodoItem(item), id)
				c.terminalizeTaskErrorIfUnresolved(id, err)
				verified = todoByID(t, c, id).VerifyResult
				if localResultRejectionReason(err) != RepairFailureCommit || !strings.Contains(err.Error(), "fixture collection settlement write failure") ||
					todoByID(t, c, id).Status != TaskVerifying || skepticCalls.Load() != 1 || forbidden.Calls() != 0 || *normalizations != 1 {
					t.Fatalf("failed completion did not retain its verified collection: votes=%d normalizations=%d error=%v", skepticCalls.Load(), *normalizations, err)
				}
				if isolated && retry == 0 {
					cp := currentResultCollection(todoByID(t, c, id))
					intent, err := readPreparedApplyIntent(c.artifactStoreRootPath(), collectedApplyKey(cp), world.worldDir(), "")
					if err != nil || intent.record == nil || intent.published {
						t.Fatalf("SQLite fault did not leave a complete unpublished manifest: %+v, %v", intent, err)
					}
					if tc.lostManifest {
						removePendingApplyManifest(t, world.worldDir(), true)
						break
					}
					writeUnverifiedApplyFixture(t, filepath.Join(world.prepared.Root, afterPreparedApplyPath))
				}
			}
			execJournal(t, c.session.Workspace, "DROP TRIGGER reject_collection_settlement")
			c = reopenRecoveryCoordinator(t, c)
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			installCompletionSkeptic(c, &skepticCalls)
			item := todoByID(t, c, id)
			_, err := c.executeTask(ctx, taskDefFromTodoItem(item), id)
			got := todoByID(t, c, id)
			if tc.lostManifest {
				_, editErr := os.Stat(filepath.Join(c.projectDir, "verifier.log"))
				_, extraErr := os.Stat(filepath.Join(c.projectDir, afterPreparedApplyPath))
				log, logErr := os.ReadFile(filepath.Join(world.prepared.Root, "verifier.log"))
				if localResultRejectionReason(err) != RepairFailureSource || got.Status != TaskBlocked || skepticCalls.Load() != 1 ||
					forbidden.Calls() != 0 || *normalizations != 1 || !errors.Is(editErr, os.ErrNotExist) || !errors.Is(extraErr, os.ErrNotExist) ||
					logErr != nil || string(log) != "verified\n" {
					t.Fatalf("loss of original manifest repeated an effect or applied unverified work: status=%s edit=%v extra=%v verifier=%q read=%v error=%v", got.Status, editErr, extraErr, log, logErr, err)
				}
				return
			}
			evidence, readErr := os.ReadFile(filepath.Join(c.projectDir, "verifier.log"))
			if err != nil || got.Status != TaskDone || got.VerifyResult == nil || !sameCanonicalEvidence(got.VerifyResult, verified) ||
				readErr != nil || string(evidence) != "verified\n" || skepticCalls.Load() != 1 || forbidden.Calls() != 0 || *normalizations != 1 {
				t.Fatalf("collection did not settle its original checks: status=%s votes=%d normalizations=%d verifier=%q read=%v error=%v",
					got.Status, skepticCalls.Load(), *normalizations, evidence, readErr, err)
			}
			if _, err := os.Stat(filepath.Join(c.projectDir, afterPreparedApplyPath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("an edit made after the original apply was sealed reached the project: %v", err)
			}
		})
	}
}

func TestResultCollectionReopensAcceptedSourcesBeforeApplySealing(t *testing.T) {
	for _, source := range []string{"work", "transcript", "accepted", "baseline"} {
		for _, temporary := range []bool{true, false} {
			name := source + "/missing"
			want := RepairFailureSource
			if temporary {
				name, want = source+"/transient", RepairFailureCommit
			}
			t.Run(name, func(t *testing.T) {
				c, id, world, normalizations := normalizedCollectionCompletionFixture(t, true, false)
				original := todoByID(t, c, id)
				cp := currentResultCollection(original)
				paths := map[string]string{
					"work": normalizedArtifactData(c, cp.Work.ArtifactID), "accepted": normalizedArtifactData(c, cp.Accepted.ArtifactID),
					"transcript": normalizedArtifactData(c, original.ExecutionReceipt.TranscriptRef),
					"baseline":   filepath.Join(world.worldDir(), attemptWorldBaselineFile),
				}
				restore := faultNormalizedEvidencePath(t, paths[source], temporary)
				c = reopenRecoveryCoordinator(t, c)
				forbidden := scriptWorker(failTurn(errors.New("accepted collection dispatched a worker or repair")))
				c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
				ctx := WithSessionResume(t.Context())
				_, err := c.executeTask(ctx, taskDefFromTodoItem(todoByID(t, c, id)), id)
				c.terminalizeTaskErrorIfUnresolved(id, err)
				got := todoByID(t, c, id)
				if localResultRejectionReason(err) != want || *normalizations != 1 || forbidden.Calls() != 0 {
					t.Fatalf("unreadable %s: status=%s expected %s got %v", source, got.Status, want, err)
				}
				if !temporary {
					if got.Status != TaskBlocked || got.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureSource {
						t.Fatalf("missing original evidence was not durably blocked: %+v", got.ExecutionReceipt)
					}
					return
				}
				if got.Status != original.Status || !sameCanonicalEvidence(got.ExecutionReceipt, original.ExecutionReceipt) {
					t.Fatalf("transient error changed original collection: status=%s", got.Status)
				}
				restore()
				c = reopenRecoveryCoordinator(t, c)
				c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
				_, err = c.executeTask(ctx, taskDefFromTodoItem(todoByID(t, c, id)), id)
				if err != nil || todoByID(t, c, id).Status != TaskDone || *normalizations != 1 || forbidden.Calls() != 0 {
					t.Fatalf("restored original evidence did not settle: error=%v", err)
				}
			})
		}
	}
}
