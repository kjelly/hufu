package team

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/media"
)

func createMediaOccurrence(t *testing.T, c *Coordinator, ctx context.Context, task TaskDef) *TodoItem {
	t.Helper()
	spec, id := prepareMediaOccurrence(t, c, ctx, task)
	created, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{spec}, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	return created[0]
}

func prepareMediaOccurrence(t *testing.T, c *Coordinator, ctx context.Context, task TaskDef, configure ...func(*TodoSpec)) (TodoSpec, string) {
	t.Helper()
	canonical, err := c.canonicalizeTaskOccurrence(task, c.session.Agents["worker"], "fixture/vision")
	if err != nil {
		t.Fatal(err)
	}
	id := c.taskTracker.TodoList().ReserveIDs(1)[0]
	spec := TodoSpec{Agent: canonical.Agent, Desc: canonical.Goal, Goal: canonical.Goal, Model: canonical.Model, ExecutionTarget: canonical.ResolvedExecutionTarget,
		MediaBinding: canonical.MediaBinding.Clone(), MediaInputDigest: canonical.MediaInputDigest, Execution: cloneExecutionContract(canonical.Execution)}
	if len(canonical.ExecutionTopology) > 1 {
		spec.ModelTopology, spec.ExecutionTopology = canonical.ModelTopology, canonical.ExecutionTopology
	}
	for _, apply := range configure {
		apply(&spec)
	}
	if err := c.finalizeMediaTaskSpec(ctx, &spec, id); err != nil {
		t.Fatal(err)
	}
	projection, err := taskOccurrenceProjectionFromSpec(spec, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitTaskOccurrence(ctx, projection, id, 1); err != nil {
		t.Fatal(err)
	}
	return spec, id
}

type mediaTaskAppendFailure struct {
	AtomicEventJournal
	afterCommit   bool
	invalidSecond bool
	failure       error
}

func (j mediaTaskAppendFailure) AppendBatchAtHead(ctx context.Context, events []RunEvent, head string) ([]RunEvent, error) {
	if j.invalidSecond {
		events[1].Payload = []byte(`{"version":1}`)
		return j.AtomicEventJournal.AppendBatchAtHead(ctx, events, head)
	}
	if j.afterCommit {
		if _, err := j.AtomicEventJournal.AppendBatchAtHead(ctx, events, head); err != nil {
			return nil, err
		}
	}
	return nil, j.failure
}

func TestTaskMediaCreationCommitsBothEventsAndReconciles(t *testing.T) {
	for _, stage := range []string{"before-commit", "after-commit", "invalid-second-event"} {
		t.Run(stage, func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			ctx := openMediaInvocation(t, c)
			task := admittedMediaTask(t, c, ctx, "turn")
			spec, id := prepareMediaOccurrence(t, c, ctx, task)
			original := c.EventJournal().(AtomicEventJournal)
			failure := errors.New("fixture task media append failure")
			c.SetEventJournal(mediaTaskAppendFailure{AtomicEventJournal: original, afterCommit: stage == "after-commit", invalidSecond: stage == "invalid-second-event", failure: failure})
			if _, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{spec}, []string{id}); err == nil {
				t.Fatal("fault injection did not fail creation")
			} else if stage != "invalid-second-event" && !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if c.todoItemByID(id) != nil {
				t.Fatal("failed append published a live task")
			}
			c.SetEventJournal(original)
			checkEvents := func(want int) {
				t.Helper()
				if err := original.VerifyHashChain(ctx); err != nil {
					t.Fatal(err)
				}
				events, err := original.ReadEvents(ctx)
				if err != nil {
					t.Fatal(err)
				}
				created, bound := 0, 0
				for _, e := range events {
					if e.TaskID == id {
						switch EventType(e.Type) {
						case EventTaskCreated:
							created++
						case EventTaskMediaBound:
							bound++
						}
					}
				}
				if created != want || bound != want {
					t.Fatalf("created=%d bound=%d want each=%d", created, bound, want)
				}
			}
			want := 0
			if stage == "after-commit" {
				want = 1
			}
			checkEvents(want)
			if _, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{spec}, []string{id}); err != nil {
				t.Fatal(err)
			}
			checkEvents(1)
			if item := c.todoItemByID(id); item == nil || item.MediaBinding == nil || item.MediaBinding.Hash != spec.MediaBinding.Hash {
				t.Fatal("retry did not reconcile the canonical task binding")
			}
		})
	}
}

func TestTaskMediaBindingOwnershipAndLease(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	task := admittedMediaTask(t, c, ctx, "turn")
	first := createMediaOccurrence(t, c, ctx, task)
	second := createMediaOccurrence(t, c, ctx, task)
	if first.MediaBinding.Hash == second.MediaBinding.Hash || first.MediaInputDigest != second.MediaInputDigest {
		t.Fatal("binding failed to separate owners from content identity")
	}
	if err := c.CommitTaskTransition(ctx, first.ID, TaskPending, TaskInProgress, "running", "", nil); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(first.ID, 1)
	identity, ok := c.activeTaskResultOccurrence(first.ID)
	if !ok {
		t.Fatal("attempt lease unavailable")
	}
	workerContext := withSubmitResultRuntimeIdentity(ctx, identity)
	canonical := taskDefFromTodoItem(c.todoItemByID(first.ID))
	if err := c.validateMediaAttempt(workerContext, canonical, first.ID); err != nil {
		t.Fatal(err)
	}
	requireMediaCode(t, c.validateMediaAttempt(ctx, canonical, first.ID), media.AccessDenied)
	other := taskDefFromTodoItem(c.todoItemByID(second.ID))
	requireMediaCode(t, c.validateMediaAttempt(workerContext, other, first.ID), media.BindingConflict)
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		refs, err := store.ListByTask(ctx, id)
		if err != nil || len(refs) != 1 || refs[0].ID != first.MediaBinding.Refs[0].ID {
			t.Fatalf("occurrence listing for %s: %+v %v", id, refs, err)
		}
	}
	if err := c.taskTracker.TodoList().SetExecutionReceipt(first.ID, &ExecutionReceipt{RunID: c.executionRunID, TaskID: first.ID, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if c.todoItemByID(first.ID).ExecutionReceipt.MediaInputDigest != first.MediaInputDigest {
		t.Fatal("receipt omitted canonical media input")
	}
	if err := c.taskTracker.TodoList().SetExecutionReceipt(first.ID, &ExecutionReceipt{MediaInputDigest: "forged"}); err == nil {
		t.Fatal("receipt override accepted")
	}
	if err := c.CommitTaskTransition(ctx, first.ID, TaskInProgress, TaskVerifying, "verify", "", nil); err != nil {
		t.Fatal(err)
	}
	requireMediaCode(t, c.validateMediaAttempt(workerContext, canonical, first.ID), media.AccessDenied)
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)
	for _, id := range []string{first.ID, second.ID} {
		item := todoItemByID(replayed, id)
		if item == nil || item.MediaBinding == nil || item.MediaInputDigest != first.MediaInputDigest {
			t.Fatal("replay lost media occurrence binding")
		}
		if _, err := newTaskOccurrenceProjection(item); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMediaTaskCacheBypass(t *testing.T) {
	c := newTestCoordinator(t)
	cache := c.TaskCache()
	cache.Store(TaskCacheStoreRequest{AgentKey: "worker", Task: "compare", Output: "old text result"})
	if _, ok := cache.Lookup(t.Context(), TaskCacheLookupRequest{Scope: TaskCacheLookupExecution, AgentKey: "worker", Task: "compare"}); !ok {
		t.Fatal("text cache control did not hit")
	}
	if _, ok := cache.Lookup(t.Context(), TaskCacheLookupRequest{Scope: TaskCacheLookupExecution, AgentKey: "worker", Task: "compare", MediaInputDigest: "image"}); ok {
		t.Fatal("image input reused a text result")
	}
	cache.Store(TaskCacheStoreRequest{AgentKey: "worker", Task: "image output", Output: "must not cache", MediaInputDigest: "image"})
	cache.Restore([]TaskCacheSeed{{AgentKey: "worker", Task: "restored image", Output: "must not restore", MediaInputDigest: "image"}})
	for _, text := range []string{"image output", "restored image"} {
		if _, ok := cache.Lookup(t.Context(), TaskCacheLookupRequest{Scope: TaskCacheLookupExecution, AgentKey: "worker", Task: text}); ok {
			t.Fatal("media result became a reusable cache seed")
		}
	}
	_, duplicates, _ := c.checkDuplicateTasks(t.Context(), []TaskDef{{Agent: "worker", Goal: "compare", MediaInputDigest: "first"}, {Agent: "worker", Goal: "compare", MediaInputDigest: "second"}})
	if len(duplicates) != 0 {
		t.Fatal("media occurrences were coalesced")
	}
}

type mediaCacheObserver struct {
	TaskCache
	lookups atomic.Int64
	stores  atomic.Int64
}

func (o *mediaCacheObserver) Lookup(ctx context.Context, request TaskCacheLookupRequest) (TaskCacheLookupResult, bool) {
	o.lookups.Add(1)
	return o.TaskCache.Lookup(ctx, request)
}
func (o *mediaCacheObserver) Store(request TaskCacheStoreRequest) {
	o.stores.Add(1)
	o.TaskCache.Store(request)
}

func TestMediaDAGKeepsIndependentOccurrencesWithoutJournalReuse(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	c.SetNoJournal(true)
	ctx := openMediaInvocation(t, c)
	task := admittedMediaTask(t, c, ctx, "turn")
	observer := &mediaCacheObserver{TaskCache: c.TaskCache()}
	c.taskCache = observer
	_, err := c.ExecuteTasks(ctx, []TaskDef{task, cloneTaskDef(task)})
	if err == nil {
		t.Fatal("unsupported media DAG succeeded")
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 2 || items[0].Status != TaskBlocked || items[1].Status != TaskBlocked {
		t.Fatalf("media DAG occurrences: %+v error=%v", items, err)
	}
	if items[0].MediaBinding == nil || items[1].MediaBinding == nil || items[0].MediaBinding.Hash == items[1].MediaBinding.Hash {
		t.Fatal("DAG lost individual image owners")
	}
	if observer.lookups.Load() != 0 || observer.stores.Load() != 0 {
		t.Fatalf("media consulted automatic reuse: lookups=%d stores=%d", observer.lookups.Load(), observer.stores.Load())
	}
	if c.journal != nil {
		t.Fatal("--no-journal unexpectedly opened the task-result journal")
	}
}

func TestMediaBranchQuotaCountsUniqueContent(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	_ = admittedMediaTask(t, c, ctx, "turn")
	view, err := c.mediaLineage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot media.InputSnapshot
	for _, s := range view.snapshots {
		snapshot = s
	}
	limits := media.Limits{MaxBranchBlobs: 1, MaxBranchBytes: snapshot.Refs[0].ByteSize}
	if err := checkMediaBranchQuota(view.snapshots, snapshot, limits); err != nil {
		t.Fatal(err)
	}
	changed := mediaQuotaCandidate(t, c, "other-content", 3)
	requireMediaCode(t, checkMediaBranchQuota(view.snapshots, changed, limits), media.LimitExceeded)
}
