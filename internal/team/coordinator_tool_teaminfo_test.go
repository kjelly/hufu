package team

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestTeamInfoTaskResultSelectorDisambiguatesCompletedTasks(t *testing.T) {
	first := taskDescription("## Task Description\nExecute exactly the §3.1 binary freeze sequence.\n\n## Result\npass")
	second := taskDescription("## Task Description\nExecute exactly the §3.2 topology creation sequence.\n\n## Result\npass")

	if !taskDescriptionMatches(first, "§3.1 binary freeze") {
		t.Fatalf("§3.1 selector did not match task description %q", first)
	}
	if taskDescriptionMatches(second, "§3.1 binary freeze") {
		t.Fatalf("§3.1 selector incorrectly matched task description %q", second)
	}
	if !taskDescriptionMatches(first, "") {
		t.Fatal("empty selector should preserve most-recent-result behavior")
	}
	if !strings.Contains(first, "§3.1") {
		t.Fatalf("test fixture lost checkpoint marker: %q", first)
	}
}

func TestTaskResultByIDSurvivesSessionReplayWithSealedManifest(t *testing.T) {
	workspace := t.TempDir()
	first := &Coordinator{session: &TeamSession{Workspace: workspace}, sessionData: NewSession(), taskTracker: NewTaskTracker()}
	item := first.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runner", Desc: "produce evidence"}})[0]
	if err := first.taskTracker.TodoList().SetTypedResult(item.ID, &TaskResult{
		TaskID: item.ID, Status: TaskResultStatusSuccess, Summary: "complete",
		RawOutputRef: &ArtifactRef{Path: "logs/task-output/1.jsonl", SHA256: "sealed", Bytes: 42},
	}); err != nil {
		t.Fatalf("set typed result: %v", err)
	}
	if err := first.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskDone, "complete", "complete"); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	first.saveCheckpoint()

	replayed := LoadSession(workspace)
	if replayed == nil {
		t.Fatal("session was not saved")
	}
	second := &Coordinator{session: &TeamSession{Workspace: workspace}, sessionData: replayed, taskTracker: NewTaskTracker(), taskResultCache: make(map[string][]cachedTaskEntry)}
	second.SetSessionData(replayed)
	response, err := taskResultByID(second, item.ID)
	if err != nil || response.IsError {
		t.Fatalf("task_result after replay: response=%#v err=%v", response, err)
	}
	if !strings.Contains(response.Content, "VERBATIM TRANSCRIPT CAPTURED") || !strings.Contains(response.Content, "sha256=sealed") {
		t.Fatalf("replayed task result omitted sealed manifest: %s", response.Content)
	}

	// Exercise the public tool schema as well: task_id requires no agent.
	tool := &teamInfoTool{coordinator: second}
	response, err = tool.Run(context.Background(), fantasy.ToolCall{Input: `{"action":"task_result","task_id":"1"}`})
	if err != nil || response.IsError || !strings.Contains(response.Content, "Completed task 1") {
		t.Fatalf("public task_id lookup = %#v err=%v", response, err)
	}
}

func TestInMemoryCompletedTaskResultBridgesTaskFilePublication(t *testing.T) {
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "poc-step-runner", Desc: `Execute exactly the §3.1 \"Freeze the candidate\" checkpoint.`}})
	if len(items) != 1 {
		t.Fatal("expected one task")
	}
	if err := tracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskDone, "", ""); err != nil {
		t.Fatalf("mark task done: %v", err)
	}
	if err := tracker.TodoList().SetTypedResult(items[0].ID, &TaskResult{
		Status:       TaskResultStatusSuccess,
		Summary:      "freeze complete",
		RawOutputRef: &ArtifactRef{Path: "/tmp/run/logs/task-output/3.jsonl", SHA256: "abc", Bytes: 3},
	}); err != nil {
		t.Fatalf("set typed result: %v", err)
	}
	coord := &Coordinator{taskTracker: tracker}
	got, ok := inMemoryCompletedTaskResult(coord, "poc-step-runner", `§3.1 \"Freeze the candidate\" checkpoint`)
	if !ok {
		t.Fatal("expected in-memory completed result")
	}
	if !strings.Contains(got, "VERBATIM TRANSCRIPT CAPTURED") || !strings.Contains(got, "/tmp/run/logs/task-output/3.jsonl") {
		t.Fatalf("fallback omitted transcript manifest: %s", got)
	}
}

func TestCompletedTaskResultForAgentNormalizesLegacyEscapedSelector(t *testing.T) {
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "runner", Desc: `checkpoint "東京"`}})
	if err := tracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskDone, "", "fallback output"); err != nil {
		t.Fatalf("mark task done: %v", err)
	}
	coord := &Coordinator{taskTracker: tracker}
	got, ok, candidates := completedTaskResultForAgent(coord, "runner", `checkpoint \"東京\"`)
	if !ok || len(candidates) != 0 {
		t.Fatalf("escaped selector result = (%q, %v, %v), want match", got, ok, candidates)
	}
	if !strings.Contains(got, items[0].ID) || !strings.Contains(got, "fallback output") {
		t.Fatalf("result omitted stable task ID or output: %s", got)
	}
}

func TestCompletedTaskResultForAgentRequiresTaskIDForAmbiguousSelector(t *testing.T) {
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "runner", Desc: "shared checkpoint A"},
		{Agent: "runner", Desc: "shared checkpoint B"},
	})
	for _, item := range items {
		if err := tracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskDone, "", "output"); err != nil {
			t.Fatalf("mark task done: %v", err)
		}
	}
	coord := &Coordinator{taskTracker: tracker}
	_, ok, candidates := completedTaskResultForAgent(coord, "runner", "shared checkpoint")
	if ok || len(candidates) != 2 {
		t.Fatalf("ambiguous selector = (ok=%v candidates=%v), want two candidates", ok, candidates)
	}
}
