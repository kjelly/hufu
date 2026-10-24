package team

import (
	"testing"
	"testing/synctest"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// cleanupTestCoordinator must be registered after the workspace and any injected
// resource cleanup. Callers must finish public invocations before test cleanup
// starts: Close can then drain all admitted writers before stores and directories
// are removed in testing's LIFO cleanup order.
func cleanupTestCoordinator(t *testing.T, c *Coordinator) {
	t.Helper()
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close test coordinator: %v", err)
		}
	})
}

type cleanupOrderRepository struct {
	contextstore.Repository
	close func() error
}

func (r cleanupOrderRepository) Close() error { return r.close() }

func TestCoordinatorFixtureCleanupDrainsBeforeResources(t *testing.T) {
	writerDone, storeClosed, workspaceRemoved := false, false, false
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(func() {
			if !storeClosed {
				t.Error("workspace removed before store closed")
			}
			workspaceRemoved = true
		})
		repo := cleanupOrderRepository{close: func() error {
			if !writerDone {
				t.Error("store closed before asynchronous writer completed")
			}
			storeClosed = true
			return nil
		}}
		// Injected stores belong to the fixture, not the coordinator.
		t.Cleanup(func() { _ = repo.Close() })
		c := &Coordinator{contextRepo: repo}
		cleanupTestCoordinator(t, c)
		c.asyncTasksWg.Go(func() {
			// Virtual time makes a writer slower than the old 100ms grace
			// deterministic without adding a real delay to the suite.
			time.Sleep(time.Second)
			writerDone = true
		})
	})
	if !writerDone || !storeClosed || !workspaceRemoved {
		t.Fatal("fixture cleanup did not complete every lifecycle stage")
	}
}

func TestCoordinatorFixtureCleanupWithoutWorkDoesNotWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		t.Cleanup(func() {
			if elapsed := time.Since(start); elapsed != 0 {
				t.Fatalf("idle cleanup advanced virtual time by %v", elapsed)
			}
		})
		cleanupTestCoordinator(t, &Coordinator{})
	})
}
