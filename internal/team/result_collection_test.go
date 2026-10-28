package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func resultCollectionFixture(t *testing.T) (*Coordinator, *TodoItem, context.Context, ExecutionReceipt) {
	return resultCollectionProviderFixture(t, "http://127.0.0.1:1/v1", false)
}

func resultCollectionProviderFixture(t *testing.T, endpoint string, runtime bool, options ...func(*Coordinator)) (*Coordinator, *TodoItem, context.Context, ExecutionReceipt) {
	t.Helper()
	return resultCollectionTaskFixture(t, endpoint, runtime, nil, options...)
}

func resultCollectionTaskFixture(t *testing.T, endpoint string, runtime bool, configureTask func(*TodoSpec), options ...func(*Coordinator)) (*Coordinator, *TodoItem, context.Context, ExecutionReceipt) {
	t.Helper()
	c := newTestCoordinator(t)
	c.maxConcurrent = 1
	c.session.Config.DefaultLLMBackend = execution.OllamaBackendName
	c.session.Agents["worker"].Generation.Model = "ollama/test"
	if runtime {
		c.session.Agents["worker"].ResultTransport = agent.ResultTransportRuntime
	}
	manager, err := agent.NewProviderManager(endpoint, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = manager
	if runtime {
		c.modelProfileRuntime = NewModelProfileRuntime(manager, false)
		// This fixture starts after the original work's provider boundary.
		c.providerBoundaryStarted = true
	}
	project := t.TempDir()
	if err := c.session.SetCompatibilityWorkspaceScope(project); err != nil {
		t.Fatal(err)
	}
	c.projectDir = project
	for _, option := range options {
		option(c)
	}
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	target := execution.ExecutionTarget{Backend: execution.OllamaBackendName, Model: "test"}
	spec := TodoSpec{Agent: "worker", Desc: "analyze the supplied evidence", Model: "ollama/test", ExecutionTarget: target, ExecutionTopology: []execution.ExecutionTarget{target}, Execution: ExecutionContract{RequiresResult: true}}
	if configureTask != nil {
		configureTask(&spec)
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
	item := items[0]
	if err := c.CommitTaskTransition(t.Context(), item.ID, TaskPending, TaskInProgress, "working", "", nil); err != nil {
		t.Fatal(err)
	}
	ctx := occurrenceTestContext(c, item.ID, 1)
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, c.executionRunID, 1, item.Agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	if err := transcript.RecordAssistantOutput("complete work report"); err != nil {
		t.Fatal(err)
	}
	ref, err := sealVerbatimTaskTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	receipt := ExecutionReceipt{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, OccurrenceAttempt: 1, ExecutionTarget: target,
		Model: "ollama/test", ModelExecutionID: "work-execution-1", ProducerID: item.Agent, TranscriptRef: ref.ID,
		StartedAt: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 27, 0, 0, 1, 0, time.UTC), Usage: &ExecutionUsage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}}
	return c, todoByID(t, c, item.ID), ctx, receipt
}

func beginCollectionFixture(t *testing.T) (*Coordinator, *TodoItem, context.Context, *ResultCollectionCheckpoint) {
	t.Helper()
	c, item, ctx, receipt := resultCollectionFixture(t)
	checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "complete work report")
	if err != nil {
		t.Fatal(err)
	}
	return c, todoByID(t, c, item.ID), ctx, checkpoint
}

func TestResultCollectionPreservesWorkAndReplaysWithoutResultContract(t *testing.T) {
	c, item, ctx, receipt := resultCollectionFixture(t)
	before, err := json.Marshal(receipt, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "complete work report")
	if err != nil {
		t.Fatal(err)
	}
	work, err := c.loadCollectionWork(t.Context(), todoByID(t, c, item.ID), checkpoint)
	if err != nil || string(work.Report) != "complete work report" {
		t.Fatalf("work=%+v err=%v", work, err)
	}
	after, err := json.Marshal(work.Receipt, json.Deterministic(true))
	if err != nil || string(before) != string(after) {
		t.Fatalf("original receipt changed: %s / %s; %v", before, after, err)
	}
	if checkpoint.State != ResultCollectionPending || checkpoint.TurnsUsed != 0 || item.ResultContract != nil || c.GetTaskResult(item.ID) != nil {
		t.Fatal("work checkpoint implied a model turn or accepted result")
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)
	if len(replayed) != 1 || replayed[0].ExecutionReceipt == nil || !reflect.DeepEqual(replayed[0].ExecutionReceipt.ResultCollection, checkpoint) || replayed[0].Status != TaskInProgress || replayed[0].TypedResult != nil {
		t.Fatalf("invalid checkpoint replay: %+v", replayed)
	}
	if _, err := c.loadCollectionWork(t.Context(), replayed[0], checkpoint); err != nil {
		t.Fatalf("replayed work cannot be verified: %v", err)
	}
}

func TestResultCollectionReservesOneConcurrentModelTurn(t *testing.T) {
	c, item, ctx, pending := beginCollectionFixture(t)
	var wg sync.WaitGroup
	results := make(chan *ResultCollectionCheckpoint, 2)
	failures := make(chan error, 2)
	for _, id := range []string{"collector-a", "collector-b"} {
		wg.Go(func() {
			checkpoint, err := c.reserveResultCollection(ctx, pending, id, []byte(`{"prompt":"return one JSON object","tools":[]}`))
			if err != nil {
				failures <- err
				return
			}
			results <- checkpoint
		})
	}
	wg.Wait()
	if len(results) != 1 || len(failures) != 1 {
		t.Fatalf("reservations=%d failures=%d", len(results), len(failures))
	}
	checkpoint := <-results
	if err := <-failures; !errors.Is(err, errResultCollectionStale) {
		t.Fatalf("unexpected competing reservation error: %v", err)
	}
	if checkpoint.TurnsUsed != 1 || checkpoint.State != ResultCollectionInFlight {
		t.Fatalf("invalid reservation: %+v", checkpoint)
	}
	if _, err := c.reserveResultCollection(ctx, checkpoint, "another-live-turn", []byte(`{"tools":[]}`)); !errors.Is(err, errResultCollectionStale) {
		t.Fatalf("a live invocation was replayed: %v", err)
	}
	current := todoByID(t, c, item.ID)
	if current.ExecutionReceipt.ResultCollection.TurnsUsed != 1 {
		t.Fatal("losing reservation consumed a model turn")
	}
}

func TestResultCollectionRejectsUntrustedOrMissingWork(t *testing.T) {
	for _, scenario := range []string{"branch", "policy", "contract", "work_hash", "cas_bytes", "response_binding", "event_failure"} {
		t.Run(scenario, func(t *testing.T) {
			c, item, ctx, checkpoint := beginCollectionFixture(t)
			copy := cloneResultCollectionCheckpoint(checkpoint)
			switch scenario {
			case "branch":
				copy.BranchID = "another-branch"
			case "policy":
				copy.PolicySHA256 = strings.Repeat("a", 64)
			case "contract":
				copy.TaskContractSHA256 = strings.Repeat("a", 64)
			case "work_hash":
				copy.Work.SHA256 = strings.Repeat("a", 64)
				copy.Work.ArtifactID = "sha256-" + copy.Work.SHA256
			case "cas_bytes":
				store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.artifactStoreRootPath())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(store.root, "data", copy.Work.ArtifactID), []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "response_binding":
				var err error
				copy, err = c.reserveResultCollection(ctx, copy, "collector-a", []byte(`{"tools":[]}`))
				if err != nil {
					t.Fatal(err)
				}
				copy, err = c.recordCollectionResponse(ctx, copy, []byte("invalid JSON is still durable evidence"))
				if err != nil {
					t.Fatal(err)
				}
				copy.InvocationID = "forged-collector"
				if _, err := c.readCollectionBlob(ctx, copy, "response", copy.Response); err == nil {
					t.Fatal("response from another invocation was accepted")
				}
				return
			case "event_failure":
				c.SetEventJournal(failingJournal{err: errors.New("storage unavailable")})
				if _, err := c.reserveResultCollection(ctx, copy, "collector-a", []byte(`{"tools":[]}`)); err == nil {
					t.Fatal("event failure was hidden")
				}
				if current := todoByID(t, c, item.ID).ExecutionReceipt.ResultCollection; !reflect.DeepEqual(current, checkpoint) {
					t.Fatal("failed append published a model reservation")
				}
				return
			}
			if _, err := c.loadCollectionWork(t.Context(), item, copy); err == nil {
				t.Fatal("invalid work evidence accepted")
			}
		})
	}
}

func TestResultCollectionPreparedCommitPersistsBeforePublication(t *testing.T) {
	for _, failAppend := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "storage_failure"}[failAppend], func(t *testing.T) {
			c, item, ctx, checkpoint := beginCollectionFixture(t)
			original := cloneExecutionReceipt(item.ExecutionReceipt)
			checkpoint, err := c.reserveResultCollection(ctx, checkpoint, "collector-1", []byte(`{"tools":[]}`))
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
			events, err := c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			restored := ReduceToTodoList(events)
			c.revokeTaskOccurrence(item.ID)
			c.taskTracker = NewTaskTracker()
			c.taskTracker.TodoList().AddReserved(restored)
			ctx = WithSessionResume(t.Context())
			if failAppend {
				c.SetEventJournal(failingJournal{err: errors.New("storage unavailable")})
			}
			err = c.commitPreparedCollection(ctx, checkpoint)
			current := todoByID(t, c, item.ID)
			if failAppend {
				if err == nil || c.GetTaskResult(item.ID) != nil || current.TypedResult != nil || current.ExecutionReceipt.ResultCollection.State != ResultCollectionPrepared {
					t.Fatalf("failed durable commit was published: err=%v item=%+v", err, current)
				}
				if c.occurrenceController(item.ID).snapshot().reserved {
					t.Fatal("reservation leaked")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			committed := current.ExecutionReceipt.ResultCollection
			if committed.State != ResultCollectionCommitted || committed.TurnsUsed != 1 || current.Status != TaskInProgress {
				t.Fatalf("unexpected completion: %+v", current)
			}
			accepted, err := c.acceptedCollectionResult(ctx, current, committed)
			if err != nil || !reflect.DeepEqual(accepted, current.TypedResult) {
				t.Fatalf("canonical result mismatch: %v", err)
			}
			events, err = c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToTodoList(events)
			if len(replayed) != 1 || !reflect.DeepEqual(replayed[0].TypedResult, accepted) {
				t.Fatal("accepted result was not replayed")
			}
			if err := c.taskTracker.TodoList().SetExecutionReceipt(item.ID, &original); err != nil {
				t.Fatal(err)
			}
			if todoByID(t, c, item.ID).ExecutionReceipt.ResultCollection.State != ResultCollectionCommitted {
				t.Fatal("late receipt reset collection progress")
			}
			original.ModelExecutionID = "new-work-attempt"
			original.ResultCollection = nil
			if err := c.taskTracker.TodoList().SetExecutionReceipt(item.ID, &original); err != nil {
				t.Fatal(err)
			}
			if todoByID(t, c, item.ID).ExecutionReceipt.ResultCollection != nil {
				t.Fatal("collection leaked to another execution")
			}
		})
	}
}

func TestResultCollectionRejectsAmbiguousClaims(t *testing.T) {
	c, item, _, _ := beginCollectionFixture(t)
	for _, raw := range []string{
		`{"status":"success","summary":"complete","summary":"other"}`,
		`{"status":"success","summary":"complete","source":"runtime"}`,
		`{"status":"success","summary":"complete","open_questions":[{"question":"one","question":"two"}]}`,
		`{"status":"success","summary":"complete"} {}`,
		"```json\n{\"status\":\"success\",\"summary\":\"complete\"}\n```",
		`[]`, `null`, `{"status":"success"}`,
	} {
		if _, err := c.decodeCollectionClaims(item.ID, []byte(raw)); err == nil {
			t.Fatalf("accepted invalid claims: %s", raw)
		}
	}
}

func TestResultCollectionRetriesOnlyRejectedSchema(t *testing.T) {
	c, _, ctx, checkpoint := beginCollectionFixture(t)
	first, err := c.reserveResultCollection(ctx, checkpoint, "first-collection", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	received, err := c.recordCollectionResponse(ctx, first, []byte(`invalid JSON`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.reserveResultCollection(ctx, received, "unvalidated-retry", []byte(`{"tools":[]}`)); !errors.Is(err, errResultCollectionStale) {
		t.Fatalf("response was not validated before retry: %v", err)
	}
	rejected, err := c.recordCollectionSchemaFailure(ctx, received)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.reserveResultCollection(ctx, rejected, "format-repair", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if second.TurnsUsed != 2 || second.Work != first.Work {
		t.Fatal("format repair reset original work or budget")
	}
	if _, err := c.readCollectionBlob(ctx, received, "response", received.Response); err != nil {
		t.Fatalf("original rejected response was lost: %v", err)
	}
}

func TestResultCollectionCrashResumeKeepsOriginalWorkAndBudget(t *testing.T) {
	c, item, ctx, checkpoint := beginCollectionFixture(t)
	checkpoint, err := c.reserveResultCollection(ctx, checkpoint, "lost-invocation", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	c.revokeTaskOccurrence(item.ID)
	c.executionRunID = "resumed-invocation"
	ctx, err = c.restoreCollectionContext(WithSessionResume(t.Context()), checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.recordCollectionResponse(ctx, checkpoint, []byte(`{"status":"success","summary":"late"}`)); !errors.Is(err, errResultCollectionStale) {
		t.Fatalf("late response accepted: %v", err)
	}
	resumed, err := c.reserveResultCollection(ctx, checkpoint, "replacement-invocation", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.TurnsUsed != 2 || resumed.Work != checkpoint.Work || resumed.SourceRunID != checkpoint.SourceRunID || resumed.InvocationRunID != c.executionRunID {
		t.Fatalf("rebound original work or reset budget: %+v", resumed)
	}
	c.revokeTaskOccurrence(item.ID)
	c.executionRunID = "third-invocation"
	ctx, err = c.restoreCollectionContext(WithSessionResume(t.Context()), resumed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.reserveResultCollection(ctx, resumed, "over-budget", []byte(`{"tools":[]}`)); !errors.Is(err, errResultCollectionBudget) {
		t.Fatalf("budget reset: %v", err)
	}
}

func TestResultCollectionOperatorGrantBindsSubmissionLease(t *testing.T) {
	c, item, ctx, checkpoint := beginCollectionFixture(t)
	checkpoint, err := c.blockResultCollection(ctx, checkpoint, repairFailureCollectionUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitTaskTransition(ctx, item.ID, TaskInProgress, TaskBlocked, "collection unavailable", "", map[string]any{"failure_event": &FailureEventPayload{FailureClass: FailureProtocol}}); err != nil {
		t.Fatal(err)
	}
	c.executionRunID = "operator-invocation"
	late := cloneExecutionReceipt(todoByID(t, c, item.ID).ExecutionReceipt)
	late.ResultCollection, late.RepairProvenance = nil, nil
	if err := c.taskTracker.TodoList().SetExecutionReceipt(item.ID, &late); err != nil {
		t.Fatal(err)
	}
	ctx = WithResultOnlyRepair(t.Context(), item.ID)
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	current := todoByID(t, c, item.ID)
	granted := current.ExecutionReceipt.ResultCollection
	if granted.Work != checkpoint.Work || granted.SubmissionRevision <= checkpoint.SubmissionRevision || granted.SubmissionDispatchID == checkpoint.SubmissionDispatchID || current.Status != TaskProtocolIncomplete {
		t.Fatal("operator grant did not bind a new submission lease")
	}
	ctx, err = c.restoreCollectionContext(ctx, granted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.reserveResultCollection(ctx, granted, "authorized-collection", []byte(`{"tools":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err == nil {
		t.Fatal("operator grant reused")
	}
}
