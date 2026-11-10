package team

import (
	"context"
	"github.com/kjelly/hufu/internal/execution"
	"testing"
)

func testTaskCache(c *Coordinator) *defaultTaskCache {
	cache, ok := c.TaskCache().(*defaultTaskCache)
	if !ok {
		panic("coordinator does not use defaultTaskCache")
	}
	return cache
}

// newTestCacheCoordinator creates a minimal Coordinator suitable for testing
// the task result cache. No LLM provider or session is needed because
// lookupTaskCache/storeTaskCache only touch the cache fields and Sidecar()
// (which returns nil when sidecarModel is empty).
func newTestCacheCoordinator() *Coordinator {
	return &Coordinator{
		taskCache: newDefaultTaskCache(taskCacheDependencies{}),
	}
}

// ── Cache Generation tests ────────────────────────────────────────────────────

func TestCacheGenerationInitiallyZero(t *testing.T) {
	c := newTestCacheCoordinator()
	if got := testTaskCache(c).generation.Load(); got != 0 {
		t.Errorf("initial cacheGeneration = %d, want 0", got)
	}
}

func TestCacheHitSameGeneration(t *testing.T) {
	c := newTestCacheCoordinator()
	testTaskCache(c).generation.Store(1)

	c.storeTaskCache("researcher", "analyze code", "result: clean")

	output, ok := c.lookupTaskCache(context.Background(), "researcher", "analyze code")
	if !ok {
		t.Fatal("expected cache hit, got miss")
	}
	if output != "result: clean" {
		t.Errorf("output = %q, want %q", output, "result: clean")
	}
}

func TestCacheMultipleGenerationsCoexist(t *testing.T) {
	c := newTestCacheCoordinator()

	// Gen 1: store task A.
	testTaskCache(c).generation.Store(1)
	c.storeTaskCache("writer", "write docs", "docs v1")

	// Gen 2: store task B.
	testTaskCache(c).generation.Store(2)
	c.storeTaskCache("writer", "write tests", "tests v2")

	// In gen 2: both tasks hit via cross-generation exact match.
	outA, okA := c.lookupTaskCache(context.Background(), "writer", "write docs")
	if !okA {
		t.Fatal("gen-1 entry should be visible in gen 2 (exact match across generations)")
	}
	if outA != "docs v1" {
		t.Errorf("write docs output = %q, want %q", outA, "docs v1")
	}
	outB, okB := c.lookupTaskCache(context.Background(), "writer", "write tests")
	if !okB {
		t.Fatal("gen-2 entry should be visible in gen 2")
	}
	if outB != "tests v2" {
		t.Errorf("write tests output = %q, want %q", outB, "tests v2")
	}
}

func TestCacheHitCaseFolding(t *testing.T) {
	c := newTestCacheCoordinator()
	testTaskCache(c).generation.Store(1)

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

func TestCacheVerifyMustMatch(t *testing.T) {
	c := newTestCacheCoordinator()
	testTaskCache(c).generation.Store(1)

	c.storeTaskCacheWithVerify("developer", "build binary", "test -f bin/app", "build succeeded")

	if _, ok := c.lookupTaskCacheWithVerify(context.Background(), "developer", "build binary", "test -f bin/app"); !ok {
		t.Fatal("expected exact verify match to hit")
	}
	if _, ok := c.lookupTaskCacheWithVerify(context.Background(), "developer", "build binary", "test -f bin/other"); ok {
		t.Fatal("expected different verify command to miss")
	}
}

func TestCacheVerifyModeMustMatch(t *testing.T) {
	c := newTestCacheCoordinator()
	c.storeTaskCacheWithVerification("developer", "inspect service", "systemctl is-active api", "success", "service is active")

	if _, ok := c.lookupTaskCacheWithVerification(context.Background(), "developer", "inspect service", "systemctl is-active api", "expected_failure"); ok {
		t.Fatal("expected_failure must not reuse a success-mode cache result")
	}
	if _, ok := c.lookupTaskCacheWithVerification(context.Background(), "developer", "inspect service", "systemctl is-active api", "observation"); ok {
		t.Fatal("observation must not reuse a success-mode cache result")
	}
	if got, ok := c.lookupTaskCacheWithVerification(context.Background(), "developer", "inspect service", "systemctl is-active api", ""); !ok || got != "service is active" {
		t.Fatalf("default success mode cache lookup = (%q, %t), want stored result", got, ok)
	}
}

func TestSessionCacheRestoreKeepsVerifyMode(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		taskCache:   newDefaultTaskCache(taskCacheDependencies{}),
	}
	c.SetSessionData(&SessionData{Tasks: []*TodoItem{{
		ID:         "1",
		Agent:      "developer",
		Desc:       "inspect service",
		Status:     TaskDone,
		Verify:     "systemctl is-active api",
		VerifyMode: "success",
		Output:     "active",
	}}})

	if _, ok := c.lookupTaskCacheWithVerification(context.Background(), "developer", "inspect service", "systemctl is-active api", "expected_failure"); ok {
		t.Fatal("restored success-mode result must not satisfy expected_failure")
	}
	if got, ok := c.lookupTaskCacheWithVerification(context.Background(), "developer", "inspect service", "systemctl is-active api", "success"); !ok || got != "active" {
		t.Fatalf("restored success-mode result = (%q, %t), want active", got, ok)
	}
}

func TestCacheMissDifferentAgent(t *testing.T) {
	c := newTestCacheCoordinator()
	testTaskCache(c).generation.Store(1)

	c.storeTaskCache("researcher", "find bugs", "3 bugs found")

	_, ok := c.lookupTaskCache(context.Background(), "developer", "find bugs")
	if ok {
		t.Error("cache entry for 'researcher' should not match 'developer'")
	}
}

func TestCacheMissDifferentTask(t *testing.T) {
	c := newTestCacheCoordinator()
	testTaskCache(c).generation.Store(1)

	c.storeTaskCache("researcher", "analyze foo.go", "result A")

	_, ok := c.lookupTaskCache(context.Background(), "researcher", "analyze bar.go")
	if ok {
		t.Error("cache entry for 'foo.go' should not match 'bar.go'")
	}
}

func TestCacheMultipleEntriesSameAgent(t *testing.T) {
	c := newTestCacheCoordinator()
	testTaskCache(c).generation.Store(1)

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
	testTaskCache(c).generation.Store(1)

	_, ok := c.lookupTaskCache(context.Background(), "any-agent", "any task")
	if ok {
		t.Error("empty cache should always return miss")
	}
}

// TestCacheGenerationBumpPreservesExactMatchAndAcceptsFreshResult verifies that an exact-match
// lookup returns the cached result even after a generation bump, since the
// same goal text means the same work. Fresh entries stored under the new
// generation are also retrievable.
func TestCacheGenerationBumpPreservesExactMatchAndAcceptsFreshResult(t *testing.T) {
	c := newTestCacheCoordinator()
	testTaskCache(c).generation.Store(1)

	// Store a result in gen 1.
	c.storeTaskCache("developer", "build binary", "build succeeded")

	// Bump generation (coordinator starts new round).
	testTaskCache(c).generation.Add(1)

	// Exact match across generations → HIT (same goal = same work).
	out, ok := c.lookupTaskCache(context.Background(), "developer", "build binary")
	if !ok {
		t.Fatal("exact match across generations should hit")
	}
	if out != "build succeeded" {
		t.Errorf("output = %q, want %q", out, "build succeeded")
	}

	// Store fresh result under gen 2 to confirm new entries work.
	c.storeTaskCache("developer", "build binary", "build succeeded v2")

	out2, ok2 := c.lookupTaskCache(context.Background(), "developer", "build binary")
	if !ok2 {
		t.Fatal("fresh gen-2 entry should be retrievable")
	}
	if out2 != "build succeeded v2" {
		t.Errorf("output = %q, want %q", out2, "build succeeded v2")
	}
}

// ── Worker-vs-coordinator generation bump tests ───────────────────────────────

func TestExecuteTasksCacheGeneration(t *testing.T) {
	for _, tt := range []struct {
		name, caller string
		want         int64
	}{
		{"coordinator", CoordTodoID, 6}, {"worker", "42", 5}, {"empty caller", "", 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			worker := scriptWorker(textTurn("done"))
			c := newTestCoordinator(t, withWorker(worker))
			c.delegatedTasks = make(map[string]int)
			c.maxConcurrent = 1
			c.session.Config.AllowFreeTextResults = true
			c.session.Agents["worker"].SideEffect = string(SideEffectNone)
			registry := NewExecutionRegistry()
			if err := registry.Register(fakeLanguageModelBackend{fakeExecutionBackend{name: "ollama", kind: execution.BackendKindLLM}}); err != nil {
				t.Fatal(err)
			}
			c.SetExecutionRegistry(registry)
			testTaskCache(c).generation.Store(5)
			ctx := withTestProtocolRepairInvocationContext(t.Context())
			if tt.caller != "" {
				ctx = context.WithValue(ctx, todoIDKey{}, tt.caller)
			}
			if output, err := c.ExecuteTasks(ctx, []TaskDef{{Agent: "worker", Goal: "complete delegated work", SideEffect: SideEffectNone}}); err != nil {
				t.Fatalf("dispatch: %v; output: %s; items: %+v", err, output, c.taskTracker.TodoList().Items())
			}
			if worker.Calls() != 1 {
				t.Fatalf("worker calls = %d, want 1", worker.Calls())
			}
			if got := testTaskCache(c).generation.Load(); got != tt.want {
				t.Fatalf("generation = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestWorkerSubTaskSharingCache simulates the deduplication scenario:
// two workers (same generation) each add the same sub-task; the second
// should get a cache hit once the first has stored its result.
func TestWorkerSubTaskSharingCache(t *testing.T) {
	c := newTestCacheCoordinator()
	testTaskCache(c).generation.Store(2) // coordinator already bumped to gen 2

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
