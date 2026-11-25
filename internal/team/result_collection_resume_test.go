package team

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/sidecar"
)

func TestResultCollectionResumesOnlyOriginalArtifactCapabilities(t *testing.T) {
	c, item, ctx, receipt := resultCollectionFixture(t)
	ref, err := mustArtifactStore(t, c.session.Workspace).Put(t.Context(), PutArtifactRequest{Kind: "evidence", Path: "source.txt", Content: []byte("verified original evidence")})
	if err != nil {
		t.Fatal(err)
	}
	originalScope := &ArtifactAccessScope{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, StoreRoot: c.artifactStoreRootPath(), AuthorizedRefs: []ArtifactRef{ref.ArtifactRef}}
	receipt.ArtifactScope = originalScope
	before, err := collectionDigest(originalScope)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "completed original work")
	if err != nil {
		t.Fatal(err)
	}
	c.revokeTaskOccurrence(item.ID)
	c.executionRunID = "new-result-recovery"
	stale := context.WithValue(ctx, artifactAccessScopeKey, originalScope)
	if _, ok := c.authorizedDecisionEvidence(stale, ref.ID); ok {
		t.Fatal("old invocation scope gained authority without sealed-work recovery")
	}
	restored, err := c.restoreCollectionContext(WithSessionResume(t.Context()), cp)
	if err != nil {
		t.Fatal(err)
	}
	if current, ok := c.authorizedDecisionEvidence(restored, ref.ID); !ok || current.SHA256 != ref.SHA256 {
		t.Fatal("recovered scope lost original artifact authority")
	}
	for _, bad := range []context.Context{context.WithValue(restored, todoIDKey{}, "different-task"), context.WithValue(restored, executionAttemptKey{}, 2)} {
		if _, ok := c.authorizedDecisionEvidence(bad, ref.ID); ok {
			t.Fatal("recovered scope escaped its task or attempt")
		}
	}
	if _, ok := c.authorizedDecisionEvidence(restored, "unknown"); ok {
		t.Fatal("recovery added an artifact capability")
	}
	after, err := collectionDigest(originalScope)
	if err != nil || before != after || originalScope.RunID == c.executionRunID {
		t.Fatal("recovery rewrote the sealed original scope", err)
	}
}

func TestResultCollectionResumeCompletesDurableClaimsWithoutWorkerOrProvider(t *testing.T) {
	for _, state := range []ResultCollectionState{ResultCollectionReceived, ResultCollectionPrepared, ResultCollectionCommitted} {
		t.Run(string(state), func(t *testing.T) {
			c, item, ctx, checkpoint := beginCollectionFixture(t)
			worker := scriptWorker(textTurn("must never execute"))
			c.workerAgentOverride = worker
			var err error
			checkpoint, err = c.reserveResultCollection(ctx, checkpoint, "collector-1", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, []byte(`{"status":"success","summary":"analysis complete","files_read":["source.go"],"files_modified":["result.go"]}`))
			if err != nil {
				t.Fatal(err)
			}
			if state != ResultCollectionReceived {
				checkpoint, err = c.prepareCollectionClaims(ctx, checkpoint)
				if err != nil {
					t.Fatal(err)
				}
			}
			if state == ResultCollectionCommitted {
				if err := c.commitPreparedCollection(ctx, checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			events, err := c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c.revokeTaskOccurrence(item.ID)
			c.taskTracker = NewTaskTracker()
			c.taskTracker.TodoList().AddReserved(ReduceToTodoList(events))
			restored := c.todoItemByID(item.ID)
			output, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(restored), restored.ID)
			if err != nil {
				t.Fatal(err)
			}
			current := c.todoItemByID(item.ID)
			if current.Status != TaskDone || current.TypedResult == nil || current.TypedResult.Source != "collected" || output == "" {
				t.Fatalf("completion=%+v output=%q", current, output)
			}
			if worker.Calls() != 0 || current.ExecutionReceipt.ResultCollection.TurnsUsed != 1 {
				t.Fatal("resume repeated worker execution or spent another model turn")
			}
			if len(current.TypedResult.FilesRead) != 1 || current.TypedResult.FilesRead[0].Path != "source.go" || len(current.TypedResult.FilesModified) != 1 || current.TypedResult.FilesModified[0].Path != "result.go" {
				t.Fatal("resume lost canonical file entries")
			}
		})
	}
}

func TestResultCollectionSealsReturnedWorkBeforeReadingTranscript(t *testing.T) {
	for _, temporary := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "temporary"}[temporary], func(t *testing.T) {
			var collections atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
					return
				}
				collections.Add(1)
				writeNativeRepairStream(t, w, map[string]any{"content": `{"status":"success","summary":"original transcript recovered"}`}, "stop", "test")
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true)
			restore := faultNormalizedEvidencePath(t, normalizedArtifactData(c, receipt.TranscriptRef), temporary)
			err := c.collectReturnedWork(ctx, c.session.Agents["worker"], receipt, "complete original work", "input")
			want := RepairFailureSource
			if temporary {
				want = RepairFailureCommit
			}
			if localResultRejectionReason(err) != want {
				t.Fatalf("transcript classification = %v, want %s", err, want)
			}
			c = reopenRecoveryCoordinator(t, c)
			before := todoByID(t, c, item.ID)
			cp, err := c.readReturnedCollection(before, 0)
			if err != nil || cp == nil || before.Status != TaskInProgress || currentResultCollection(before).Work != cp.Work {
				t.Fatalf("original transcript stopped before sealing: status=%s checkpoint=%+v err=%v", before.Status, cp, err)
			}
			forbidden := scriptWorker(failTurn(errors.New("work or repair repeated")))
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			c.executionRunID = "resume-transcript-collection"
			if temporary {
				restore()
			}
			count, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			after := todoByID(t, c, item.ID)
			if count != 1 || forbidden.Calls() != 0 || currentResultCollection(after).Work != cp.Work {
				t.Fatalf("work changed across resume: count=%d reruns=%d error=%v", count, forbidden.Calls(), err)
			}
			if temporary && (err != nil || after.Status != TaskDone || currentResultCollection(after).TurnsUsed != 1 || collections.Load() != 1) ||
				!temporary && (localResultRejectionReason(err) != RepairFailureSource || after.Status != TaskBlocked || currentResultCollection(after).TurnsUsed != 0 || collections.Load() != 0) {
				t.Fatalf("transcript recovery status=%s collections=%d error=%v", after.Status, collections.Load(), err)
			}
		})
	}
}

func TestResultCollectionBlockedResumeCannotReplayWork(t *testing.T) {
	c, item, ctx, checkpoint := beginCollectionFixture(t)
	worker := scriptWorker(textTurn("must never execute"))
	c.workerAgentOverride = worker
	if _, err := c.blockResultCollection(ctx, checkpoint, RepairFailureSource); err != nil {
		t.Fatal(err)
	}
	_, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(item), item.ID)
	if err == nil || worker.Calls() != 0 || c.todoItemByID(item.ID).Status != TaskBlocked {
		t.Fatalf("blocked collection was dispatched: calls=%d err=%v", worker.Calls(), err)
	}
}

func TestResultCollectionRequestEvidenceIsVerifiedBeforeSpendingTurn(t *testing.T) {
	for _, scenario := range []string{"initial", "repair", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			var collections atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
					return
				}
				collections.Add(1)
				writeNativeRepairStream(t, w, map[string]any{"content": `{"status":"success","summary":"work kept after request storage recovery"}`}, "stop", "test")
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true)
			cp, err := c.beginResultCollection(ctx, item.ID, receipt, "completed original work")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "repair" {
				cp, err = c.reserveResultCollection(ctx, cp, "original-invocation", []byte(`{"tools":[]}`))
				if err != nil {
					t.Fatal(err)
				}
				cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":`))
				if err != nil {
					t.Fatal(err)
				}
				cp, err = c.recordCollectionFormatFailure(ctx, cp)
				if err != nil {
					t.Fatal(err)
				}
			}
			worker := c.session.Agents[item.Agent]
			invocation, request, err := c.prepareCollectionInvocation(ctx, worker, cp, "faulted-invocation", "")
			if err != nil {
				t.Fatal(err)
			}
			prospective := *cp
			prospective.InvocationID, prospective.InvocationRunID = "faulted-invocation", c.executionRunID
			ref, err := c.putCollectionBlob(ctx, &prospective, "request", request)
			if err != nil {
				t.Fatal(err)
			}
			path := normalizedArtifactData(c, ref.ArtifactID)
			want := RepairFailureCommit
			var restore func()
			if scenario == "corrupt" {
				want = RepairFailureSource
				if err := os.WriteFile(path, []byte("wrong immutable request bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				restore = faultNormalizedEvidencePath(t, path, true)
			}
			_, err = c.executeFrozenCollectionTurn(ctx, invocation, worker, cp, prospective.InvocationID, request)
			if localResultRejectionReason(err) != want || collections.Load() != 0 {
				t.Fatalf("request failure = %v, provider calls = %d, want %s and zero calls", err, collections.Load(), want)
			}
			c = reopenRecoveryCoordinator(t, c)
			before := currentResultCollection(todoByID(t, c, item.ID))
			if before == nil || before.Work != cp.Work || before.TurnsUsed != cp.TurnsUsed {
				t.Fatalf("failure changed original work or spent a turn: %+v", before)
			}
			if want == RepairFailureSource {
				if before.State != ResultCollectionBlocked || before.FailureReason != want {
					t.Fatalf("invalid immutable request was not blocked: %+v", before)
				}
				return
			}
			if before.State != cp.State || !sameCanonicalEvidence(before.Response, cp.Response) {
				t.Fatalf("storage fault replaced previous progress: before=%+v after=%+v", cp, before)
			}
			restore()
			forbidden := scriptWorker(failTurn(errors.New("work or legacy repair repeated")))
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			c.executionRunID = "restart-before-request-reservation"
			count, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			item = todoByID(t, c, item.ID)
			after := currentResultCollection(item)
			if err != nil || count != 1 || item.Status != TaskDone || after == nil || after.Work != cp.Work ||
				after.TurnsUsed != cp.TurnsUsed+1 || collections.Load() != 1 || forbidden.Calls() != 0 {
				t.Fatalf("resume status=%s checkpoint=%+v count=%d collector=%d worker=%d err=%v", item.Status, after, count, collections.Load(), forbidden.Calls(), err)
			}
		})
	}
}

func TestResultCollectionFanoutResumesPreparedLeafWithoutWorker(t *testing.T) {
	parent, item, _, receipt := resultCollectionFixture(t)
	shared := filepath.Join(parent.session.Workspace, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(shared, "original.txt")
	if err := os.WriteFile(source, []byte("shared at dispatch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original.txt", filepath.Join(shared, "source-link")); err != nil {
		t.Fatal(err)
	}
	leafWorkspace, err := parent.createFanoutWorkspace(item.Agent, receipt.Model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leafWorkspace, "delivery.txt"), []byte("durable leaf delivery"), 0o600); err != nil {
		t.Fatal(err)
	}
	leaf := cloneCoordinator(parent, cloneSession(parent.session, leafWorkspace))
	leaf.modelFanout = &modelFanoutBinding{parent: parent, taskID: item.ID, index: 0, target: receipt.ExecutionTarget}
	leaf.taskTracker = NewTaskTracker()
	leaf.taskTracker.TodoList().AddReserved([]*TodoItem{cloneTodoItem(item)})
	leafCtx := occurrenceTestContext(leaf, item.ID, receipt.Attempt)
	checkpoint, err := leaf.beginResultCollection(leafCtx, item.ID, receipt, "complete work report")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = leaf.reserveResultCollection(leafCtx, checkpoint, "leaf-collector", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = leaf.recordCollectionResponse(leafCtx, checkpoint, []byte(`{"status":"success","summary":"analysis complete","artifacts":[{"path":"delivery.txt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.prepareCollectionClaims(leafCtx, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("changed after dispatch"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := parent.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parent.revokeTaskOccurrence(item.ID)
	parent.taskTracker = NewTaskTracker()
	parent.taskTracker.TodoList().AddReserved(ReduceToTodoList(events))
	forbidden := scriptWorker(failTurn(errors.New("recovered leaf dispatched the worker")))
	parent.workerAgentOverride = forbidden
	task := taskDefFromTodoItem(parent.todoItemByID(item.ID))
	def := cloneAgentDef(parent.session.Agents["worker"])
	recovery, err := parent.prepareFanoutCollectionLeaf(t.Context(), parent.todoItemByID(item.ID), 0, task.ExecutionTopology[0])
	if err != nil {
		t.Fatal(err)
	}
	output, err := parent.executeSingleAgentWithModel(WithSessionResume(t.Context()), "worker", def, task, item.ID, "main", 0, &recovery)
	if err != nil {
		t.Fatal(err)
	}
	current := parent.todoItemByID(item.ID)
	if output == "" || current.Status != TaskInProgress || current.TypedResult != nil {
		t.Fatalf("leaf completed parent: status=%s output=%q", current.Status, output)
	}
	if current.ExecutionReceipt.ResultCollection.State != ResultCollectionCommitted || current.ExecutionReceipt.ResultCollection.TurnsUsed != 1 || forbidden.Calls() != 0 {
		t.Fatal("leaf recovery spent another model turn")
	}
	retained := filepath.Join(leafWorkspace, "shared", "source-link")
	if target, err := os.Readlink(retained); err != nil || target != "original.txt" {
		t.Fatalf("retained shared symlink = %q, %v", target, err)
	}
	if original, err := os.ReadFile(retained); err != nil || string(original) != "shared at dispatch" {
		t.Fatalf("retained shared evidence = %q, %v", original, err)
	}
	accepted, err := parent.acceptedCollectionResult(t.Context(), current, current.ExecutionReceipt.ResultCollection)
	if err != nil || len(accepted.Artifacts) != 1 {
		t.Fatalf("leaf artifact admission: %v", err)
	}
	if err := os.RemoveAll(leafWorkspace); err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileArtifactStoreReadOnly(parent.artifactStoreRootPath(), parent.artifactStoreRootPath())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.Open(t.Context(), accepted.Artifacts[0].ID)
	if err != nil {
		t.Fatalf("accepted leaf artifact depends on disposable workspace: %v", err)
	}
	body, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || string(body) != "durable leaf delivery" {
		t.Fatalf("durable delivery=%q read=%v close=%v", body, err, closeErr)
	}

}

func TestResultCollectionFanoutRestoresUnpublishedWorkBySlot(t *testing.T) {
	parent, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
		// Repeated targets must still have independent work seals.
		spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
	})
	first := sealFanoutWorkForRecovery(t, parent, item, receipt, 0)
	execJournal(t, parent.session.Workspace, `CREATE TRIGGER refuse_fanout_collection BEFORE INSERT ON events WHEN NEW.type='task_result_collection_checkpointed' BEGIN SELECT RAISE(ABORT, 'fixture leaf checkpoint write failure'); END`)
	second := sealFanoutWorkForRecovery(t, parent, item, receipt, 1)
	execJournal(t, parent.session.Workspace, "DROP TRIGGER refuse_fanout_collection")
	reader := reopenRecoveryCoordinator(t, parent)
	before := todoByID(t, reader, item.ID)
	if len(before.ExecutionReceipts) != 1 || before.ExecutionReceipts[0].ResultCollection.Work != first.Work {
		t.Fatalf("unpublished leaf appeared in journal projection: %+v", before.ExecutionReceipts)
	}
	if err := reader.restoreReturnedCollections(t.Context(), before); err != nil {
		t.Fatal(err)
	}
	for slot, original := range []*ResultCollectionCheckpoint{first, second} {
		recovery, err := reader.prepareFanoutCollectionLeaf(t.Context(), todoByID(t, reader, item.ID), slot, receipt.ExecutionTarget)
		checkpoint, work := recovery.checkpoint, recovery.work
		if err != nil || checkpoint == nil || work == nil || checkpoint.Work != original.Work || checkpoint.TurnsUsed != 0 || work.Receipt.ModelExecutionID != original.ModelExecutionID {
			t.Fatalf("leaf %d lost its work: checkpoint=%+v error=%v", slot, checkpoint, err)
		}
	}
}

func TestResultCollectionFanoutFreshSlotIgnoresSiblingCheckpoint(t *testing.T) {
	parent, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
		spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
	})
	worker := scriptWorker(textTurn("fresh sibling output"))
	sealFanoutWorkForRecovery(t, parent, item, receipt, 0)
	parent = reopenRecoveryCoordinator(t, parent)
	parent.workerAgentOverride = worker
	if !hasCurrentFanoutCollection(todoByID(t, parent, item.ID)) {
		t.Fatal("sibling's checkpoint was not visible on the parent")
	}
	// Slot 1 reaches the leaf entry after slot 0 has published its checkpoint.
	// The parent's launch mode was captured while neither slot had published.
	task := taskDefFromTodoItem(item)
	output, err := parent.executeSingleAgentWithModel(t.Context(), item.Agent, cloneAgentDef(parent.session.Agents[item.Agent]), task, item.ID, "extra-1", 1, nil)
	if err != nil || worker.Calls() != 1 || !strings.Contains(output, "fresh sibling output") {
		t.Fatalf("delayed sibling was not dispatched: calls=%d output=%q error=%v", worker.Calls(), output, err)
	}
}

func TestResultCollectionFanoutResumesOnlyCurrentDAGOccurrence(t *testing.T) {
	parent, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
		spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
	})
	oldReceipt := receipt
	oldReceipt.Attempt = 2
	oldReceipt.TranscriptRef = fanoutFixtureTranscript(t, parent, item, 2)
	old := sealFanoutWorkForRecovery(t, parent, item, oldReceipt, 0)
	if err := parent.CommitTaskTransition(t.Context(), item.ID, TaskInProgress, TaskDone, "", "first wave", nil); err != nil {
		t.Fatal(err)
	}
	if err := parent.CommitTaskResetForRetry(t.Context(), item.ID, "another DAG wave"); err != nil {
		t.Fatal(err)
	}
	if err := parent.CommitTaskTransition(t.Context(), item.ID, TaskPending, TaskInProgress, "", "", attemptStartMetadata(1)); err != nil {
		t.Fatal(err)
	}
	item = todoByID(t, parent, item.ID)
	receipt.OccurrenceAttempt = item.Retries + 1
	fresh := sealFanoutWorkForRecovery(t, parent, item, receipt, 0)
	parent = reopenRecoveryCoordinator(t, parent)
	current := todoByID(t, parent, item.ID)
	if currentResultCollection(current) != nil || !hasCurrentFanoutCollection(current) {
		t.Fatalf("parent's historical singular receipt hid current leaf: %+v", current.ExecutionReceipt)
	}
	recovery, err := parent.prepareFanoutCollectionLeaf(t.Context(), current, 0, receipt.ExecutionTarget)
	cp := recovery.checkpoint
	if err != nil || cp == nil || cp.Work != fresh.Work || cp.Work == old.Work {
		t.Fatalf("selected earlier DAG wave: checkpoint=%+v error=%v", cp, err)
	}
}

func fanoutFixtureTranscript(t *testing.T, parent *Coordinator, item *TodoItem, attempt int) string {
	t.Helper()
	transcript, err := newTaskTranscriptForAttempt(parent.session.Workspace, item.ID, parent.executionRunID, attempt, item.Agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	if err := transcript.RecordAssistantOutput("original fanout work"); err != nil {
		t.Fatal(err)
	}
	ref, err := sealVerbatimTaskTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	return ref.ID
}

func TestResultCollectionFanoutResumesSealedJudgeAfterTerminalWriteFault(t *testing.T) {
	for _, publication := range []struct{ name, statement string }{
		{"parent_event", "BEFORE INSERT ON events WHEN NEW.type='task_completed'"},
		{"output_document", "BEFORE INSERT ON runtime_documents WHEN NEW.name LIKE 'result-collection-fanout-output:%'"},
	} {
		t.Run(publication.name, func(t *testing.T) {
			testResumesSealedFanoutJudge(t, publication.statement)
		})
	}
}

func testResumesSealedFanoutJudge(t *testing.T, trigger string) {
	t.Helper()
	compiled, err := CompileResultContractDocument("findings.json", []byte(collectedFindingSchema))
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	var normalizations atomic.Int32
	parent, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
		spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
		spec.ResultContract = &ref
	}, func(c *Coordinator) {
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: func(_ context.Context, draft []byte) ([]byte, error) {
			normalizations.Add(1)
			return draft, nil
		}})
	})
	for i := range 2 {
		leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, parent, item, receipt, i)
		if err != nil {
			t.Fatal(err)
		}
		commitNormalizedCollectionFixture(t, leaf, ctx, cp, `{"findings":[]}`)
	}
	parent = reopenRecoveryCoordinator(t, parent)
	graft := scriptWorker(textTurn(`{"reason":"original judge explanation","grafts":[]}`))
	judge := sidecar.NewSidecarForTest("judge", graft)
	var judgments int
	judge.SetDecisionExecutor(func(context.Context, sidecar.DecisionInvocation) (decisionrt.Result, error) {
		judgments++
		return decisionrt.Result{Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "candidate_0"}}, nil
	})
	def := parent.session.Agents[item.Agent]
	parent.SetAgentPool(&mockAgentPool{resolveDef: def, resolveKey: item.Agent, judgeSidecar: judge})
	forbidden := scriptWorker(failTurn(errors.New("accepted leaf ran again")))
	parent.workerAgentOverride, parent.repairAgentOverride = forbidden, forbidden
	parent.providerBoundaryStart = func(context.Context, string) error { return nil }
	execJournal(t, parent.session.Workspace, `CREATE TRIGGER refuse_parent_done `+trigger+` BEGIN SELECT RAISE(ABORT, 'fixture parent completion write failure'); END`)
	task := taskDefFromTodoItem(item)
	_, err = parent.executeTaskWithExtraModels(WithSessionResume(t.Context()), item.Agent, def, task, item.ID)
	if localResultRejectionReason(err) != RepairFailureCommit || judgments != 1 || graft.Calls() != 1 {
		t.Fatalf("terminal fault=%v judgments=%d grafts=%d", err, judgments, graft.Calls())
	}
	parent.terminalizeTaskErrorIfUnresolved(item.ID, err)
	execJournal(t, parent.session.Workspace, "DROP TRIGGER refuse_parent_done")
	parent = reopenRecoveryCoordinator(t, parent)
	before := todoByID(t, parent, item.ID)
	saved, _, err := parent.readFanoutSettlement(t.Context(), before)
	if err != nil || saved == nil || !strings.Contains(saved.Output, "original judge explanation") {
		t.Fatalf("finished judge output not sealed: status=%s output=%+v error=%v", before.Status, saved, err)
	}
	parent.SetAgentPool(&mockAgentPool{resolveDef: def, resolveKey: item.Agent, judgeSidecar: judge})
	parent.workerAgentOverride, parent.repairAgentOverride = forbidden, forbidden
	parent.providerBoundaryStart = func(context.Context, string) error { return errors.New("settled fanout crossed provider boundary") }
	parent.providerBoundaryStarted = false
	count, err := parent.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	after := todoByID(t, parent, item.ID)
	if count != 1 || err != nil || after.Status != TaskDone || after.Output != saved.Output ||
		forbidden.Calls() != 0 || normalizations.Load() != 2 || judgments != 1 || graft.Calls() != 1 {
		t.Fatalf("count=%d status=%s work=%d normalization=%d judge=%d graft=%d error=%v",
			count, after.Status, forbidden.Calls(), normalizations.Load(), judgments, graft.Calls(), err)
	}
}

func sealFanoutWorkForRecovery(t *testing.T, parent *Coordinator, item *TodoItem, receipt ExecutionReceipt, slot int) *ResultCollectionCheckpoint {
	t.Helper()
	_, _, cp, err := beginFanoutWorkForRecovery(t, parent, item, receipt, slot)
	if cp == nil || (slot == 0 && err != nil) || (slot == 1 && localResultRejectionReason(err) != RepairFailureCommit) {
		t.Fatalf("seal leaf %d: checkpoint=%+v error=%v", slot, cp, err)
	}
	return cp
}

func beginFanoutWorkForRecovery(t *testing.T, parent *Coordinator, item *TodoItem, receipt ExecutionReceipt, slot int, prepare ...func(*Coordinator, context.Context, *ExecutionReceipt)) (*Coordinator, context.Context, *ResultCollectionCheckpoint, error) {
	t.Helper()
	workspace, err := os.MkdirTemp(parent.session.Workspace, "leaf-")
	if err != nil {
		t.Fatal(err)
	}
	leaf := cloneCoordinator(parent, cloneSession(parent.session, workspace))
	leaf.modelFanout = &modelFanoutBinding{parent: parent, taskID: item.ID, index: slot, target: receipt.ExecutionTarget}
	leaf.taskTracker = NewTaskTracker()
	leaf.taskTracker.TodoList().AddReserved([]*TodoItem{cloneTodoItem(item)})
	ctx := occurrenceTestContext(leaf, item.ID, receipt.Attempt)
	receipt.ModelExecutionID = fmt.Sprintf("leaf-execution-%d-wave-%d", slot, item.Retries)
	for _, setup := range prepare {
		setup(leaf, ctx, &receipt)
	}
	cp, err := leaf.beginResultCollection(ctx, item.ID, receipt, "complete leaf work")
	return leaf, ctx, cp, err
}

func TestResultCollectionGrantCanContinueEarlierDurableProgress(t *testing.T) {
	for _, reason := range []RepairFailureReason{repairFailureCollectionUnavailable, RepairFailureCommit, RepairFailureSource} {
		t.Run(string(reason), func(t *testing.T) {
			c, item, ctx, checkpoint := beginCollectionFixture(t)
			c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: reason, Checkpoint: checkpoint, Cause: errors.New("reporting interrupted")})
			c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "reporting interrupted", NeedsHuman, FailureProtocol, TaskBlocked, "complete work report")
			c.executionRunID = "resumed-reporting"
			ctx = WithResultOnlyRepair(ctx, item.ID)
			err := c.authorizeOperatorResultRepair(ctx)
			if reason == RepairFailureSource {
				if err == nil {
					t.Fatal("operator grant bypassed a source rejection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			current := c.todoItemByID(item.ID)
			granted := current.ExecutionReceipt.ResultCollection
			if granted.Work != checkpoint.Work || granted.TurnsUsed != 0 || granted.TurnLimit != 2 {
				t.Fatalf("earlier progress lost or reset: %+v", granted)
			}
			provenance := current.ExecutionReceipt.RepairProvenance
			if reason == RepairFailureCommit && provenance.OperatorGrants[0].AdditionalAttempts != 0 {
				t.Fatal("storage continuation added model budget")
			}
			if _, err := c.restoreCollectionContext(ctx, granted); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResultCollectionCommittedGrantFinalizesWithoutReadmission(t *testing.T) {
	c, item, ctx, checkpoint := beginCollectionFixture(t)
	checkpoint, err := c.reserveResultCollection(ctx, checkpoint, "collector-before-commit", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, []byte(`{"status":"success","summary":"analysis complete"}`))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = c.prepareCollectionClaims(ctx, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.commitPreparedCollection(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureCommit, Cause: errors.New("projection interrupted after commit")})
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "projection interrupted", NeedsHuman, FailureProtocol, TaskBlocked, "complete work report")
	c.executionRunID = "finalize-accepted-result"
	ctx = WithResultOnlyRepair(t.Context(), item.ID)
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	current := c.todoItemByID(item.ID)
	if current.ExecutionReceipt.ResultCollection.State != ResultCollectionCommitted {
		t.Fatal("grant demoted accepted bytes to a fresh submission")
	}
	if _, err := c.executeTask(WithSessionResume(ctx), taskDefFromTodoItem(current), item.ID); err != nil {
		t.Fatal(err)
	}
	current = c.todoItemByID(item.ID)
	if current.Status != TaskDone || current.ExecutionReceipt.ResultCollection.TurnsUsed != 1 {
		t.Fatal("accepted result was not finalized locally")
	}
}

func TestCurrentResultCollectionMatchesLifecycleOccurrence(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		retries, work, occurrence int
		current                   bool
	}{
		{"initial", 0, 1, 1, true}, {"execution retry", 1, 1, 1, false}, {"new dispatch", 1, 1, 2, true},
		{"in-dispatch retry", 0, 2, 1, true}, {"legacy current", 0, 1, 0, true}, {"legacy unknown", 1, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := &TodoItem{Retries: tc.retries, ExecutionReceipt: &ExecutionReceipt{Attempt: tc.work, OccurrenceAttempt: tc.occurrence, ResultCollection: &ResultCollectionCheckpoint{Attempt: tc.work}}}
			if (currentResultCollection(item) != nil) != tc.current {
				t.Fatal("collection selected across lifecycle occurrences")
			}
		})
	}
}

func TestResultCollectionDAGRetryExecutesFreshWork(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprint(replay), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"id\":\"fresh\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"{\\\"status\\\":\\\"success\\\",\\\"summary\\\":\\\"corrected work\\\"}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionTaskFixture(t, server.URL+"/v1", true, func(spec *TodoSpec) {
				spec.SideEffect = SideEffectNone
				spec.Recovery = RecoveryRetry
				spec.VerifySpec = &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{Pointer: "/summary", Op: "non_empty"}}}
			})
			// Exercise the same model execution identity used by real dispatches,
			// rather than the fixture's unrelated work-execution-1 placeholder.
			receipt.ModelExecutionID = c.newTaskContextRequest(taskDefFromTodoItem(item), item.ID, 1, ContextTriggerTaskDispatch, item.Agent, "worker", nil).ModelExecutionID
			cp, err := c.beginResultCollection(ctx, item.ID, receipt, "original work")
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.reserveResultCollection(ctx, cp, "first-collector", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":"success","summary":"original work"}`))
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
			if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(item), item.ID); err != nil {
				t.Fatal(err)
			}
			task := taskDefFromTodoItem(item)
			originalReceipt := cloneExecutionReceipt(collectionReceipt(c.todoItemByID(item.ID), cp))
			scheduler := mustNewDAGScheduler(t, c, []TaskDef{task}, []*TodoItem{item}, nil)
			scheduler.states[0] = TaskDone
			if err := scheduler.resetTaskWithRemediation(t.Context(), 0, "verifier requires corrected work", &RemediationContext{FailureClass: FailureVerify, Summary: "correct the report"}); err != nil {
				t.Fatal(err)
			}
			item = c.todoItemByID(item.ID)
			if item.Retries != 1 || currentResultCollection(item) != nil || item.ExecutionReceipt.ResultCollection.State != ResultCollectionCommitted {
				t.Fatal("reset reused or erased historical collection")
			}
			if replay {
				events, err := c.EventJournal().ReadEvents(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				c.taskTracker = NewTaskTracker()
				c.taskTracker.TodoList().AddReserved(ReduceToTodoList(events))
				item = c.todoItemByID(item.ID)
			}
			worker := scriptWorker(textTurn("corrected work after verifier feedback"))
			c.workerAgentOverride = worker
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID); err != nil {
				t.Fatal(err)
			}
			current := c.todoItemByID(item.ID)
			fresh := currentResultCollection(current)
			if worker.Calls() != 1 || calls.Load() != 1 || current.Status != TaskDone || fresh == nil || fresh.Work == cp.Work || fresh.ModelExecutionID == cp.ModelExecutionID || current.ExecutionReceipt.OccurrenceAttempt != 2 || current.TypedResult.Summary != "corrected work" {
				t.Fatalf("work=%d collection=%d status=%s", worker.Calls(), calls.Load(), current.Status)
			}
			if !sameCanonicalEvidence(&originalReceipt, collectionReceipt(current, cp)) {
				t.Fatal("DAG retry rewrote the prior lifecycle's receipt or verification")
			}
		})
	}
}
