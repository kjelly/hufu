package team

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type recordingExperienceProcessor struct {
	prepared  []RunFinalizationInput
	finalized []CompletionGateDecision
}

func (p *recordingExperienceProcessor) Prepare(_ context.Context, input RunFinalizationInput) error {
	p.prepared = append(p.prepared, input)
	return nil
}

func (p *recordingExperienceProcessor) Finalize(_ context.Context, _ RunFinalizationInput, decision CompletionGateDecision) error {
	p.finalized = append(p.finalized, decision)
	return nil
}

func TestFinalizeRunUsesOneExperienceProcessorForAcceptedRun(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "finalizer"}},
		taskTracker: NewTaskTracker(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verified"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	manifest := &EvidenceManifest{SchemaVersion: 1, RunID: "run-1", Status: "accepted"}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	c.lastEvidenceManifest = manifest
	processor := &recordingExperienceProcessor{}
	c.SetExperienceProcessor(processor)
	result := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true}
	acceptance := &AcceptanceResult{State: AcceptancePassed, Passed: true}
	got := c.FinalizeRun(context.Background(), result, acceptance)
	if got == nil || got.Outcome != RunOutcomeCompleted || !got.GoalSatisfied {
		t.Fatalf("final result = %#v", got)
	}
	if len(processor.prepared) != 1 || len(processor.finalized) != 1 || !processor.finalized[0].Accepted {
		t.Fatalf("experience lifecycle = prepared:%d finalized:%#v", len(processor.prepared), processor.finalized)
	}
}

func TestFinalizeRunRejectsExperienceForFailedOutcome(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: NewTaskTracker()}
	processor := &recordingExperienceProcessor{}
	c.SetExperienceProcessor(processor)
	result := &RunResult{Outcome: RunOutcomeFailed, GoalSatisfied: false}
	got := c.FinalizeRun(context.Background(), result, &AcceptanceResult{State: AcceptanceFailed})
	if got == nil || len(processor.prepared) != 1 || len(processor.finalized) != 1 || processor.finalized[0].Accepted {
		t.Fatalf("failed finalization did not reject experience: result=%#v finalized=%#v", got, processor.finalized)
	}
}

func TestFailedDirectRunUsesExperienceProcessor(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		taskTracker: NewTaskTracker(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "failed direct task"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskError, "failed")
	processor := &recordingExperienceProcessor{}
	c.SetExperienceProcessor(processor)
	result := c.finalizeDirectRun(context.Background(), item.ID, false, "worker failed")
	if result == nil || len(processor.prepared) != 1 || len(processor.finalized) != 1 || processor.finalized[0].Accepted {
		t.Fatalf("failed direct run bypassed common experience finalization: result=%#v finalized=%#v", result, processor.finalized)
	}
	if result.EvidenceManifest == nil {
		t.Fatal("failed direct run did not seal an evidence manifest")
	}
}

func TestDefaultExperienceProcessorIgnoresStaleMarkdownAndIsIdempotent(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	if err := SaveSTM(workspace, stmSectionDecisions+"\n\n- stale markdown to ignore\n"); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		contextRepo:    repo,
		projectDir:     "/project",
		executionRunID: "run-1",
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		taskTracker:    NewTaskTracker(),
	}
	if err := c.appendCanonicalContext(context.Background(), contextstore.ContextDecision, "decision from current run", "task_done", map[string]string{"verified": "true"}); err != nil {
		t.Fatal(err)
	}

	processor := &defaultExperienceProcessor{c: c}
	input := RunFinalizationInput{
		RunID: "run-1",
		Tasks: []TodoItem{{ID: "1", Status: TaskDone, Desc: "task"}},
	}
	if err := processor.Prepare(context.Background(), input); err != nil {
		t.Fatal(err)
	}

	scope := contextstore.Scope{ProjectID: "/project", TeamID: "team"}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: scope, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Content, "decision from current run") || strings.Contains(items[0].Content, "stale markdown") {
		t.Fatalf("prepared items = %#v", items)
	}

	// Re-running Prepare must be idempotent
	if err := processor.Prepare(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	items2, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: scope, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items2) != 1 {
		t.Fatalf("expected 1 item after re-running Prepare, got %d", len(items2))
	}
}

func TestDefaultExperienceProcessorDoesNotPromotePreviousFailedRunRecord(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	c := &Coordinator{
		contextRepo:    repo,
		projectDir:     "/project",
		executionRunID: "run-A",
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		taskTracker:    NewTaskTracker(),
	}
	// Failed run A creates a shared record.
	if err := c.appendCanonicalContext(context.Background(), contextstore.ContextDecision, "decision from failed run A", "task_done", map[string]string{"verified": "true"}); err != nil {
		t.Fatal(err)
	}

	// Accepted run B must not promote A's record into a candidate.
	c.executionRunID = "run-B"
	processor := &defaultExperienceProcessor{c: c}
	input := RunFinalizationInput{
		RunID: "run-B",
		Tasks: []TodoItem{{ID: "1", Status: TaskDone, Desc: "task"}},
	}
	if err := processor.Prepare(context.Background(), input); err != nil {
		t.Fatal(err)
	}

	// Persistent candidates are session-neutral; a promotion of A's record
	// would appear here as a new candidate under run B.
	persistentScope := contextstore.Scope{ProjectID: "/project", TeamID: "team"}
	candidates, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: persistentScope, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("run B promoted A's record into a candidate: %#v", candidates)
	}

	// A's session record must remain a run-bound candidate (never confirmed by
	// a later run's Prepare) and run-tagged.
	sessionItems, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: c.contextScope(), Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionItems) != 1 {
		t.Fatalf("A's session record missing: %#v", sessionItems)
	}
	if sessionItems[0].Lifecycle != contextstore.LifecycleCandidate {
		t.Fatalf("A's record lifecycle changed: %s", sessionItems[0].Lifecycle)
	}
	if sessionItems[0].Metadata["run_id"] != "run-A" {
		t.Fatalf("A's record run_id = %q, want run-A", sessionItems[0].Metadata["run_id"])
	}
}

// TestFailedRunSharedContextIsNeitherPromptVisibleNorPromotable pins the
// P1#3 contract: run-produced shared context is a candidate bound to the run,
// promoted only by the accepted finalizer. A failed run's records are rejected
// and must never appear in a later direct-agent prompt nor be re-proposed as
// persistent candidates.
func TestFailedRunSharedContextIsNeitherPromptVisibleNorPromotable(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	c := &Coordinator{
		contextRepo:    repo,
		projectDir:     "/project",
		executionRunID: "run-A",
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		taskTracker:    NewTaskTracker(),
	}
	// Failed run A produces shared context.
	if err := c.appendCanonicalContext(context.Background(), contextstore.ContextDecision, "decision from failed run A", "task_done", map[string]string{"verified": "true"}); err != nil {
		t.Fatal(err)
	}

	// Run A fails the completion gate; the finalizer rejects its candidates.
	processor := &defaultExperienceProcessor{c: c}
	manifestA := &EvidenceManifest{RunID: "run-A", ManifestHash: "hash-A", Status: "rejected"}
	if err := processor.Finalize(context.Background(), RunFinalizationInput{RunID: "run-A", Evidence: manifestA}, CompletionGateDecision{Accepted: false, Reasons: []string{"run A failed"}}); err != nil {
		t.Fatal(err)
	}

	// A's record is rejected, never confirmed.
	sessionItems, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: c.contextScope(), Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionItems) != 1 || sessionItems[0].Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("A's record after failed finalization = %#v, want rejected", sessionItems)
	}

	// A later direct-agent run must not see A's record in the prompt bundle.
	c.executionRunID = "run-B"
	bundle, canonical, err := c.canonicalContextBundleForQuery(context.Background(), "later direct agent goal")
	if err != nil || !canonical {
		t.Fatalf("canonicalContextBundleForQuery = (%v, %t, %v)", bundle, canonical, err)
	}
	for _, item := range bundle.SharedSession {
		if strings.Contains(item.Content, "decision from failed run A") {
			t.Fatalf("failed run A's record is prompt-visible in run B: %#v", item)
		}
	}

	// Run B's Prepare must not promote A's rejected record into a candidate.
	if err := processor.Prepare(context.Background(), RunFinalizationInput{RunID: "run-B", Tasks: []TodoItem{{ID: "1", Status: TaskDone, Desc: "task"}}}); err != nil {
		t.Fatal(err)
	}
	persistent, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "/project", TeamID: "team"}, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range persistent {
		if strings.Contains(item.Content, "decision from failed run A") {
			t.Fatalf("run B promoted failed run A's record: %#v", item)
		}
	}
}

// TestAcceptedRunSharedContextIsPromotedAndVisibleToItsOwnRun pins the positive
// side of P1#3: the current run sees its own run-produced candidates in the
// prompt bundle, and the accepted finalizer promotes them to confirmed
// knowledge bound to the evidence manifest.
func TestAcceptedRunSharedContextIsPromotedAndVisibleToItsOwnRun(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	c := &Coordinator{
		contextRepo:    repo,
		projectDir:     "/project",
		executionRunID: "run-A",
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		taskTracker:    NewTaskTracker(),
	}
	if err := c.appendCanonicalContext(context.Background(), contextstore.ContextDecision, "decision from accepted run A", "task_done", map[string]string{"verified": "true"}); err != nil {
		t.Fatal(err)
	}

	// The current run sees its own candidate in the prompt bundle.
	bundle, canonical, err := c.canonicalContextBundleForQuery(context.Background(), "current run goal")
	if err != nil || !canonical {
		t.Fatalf("canonicalContextBundleForQuery = (%v, %t, %v)", bundle, canonical, err)
	}
	found := false
	for _, item := range bundle.SharedSession {
		if strings.Contains(item.Content, "decision from accepted run A") {
			found = true
		}
	}
	if !found {
		t.Fatalf("current run's own candidate missing from prompt bundle: %#v", bundle.SharedSession)
	}

	// The accepted finalizer promotes the candidate to confirmed knowledge.
	processor := &defaultExperienceProcessor{c: c}
	manifestA := &EvidenceManifest{RunID: "run-A", ManifestHash: "hash-A", Status: "accepted"}
	if err := processor.Finalize(context.Background(), RunFinalizationInput{RunID: "run-A", Evidence: manifestA}, CompletionGateDecision{Accepted: true}); err != nil {
		t.Fatal(err)
	}
	sessionItems, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: c.contextScope(), Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionItems) != 1 || sessionItems[0].Lifecycle != contextstore.LifecycleConfirmed {
		t.Fatalf("accepted run A's record = %#v, want confirmed", sessionItems)
	}
	if sessionItems[0].Metadata["manifest_hash"] != "hash-A" {
		t.Fatalf("confirmed record manifest_hash = %q, want hash-A (bound to accepted manifest)", sessionItems[0].Metadata["manifest_hash"])
	}
}

func TestFinalizeRunRejectsAsyncReflexionCandidateBarrier(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	runID := "run-failed-barrier"
	agentDef := &agent.AgentDef{
		Name:     "worker",
		MemoryID: "worker-mem-1",
		Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent},
	}
	c := &Coordinator{
		contextRepo:     repo,
		workerMemorySvc: NewWorkerMemoryService(repo, nil),
		projectDir:      "/project",
		executionRunID:  runID,
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name: "team",
			},
			Agents: map[string]*agent.AgentDef{"worker": agentDef},
		},
		taskTracker: NewTaskTracker(),
	}

	barrierHold := make(chan struct{})
	barrierDone := make(chan struct{})

	// Launch async reflexion write held on a barrier (simulating untracked or late race)
	go func() {
		<-barrierHold
		c.persistPrivateReflexionLesson("worker", "task-1", "avoid this approach")
		close(barrierDone)
	}()

	// Run FinalizeRun with failed outcome while the async write is held
	result := &RunResult{Outcome: RunOutcomeFailed, GoalSatisfied: false}
	c.FinalizeRun(context.Background(), result, &AcceptanceResult{State: AcceptanceFailed})

	// Release the barrier and wait for the async write to complete
	close(barrierHold)
	<-barrierDone

	// Assert that no candidate remains with LifecycleCandidate for this run in SQLite repository
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{
		Scope:             contextstore.Scope{ProjectID: "/project", TeamID: "team", AgentID: "worker-mem-1"},
		Visibility:        contextstore.VisibilitySubtree,
		IncludeCandidates: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
			t.Fatalf("found undecided candidate %q for failed run %q", it.ID, runID)
		}
	}
}

func TestFinalizeRun_RejectionAppendFailure_DowngradesRunAndSurfacesError(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, "context.sqlite")
	baseRepo, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer baseRepo.Close()

	failingRepo := &failingAppendRepo{Repository: baseRepo, failAppend: false}

	runID := "run-fail-closed-rejection"
	agentDef := &agent.AgentDef{
		Name:     "worker",
		MemoryID: "worker-mem-1",
		Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent},
	}
	c := &Coordinator{
		contextRepo:     failingRepo,
		workerMemorySvc: NewWorkerMemoryService(failingRepo, nil),
		sharedMemorySvc: NewSharedMemoryService(failingRepo),
		projectDir:      "/project",
		executionRunID:  runID,
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name: "team",
			},
			Agents: map[string]*agent.AgentDef{"worker": agentDef},
		},
		taskTracker: NewTaskTracker(),
	}

	// 1. Save worker and shared candidates while Append works
	c.persistPrivateReflexionLesson("worker", "task-1", "private lesson")
	if _, err := c.sharedMemoryService().Propose(context.Background(), SharedMemoryProposal{
		Scope:    c.contextScope(),
		Content:  "shared lesson",
		Section:  ltmSectionPatterns,
		Category: "pattern",
		Source:   "memory_save",
		RunID:    runID,
	}); err != nil {
		t.Fatalf("Propose shared candidate: %v", err)
	}

	// 2. Trigger Append failure on the repository
	failingRepo.failAppend = true

	// 3. FinalizeRun for a non-accepted (failed) run
	initialResult := &RunResult{Outcome: RunOutcomeFailed, GoalSatisfied: false}
	finalResult := c.FinalizeRun(context.Background(), initialResult, &AcceptanceResult{State: AcceptanceFailed})

	// 4. Verify that the finalization error surfaced and was not silently swallowed
	if finalResult == nil {
		t.Fatal("FinalizeRun returned nil")
	}
	if !strings.Contains(finalResult.Reason, "finalize rejected run") && !strings.Contains(finalResult.Reason, "append") {
		t.Fatalf("expected finalResult.Reason to contain rejection failure, got: %q", finalResult.Reason)
	}
	if finalResult.StopReason != StopReasonEvidenceIncomplete {
		t.Fatalf("expected StopReasonEvidenceIncomplete, got: %v", finalResult.StopReason)
	}
}

func TestCoordinatorRun_AbortedPathsRejectCandidatesAcrossRestart(t *testing.T) {
	t.Run("provider failure recordRunAborted", func(t *testing.T) {
		workspace := t.TempDir()
		dbPath := filepath.Join(workspace, "context.sqlite")
		repo, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}

		runID := "run-provider-abort"
		agentDef := &agent.AgentDef{
			Name:     "worker",
			MemoryID: "worker-mem-1",
			Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent},
		}
		c := &Coordinator{
			contextRepo:     repo,
			workerMemorySvc: NewWorkerMemoryService(repo, nil),
			sharedMemorySvc: NewSharedMemoryService(repo),
			projectDir:      "/project",
			executionRunID:  runID,
			session: &TeamSession{
				Workspace: workspace,
				Config: agent.TeamConfig{
					Name: "team",
				},
				Agents: map[string]*agent.AgentDef{"worker": agentDef},
			},
			taskTracker: NewTaskTracker(),
		}

		// Seed private, shared, and run-shared candidates
		c.persistPrivateReflexionLesson("worker", "task-1", "private lesson")
		if _, err := c.sharedMemoryService().Propose(context.Background(), SharedMemoryProposal{
			Scope:    c.contextScope(),
			Content:  "shared lesson",
			Section:  ltmSectionPatterns,
			Category: "pattern",
			Source:   "memory_save",
			RunID:    runID,
		}); err != nil {
			t.Fatal(err)
		}
		if err := c.appendCanonicalContext(context.Background(), contextstore.ContextDecision, "decision", "task_done", nil); err != nil {
			t.Fatal(err)
		}

		// Simulate coordinator provider error abort
		c.recordRunAborted(errors.New("provider connection refused"))

		// Close repo and reopen to verify durability
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo2, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer repo2.Close()

		// Query all items for this project/team
		items, err := repo2.Query(context.Background(), contextstore.RepositoryQuery{
			Scope:             contextstore.Scope{ProjectID: "/project", TeamID: "team"},
			Visibility:        contextstore.VisibilitySubtree,
			IncludeCandidates: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
				t.Fatalf("found unresolved candidate %q after provider abort", it.ID)
			}
		}
	})

	t.Run("cancellation with cancelled context", func(t *testing.T) {
		workspace := t.TempDir()
		dbPath := filepath.Join(workspace, "context.sqlite")
		repo, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}

		runID := "run-cancelled-abort"
		agentDef := &agent.AgentDef{
			Name:     "worker",
			MemoryID: "worker-mem-1",
			Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent},
		}
		c := &Coordinator{
			contextRepo:     repo,
			workerMemorySvc: NewWorkerMemoryService(repo, nil),
			sharedMemorySvc: NewSharedMemoryService(repo),
			projectDir:      "/project",
			executionRunID:  runID,
			session: &TeamSession{
				Workspace: workspace,
				Config: agent.TeamConfig{
					Name: "team",
				},
				Agents: map[string]*agent.AgentDef{"worker": agentDef},
			},
			taskTracker: NewTaskTracker(),
		}

		// Seed candidates
		c.persistPrivateReflexionLesson("worker", "task-1", "private lesson")
		if _, err := c.sharedMemoryService().Propose(context.Background(), SharedMemoryProposal{
			Scope:    c.contextScope(),
			Content:  "shared lesson",
			Section:  ltmSectionPatterns,
			Category: "pattern",
			Source:   "memory_save",
			RunID:    runID,
		}); err != nil {
			t.Fatal(err)
		}

		// Simulate coordinator user cancellation
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		c.recordRunAborted(cancelledCtx.Err())

		// Reopen database
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo2, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer repo2.Close()

		items, err := repo2.Query(context.Background(), contextstore.RepositoryQuery{
			Scope:             contextstore.Scope{ProjectID: "/project", TeamID: "team"},
			Visibility:        contextstore.VisibilitySubtree,
			IncludeCandidates: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
				t.Fatalf("found unresolved candidate %q after cancellation abort", it.ID)
			}
		}
	})

	t.Run("direct agent cancellation with cancelled context", func(t *testing.T) {
		workspace := t.TempDir()
		dbPath := filepath.Join(workspace, "context.sqlite")
		repo, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}

		runID := "run-direct-cancelled"
		agentDef := &agent.AgentDef{
			Name:     "worker",
			MemoryID: "worker-mem-1",
			Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent},
		}
		c := &Coordinator{
			contextRepo:     repo,
			workerMemorySvc: NewWorkerMemoryService(repo, nil),
			sharedMemorySvc: NewSharedMemoryService(repo),
			projectDir:      "/project",
			executionRunID:  runID,
			session: &TeamSession{
				Workspace: workspace,
				Config: agent.TeamConfig{
					Name: "team",
				},
				Agents: map[string]*agent.AgentDef{"worker": agentDef},
			},
			taskTracker: NewTaskTracker(),
		}

		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "direct task"}})[0]
		c.persistPrivateReflexionLesson("worker", item.ID, "direct lesson")

		// Create cancelled context and finalize direct run
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		res := c.finalizeDirectRun(cancelledCtx, item.ID, false, "cancelled by user")
		if res == nil {
			t.Fatal("finalizeDirectRun returned nil")
		}

		// Reopen database
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo2, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer repo2.Close()

		items, err := repo2.Query(context.Background(), contextstore.RepositoryQuery{
			Scope:             contextstore.Scope{ProjectID: "/project", TeamID: "team"},
			Visibility:        contextstore.VisibilitySubtree,
			IncludeCandidates: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
				t.Fatalf("found unresolved candidate %q after direct agent cancellation", it.ID)
			}
		}
	})

	t.Run("terminal unresolved run fallback", func(t *testing.T) {
		workspace := t.TempDir()
		dbPath := filepath.Join(workspace, "context.sqlite")
		repo, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}

		runID := "run-terminal-unresolved"
		agentDef := &agent.AgentDef{
			Name:     "worker",
			MemoryID: "worker-mem-1",
			Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent},
		}
		c := &Coordinator{
			contextRepo:     repo,
			workerMemorySvc: NewWorkerMemoryService(repo, nil),
			sharedMemorySvc: NewSharedMemoryService(repo),
			projectDir:      "/project",
			executionRunID:  runID,
			session: &TeamSession{
				Workspace: workspace,
				Config: agent.TeamConfig{
					Name: "team",
				},
				Agents: map[string]*agent.AgentDef{"worker": agentDef},
			},
			taskTracker: NewTaskTracker(),
		}

		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "failed task"}})[0]
		c.taskTracker.TodoList().UpdateStatus(item.ID, TaskError, "worker crashed")
		c.persistPrivateReflexionLesson("worker", item.ID, "private lesson")
		if _, err := c.sharedMemoryService().Propose(context.Background(), SharedMemoryProposal{
			Scope:    c.contextScope(),
			Content:  "shared lesson",
			Section:  ltmSectionPatterns,
			Category: "pattern",
			Source:   "memory_save",
			RunID:    runID,
		}); err != nil {
			t.Fatal(err)
		}

		summary := c.finalizeTerminalUnresolvedRun()
		if summary == "" {
			t.Fatal("finalizeTerminalUnresolvedRun returned empty summary")
		}

		// Reopen database
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo2, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer repo2.Close()

		items, err := repo2.Query(context.Background(), contextstore.RepositoryQuery{
			Scope:             contextstore.Scope{ProjectID: "/project", TeamID: "team"},
			Visibility:        contextstore.VisibilitySubtree,
			IncludeCandidates: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
				t.Fatalf("found unresolved candidate %q after terminal unresolved fallback", it.ID)
			}
		}
	})
}
