package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

func coordinatorWithTaskResult(todoID string, result *TaskResult) *Coordinator {
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{}},
		taskTracker: NewTaskTracker(),
	}
	c.storeSubmittedTaskResult(todoID, result)
	return c
}

// TestResolveFactRefs_SubstitutesFactByName proves the core value proposition:
// a coordinator never retypes a value an earlier task already discovered
// (here, a JSON list) into a later task's prose -- the runtime substitutes it
// verbatim from that task's own submitted result.
func TestResolveFactRefs_SubstitutesFactByName(t *testing.T) {
	c := coordinatorWithTaskResult("1", &TaskResult{
		TaskID: "1", Status: "success", Summary: "discovered targets", Source: "submitted",
		Facts: map[string]any{"container_ids": []any{"c1", "c2", "c3"}, "count": float64(3)},
	})

	tasks := []TaskDef{{
		Agent: "worker", Goal: "Inspect containers {ids}.", Constraints: "Expect {n} containers.",
		FactRefs: []FactRef{
			{Name: "ids", TaskID: "1", Fact: "container_ids"},
			{Name: "n", TaskID: "1", Fact: "count"},
		},
	}}

	resolved, err := c.resolveFactRefs(tasks)
	if err != nil {
		t.Fatalf("resolveFactRefs failed: %v", err)
	}
	if resolved[0].Goal != `Inspect containers ["c1","c2","c3"].` {
		t.Fatalf("goal = %q", resolved[0].Goal)
	}
	if resolved[0].Constraints != "Expect 3 containers." {
		t.Fatalf("constraints = %q", resolved[0].Constraints)
	}
	if resolved[0].FactRefs != nil {
		t.Fatalf("expected FactRefs cleared after resolution, got %#v", resolved[0].FactRefs)
	}
}

// TestResolveFactRefs_StringFactIsNotJSONQuoted proves a plain string fact
// (e.g. a resolved path) substitutes literally, not as a quoted JSON string.
func TestResolveFactRefs_StringFactIsNotJSONQuoted(t *testing.T) {
	c := coordinatorWithTaskResult("1", &TaskResult{
		TaskID: "1", Status: "success", Summary: "s", Source: "submitted",
		Facts: map[string]any{"resolved_path": "/workspace/out/report.json"},
	})
	tasks := []TaskDef{{Agent: "w", Goal: "Read {path}.", FactRefs: []FactRef{{Name: "path", TaskID: "1", Fact: "resolved_path"}}}}
	resolved, err := c.resolveFactRefs(tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved[0].Goal != "Read /workspace/out/report.json." {
		t.Fatalf("goal = %q", resolved[0].Goal)
	}
}

// TestResolveFactRefs_SubstitutesArtifactByDescription proves artifact
// resolution: the description acts as the stable name, resolving to that
// artifact's path.
func TestResolveFactRefs_SubstitutesArtifactByDescription(t *testing.T) {
	workspace := t.TempDir()
	artifactPath := filepath.Join(workspace, "inputs", "items.tsv")
	c := coordinatorWithTaskResult("1", &TaskResult{
		TaskID: "1", Status: "success", Summary: "s", Source: "submitted",
		Artifacts: []ArtifactRef{{Path: artifactPath, Description: "items"}},
	})
	tasks := []TaskDef{{Agent: "w", Goal: "Read {items}.", FactRefs: []FactRef{{Name: "items", TaskID: "1", Artifact: "items"}}}}
	resolved, err := c.resolveFactRefs(tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved[0].Goal != "Read "+artifactPath+"." {
		t.Fatalf("goal = %q", resolved[0].Goal)
	}
}

// Characterizes the legacy artifact hand-off: FactRef resolves a description
// to its path only. It does not verify the declared digest, producer attempt,
// or source freshness before another task consumes that path.
func TestCharacterizationFactRefAcceptsStaleArtifactPath(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "inputs", "items.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"items":["alpha","beta","gamma"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := coordinatorWithTaskResult("producer", &TaskResult{
		TaskID: "producer", Status: TaskResultStatusSuccess, Summary: "produced items", Source: "submitted",
		Artifacts: []ArtifactRef{{ID: "artifact-1", Path: path, Description: "items", SHA256: "digest-before-change", RunID: "run-1", Attempt: 1}},
	})
	if err := os.WriteFile(path, []byte(`{"items":["alpha","beta"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := c.resolveFactRefs([]TaskDef{{
		Agent: "worker", Goal: "consume {items}",
		FactRefs: []FactRef{{Name: "items", TaskID: "producer", Artifact: "items"}},
	}})
	if err != nil {
		t.Fatalf("legacy path resolution unexpectedly rejected changed artifact: %v", err)
	}
	if got, want := resolved[0].Goal, "consume "+path; got != want {
		t.Fatalf("goal = %q, want %q", got, want)
	}
}

func TestResolveFactRefs_NoFactRefsPassThroughUnchanged(t *testing.T) {
	c := &Coordinator{session: &TeamSession{}, taskTracker: NewTaskTracker()}
	tasks := []TaskDef{{Agent: "a", Goal: "g1"}, {Agent: "b", Goal: "g2"}}
	resolved, err := c.resolveFactRefs(tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resolved) != 2 || resolved[0].Goal != "g1" || resolved[1].Goal != "g2" {
		t.Fatalf("resolved = %#v", resolved)
	}
}

func TestResolveFactRefs_UnresolvedTaskFailsClosed(t *testing.T) {
	c := &Coordinator{session: &TeamSession{}, taskTracker: NewTaskTracker()}
	tasks := []TaskDef{{Agent: "w", Goal: "{x}", FactRefs: []FactRef{{Name: "x", TaskID: "99", Fact: "missing"}}}}
	if _, err := c.resolveFactRefs(tasks); err == nil || !strings.Contains(err.Error(), "no submitted result yet") {
		t.Fatalf("expected an unresolved-task error, got %v", err)
	}
}

func TestResolveFactRefs_UnknownFactNameFailsClosed(t *testing.T) {
	c := coordinatorWithTaskResult("1", &TaskResult{TaskID: "1", Status: "success", Summary: "s", Source: "submitted", Facts: map[string]any{"a": 1}})
	tasks := []TaskDef{{Agent: "w", Goal: "{x}", FactRefs: []FactRef{{Name: "x", TaskID: "1", Fact: "b"}}}}
	if _, err := c.resolveFactRefs(tasks); err == nil || !strings.Contains(err.Error(), `no fact named "b"`) {
		t.Fatalf("expected an unknown-fact error, got %v", err)
	}
}

func TestResolveFactRefs_UnknownArtifactDescriptionFailsClosed(t *testing.T) {
	c := coordinatorWithTaskResult("1", &TaskResult{TaskID: "1", Status: "success", Summary: "s", Source: "submitted"})
	tasks := []TaskDef{{Agent: "w", Goal: "{x}", FactRefs: []FactRef{{Name: "x", TaskID: "1", Artifact: "manifest"}}}}
	if _, err := c.resolveFactRefs(tasks); err == nil || !strings.Contains(err.Error(), `no artifact named "manifest"`) {
		t.Fatalf("expected an unknown-artifact error, got %v", err)
	}
}

func TestResolveFactRefs_RequiresExactlyOneOfFactOrArtifact(t *testing.T) {
	c := coordinatorWithTaskResult("1", &TaskResult{TaskID: "1", Status: "success", Summary: "s", Source: "submitted"})
	for _, ref := range []FactRef{
		{Name: "x", TaskID: "1"},                           // neither
		{Name: "x", TaskID: "1", Fact: "a", Artifact: "b"}, // both
	} {
		tasks := []TaskDef{{Agent: "w", Goal: "{x}", FactRefs: []FactRef{ref}}}
		if _, err := c.resolveFactRefs(tasks); err == nil || !strings.Contains(err.Error(), "exactly one of fact or artifact") {
			t.Fatalf("ref=%#v: expected an exactly-one error, got %v", ref, err)
		}
	}
}

// TestSubmitResultTool_StoresFacts proves the producer side end to end
// through the real submit_result tool, not just the TaskResult struct: a
// worker's own "facts" argument is parsed, stored, and retrievable via
// GetTaskResult -- exactly what resolveFactRefs reads from.
func TestSubmitResultTool_StoresFacts(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "discover targets"}})[0]

	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"done","facts":{"container_ids":["c1","c2"],"count":2}}`,
	})
	if err != nil {
		t.Fatalf("submit_result error = %v", err)
	}
	if response.IsError {
		t.Fatalf("submit_result rejected facts: %#v", response)
	}
	got := c.GetTaskResult(item.ID)
	if got == nil {
		t.Fatal("expected a stored task result")
	}
	ids, ok := got.Facts["container_ids"].([]any)
	if !ok || len(ids) != 2 || ids[0] != "c1" || ids[1] != "c2" {
		t.Fatalf("stored facts[container_ids] = %#v", got.Facts["container_ids"])
	}
}

// TestExecuteTasks_ResolvesFactRefsBeforeAnyTodoCreation proves the
// ExecuteTasks wiring end to end, mirroring the fan_out integration test.
func TestExecuteTasks_ResolvesFactRefsBeforeAnyTodoCreation(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{})
	c.session.Workspace = t.TempDir()
	c.storeSubmittedTaskResult("1", &TaskResult{TaskID: "1", Status: "success", Summary: "s", Source: "submitted", Facts: map[string]any{"a": "b"}})

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{
		{Agent: "worker", Goal: "x", FactRefs: []FactRef{{Name: "missing", TaskID: "1", Fact: "does-not-exist"}}},
	})
	if err == nil {
		t.Fatal("expected an error for an unresolvable fact_ref")
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 0 {
		t.Fatalf("rejected fact_ref dispatch created %d TODOs, want none", got)
	}
}
