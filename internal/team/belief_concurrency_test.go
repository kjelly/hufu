package team

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

type blockingBeliefReadJournal struct {
	ConditionalEventJournal
	once             sync.Once
	entered, release chan struct{}
	otherID          string
	otherAppended    chan struct{}
}

func (j *blockingBeliefReadJournal) VerifyHashChain(ctx context.Context) error {
	j.once.Do(func() { close(j.entered); <-j.release })
	return j.ConditionalEventJournal.VerifyHashChain(ctx)
}

func (j *blockingBeliefReadJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	durable, err := j.ConditionalEventJournal.Append(ctx, event)
	if err == nil && event.TaskID == j.otherID && event.Type == "task_completed" {
		close(j.otherAppended)
	}
	return durable, err
}

func TestBeliefBookkeepingReleasesTaskProjectionLock(t *testing.T) {
	c, _, first, events := newActionProgressTransitionCoordinator(t)
	events.SetBranchID("main")
	c.session.Dir = t.TempDir()
	writeBeliefModel(t, c.session.Dir)
	c.session.Config.Belief = &agent.BeliefConfig{ModelFile: "model.json", Experiment: "comparison"}
	configureBeliefTestPolicy(t, c.session)
	if err := compileTeamBelief(c.session); err != nil {
		t.Fatal(err)
	}
	c.refreshBeliefs(nil)
	_, other := createAdmittedTestTask(t, c, TaskDef{ID: "independent", Agent: "executor", Goal: "independent transition", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}})
	otherID := other.ID
	for _, id := range []string{first.ID, otherID} {
		receipt := &ExecutionReceipt{RunID: events.runID, TaskID: id, Attempt: 1, OccurrenceAttempt: 1, ModelExecutionID: "execution-" + id, RunInputSnapshotHash: "synthetic-input", VerifyResult: &VerificationResult{ExitCode: 0, Fingerprint: "synthetic-verifier", Command: "must-not-run"}}
		if err := c.taskTracker.TodoList().SetExecutionReceipt(id, receipt); err != nil {
			t.Fatal(err)
		}
	}
	journal := &blockingBeliefReadJournal{ConditionalEventJournal: c.EventJournal().(ConditionalEventJournal), entered: make(chan struct{}), release: make(chan struct{}), otherID: otherID, otherAppended: make(chan struct{})}
	c.SetEventJournal(journal)
	var wg sync.WaitGroup
	var release sync.Once
	defer func() { release.Do(func() { close(journal.release) }); wg.Wait() }()
	firstDone := make(chan error, 1)
	wg.Go(func() {
		firstDone <- c.CommitTaskTransition(t.Context(), first.ID, TaskInProgress, TaskDone, "", "", nil)
	})
	select {
	case <-journal.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("belief read did not start")
	}
	// The original task must already be published while its advisory reader
	// is blocked. Holding the lock through the read fails here and below.
	if c.todoItemByID(first.ID).Status != TaskDone {
		t.Fatal("belief I/O preceded live publication")
	}
	progressDone := make(chan error, 1)
	wg.Go(func() {
		progressDone <- c.CommitTaskTransition(t.Context(), otherID, TaskPending, TaskInProgress, "", "", nil)
	})
	select {
	case err := <-progressDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("independent task waited for belief I/O")
	}
	otherDone := make(chan error, 1)
	wg.Go(func() {
		otherDone <- c.CommitTaskTransition(t.Context(), otherID, TaskInProgress, TaskDone, "", "", nil)
	})
	select {
	case <-journal.otherAppended:
	case <-time.After(5 * time.Second):
		t.Fatal("independent terminal append waited for belief I/O")
	}
	published := make(chan TaskStatus, 1)
	wg.Go(func() {
		c.taskProjectionMu.Lock()
		defer c.taskProjectionMu.Unlock()
		published <- c.todoItemByID(otherID).Status
	})
	select {
	case status := <-published:
		if status != TaskDone {
			t.Fatalf("live publication=%s", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal publication held the task lock while waiting for beliefs")
	}
	release.Do(func() { close(journal.release) })
	for _, done := range []chan error{firstDone, otherDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("belief processing did not drain")
		}
	}
	assertActionProgressTransitionReplay(t, c, events)
	views := c.BeliefStates()
	if len(views) != 1 || len(views[0].Epochs) != 2 || len(views[0].Suggestions) != 2 {
		t.Fatalf("lost serialized belief updates: %+v", views)
	}
	for _, suggestion := range views[0].Suggestions {
		if suggestion.Record.DecisionGeneration != 1 || suggestion.Validation != "verified" {
			t.Fatalf("invalid suggestion after concurrent append: %+v", suggestion)
		}
	}
}
