package team

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResultCollectionFanoutResumesObjectiveVerificationAtOriginalBoundary(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		outcomeFault     string
		corruptedBinding bool
		changedOutcome   bool
		symlinkMember    string
	}{
		{name: "terminal publication failed"},
		{name: "objective outcome projection interrupted", outcomeFault: objectiveVerification},
		{name: "skeptic outcome projection interrupted", outcomeFault: adversarialVerification},
		{name: "forged verifier binding before sibling collection", corruptedBinding: true},
		{name: "pending objective output changed before sibling", outcomeFault: objectiveVerification, changedOutcome: true},
		{name: "symlink replaces pending objective outcome", outcomeFault: objectiveVerification, symlinkMember: runtimeEffectBodyFile},
		{name: "symlink replaces pending objective digest", outcomeFault: objectiveVerification, symlinkMember: "sha256"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var skepticCalls atomic.Int32
			withSkeptic := scenario.outcomeFault == adversarialVerification
			pendingSibling := scenario.corruptedBinding || scenario.changedOutcome || scenario.symlinkMember != ""
			c, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
				spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
				spec.SideEffect = SideEffectNone
				spec.Verify = "printf 'verified\\n' >> fanout-verification.log && test -s fanout-verification.log"
				if withSkeptic {
					spec.AdversarialVerify = 1
				}
			})
			for index := range item.ExecutionTopology {
				leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, c, item, receipt, index)
				if err != nil {
					t.Fatal(err)
				}
				if index != 0 || !pendingSibling {
					recordFanoutResponse(t, c, leaf, ctx, cp, ResultCollectionReceived, `{"status":"success","summary":"exact local output"}`)
				}
			}
			c = reopenRecoveryCoordinator(t, c)
			if withSkeptic {
				installCompletionSkeptic(c, &skepticCalls)
			}
			item = todoByID(t, c, item.ID)
			task := taskDefFromTodoItem(item)
			target := item.ExecutionTopology[0]
			wantExecutions := 1
			if !pendingSibling {
				finishVerifiedFanoutSibling(t, c, item, task, 0, target)
				checkFanoutVerifications(t, c, 1)
				item = todoByID(t, c, item.ID)
				wantExecutions++
			}
			recovery, err := c.prepareFanoutCollectionLeaf(t.Context(), item, 1, target)
			if err != nil {
				t.Fatal(err)
			}
			restore := faultFanoutObjectiveHandoff(t, c, item, recovery.work.Workspace, scenario.outcomeFault)
			def := cloneAgentDef(c.session.Agents[item.Agent])
			def.Generation.Model, def.ExtraModels = target.String(), nil
			output, err := c.executeSingleAgentWithModel(WithSessionResume(t.Context()), item.Agent, def, task, item.ID, fanoutSlotName(1), 1, &recovery)
			if localResultRejectionReason(err) != RepairFailureCommit {
				t.Fatalf("expected original verifier handoff write fault, got %v", err)
			}
			checkFanoutVerifications(t, c, wantExecutions)
			item = todoByID(t, c, item.ID)
			second := currentFanoutCollection(item, 1)
			var original *VerificationResult
			if scenario.outcomeFault != "" {
				key := normalizedCompletionKey(second.BranchID, collectionWorkIdentity(second)) + "/" + objectiveVerification + "/outcome"
				dir := runtimeEffectRecordPath(c.artifactStoreRootPath(), key)
				body, _, foundErr := readSealedRuntimeEffect(dir, normalizedVerificationRecordMaxBytes)
				var record normalizedVerificationRecord
				if foundErr != nil || json.Unmarshal(body, &record) != nil || record.Outcome.Result == nil {
					t.Fatalf("completed verifier output was not sealed: %v", foundErr)
				}
				original = record.Outcome.Result
				if scenario.changedOutcome {
					record.Outcome.Result.Stdout = "substituted verifier output"
					changed, err := marshalNormalizedCheckpoint(record)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, runtimeEffectBodyFile), changed, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if scenario.symlinkMember != "" {
					member := filepath.Join(dir, scenario.symlinkMember)
					if err := os.Rename(member, member+".original"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Base(member)+".original", member); err != nil {
						t.Fatal(err)
					}
				}
			} else if receipt := collectionReceipt(item, second); receipt != nil {
				original = receipt.VerifyResult
			}
			restore()
			if scenario.symlinkMember != "" {
				assertFanoutPreflightRejects(t, c, item, "must be a regular file", currentFanoutCollection(item, 0), second)
				checkFanoutVerifications(t, c, wantExecutions)
				return
			}
			if scenario.changedOutcome {
				assertFanoutPreflightRejects(t, c, item, "body differs from its sealed digest", currentFanoutCollection(item, 0), second)
				checkFanoutVerifications(t, c, wantExecutions)
				return
			}
			if scenario.corruptedBinding {
				key := normalizedCompletionKey(second.BranchID, collectionWorkIdentity(second))
				execJournal(t, recovery.work.Workspace, `UPDATE runtime_documents SET body=CAST(json_set(body, '$.binding_sha256', ?) AS BLOB) WHERE name=?`, strings.Repeat("a", 64), key)
				assertFanoutPreflightRejects(t, c, item, "does not match the accepted work", currentFanoutCollection(item, 0), second)
				checkFanoutVerifications(t, c, wantExecutions)
				return
			}

			c = reopenRecoveryCoordinator(t, c)
			if withSkeptic {
				installCompletionSkeptic(c, &skepticCalls)
			}
			c.executionRunID = "fanout-cold-verifier-recovery"
			forbidden := scriptWorker(failTurn(errors.New("work or collection repeated during verifier recovery")))
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			ctx := WithSessionResume(t.Context())
			rejectFanoutProviderBoundary(t, c, ctx)
			count, err := c.ResumeInterruptedTasks(ctx)
			item = todoByID(t, c, item.ID)
			checkFanoutVerifications(t, c, wantExecutions)
			if count != 1 || forbidden.Calls() != 0 {
				t.Fatalf("resume count=%d work calls=%d err=%v", count, forbidden.Calls(), err)
			}
			if withSkeptic && skepticCalls.Load() != 2 {
				t.Fatalf("recovery repeated or lost skeptic work: calls=%d err=%v", skepticCalls.Load(), err)
			}
			c = reopenRecoveryCoordinator(t, c)
			item = todoByID(t, c, item.ID)
			saved := collectionReceipt(item, currentFanoutCollection(item, 1))
			if err != nil || item.Status != TaskDone || saved == nil || !strings.Contains(item.Output, output) ||
				original == nil || !sameCanonicalEvidence(original, saved.VerifyResult) {
				t.Fatalf("original verifier outcome not replayed: status=%s sealed=%+v original=%+v err=%v", item.Status, saved, original, err)
			}
		})
	}
}

func faultFanoutObjectiveHandoff(t *testing.T, c *Coordinator, item *TodoItem, leafWorkspace, checkpointFault string) func() {
	t.Helper()
	if checkpointFault != "" {
		field := "$.objective.completed"
		if checkpointFault == adversarialVerification {
			field = "$.skeptic.completed"
		}
		execJournal(t, leafWorkspace, "CREATE TRIGGER refuse_objective_outcome BEFORE UPDATE ON runtime_documents WHEN NEW.name LIKE 'normalized-completion/%' AND json_extract(NEW.body, '"+field+"')=1 BEGIN SELECT RAISE(ABORT, 'fixture verifier outcome write fault'); END")
		return func() { execJournal(t, leafWorkspace, "DROP TRIGGER refuse_objective_outcome") }
	}
	name, _, err := c.fanoutLeafDocument(item, 1)
	if err != nil {
		t.Fatal(err)
	}
	path := fanoutStagePath(c.artifactStoreRootPath(), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(path), path); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResultCollectionFanoutValidatesSavedOutcomeBeforeLeafReceipt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  string
		check string
	}{
		{name: "exit code replaced", query: `UPDATE runtime_documents SET body=CAST(json_set(body, '$.objective.result.exit_code', 0) AS BLOB) WHERE name=?`, want: "does not match the sealed verification outcome"},
		{name: "checkpoint deleted", query: `DELETE FROM runtime_documents WHERE name=?`, want: "does not match the sealed objective verification outcome"},
		{name: "exit code and checksum replaced", check: objectiveVerification, want: "does not match the sealed objective verification outcome"},
		{name: "skeptic outcome and checksum replaced", check: adversarialVerification, want: "does not match the sealed adversarial verification outcome"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testFanoutSavedOutcomeFault(t, tc.query, tc.want, tc.check)
		})
	}
}

func testFanoutSavedOutcomeFault(t *testing.T, query, want, check string) {
	t.Helper()
	c, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
		spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
		spec.SideEffect = SideEffectNone
		spec.Verify = "printf 'verified\\n' >> fanout-verification.log && false"
		spec.VerifyMode = "expected_failure"
		if check == adversarialVerification {
			spec.AdversarialVerify = 1
		}
	})
	_, _, pending, err := beginFanoutWorkForRecovery(t, c, item, receipt, 0)
	if err != nil {
		t.Fatal(err)
	}
	leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, c, item, receipt, 1)
	if err != nil {
		t.Fatal(err)
	}
	cp = recordFanoutResponse(t, c, leaf, ctx, cp, ResultCollectionPrepared, `{"status":"success","summary":"verified work"}`)
	if err := leaf.commitPreparedCollection(ctx, cp); err != nil {
		t.Fatal(err)
	}
	verified, err := leaf.verifyNormalizedDeliverable(ctx, item.ID, leaf.session.Agents[item.Agent], taskDefFromTodoItem(item), leaf.GetTaskResult(item.ID), "")
	if err != nil || verified == nil || verified.ExitCode != 1 {
		t.Fatalf("original verifier outcome=%+v err=%v", verified, err)
	}
	if check == adversarialVerification {
		if err := leaf.verifyNormalizedWithSkeptic(ctx, taskDefFromTodoItem(item), item.ID, leaf.GetTaskResult(item.ID), ""); err == nil {
			t.Fatal("skeptic with no valid judgments should reject the result")
		}
	}
	checkFanoutVerifications(t, c, 1)
	c = reopenRecoveryCoordinator(t, c)
	item = todoByID(t, c, item.ID)
	committed := currentFanoutCollection(item, 1)
	if original := collectionReceipt(item, committed); original == nil || committed.State != ResultCollectionCommitted || original.VerifyResult != nil {
		t.Fatalf("test must stop before the terminal verification receipt: %+v", original)
	}
	key := normalizedCompletionKey(committed.BranchID, collectionWorkIdentity(committed))
	if query == "" {
		saved, found, err := readNormalizedCompletionCheckpoint(workspaceDocs(leaf.session.Workspace), c.artifactStoreRootPath(), key)
		if err != nil || !found {
			t.Fatalf("read original verification: found=%t err=%v", found, err)
		}
		if check == adversarialVerification {
			if !saved.Skeptic.Completed || saved.Skeptic.Error == "" {
				t.Fatalf("original rejecting skeptic outcome was not saved: %+v", saved.Skeptic)
			}
			saved.Skeptic.Error = ""
		} else {
			saved.Objective.Result.ExitCode, saved.Objective.Error = 0, ""
		}
		saved.ChecksSHA256, err = saved.checksDigest()
		if err != nil {
			t.Fatal(err)
		}
		body, err := marshalNormalizedCheckpoint(saved)
		if err != nil {
			t.Fatal(err)
		}
		execJournal(t, leaf.session.Workspace, `UPDATE runtime_documents SET body=? WHERE name=?`, body, key)
	} else {
		execJournal(t, leaf.session.Workspace, query, key)
	}
	assertFanoutPreflightRejects(t, c, item, want, pending, committed)
	checkFanoutVerifications(t, c, 1)
}

func TestResultCollectionFanoutPreservesAcceptedNonzeroVerification(t *testing.T) {
	for _, mode := range []string{"expected_failure", "observation"} {
		for _, fault := range []bool{false, true} {
			name := mode
			if fault {
				name += "/cold publication recovery"
			}
			t.Run(name, func(t *testing.T) {
				c, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
					spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
					spec.SideEffect = SideEffectNone
					spec.Verify = "printf 'verified\\n' >> fanout-verification.log && false"
					spec.VerifyMode = mode
				})
				for index := range item.ExecutionTopology {
					leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, c, item, receipt, index)
					if err != nil {
						t.Fatal(err)
					}
					recordFanoutResponse(t, c, leaf, ctx, cp, ResultCollectionReceived, `{"status":"success","summary":"verified work"}`)
				}
				c = reopenRecoveryCoordinator(t, c)
				c.executionRunID = "fanout-nonzero-verification"
				ctx := WithSessionResume(t.Context())
				rejectFanoutProviderBoundary(t, c, ctx)
				var original []*VerificationResult
				if fault {
					execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_fanout_done BEFORE INSERT ON events WHEN NEW.type='task_completed' BEGIN SELECT RAISE(ABORT, 'fixture publication fault'); END`)
					count, err := c.ResumeInterruptedTasks(ctx)
					if count != 1 || localResultRejectionReason(err) != RepairFailureCommit {
						t.Fatalf("expected failed aggregate publication, count=%d, err=%v", count, err)
					}
					execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_fanout_done")
					c = reopenRecoveryCoordinator(t, c)
					c.executionRunID = "fanout-nonzero-verification-recovery"
					rejectFanoutProviderBoundary(t, c, ctx)
					item = todoByID(t, c, item.ID)
					for index := range item.ExecutionTopology {
						leaf, _, err := c.readFanoutLeafCompletion(ctx, item, index)
						if err != nil || leaf == nil || leaf.Receipt.VerifyResult == nil || leaf.Receipt.VerifyResult.ExitCode != 1 {
							t.Fatalf("nonzero accepted result was not preserved in slot %d: %+v, %v", index, leaf, err)
						}
						original = append(original, leaf.Receipt.VerifyResult)
					}
				}
				count, err := c.ResumeInterruptedTasks(ctx)
				item = todoByID(t, c, item.ID)
				if count != 1 || err != nil || item.Status != TaskDone {
					t.Fatalf("nonzero verification failed in mode %q: count=%d, status=%s, err=%v", mode, count, item.Status, err)
				}
				checkFanoutVerifications(t, c, len(item.ExecutionTopology))
				for index := range item.ExecutionTopology {
					saved := collectionReceipt(item, currentFanoutCollection(item, index))
					if saved == nil || saved.VerifyResult == nil || saved.VerifyResult.ExitCode != 1 || (fault && !sameCanonicalEvidence(saved.VerifyResult, original[index])) {
						t.Fatalf("accepted verification changed in slot %d: %+v", index, saved)
					}
				}
			})
		}
	}
}
