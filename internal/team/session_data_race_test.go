package team

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// newSessionRaceCoordinator builds a minimal Coordinator whose shared
// sessionData is exercised by persistContextManifest and saveCheckpoint. It
// omits the worker agent and event store: persistContextManifest no-ops its
// event/observation/report side effects when those sub-services are absent,
// so the test isolates the session-state concurrency.
func newSessionRaceCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name:    "session-race",
				Timeout: 30,
			},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:    time.Now(),
		taskTracker:    NewTaskTracker(),
		reportStatus:   func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID: "run-race",
	}
	return c
}

// TestSessionDataConcurrentCheckpointNoRace drives persistContextManifest and
// saveCheckpoint from many goroutines against a single shared Coordinator,
// which is the race that parallel task execution (dag_scheduler -> executeTask)
// creates. Under -race this must report no data race, and the final persisted
// session must reflect a coherent superset of the manifests and tasks written.
func TestSessionDataConcurrentCheckpointNoRace(t *testing.T) {
	c := newSessionRaceCoordinator(t)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "race-task-1"},
		{Agent: "worker", Desc: "race-task-2"},
	})
	taskIDs := []string{items[0].ID, items[1].ID}

	const goroutines = 4
	const iterations = 20
	// A bounded set of coordinator-manifest identities keeps the session JSON
	// small: persistContextManifest replaces an existing identity rather than
	// appending, so the snapshot/SaveSession marshal stays O(1) per iteration
	// instead of growing O(n). The race detector only needs concurrent access,
	// not volume, to flag a hazard.
	coordIDs := [4]string{"c0", "c1", "c2", "c3"}
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Task-scoped manifest: writes sessionData.Tasks via the todo list.
				taskID := taskIDs[g%len(taskIDs)]
				manifest := &ContextInjectionManifest{
					TaskID:           taskID,
					Agent:            "worker",
					RequestID:        fmt.Sprintf("req-%d-%d", g, i),
					ModelExecutionID: fmt.Sprintf("exec-%d-%d", g, i),
					Attempt:          i,
					Trigger:          "test",
					Purpose:          "race",
				}
				if err := c.persistContextManifest(manifest); err != nil {
					t.Errorf("persistContextManifest task-scoped: %v", err)
					return
				}
				// Coordinator-scoped manifest: reuses a bounded identity so the
				// slice replaces instead of growing without bound.
				coord := &ContextInjectionManifest{
					Agent:            "coordinator",
					RequestID:        coordIDs[i%len(coordIDs)],
					ModelExecutionID: coordIDs[i%len(coordIDs)],
					Attempt:          i,
					Trigger:          "test",
					Purpose:          "race",
				}
				if err := c.persistContextManifest(coord); err != nil {
					t.Errorf("persistContextManifest coordinator-scoped: %v", err)
					return
				}
				c.saveCheckpoint()
			}
		}()
	}
	wg.Wait()

	// Read the final coordinator manifests under the read lock.
	var manifestCount int
	c.viewSessionData(func(sd *SessionData) {
		manifestCount = len(sd.CoordinatorContextManifests)
	})
	// Every task-scoped manifest is dual-written to the todo list; the
	// coordinator-scoped manifests accumulate on the session. Both must be
	// present and non-empty after the storm.
	if manifestCount == 0 {
		t.Fatalf("expected coordinator manifests, got 0")
	}
	for _, tid := range taskIDs {
		if !c.taskTracker.TodoList().Has(tid) {
			t.Fatalf("task %s disappeared from tracker after concurrent checkpoints", tid)
		}
	}
}

// TestSessionDataConcurrentReadWhileWrite verifies readers (snapshotSessionData
// and the acceptance/manifest report path) observe a consistent copy while
// writers mutate the shared state, exercising the RWMutex read/write split.
func TestSessionDataConcurrentReadWhileWrite(t *testing.T) {
	c := newSessionRaceCoordinator(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			// Bounded identity: replace instead of append to keep the session
			// snapshot marshal O(1) while still mutating under the write lock.
			coord := &ContextInjectionManifest{
				Agent:            "coordinator",
				RequestID:        "rreq",
				ModelExecutionID: "rexec",
				Attempt:          i,
			}
			_ = c.persistContextManifest(coord)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			var manifests []ContextInjectionManifest
			c.viewSessionData(func(sd *SessionData) {
				manifests = append(manifests, sd.CoordinatorContextManifests...)
			})
			_ = manifests
		}
	}()
	wg.Wait()
	_ = context.Background()
}
