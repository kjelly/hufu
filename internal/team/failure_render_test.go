package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func testFailureEvent() *FailureEventPayload {
	exitCode := 127
	return &FailureEventPayload{
		TaskID: "12", Phase: "verification", FailureClass: FailureEnvironment,
		RetryDisposition: ReplanRequired, Command: "tool status", WorkDir: "/project",
		Shell: "sh", ExitCode: &exitCode, Stdout: "", Stderr: "tool: not found",
		Fingerprint: "fp-12", Hint: "use an explicit relative path", Summary: "verification failed",
	}
}

func TestFailureRenderingIsSharedByWorkspaceAndJournal(t *testing.T) {
	event := testFailureEvent()
	want := RenderFailureText(event)
	if want == "" || strings.Contains(want, "task description") {
		t.Fatalf("unexpected canonical failure text: %q", want)
	}

	workspace := t.TempDir()
	if err := writeTaskFileWithFailureEvent(workspace, "demo", "coder", "20260802-000000", "error", "private task prompt", "", "legacy detail", event); err != nil {
		t.Fatal(err)
	}
	if err := writeStatusWithFailureEvent(workspace, "coder", "error", "private task prompt", "legacy detail", event); err != nil {
		t.Fatal(err)
	}
	taskData, err := os.ReadFile(filepath.Join(workspace, "tasks", "demo", "coder", "20260802-000000.md"))
	if err != nil {
		t.Fatal(err)
	}
	statusData, err := os.ReadFile(filepath.Join(workspace, "status", "coder.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(taskData), want) || !strings.Contains(string(statusData), "failure task_id=12") {
		t.Fatalf("workspace outputs did not use canonical failure rendering")
	}

	journal, err := openTaskJournal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{journal: journal, round: 3}
	coordinator.recordTaskFailureWithEvent("coder", strings.Repeat("private task prompt ", 1000), "legacy detail", event)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(taskJournalPath(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"failure_event"`) || !strings.Contains(string(data), `"failure_class":"environment"`) {
		t.Fatalf("journal missing structured failure event: %s", data)
	}
	if strings.Contains(string(data), "private task prompt") || strings.Contains(string(data), `"desc":"private task prompt"`) {
		t.Fatalf("journal err record retained full task prompt: %s", data)
	}
	if !strings.Contains(string(data), `"task_id":"12"`) {
		t.Fatalf("journal err record missing task id: %s", data)
	}
}

func TestFailureMarkdownUsesCanonicalText(t *testing.T) {
	event := testFailureEvent()
	markdown := RenderFailureMarkdown(event)
	if !strings.Contains(markdown, "```text") || !strings.Contains(markdown, RenderFailureText(event)) {
		t.Fatalf("markdown renderer diverged from canonical text: %q", markdown)
	}
}

func TestFailureDisplayTextUsesStructuredEvidenceAndMasksLegacyDetail(t *testing.T) {
	event := testFailureEvent()
	event.FailureClass = FailureEnvironment
	event.RetryDisposition = ReplanRequired
	item := &TodoItem{ID: "12", Status: TaskError, Detail: "api_token=raw-detail-secret", FailureEvent: event}
	got := FailureDisplayText(item)
	if strings.Contains(got, "raw-detail-secret") || !strings.Contains(got, "class=environment") || !strings.Contains(got, "disposition=replan_required") {
		t.Fatalf("structured failure display = %q", got)
	}

	legacy := FailureDisplayText(&TodoItem{ID: "legacy", Status: TaskError, Detail: "password=legacy-secret"})
	if strings.Contains(legacy, "legacy-secret") || !strings.Contains(legacy, "task_id=legacy") || !strings.Contains(legacy, "phase=legacy") {
		t.Fatalf("legacy failure display = %q", legacy)
	}

	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "done", Desc: "completed"}, {Agent: "failed", Desc: "failed"}})
	items[0].Status = TaskDone
	items[1].Status = TaskError
	items[1].Detail = "api_token=summary-secret"
	items[1].FailureEvent = event
	summary := (&Coordinator{taskTracker: tracker}).summaryFromTodos(errors.New("stopped"))
	if strings.Contains(summary, "summary-secret") || !strings.Contains(summary, "class=environment") {
		t.Fatalf("LLM-free text summary leaked or lost structured failure: %q", summary)
	}
}

func TestCoordinatorToolFailurePathsUseStructuredDisplay(t *testing.T) {
	event := testFailureEvent()
	event.FailureClass = FailureEnvironment
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "failed task"}})[0]
	item.Status = TaskError
	item.Detail = "api_token=tool-secret"
	item.FailureEvent = event
	c := &Coordinator{taskTracker: tracker}

	statusContext := c.buildTaskStatusContext()
	if strings.Contains(statusContext, "tool-secret") || !strings.Contains(statusContext, "class=environment") {
		t.Fatalf("task status context leaked or lost structured failure: %q", statusContext)
	}
	listResponse, err := (&todoTool{coordinator: c}).handleList("worker")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listResponse.Content, "tool-secret") || !strings.Contains(listResponse.Content, "class=environment") {
		t.Fatalf("todo tool list leaked or lost structured failure: %q", listResponse.Content)
	}
	failed := formatFailedTasks([]*TodoItem{item})
	if strings.Contains(failed, "tool-secret") || !strings.Contains(failed, "class=environment") {
		t.Fatalf("failed-task tool output leaked or lost structured failure: %q", failed)
	}
}

func TestCoordinatorToolDisplaysNormalDetailsWithoutFailureClassification(t *testing.T) {
	tests := []struct {
		name   string
		status TaskStatus
	}{
		{name: "pending", status: TaskPending},
		{name: "in progress", status: TaskInProgress},
		{name: "done", status: TaskDone},
		{name: "error", status: TaskError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTaskTracker()
			item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "task"}})[0]
			item.Status = tt.status
			item.Detail = "waiting for dependency api_token=detail-secret"
			if tt.status == TaskError {
				item.FailureEvent = &FailureEventPayload{TaskID: item.ID, Phase: "execution", FailureClass: FailureExecution, RetryDisposition: RetryNone, Summary: "failed"}
			}
			c := &Coordinator{taskTracker: tracker}
			statusContext := c.buildTaskStatusContext()
			listResponse, err := (&todoTool{coordinator: c}).handleList("worker")
			if err != nil {
				t.Fatal(err)
			}
			for name, output := range map[string]string{"status": statusContext, "list": listResponse.Content} {
				if tt.status != TaskError && !strings.Contains(output, "waiting for dependency") {
					t.Fatalf("%s output lost normal detail: %q", name, output)
				}
				if strings.Contains(output, "detail-secret") {
					t.Fatalf("%s output lost/redacted detail: %q", name, output)
				}
				if tt.status == TaskError && !strings.Contains(output, "class=execution") {
					t.Fatalf("%s output lost structured failure: %q", name, output)
				}
				if tt.status != TaskError && strings.Contains(output, "phase=legacy") {
					t.Fatalf("%s normal status was rendered as synthetic failure: %q", name, output)
				}
			}
		})
	}
}

func TestTeamInfoTodoStatusUsesStructuredFailureDisplay(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "failed"}})[0]
	item.Status = TaskError
	item.Detail = "password=legacy-secret"
	item.FailureEvent = &FailureEventPayload{TaskID: item.ID, Phase: "execution", FailureClass: FailureExecution, RetryDisposition: ReplanRequired, Summary: "worker failed"}
	c := &Coordinator{taskTracker: tracker, agentPool: &mockAgentPool{resolveDef: &agent.AgentDef{Name: "worker"}}}
	response, err := (&teamInfoTool{}).handleTodoStatus(c, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Content, "legacy-secret") || !strings.Contains(response.Content, "class=execution") || !strings.Contains(response.Content, "disposition=replan_required") {
		t.Fatalf("team info failure output = %q", response.Content)
	}
}

func TestRequestAgentDuplicateUsesNormalDetailOrStructuredFailure(t *testing.T) {
	tests := []struct {
		name       string
		status     TaskStatus
		failure    *FailureEventPayload
		wantDetail string
		wantFail   string
	}{
		{name: "normal", status: TaskPending, wantDetail: "waiting for dependency"},
		{name: "failure", status: TaskError, failure: &FailureEventPayload{Phase: "execution", FailureClass: FailureExecution, RetryDisposition: RetryNone, Summary: "worker failed"}, wantFail: "class=execution"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTaskTracker()
			item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "same task"}})[0]
			item.Status = tt.status
			item.Detail = tt.wantDetail + " api_token=duplicate-secret"
			item.FailureEvent = tt.failure
			c := &Coordinator{taskTracker: tracker, agentPool: &mockAgentPool{resolveDef: &agent.AgentDef{Name: "worker"}}}
			response, err := (&requestAgentTool{coordinator: c}).Run(context.Background(), fantasy.ToolCall{Input: `{"goal":"same task","agent":"worker"}`})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(response.Content, "duplicate-secret") {
				t.Fatalf("duplicate output leaked secret: %q", response.Content)
			}
			if tt.wantFail != "" && !strings.Contains(response.Content, tt.wantFail) {
				t.Fatalf("duplicate output lost structured failure: %q", response.Content)
			}
			if tt.wantDetail != "" && !strings.Contains(response.Content, tt.wantDetail) {
				t.Fatalf("duplicate output lost normal detail: %q", response.Content)
			}
		})
	}
}

func TestFailureEventAndEventStoreMaskSecrets(t *testing.T) {
	event := &FailureEventPayload{
		TaskID: "12", Phase: "verification", FailureClass: FailureVerify,
		RetryDisposition: RetryNone, Command: "curl api_token=command-secret-value",
		WorkDir: "/tmp/password=path-secret-value", Shell: "sh", Stdout: "token=stdout-secret-value",
		Stderr: "Authorization: Bearer bearer-secret-value", Hint: "secret=hint-secret-value",
		Summary: "api_key=summary-secret-value",
	}
	rendered := RenderFailureText(event)
	for _, secret := range []string{"command-secret-value", "path-secret-value", "stdout-secret-value", "bearer-secret-value", "hint-secret-value", "summary-secret-value"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("secret %q leaked from public renderer: %s", secret, rendered)
		}
	}
	publicJSON, err := json.Marshal(FailureEventsFromTodos([]*TodoItem{{ID: "12", FailureEvent: event}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicJSON), "secret-value") {
		t.Fatalf("secret leaked from JSON failure export: %s", publicJSON)
	}

	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"failure_event": event, "output": "password=output-secret-value"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(RunEvent{Type: "task_failed", TaskID: "12", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(workspace, logsDir, eventStoreFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"command-secret-value", "path-secret-value", "stdout-secret-value", "bearer-secret-value", "hint-secret-value", "summary-secret-value", "output-secret-value"} {
		if strings.Contains(string(stored), secret) {
			t.Fatalf("secret %q leaked into event store: %s", secret, stored)
		}
	}
}
