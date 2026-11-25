package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestNormalizerConvergesNoisyBatchAndPreservesReceipt(t *testing.T) {
	if !golangruntime.NormalizerSupported() {
		t.Skip("bounded normalizer runtime requires Linux")
	}
	c, item := resultContractCoordinator(t, true)
	c.session.Workspace = t.TempDir()
	const capability = "normalize-result"
	dir := t.TempDir()
	const source = `package main
import ("context"; "io"; "strings")
func Run(_ context.Context, in io.Reader, out io.Writer) error {
  raw, err := io.ReadAll(in)
  if err != nil { return err }
  clean := strings.ReplaceAll(string(raw), "  APPROVE  ", "approve")
  clean = strings.ReplaceAll(clean, "  package", "package")
  _, err = io.WriteString(out, clean)
  return err
}`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	c.session.Config.ActionProviders = map[string]agent.ActionProviderConfig{
		capability: {Runtime: "golang", Source: ".", Mode: golangruntime.TrustedStaticMode},
	}
	c.session.ProviderRegistry = NewProviderRegistry()
	if err := registerConfiguredActionProviders(c.session.ProviderRegistry, c.session.Config.ActionProviders, dir, nil); err != nil {
		t.Fatal(err)
	}
	item.ResultContract.Normalizer = capability
	item.ResultContract.NormalizerSHA256 = actionProviderIdentityHash(c.session, capability)
	var findings []string
	for i := range 54 {
		findings = append(findings, fmt.Sprintf(`{"summary":"  package%02d finding"}`, i))
	}
	draft := `{ "verdict": "  APPROVE  ", "findings": [` + strings.Join(findings, ", ") + `] }`
	output := strings.NewReplacer("  APPROVE  ", "approve", "  package", "package").Replace(draft)
	resp, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Input: `{"status":"success","summary":"reviewed","structured_payload":` + draft + `}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("noisy batch rejected: %s, %v", resp.Content, err)
	}
	result := c.GetTaskResult(item.ID)
	receipt := &ExecutionReceipt{}
	c.applyResultContractReceipt(item.ID, result, receipt)
	var normalized struct {
		Findings []struct {
			Summary string `json:"summary"`
		} `json:"findings"`
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(result.StructuredPayload.Value, &normalized); err != nil {
		t.Fatal(err)
	}
	if normalized.Verdict != "approve" || len(normalized.Findings) != len(findings) {
		t.Fatalf("normalization lost findings: %+v", normalized)
	}
	for i, finding := range normalized.Findings {
		if want := fmt.Sprintf("package%02d finding", i); finding.Summary != want {
			t.Fatalf("canonical finding = %q, want %q", finding.Summary, want)
		}
	}
	if receipt.ResultRawPayloadSHA256 != sha256HexBytes([]byte(draft)) ||
		receipt.ResultNormalizerOutputSHA256 != sha256HexBytes([]byte(output)) ||
		receipt.ResultPayloadSHA256 != sha256HexBytes(result.StructuredPayload.Value) ||
		!receiptMatchesResultPayload(receipt, result.StructuredPayload) {
		t.Fatal("the receipt failed to preserve the draft, action output and canonical payload")
	}
}

type interruptedResultSink struct{}

func (interruptedResultSink) Submit(context.Context, string, TaskResult) error {
	return errors.New("storage unavailable at result commit")
}

type interruptedProcessSink struct{}

func (interruptedProcessSink) Submit(context.Context, string, TaskResult) error {
	panic(interruptedProcessSink{})
}

func TestNormalizedContinuationUsesOriginalEvidenceScope(t *testing.T) {
	t.Run("required artifact coverage", func(t *testing.T) {
		c, item, ctx, _, view := requiredReadFixture(t, "complete original source")
		ref := item.WorksetBinding.Inputs[0]
		deliverRequiredRead(t, c, ctx, view, "before-interruption", fmt.Sprintf(`{"artifact_ref":%q,"byte_offset":0}`, ref.ID))
		resumed, live, recovered := normalizationEvidenceResumeFixture(t, c, item, ctx)
		if err := resumed.validateRequiredArtifactReadSubmission(live, item.ID); err == nil {
			t.Fatal("a fresh dispatch borrowed another dispatch's read coverage")
		}
		if err := resumed.validateRequiredArtifactReadSubmission(recovered, item.ID); err != nil {
			t.Fatal(err)
		}
		proof := *recovered.Value(directNormalizationRecoveryKey{}).(*directNormalizationRecovery)
		proof.submission.Identity.DispatchID = "unobserved-dispatch"
		forged := context.WithValue(live, directNormalizationRecoveryKey{}, &proof)
		var missing *artifactReadCoverageIncomplete
		if err := resumed.validateRequiredArtifactReadSubmission(forged, item.ID); !errors.As(err, &missing) {
			t.Fatalf("wrong work dispatch coverage = %v, want missing evidence", err)
		}
	})
	t.Run("issued evidence keys and step receipts", func(t *testing.T) {
		f := newArtifactKeysFixture(t)
		f.readSource(t)
		resumed, live, recovered := normalizationEvidenceResumeFixture(t, f.c, f.item, f.ctx)
		compiled := resumed.session.ResultContracts["keys.json"]
		if _, err := resumed.resolveResultEvidenceKeys(live, f.item.ID, f.submission(f.key)); err == nil {
			t.Fatal("a fresh dispatch accepted an original evidence key")
		}
		refs, err := resumed.verifiedResultEvidenceKeyRefs(recovered, f.item.ID, compiled)
		if err != nil || refs[f.key].ID != f.ref.ID || refs[f.key].SHA256 != f.ref.SHA256 {
			t.Fatalf("original evidence key resolved as %+v, %v", refs[f.key], err)
		}
		resolved, err := resumed.resolveResultEvidenceKeys(recovered, f.item.ID, f.submission(f.key))
		if err != nil || !strings.Contains(resolved, f.ref.ID) {
			t.Fatalf("original evidence was not authorized: %s, %v", resolved, err)
		}
		origin := recovered.Value(directNormalizationRecoveryKey{}).(*directNormalizationRecovery).submission.Identity
		receipt := ExecutionStepReceipt{ID: "step-receipt", StepID: "inspect", ExecutionKind: "tool_call", RunID: origin.RunID,
			BranchID: resumed.activeBranchID(), TaskID: origin.TaskID, Attempt: origin.Attempt, OccurrenceRevision: origin.OccurrenceRevision}
		if err := resumed.executionStepReceiptRegistry().Record(receipt); err != nil {
			t.Fatal(err)
		}
		result := TaskResult{ReceiptIDs: []string{receipt.ID}}
		if err := resumed.validateTaskResultReceiptClaims(live, f.item.ID, &result); err == nil {
			t.Fatal("a fresh dispatch accepted a historical step receipt")
		}
		if err := resumed.validateTaskResultReceiptClaims(recovered, f.item.ID, &result); err != nil {
			t.Fatal(err)
		}
		resumed.setCurrentTaskAttempt(f.item.ID, 1)
		if _, err := resumed.resolveResultEvidenceKeys(recovered, f.item.ID, f.submission(f.key)); err == nil {
			t.Fatal("a replaced write lease retained access to original evidence")
		}
	})
	t.Run("memory attribution", func(t *testing.T) {
		c, manifest := memoryValidationCoordinator(t)
		c.session = &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{MemoryLearning: agent.MemoryLearningPolicy{Mode: agent.MemoryLearningObserve}}}
		c.setCurrentTaskAttempt(manifest.TaskID, manifest.Attempt)
		original, ok := c.activeTaskResultOccurrence(manifest.TaskID)
		if !ok {
			t.Fatal("memory source occurrence missing")
		}
		ctx := withSubmitResultRuntimeIdentity(t.Context(), original)
		resumed, live, recovered := normalizationEvidenceResumeFixture(t, c, c.todoItemByID(manifest.TaskID), ctx)
		resumed.contextRepo = c.contextRepo
		t.Cleanup(func() { resumed.contextRepo = nil })
		result := TaskResult{MemoryUses: []MemoryUseRef{{RetrievalID: manifest.RetrievalID, ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}}
		if err := resumed.validateMemoryUseClaims(live, manifest.TaskID, &result); err == nil {
			t.Fatal("fresh dispatch accepted a historical memory manifest")
		}
		if err := resumed.validateMemoryUseClaims(recovered, manifest.TaskID, &result); err != nil || result.Attempt != 1 {
			t.Fatalf("could not attribute original memory: %+v, %v", result, err)
		}
	})
	t.Run("dependency delivery at commit", func(t *testing.T) {
		c, _, item, _, _ := evidenceDeliveryFixture(t)
		c.session.Workspace = t.TempDir()
		source := occurrenceTestContext(c, item.ID, 1)
		resumed, live, recovered := normalizationEvidenceResumeFixture(t, c, item, source)
		identity, err := submitResultRuntimeIdentityFromContext(live, resumed, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := resumed.beginTaskResultSubmission(identity)
		if err != nil {
			t.Fatal(err)
		}
		result := &TaskResult{Status: TaskResultStatusSuccess, Summary: "Review complete"}
		if err := tx.commit(live, result); err == nil {
			t.Fatal("a fresh dispatch committed without proof of dependency delivery")
		}
		if err := tx.commit(recovered, result); err != nil {
			t.Fatal(err)
		}
		tx.finish()
	})
}

func normalizationEvidenceResumeFixture(t *testing.T, c *Coordinator, item *TodoItem, source context.Context) (*Coordinator, context.Context, context.Context) {
	t.Helper()
	original, err := submitResultRuntimeIdentityFromContext(source, c, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	branch := c.activeBranchID()
	resumed := newTestCoordinator(t, withCoordinatorWorkspace(c.session.Workspace))
	resumed.executionRunID = "normalization-evidence-resume"
	resumed.session.Config = c.session.Config
	resumed.session.ResultContracts = c.session.ResultContracts
	resumed.taskTracker.TodoList().Restore(c.taskTracker.TodoList().Items())
	events, err := NewEventStore(resumed.session.Workspace, resumed.executionRunID, "resume")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	events.SetBranchID(branch)
	resumed.eventStore = events
	resumed.SetEventJournal(eventStoreJournal{store: events})
	resumed.setCurrentTaskAttempt(item.ID, original.Attempt)
	current, ok := resumed.activeTaskResultOccurrence(item.ID)
	if !ok || current.RunID == original.RunID || current.DispatchID == original.DispatchID {
		t.Fatal("continuation did not obtain a fresh occurrence lease")
	}
	live := withSubmitResultRuntimeIdentity(source, current)
	recovered := context.WithValue(live, directNormalizationRecoveryKey{}, &directNormalizationRecovery{
		submission: directNormalizationSubmission{BranchID: branch, Identity: original},
	})
	return resumed, live, recovered
}

const rejectedNormalizationDraft = `{"verdict":"unknown"}`

func newDirectNormalizationRun(t *testing.T, normalize func(context.Context, []byte) ([]byte, error), configure func(*TodoSpec)) (*Coordinator, *EventStore, *TodoItem) {
	t.Helper()
	c := newTestCoordinator(t)
	c.SetSessionData(&SessionData{})
	events, err := NewEventStore(c.session.Workspace, c.executionRunID, "work")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = events
	c.SetEventJournal(eventStoreJournal{store: events})
	item := admitDirectNormalizationTask(t, c, normalize, configure)
	return c, events, item
}

func admitDirectNormalizationTask(t *testing.T, c *Coordinator, normalize func(context.Context, []byte) ([]byte, error), configure func(*TodoSpec)) *TodoItem {
	t.Helper()
	if err := c.resolveRunInputsForInvocation(t.Context(), "review"); err != nil {
		t.Fatal(err)
	}
	if err := c.mutateSessionData(func(*SessionData) error { return nil }); err != nil {
		t.Fatal(err)
	}
	compiled, ref := compiledReviewContract(t, true)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	snapshot := c.RunInputSnapshot()
	ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: normalize})
	spec := TodoSpec{Agent: "worker", Desc: "review", ResultContract: &ref, Recovery: RecoveryManual, SideEffect: SideEffectExternalWrite,
		Execution: ExecutionContract{RequiresResult: true}, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
	if configure != nil {
		configure(&spec)
	}
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
		t.Fatal(err)
	}
	items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatal(err)
	}
	return items[0]
}

func restartDirectNormalizationRun(t *testing.T, original *Coordinator, checkpoint *SessionData, item *TodoItem, normalize func(context.Context, []byte) ([]byte, error)) (*Coordinator, *EventStore) {
	t.Helper()
	reader := newTestCoordinator(t)
	reader.session.Workspace = original.session.Workspace
	reader.session.Scope, reader.projectDir = original.session.Scope, original.projectDir
	reader.session.Config, reader.session.ResultContracts = original.session.Config, original.session.ResultContracts
	reader.session.Agents, reader.session.InvariantCatalog = original.session.Agents, original.session.InvariantCatalog
	reader.session.ProviderRegistry = NewProviderRegistry()
	reader.session.ProviderRegistry.Register(item.ResultContract.Normalizer, &mockNormalizerProvider{fn: normalize})
	reader.executionRunID = "direct-normalization-continuation"
	reader.SetSessionData(checkpoint)
	if err := reader.selectRecoveryRunInputSnapshot(original.RunInputSnapshot()); err != nil {
		t.Fatal(err)
	}
	events, err := NewEventStore(reader.session.Workspace, reader.executionRunID, "resume")
	if err != nil {
		t.Fatal(err)
	}
	reader.eventStore = events
	reader.SetEventJournal(eventStoreJournal{store: events})
	t.Cleanup(func() { _ = events.Close() })
	return reader, events
}

func TestNormalizerResumesValidationAfterActionInterruption(t *testing.T) {
	for _, tc := range []struct {
		name, draft        string
		staged, correction bool
	}{
		{name: "unsealed_reservation", draft: rejectedNormalizationDraft},
		{name: "sealed_invalid_draft", draft: rejectedNormalizationDraft, staged: true},
		{name: "sealed_invalid_correction", draft: `{"verdict":"unresolved"}`, staged: true, correction: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testValidationAfterNormalizationInterruption(t, tc.draft, tc.staged, tc.correction)
		})
	}
}

func TestInterruptedNormalizationSettlesUnavailableEvidence(t *testing.T) {
	for _, evidence := range []string{"missing_stage", "corrupt_stage", "corrupt_envelope", "temporary_transcript_read"} {
		t.Run(evidence, func(t *testing.T) {
			c, journal, item := newDirectNormalizationRun(t, echoNormalization, nil)
			stopBeforeDirectNormalizationReceipt(t, t.Context(), c, item, normalizationWire(approvedNormalizationDraft), false)
			durable := todoByID(t, c, item.ID)
			sealed, err := c.readDirectNormalizationCheckpoint(durable)
			if err != nil || sealed == nil {
				t.Fatalf("interrupted dispatch has no receipt checkpoint: %v", err)
			}
			docs := workspaceDocs(c.session.Workspace)
			proof := sealed.Receipt.InterruptedResultNormalization
			stageKey := normalizationDocument(*item.ResultContract, proof.OccurrenceSHA256, proof.RawSHA256)
			var restoreTranscript func() error
			switch evidence {
			case "missing_stage":
				err = docs.deleteDocument(stageKey)
			case "corrupt_stage":
				err = docs.putDocument(stageKey, []byte(`{"output_sha256":"corrupt"}`))
			case "corrupt_envelope":
				err = docs.putDocument(directNormalizationSubmissionPrefix+proof.SubmissionSHA256, []byte(`{"wire":"corrupt"}`))
			case "temporary_transcript_read":
				path := filepath.Join(runtimestore.ArtifactRoot(c.artifactStoreRootPath()), "data", sealed.Receipt.TranscriptRef)
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				// A path read fault is temporary even for a privileged test runner.
				err = os.Symlink(filepath.Base(path), path)
				restoreTranscript = func() error { return os.Rename(path+".saved", path) }
			}
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := LoadSession(c.session.Workspace)
			if err := journal.Close(); err != nil || checkpoint == nil {
				t.Fatalf("interrupted checkpoint unavailable: %v", err)
			}
			rejectReentry := func(context.Context, []byte) ([]byte, error) {
				t.Error("source recovery repeated normalization")
				return nil, errors.New("unexpected normalizer call")
			}
			reader, resumeJournal := restartDirectNormalizationRun(t, c, checkpoint, item, rejectReentry)
			forbidden := scriptWorker(failTurn(errors.New("unexpected model call")))
			reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
			ctx := WithSessionResume(t.Context())
			count, err := reader.ResumeInterruptedTasks(ctx)
			if restoreTranscript != nil {
				if count != 0 || localResultRejectionReason(err) != RepairFailureCommit || todoByID(t, reader, item.ID).Status != TaskInProgress {
					t.Fatalf("temporary source fault was settled as permanent: count=%d status=%s error=%v", count, todoByID(t, reader, item.ID).Status, err)
				}
				if err := restoreTranscript(); err != nil {
					t.Fatal(err)
				}
				count, err = reader.ResumeInterruptedTasks(ctx)
				if err != nil || count != 1 || todoByID(t, reader, item.ID).Status != TaskDone || forbidden.Calls() != 0 {
					t.Fatalf("retry changed the dispatch: count=%d status=%s model calls=%d error=%v", count, todoByID(t, reader, item.ID).Status, forbidden.Calls(), err)
				}
				return
			}
			if count != 1 || localResultRejectionReason(err) != RepairFailureSource {
				t.Fatalf("unavailable source did not settle: count=%d error=%v", count, err)
			}
			if err := resumeJournal.Close(); err != nil {
				t.Fatal(err)
			}
			checkpoint = normalizationResumeProjection(t, reader)
			again, _ := restartDirectNormalizationRun(t, c, checkpoint, item, rejectReentry)
			again.workerAgentOverride, again.repairAgentOverride = forbidden, forbidden
			count, err = again.ResumeInterruptedTasks(ctx)
			blocked := todoByID(t, again, item.ID)
			receipt := blocked.ExecutionReceipt
			if err != nil || count != 0 || blocked.Status != TaskBlocked || receipt == nil ||
				receipt.WorkSubmissionFailure != RepairFailureSource || receipt.RepairProvenance == nil ||
				receipt.RepairProvenance.FailureReason != RepairFailureSource || receipt.RunID != sealed.Receipt.RunID ||
				receipt.TranscriptRef != sealed.Receipt.TranscriptRef || forbidden.Calls() != 0 {
				t.Fatalf("source block did not survive restart: count=%d status=%s receipt=%+v model calls=%d error=%v", count, blocked.Status, receipt, forbidden.Calls(), err)
			}
		})
	}
}

func testValidationAfterNormalizationInterruption(t *testing.T, draft string, staged, correction bool) {
	t.Helper()
	originalCalls := 0
	c, events, item := newDirectNormalizationRun(t, func(_ context.Context, raw []byte) ([]byte, error) {
		originalCalls++
		if !staged {
			panic(interruptedProcessSink{})
		}
		return raw, nil
	}, nil)
	wire := normalizationWire(draft)
	interruptBeforeNormalizationValidation(t, c, item, wire, draft, staged, correction)
	checkpoint := LoadSession(c.session.Workspace)
	if err := events.Close(); err != nil || checkpoint == nil {
		t.Fatalf("recoverable checkpoint unavailable: %v", err)
	}
	actionCalls := 0
	reader, journal := restartDirectNormalizationRun(t, c, checkpoint, item, func(_ context.Context, raw []byte) ([]byte, error) {
		actionCalls++
		return raw, nil
	})
	forbidden := scriptWorker(failTurn(errors.New("unexpected model invocation")))
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
	wantCorrection := staged && !correction
	const correctedDraft = `{"verdict":"approve","findings":[]}`
	var repair *scriptedWorker
	if wantCorrection {
		model, _ := normalizationStreamedSubmission(reader, item.ID, normalizationWire(correctedDraft), nil)
		repair = scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
			if !strings.Contains(call.Prompt, wire) || !strings.Contains(call.Prompt, "Last rejected submit_result") {
				t.Error("the one correction did not receive the exact rejected submission")
			}
			return model.Stream(ctx, call)
		}})
		reader.repairAgentOverride = repair
	}
	ctx := WithSessionResume(withTestProtocolRepairInvocationContext(t.Context()))
	count, err := reader.ResumeInterruptedTasks(ctx)
	got := todoByID(t, reader, item.ID)
	if wantCorrection {
		p := got.ExecutionReceipt.RepairProvenance
		if err != nil || count != 1 || got.Status != TaskDone || !p.Success || p.RepairAttempts != 1 ||
			forbidden.Calls() != 0 || repair.Calls() != 1 || actionCalls != 1 || originalCalls != 1 {
			t.Fatalf("schema correction: count=%d err=%v status=%s provenance=%+v worker/repair/action calls=%d/%d/%d",
				count, err, got.Status, p, forbidden.Calls(), repair.Calls(), actionCalls)
		}
		return
	}
	wantReason, wantAttempts, wantOriginalCalls := RepairFailureAction, 0, 1
	if correction {
		wantReason, wantAttempts, wantOriginalCalls = RepairFailureInvalidSchema, 1, 2
	}
	if count != 1 || err == nil || got.Status != TaskBlocked || got.ExecutionReceipt == nil ||
		got.ExecutionReceipt.RepairProvenance.FailureReason != wantReason || got.ExecutionReceipt.RepairProvenance.RepairAttempts != wantAttempts {
		t.Fatalf("interrupted normalization did not settle: count=%d err=%v task=%+v", count, err, got)
	}
	history, err := journal.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(history)
	if err != nil {
		t.Fatal(err)
	}
	if blocked := todoItemByID(replayed, item.ID); blocked == nil || blocked.Status != TaskBlocked ||
		blocked.ExecutionReceipt.RepairProvenance.FailureReason != wantReason {
		t.Fatal("replay lost the typed terminal normalization failure")
	}
	if count, err := reader.ResumeInterruptedTasks(ctx); count != 0 || err != nil ||
		forbidden.Calls() != 0 || originalCalls != wantOriginalCalls || actionCalls != 0 {
		t.Fatalf("second resume repeated work: count=%d err=%v model/action calls=%d/%d", count, err, forbidden.Calls(), actionCalls)
	}
}

func interruptBeforeNormalizationValidation(t *testing.T, c *Coordinator, item *TodoItem, wire, draft string, staged, correction bool) {
	t.Helper()
	tool := c.gatePolicyTools([]fantasy.AgentTool{&submitResultTool{coordinator: c, todoID: item.ID}})[0]
	interrupted := scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		const callID = "interrupted-validation"
		if err := call.OnToolCall(fantasy.ToolCallContent{ToolCallID: callID, ToolName: submitResultToolName, Input: wire}); err != nil {
			return nil, err
		}
		if !staged {
			_, err := tool.Run(ctx, fantasy.ToolCall{ID: callID, Name: submitResultToolName, Input: wire})
			return nil, err
		}
		identity, err := submitResultRuntimeIdentityFromContext(ctx, c, item.ID)
		if err != nil {
			return nil, err
		}
		_, err = c.normalizeDirectSubmission(ctx, identity, SubmitResultInput{normalizerSubmission: wire, normalizerDraft: []byte(draft)})
		if err != nil {
			return nil, err
		}
		panic(interruptedProcessSink{})
	}})
	c.workerAgentOverride = interrupted
	if correction {
		c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizationWire(rejectedNormalizationDraft), nil)
		c.repairAgentOverride = interrupted
	}
	defer func() {
		if reason := recover(); reason != (interruptedProcessSink{}) {
			t.Fatalf("expected interruption before schema validation, got %v", reason)
		}
	}()
	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(item), item.ID)
	t.Logf("execution returned before interruption: %v", err)
}

func TestSealedNormalizationBlocksWhenOriginalIsolatedWorldIsGone(t *testing.T) {
	workerRuns := 0
	fixture := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
		return &isolatedEditAgent{c: c, edit: func(string, string, int) error {
			workerRuns++
			return errors.New("worker unexpectedly replayed")
		}}
	})
	item, world := fixture.crashedProtocolIncompleteTask(t, "settle original isolated review")
	if err := os.RemoveAll(world.worldDir()); err != nil {
		t.Fatal(err)
	}
	// This boundary receives the receipt after the checkpoint and its
	// original sources have already been verified by ResumeInterruptedTasks.
	itemCopy := *item
	itemCopy.ResultContract = &ResultContractRef{Normalizer: "canonicalize", NormalizerSHA256: "provider-id"}
	digest := sha256HexBytes([]byte("sealed"))
	itemCopy.ExecutionReceipt = &ExecutionReceipt{
		RepairProvenance: &RepairProvenance{FailureReason: RepairFailureCommit},
		InterruptedResultNormalization: ResultNormalizationEvidence{
			SubmissionSHA256: digest, OccurrenceSHA256: digest, RawSHA256: digest, OutputSHA256: digest, ProviderSHA256: "provider-id",
		},
	}
	_, _, _, handled, err := fixture.c.resumeIsolatedProtocolRepair(t.Context(), taskDefFromTodoItem(&itemCopy), &itemCopy)
	if !handled || err == nil || workerRuns != 0 || fixture.c.todoItemByID(item.ID).Status != TaskBlocked {
		t.Fatalf("missing original world: handled=%v error=%v worker runs=%d", handled, err, workerRuns)
	}
}

func testDirectNormalizationInterruption(t *testing.T, abrupt, correction, verbatim bool) {
	const draft = `{"verdict": "APPROVE"}`
	const output = `{"verdict":"approve","findings":[]}`
	actionCalls := 0
	c, events, item := newDirectNormalizationRun(t, func(_ context.Context, raw []byte) ([]byte, error) {
		actionCalls++
		if correction && string(raw) == rejectedNormalizationDraft {
			return raw, nil
		}
		if string(raw) != draft {
			t.Fatalf("draft = %s", raw)
		}
		return []byte(output), nil
	}, func(spec *TodoSpec) {
		if verbatim {
			spec.OutputMode = TaskOutputModeVerbatim
		}
	})
	const evidenceFile = "observations.txt"
	const originalArtifact = "original worker evidence"
	if err := os.WriteFile(filepath.Join(c.session.Workspace, evidenceFile), []byte(originalArtifact), 0o600); err != nil {
		t.Fatal(err)
	}
	unexpectedRepair := scriptWorker(textTurn("unexpected repair"))
	c.repairAgentOverride = unexpectedRepair
	wire := `{"status":"success","summary":"reviewed","artifacts":[{"path":"` + evidenceFile + `"}],"structured_payload":` + draft + `}`
	expectedArtifact := originalArtifact
	corrections := 0
	if correction {
		wire = `{"status":"success","summary":"reviewed","structured_payload":` + draft + `}`
		expectedArtifact = ""
		corrections = 1
	}
	runCtx := withTestProtocolRepairInvocationContext(t.Context())
	legacyOperator := !abrupt && correction
	if abrupt {
		stopBeforeDirectNormalizationReceipt(t, runCtx, c, item, wire, correction)
	} else {
		streamed, model := normalizationStreamedSubmission(c, item.ID, wire, interruptedResultSink{})
		worker := model
		c.workerAgentOverride = streamed
		if correction {
			c.repairAgentOverride = streamed
			c.workerAgentOverride, worker = normalizationStreamedSubmission(c, item.ID, normalizationWire(rejectedNormalizationDraft), nil)
		}
		_, err := c.executeTask(runCtx, taskDefFromTodoItem(item), item.ID)
		if err == nil || worker.calls != 1 || model.calls != 1 {
			t.Fatalf("interruption outcome=%v worker/failing submission calls=%d/%d", err, worker.calls, model.calls)
		}
		if legacyOperator {
			// Older runs left a sealed correction blocked. Such receipts remain
			// operator-resumable after upgrading to the ordinary resume path.
			if err := c.PersistFailureWithClassAndStatusError(item.Agent, item.Desc, item.ID, "sealed correction", NeedsHuman, FailureProtocol, TaskBlocked); err != nil {
				t.Fatal(err)
			}
		}
		c.saveCheckpoint()
	}
	if actionCalls != 1+corrections || unexpectedRepair.Calls() != 0 {
		t.Fatalf("normalizer/unexpected repair calls=%d/%d", actionCalls, unexpectedRepair.Calls())
	}
	checkpoint := LoadSession(c.session.Workspace)
	if checkpoint == nil {
		t.Fatal("interrupted attempt checkpoint missing")
	}
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.session.Workspace, evidenceFile), []byte("new worktree contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	persisted := todoItemByID(checkpoint.Tasks, item.ID)
	activeStatus := TaskInProgress
	if correction {
		activeStatus = TaskProtocolIncomplete
	}
	if persisted == nil || (abrupt && (persisted.Status != activeStatus || canResumeDirectNormalization(persisted))) ||
		(!abrupt && !canResumeDirectNormalization(persisted)) {
		t.Fatalf("wrong interruption boundary in durable checkpoint: %+v", persisted)
	}
	// Inspect the stored stage from its receipt alone, after discarding the
	// original worker and normalizer instances.
	reader, resumeEvents := restartDirectNormalizationRun(t, c, checkpoint, item, func(context.Context, []byte) ([]byte, error) {
		t.Error("direct continuation invoked the normalizer")
		return nil, errors.New("unexpected normalizer call")
	})
	forbidden := scriptWorker(failTurn(errors.New("unexpected model call")))
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
	ctx := WithResultOnlyRepair(WithSessionResume(t.Context()), item.ID)
	receipt := persisted.ExecutionReceipt
	if abrupt {
		loaded, err := reader.loadInterruptedNormalizationReceipt(ctx, persisted)
		receipt = loaded
		if err != nil || receipt == nil {
			t.Fatalf("original dispatch lacks a recoverable normalization checkpoint: %v", err)
		}
	}
	evidence := receipt.InterruptedResultNormalization
	if evidence.RawSHA256 != sha256HexBytes([]byte(draft)) || evidence.OutputSHA256 != sha256HexBytes([]byte(output)) ||
		evidence.ProviderSHA256 != item.ResultContract.NormalizerSHA256 || !validCollectionDigest(evidence.SubmissionSHA256) {
		t.Fatalf("sealed normalization identity = %+v", evidence)
	}
	if !legacyOperator {
		ctx = WithSessionResume(t.Context())
		if count, err := reader.ResumeInterruptedTasks(ctx); err != nil || count != 1 {
			t.Fatalf("crash continuation count=%d error=%v", count, err)
		}
	} else {
		authorizeInterruptedDirectNormalization(t, ctx, reader, item.ID, evidence)
		authorized := todoByID(t, reader, item.ID)
		if _, err := reader.resumeProtocolIncompleteTask(ctx, taskDefFromTodoItem(authorized), authorized); err != nil {
			t.Fatal(err)
		}
	}
	assertDirectNormalizationContinuation(t, reader, resumeEvents, item.ID, forbidden, actionCalls, evidence, expectedArtifact, corrections, legacyOperator)
	if verbatim {
		assertRecoveredNormalizationTranscript(t, reader, item.ID, receipt)
	}
}

func normalizationWire(draft string) string {
	return `{"status":"success","summary":"reviewed","structured_payload":` + draft + `}`
}

func assertDirectNormalizationSubmission(t *testing.T, c *Coordinator, evidence ResultNormalizationEvidence, wire string) ResultNormalizationEvidence {
	t.Helper()
	body, found, err := workspaceDocs(c.session.Workspace).getDocument(directNormalizationSubmissionPrefix + evidence.SubmissionSHA256)
	var submission directNormalizationSubmission
	if err != nil || !found || sha256HexBytes(body) != evidence.SubmissionSHA256 || json.Unmarshal(body, &submission) != nil || string(submission.Wire) != wire {
		t.Fatalf("interruption receipt did not preserve the original submission: found=%t err=%v", found, err)
	}
	draft, err := submittedNormalizerDraft(submission.Wire, true)
	if err != nil || sha256HexBytes(draft) != evidence.RawSHA256 {
		t.Fatalf("sealed submission did not reproduce the original draft: err=%v", err)
	}
	evidence.SubmissionSHA256 = ""
	return evidence
}

func normalizationStreamedSubmission(c *Coordinator, taskID, wire string, sink TaskResultSink) (fantasy.Agent, *rejectedResultStreamModel) {
	model := &rejectedResultStreamModel{toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls, input: func(int) string { return wire }}
	tools := c.gatePolicyTools([]fantasy.AgentTool{&submitResultTool{coordinator: c, todoID: taskID, sink: sink}})
	return fantasy.NewAgent(model, fantasy.WithTools(tools...), fantasy.WithMaxRetries(0)), model
}

func assertRecoveredNormalizationTranscript(t *testing.T, c *Coordinator, taskID string, receipt *ExecutionReceipt) {
	t.Helper()
	item := todoByID(t, c, taskID)
	result := item.TypedResult
	ref := result.Outputs[rawTranscriptOutputName].Artifact
	if ref == nil || result.RawOutputRef == nil || ref.ID != receipt.TranscriptRef || ref.ID != result.RawOutputRef.ID ||
		!strings.Contains(item.Output, "artifact_ref="+ref.ID) {
		t.Fatalf("recovered verbatim output lacks original transcript: %+v", result)
	}
	secret, err := GetSystemSecret()
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range result.Evidence {
		if evidence.Type == "task_transcript" && evidence.Value == ref.ID && VerifyEvidenceSignature(evidence, secret, taskID, receipt.RunID) {
			return
		}
	}
	t.Fatal("recovered verbatim transcript was not signed under the original run")
}

func stopBeforeDirectNormalizationReceipt(t *testing.T, ctx context.Context, c *Coordinator, item *TodoItem, wire string, correction bool) {
	t.Helper()
	tool := c.gatePolicyTools([]fantasy.AgentTool{&submitResultTool{coordinator: c, todoID: item.ID, sink: interruptedProcessSink{}}})[0]
	interrupted := scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		const id = "original-submission"
		if err := call.OnToolCall(fantasy.ToolCallContent{ToolCallID: id, ToolName: submitResultToolName, Input: wire}); err != nil {
			return nil, err
		}
		response, err := tool.Run(ctx, fantasy.ToolCall{ID: id, Name: submitResultToolName, Input: wire})
		return nil, fmt.Errorf("submit returned before process stop: %v %s", err, response.Content)
	}})
	if correction {
		worker := &rejectedResultStreamModel{toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls, input: func(int) string {
			return `{"status":"success","summary":"reviewed","structured_payload":` + rejectedNormalizationDraft + `}`
		}}
		c.workerAgentOverride = fantasy.NewAgent(worker, fantasy.WithTools(c.gatePolicyTools([]fantasy.AgentTool{
			&submitResultTool{coordinator: c, todoID: item.ID},
		})...), fantasy.WithMaxRetries(0))
		c.repairAgentOverride = interrupted
	} else {
		c.workerAgentOverride = interrupted
	}
	defer func() {
		if reason := recover(); reason != (interruptedProcessSink{}) {
			t.Fatalf("expected to stop before the attempt receipt, got %v", reason)
		}
	}()
	_, err := c.executeTask(ctx, taskDefFromTodoItem(item), item.ID)
	t.Logf("execution returned before commit barrier: %v", err)
}

func authorizeInterruptedDirectNormalization(t *testing.T, ctx context.Context, reader *Coordinator, taskID string, evidence ResultNormalizationEvidence) {
	t.Helper()
	docs := workspaceDocs(reader.session.Workspace)
	key := directNormalizationSubmissionPrefix + evidence.SubmissionSHA256
	original, found, err := docs.getDocument(key)
	if err != nil || !found {
		t.Fatalf("original envelope missing: %v", err)
	}
	if err := docs.putDocument(key, []byte(`{"wire":"changed"}`)); err != nil {
		t.Fatal(err)
	}
	if err := reader.validateResumeRepairSources(ctx, taskID); err == nil || !strings.Contains(err.Error(), "original normalization submission evidence is invalid") {
		t.Fatalf("source preflight accepted a corrupt submission: %v", err)
	}
	if err := reader.authorizeOperatorResultRepair(ctx); err == nil || todoByID(t, reader, taskID).Status != TaskBlocked {
		t.Fatalf("operator authorization accepted a corrupt submission: %v", err)
	}
	if err := docs.putDocument(key, original); err != nil {
		t.Fatal(err)
	}
	if err := reader.validateResumeRepairSources(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if err := reader.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertDirectNormalizationContinuation(t *testing.T, reader *Coordinator, resumeEvents *EventStore, taskID string, forbidden *scriptedWorker, actionCalls int, evidence ResultNormalizationEvidence, originalArtifact string, corrections int, operator bool) {
	t.Helper()
	done := todoByID(t, reader, taskID)
	accepted := done.TypedResult
	if done.Status != TaskDone || accepted == nil || accepted.StructuredPayload == nil || forbidden.Calls() != 0 || actionCalls != 1+corrections {
		t.Fatalf("local continuation status=%s result=%+v model/normalizer calls=%d/%d", done.Status, accepted, forbidden.Calls(), actionCalls)
	}
	normalized := accepted.StructuredPayload.Normalization
	if originalArtifact == "" && len(accepted.Artifacts) != 0 ||
		originalArtifact != "" && (len(accepted.Artifacts) != 1 || accepted.Artifacts[0].SHA256 != sha256HexBytes([]byte(originalArtifact))) {
		t.Fatalf("original artifact not preserved: %+v", accepted.Artifacts)
	}
	provenance := done.ExecutionReceipt.RepairProvenance
	if normalized == nil || normalized.OccurrenceSHA256 != evidence.OccurrenceSHA256 || sha256HexBytes(normalized.Raw) != evidence.RawSHA256 ||
		normalized.OutputSHA256 != evidence.OutputSHA256 || accepted.StructuredPayload.SHA256 != sha256HexBytes(accepted.StructuredPayload.Value) ||
		!provenance.Success || provenance.RepairAttempts != corrections {
		t.Fatalf("local continuation changed sealed identity or repair budget: %+v, %+v", normalized, provenance)
	}
	if operator && (len(provenance.OperatorGrants) != 1 || provenance.OperatorGrants[0].AdditionalAttempts != 0) || !operator && len(provenance.OperatorGrants) != 0 {
		t.Fatalf("unexpected continuation grants: %+v", provenance.OperatorGrants)
	}
	history, err := resumeEvents.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(history)
	if err != nil {
		t.Fatal(err)
	}
	replay := todoItemByID(replayed, taskID)
	if replay == nil || replay.Status != TaskDone || replay.TypedResult == nil || !receiptMatchesResultPayload(replay.ExecutionReceipt, replay.TypedResult.StructuredPayload) ||
		replay.ExecutionReceipt.RepairProvenance.RepairAttempts != corrections || replay.ExecutionReceipt.InterruptedResultNormalization != evidence {
		t.Fatal("replay lost the accepted payload or original interruption receipt")
	}
}
