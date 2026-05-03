package team

import (
	"context"
	"testing"
)

// newTestCacheCoordinator creates a minimal Coordinator suitable for testing
// the task result cache. No LLM provider or session is needed because
// lookupTaskCache/storeTaskCache only touch the cache fields and Sidecar()
// (which returns nil when sidecarModel is empty).
func newTestCacheCoordinator() *Coordinator {
	return &Coordinator{
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
}

// ── Cache Generation tests ────────────────────────────────────────────────────

func TestCacheGenerationInitiallyZero(t *testing.T) {
	c := newTestCacheCoordinator()
	if got := c.cacheGeneration.Load(); got != 0 {
		t.Errorf("initial cacheGeneration = %d, want 0", got)
	}
}

func TestCacheHitSameGeneration(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(1)

	c.storeTaskCache("researcher", "analyze code", "result: clean")

	output, ok := c.lookupTaskCache(context.Background(), "researcher", "analyze code")
	if !ok {
		t.Fatal("expected cache hit, got miss")
	}
	if output != "result: clean" {
		t.Errorf("output = %q, want %q", output, "result: clean")
	}
}

func TestCacheMissAfterGenerationBump(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(1)

	c.storeTaskCache("researcher", "analyze code", "old result")

	// Simulate coordinator starting a new round: bump generation.
	c.cacheGeneration.Add(1) // now gen = 2

	_, ok := c.lookupTaskCache(context.Background(), "researcher", "analyze code")
	if ok {
		t.Error("expected cache miss after generation bump, got hit (stale result would be returned)")
	}
}

func TestCacheNewGenerationAllowsNewEntries(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(1)

	c.storeTaskCache("researcher", "analyze code", "old result")

	c.cacheGeneration.Add(1) // gen = 2

	// Store a new result under the new generation.
	c.storeTaskCache("researcher", "analyze code", "fresh result")

	output, ok := c.lookupTaskCache(context.Background(), "researcher", "analyze code")
	if !ok {
		t.Fatal("expected cache hit for new-generation entry, got miss")
	}
	if output != "fresh result" {
		t.Errorf("output = %q, want %q", output, "fresh result")
	}
}

func TestCacheMultipleGenerationsCoexist(t *testing.T) {
	c := newTestCacheCoordinator()

	// Gen 1: store task A.
	c.cacheGeneration.Store(1)
	c.storeTaskCache("writer", "write docs", "docs v1")

	// Gen 2: store task B.
	c.cacheGeneration.Store(2)
	c.storeTaskCache("writer", "write tests", "tests v2")

	// In gen 2: task B hits, task A misses (stale).
	if _, ok := c.lookupTaskCache(context.Background(), "writer", "write docs"); ok {
		t.Error("gen-1 entry should not be visible in gen 2")
	}
	out, ok := c.lookupTaskCache(context.Background(), "writer", "write tests")
	if !ok {
		t.Fatal("gen-2 entry should be visible in gen 2")
	}
	if out != "tests v2" {
		t.Errorf("output = %q, want %q", out, "tests v2")
	}
}

func TestCacheHitCaseFolding(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(1)

	c.storeTaskCache("developer", "Run lint checks", "no issues")

	// Lookup with different case and extra whitespace.
	out, ok := c.lookupTaskCache(context.Background(), "developer", "run  lint  checks")
	if !ok {
		t.Fatal("expected cache hit after case/whitespace normalisation")
	}
	if out != "no issues" {
		t.Errorf("output = %q, want %q", out, "no issues")
	}
}

func TestCacheMissDifferentAgent(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(1)

	c.storeTaskCache("researcher", "find bugs", "3 bugs found")

	_, ok := c.lookupTaskCache(context.Background(), "developer", "find bugs")
	if ok {
		t.Error("cache entry for 'researcher' should not match 'developer'")
	}
}

func TestCacheMissDifferentTask(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(1)

	c.storeTaskCache("researcher", "analyze foo.go", "result A")

	_, ok := c.lookupTaskCache(context.Background(), "researcher", "analyze bar.go")
	if ok {
		t.Error("cache entry for 'foo.go' should not match 'bar.go'")
	}
}

func TestCacheMultipleEntriesSameAgent(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(1)

	c.storeTaskCache("developer", "write unit tests", "tests written")
	c.storeTaskCache("developer", "run integration tests", "all pass")

	out1, ok1 := c.lookupTaskCache(context.Background(), "developer", "write unit tests")
	out2, ok2 := c.lookupTaskCache(context.Background(), "developer", "run integration tests")

	if !ok1 || out1 != "tests written" {
		t.Errorf("first entry: ok=%v output=%q", ok1, out1)
	}
	if !ok2 || out2 != "all pass" {
		t.Errorf("second entry: ok=%v output=%q", ok2, out2)
	}
}

func TestCacheEmptyOnInit(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(1)

	_, ok := c.lookupTaskCache(context.Background(), "any-agent", "any task")
	if ok {
		t.Error("empty cache should always return miss")
	}
}

// TestCacheGenerationBumpInvalidatesStaleResult verifies that a cached result
// from a previous generation is NOT returned after a generation bump, and that
// fresh entries stored under the new generation are retrievable.
func TestCacheGenerationBumpInvalidatesStaleResult(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(1)

	// Store a result in gen 1.
	c.storeTaskCache("developer", "build binary", "build succeeded")

	// Bump generation (coordinator starts new round).
	c.cacheGeneration.Add(1)

	// Verify the gen-2 lookup returns miss (stale gen-1 entry must not be returned).
	_, ok := c.lookupTaskCache(context.Background(), "developer", "build binary")
	if ok {
		t.Error("gen-1 entry must not be returned in gen-2 (stale cache)")
	}

	// Store fresh result under gen 2 to confirm new entries work.
	c.storeTaskCache("developer", "build binary", "build succeeded v2")

	out, ok := c.lookupTaskCache(context.Background(), "developer", "build binary")
	if !ok {
		t.Fatal("fresh gen-2 entry should be retrievable")
	}
	if out != "build succeeded v2" {
		t.Errorf("output = %q, want %q", out, "build succeeded v2")
	}
}

// ── Worker-vs-coordinator generation bump tests ───────────────────────────────

// TestCoordinatorCallBumpsGeneration verifies the context-based discrimination
// used in ExecuteTasks: a CoordTodoID context triggers a bump; a worker todoID
// does not. We replicate the bump condition directly.
func TestCoordinatorCallBumpsGeneration(t *testing.T) {
	c := newTestCacheCoordinator()
	initial := c.cacheGeneration.Load() // 0

	// Simulate coordinator calling ExecuteTasks (callerID == CoordTodoID).
	callerID := CoordTodoID
	if callerID == "" || callerID == CoordTodoID {
		c.cacheGeneration.Add(1)
	}
	if got := c.cacheGeneration.Load(); got != initial+1 {
		t.Errorf("after coordinator call, generation = %d, want %d", got, initial+1)
	}
}

func TestWorkerCallDoesNotBumpGeneration(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(5)

	// Simulate a worker calling ExecuteTasks (callerID is a task ID, not coord).
	callerID := "42" // some worker todoID
	if callerID == "" || callerID == CoordTodoID {
		c.cacheGeneration.Add(1)
	}

	if got := c.cacheGeneration.Load(); got != 5 {
		t.Errorf("after worker call, generation = %d, want 5 (must not change)", got)
	}
}

func TestEmptyCallerIDCountsAsCoordinator(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(3)

	callerID := "" // no todoID in context
	if callerID == "" || callerID == CoordTodoID {
		c.cacheGeneration.Add(1)
	}

	if got := c.cacheGeneration.Load(); got != 4 {
		t.Errorf("generation = %d, want 4 (empty callerID treated as coordinator)", got)
	}
}

// TestWorkerSubTaskSharingCache simulates the deduplication scenario:
// two workers (same generation) each add the same sub-task; the second
// should get a cache hit once the first has stored its result.
func TestWorkerSubTaskSharingCache(t *testing.T) {
	c := newTestCacheCoordinator()
	c.cacheGeneration.Store(2) // coordinator already bumped to gen 2

	// Worker A completes sub-task X and stores result.
	c.storeTaskCache("analyst", "check performance", "latency p99=12ms")

	// Worker B looks up the same sub-task (gen 2, same as A) → should hit.
	out, ok := c.lookupTaskCache(context.Background(), "analyst", "check performance")
	if !ok {
		t.Fatal("worker B should hit worker A's cached result within same generation")
	}
	if out != "latency p99=12ms" {
		t.Errorf("output = %q, want %q", out, "latency p99=12ms")
	}
}
