package team

import (
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestBindTaskFailureDetailUsesTaskIdentity(t *testing.T) {
	detail := "source=error | current=worker=other todo=wrong | last_tool=write | error=canonicalize failed"
	got := bindTaskFailureDetail(detail, "reviewer", "Review assigned source", "todo-2", 3)
	for _, want := range []string{
		"source=error", "error=canonicalize failed", "current=task agent=reviewer todo_id=todo-2 attempt=3",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("bound failure detail %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "worker=other") || strings.Contains(got, "last_tool=write") {
		t.Fatalf("bound failure detail retained coordinator-global identity: %q", got)
	}
}

func TestBindTaskFailureDetailRetainsIdentityWhenRawDetailIsLong(t *testing.T) {
	detail := "source=error | error=" + strings.Repeat("x", 2000)
	got := bindTaskFailureDetail(detail, "worker", "long review", "todo-long", 4)
	if !strings.Contains(got, "todo_id=todo-long") || !strings.Contains(got, "agent=worker") {
		t.Fatalf("bounded failure detail = %q, lost task identity", got)
	}
}

func TestBindTaskFailureDetailRemainsTaskLocalUnderConcurrency(t *testing.T) {
	type failure struct {
		agent string
		task  string
		id    string
	}
	failures := []failure{
		{agent: "worker-a", task: "audit A", id: "todo-a"},
		{agent: "worker-b", task: "audit B", id: "todo-b"},
	}
	start := make(chan struct{})
	results := make([]string, len(failures))
	var wg sync.WaitGroup
	for i, item := range failures {
		wg.Add(1)
		go func(i int, item failure) {
			defer wg.Done()
			<-start
			results[i] = bindTaskFailureDetail("source=error | current=shared | last_tool=shared | error=failed", item.agent, item.task, item.id, i+1)
		}(i, item)
	}
	close(start)
	wg.Wait()
	for i, item := range failures {
		if !strings.Contains(results[i], "agent="+item.agent) || !strings.Contains(results[i], "todo_id="+item.id) {
			t.Fatalf("result[%d] = %q, missing task identity", i, results[i])
		}
		for j, other := range failures {
			if i != j && strings.Contains(results[i], other.agent) {
				t.Fatalf("result[%d] = %q contains another task's agent %q", i, results[i], other.agent)
			}
		}
	}
}

func TestFailureFingerprintDetailRemainsStableAcrossTaskIdentity(t *testing.T) {
	first := bindTaskFailureDetail("source=error | current=shared-a | last_tool=write | error=boom", "worker-a", "audit A", "todo-a", 1)
	second := bindTaskFailureDetail("source=error | current=shared-b | last_tool=read | error=boom", "worker-b", "audit B", "todo-b", 1)
	if got, want := failureDetailWithoutCoordinatorFields(first), "source=error | error=boom"; got != want {
		t.Fatalf("first fingerprint detail = %q, want %q", got, want)
	}
	if got, want := failureDetailWithoutCoordinatorFields(second), "source=error | error=boom"; got != want {
		t.Fatalf("second fingerprint detail = %q, want %q", got, want)
	}
}

func TestPersistFailureBindsDurableDetailButKeepsRawFailureProjection(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "audit source"}})[0]
	c := &Coordinator{
		taskTracker:    tracker,
		session:        &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "team"}},
		executionRunID: "run-1",
		reportStatus:   func(StatusEvent) {},
	}
	raw := "source=error | current=worker=wrong todo=wrong | last_tool=wrong | error=boom"
	c.PersistFailure("worker", item.Desc, item.ID, raw)

	if !strings.Contains(item.Detail, "todo_id="+item.ID) || !strings.Contains(item.Detail, "agent=worker") {
		t.Fatalf("persisted task detail = %q, want task-bound identity", item.Detail)
	}
	if strings.Contains(item.Detail, "worker=wrong") || strings.Contains(item.Detail, "last_tool=wrong") {
		t.Fatalf("persisted task detail retained global identity: %q", item.Detail)
	}
	_, _, _, remembered := c.GetLastFailureContext()
	if remembered != raw {
		t.Fatalf("last failure projection = %q, want raw caller detail %q", remembered, raw)
	}
}

func TestPersistFailureDoesNotTrustSameAgentAndTaskSnapshot(t *testing.T) {
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "same review"},
		{Agent: "worker", Desc: "same review"},
	})
	c := &Coordinator{
		taskTracker:    tracker,
		session:        &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "team"}},
		executionRunID: "run-1",
		reportStatus:   func(StatusEvent) {},
	}
	// Simulate the shared snapshot being left on the other concurrent Todo.
	c.updateSnapshot(func(s *currentSnapshot) {
		s.Agent = items[1].Agent
		s.Task = items[1].Desc
		s.TodoID = items[1].ID
		s.Stage = "tool"
		s.Tool = "read"
	})
	c.PersistFailure(items[0].Agent, items[0].Desc, items[0].ID, "source=error | error=boom-first")

	if !strings.Contains(items[0].Detail, "todo_id="+items[0].ID) || !strings.Contains(items[0].Detail, "agent=worker") {
		t.Fatalf("task detail = %q, want caller-owned task identity", items[0].Detail)
	}
	if strings.Contains(items[0].Detail, items[1].ID) || !strings.Contains(items[0].Detail, "error=boom-first") {
		t.Fatalf("task detail retained shared snapshot identity: %q", items[0].Detail)
	}

	// A second concurrent task with the same agent and description must receive
	// its own identity even when its raw error has no coordinator-global fields.
	c.PersistFailure(items[1].Agent, items[1].Desc, items[1].ID, "source=error | error=boom-second")
	if !strings.Contains(items[1].Detail, "todo_id="+items[1].ID) || !strings.Contains(items[1].Detail, "error=boom-second") {
		t.Fatalf("second task detail = %q, want caller-owned identity and raw error", items[1].Detail)
	}
	if strings.Contains(items[1].Detail, items[0].ID) || strings.Contains(items[0].Detail, "boom-second") {
		t.Fatalf("same-agent task details crossed ownership boundaries: first=%q second=%q", items[0].Detail, items[1].Detail)
	}
}
