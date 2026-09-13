package team

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskCacheServiceStableGetterAndRuntimeInjection(t *testing.T) {
	c := &Coordinator{}
	first := c.TaskCache()
	if first != c.TaskCache() {
		t.Fatal("TaskCache returned a new instance on the second call")
	}
	if got := c.RuntimeServices().TaskCache; got != first {
		t.Fatal("RuntimeServices did not expose the active task cache")
	}

	replacement := newDefaultTaskCache(taskCacheDependencies{})
	c.setRuntimeServices(RuntimeServices{TaskCache: replacement})
	if got := c.TaskCache(); got != replacement {
		t.Fatal("runtime service injection did not replace the task cache")
	}
}

func TestTaskCacheServiceForkIsDeepAndIsolated(t *testing.T) {
	var parentJournalWrites atomic.Int64
	parent := newDefaultTaskCache(taskCacheDependencies{
		Identity: func(TaskCacheLookupRequest) CacheIdentity {
			return CacheIdentity{AgentIdentity: "parent"}
		},
		AppendJournal: func(journalRecord) {
			parentJournalWrites.Add(1)
		},
	})
	parent.generation.Store(7)
	spec := &VerificationSpec{Type: VerifyCommandExit, Command: "true"}
	verification := &VerificationResult{Spec: spec, ExitCode: 0, EvaluatedAt: time.Now(), Fingerprint: "fingerprint"}
	parent.Restore([]TaskCacheSeed{{
		AgentKey: "builder", Task: "restored", Output: "parent output",
		VerifySpec: spec, Verification: verification, Pinned: true,
	}})
	parent.Store(TaskCacheStoreRequest{AgentKey: "builder", Task: "parent only", Output: "parent"})

	child := parent.Fork(taskCacheDependencies{
		Identity: func(TaskCacheLookupRequest) CacheIdentity {
			return CacheIdentity{AgentIdentity: "child"}
		},
	}).(*defaultTaskCache)
	if got := child.currentGeneration(); got != 0 {
		t.Fatalf("fork active generation = %d, want 0", got)
	}
	if got := child.entries["builder"][0].generation; got != 7 {
		t.Fatalf("forked entry generation = %d, want retained generation 7", got)
	}
	if child.entries["builder"][0].verifySpec == parent.entries["builder"][0].verifySpec {
		t.Fatal("fork shared VerificationSpec pointer with parent")
	}
	if child.entries["builder"][0].verification == parent.entries["builder"][0].verification {
		t.Fatal("fork shared VerificationResult pointer with parent")
	}

	child.entries["builder"][0].verifySpec.Command = "false"
	child.Invalidate(TaskCacheInvalidateRequest{AgentKey: "builder", Task: "parent only"})
	child.Store(TaskCacheStoreRequest{AgentKey: "builder", Task: "child only", Output: "child"})
	if got := parent.entries["builder"][0].verifySpec.Command; got != "true" {
		t.Fatalf("child mutation changed parent verification spec to %q", got)
	}
	if _, ok := parent.Lookup(t.Context(), TaskCacheLookupRequest{Scope: TaskCacheLookupExecution, AgentKey: "builder", Task: "parent only"}); !ok {
		t.Fatal("child invalidation removed the parent entry")
	}
	childEntry := child.entries["builder"][len(child.entries["builder"])-1]
	if got := childEntry.identity.AgentIdentity; got != "child" {
		t.Fatalf("child store used identity %q, want clone dependency", got)
	}
	if got := parentJournalWrites.Load(); got != 1 {
		t.Fatalf("child mutation wrote parent journal; parent writes = %d, want 1", got)
	}
}

func TestTaskCacheServiceRestorePreservesOrderAndDeduplicatesJournalSeeds(t *testing.T) {
	cache := newDefaultTaskCache(taskCacheDependencies{})
	cache.Restore([]TaskCacheSeed{
		{AgentKey: "worker", Task: "one", Output: "first", Pinned: true},
		{AgentKey: "worker", Task: "one", Output: "second", Pinned: true},
	})
	cache.Restore([]TaskCacheSeed{
		{AgentKey: "worker", Task: "one", Output: "journal duplicate", Pinned: true, Deduplicate: true},
		{AgentKey: "worker", Task: "two", Output: "journal new", Pinned: true, Deduplicate: true},
	})

	entries := cache.entriesFor("worker")
	if len(entries) != 3 {
		t.Fatalf("restored entry count = %d, want 3", len(entries))
	}
	if entries[0].output != "first" || entries[1].output != "second" || entries[2].taskDesc != "two" {
		t.Fatalf("restore order changed: %#v", entries)
	}
	for _, entry := range entries {
		if !entry.pinned {
			t.Fatalf("restored entry is not pinned: %#v", entry)
		}
	}
}

func TestTaskCacheServiceConcurrentOperations(t *testing.T) {
	cache := newDefaultTaskCache(taskCacheDependencies{})
	ctx := t.Context()
	var wg sync.WaitGroup
	for i := range 32 {
		i := i
		wg.Go(func() {
			task := fmt.Sprintf("task-%d", i)
			cache.Store(TaskCacheStoreRequest{AgentKey: "worker", Task: task, Output: task})
			cache.Lookup(ctx, TaskCacheLookupRequest{Scope: TaskCacheLookupExecution, AgentKey: "worker", Task: task})
			cache.Restore([]TaskCacheSeed{{AgentKey: "worker", Task: task + "-restored", Output: task, Pinned: true}})
			cache.Invalidate(TaskCacheInvalidateRequest{AgentKey: "worker", Task: task})
		})
	}
	wg.Wait()
}
