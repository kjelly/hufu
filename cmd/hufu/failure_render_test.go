package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestFailureOutputsUseCanonicalPayload(t *testing.T) {
	exitCode := 127
	event := &team.FailureEventPayload{
		TaskID: "12", Phase: "verification", FailureClass: team.FailureEnvironment,
		RetryDisposition: team.ReplanRequired, Command: "tool status", WorkDir: "/project",
		Shell: "sh", ExitCode: &exitCode, Stderr: "tool: not found", Fingerprint: "fp-12",
		Hint: "use an explicit relative path", Summary: "verification failed",
	}
	canonical := team.RenderFailureText(event)
	status := team.StatusEvent{Type: "failure", Data: map[string]any{"failure_event": event}}
	if got := failureEventFromStatus(status); got == nil || team.RenderFailureText(got) != canonical {
		t.Fatalf("status event did not preserve canonical payload")
	}

	data := &reportData{Todos: []*team.TodoItem{{ID: "12", Status: team.TaskProtocolIncomplete, Detail: "api_token=table-secret", FailureEvent: event}}}
	report := buildReportMD(data, "demo", "")
	if !strings.Contains(report, canonical) || strings.Contains(report, "table-secret") {
		t.Fatalf("report missing canonical failure rendering: %s", report)
	}

	tracker := team.NewTaskTracker()
	items := tracker.TodoList().AddBatch([]team.TodoSpec{{Agent: "coder", Desc: "private task prompt"}})
	items[0].Status = team.TaskError
	items[0].Detail = "api_token=unresolved-secret"
	if err := tracker.TodoList().SetFailureEvent(items[0].ID, event); err != nil {
		t.Fatal(err)
	}
	coord := &team.Coordinator{}
	coord.SetTaskTracker(tracker)
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	tc := &teamContext{teamName: "demo", coordinator: coord}
	err = printResultJSON("failed", map[string]*teamContext{"demo": tc}, nil)
	_ = w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(r)
	var output jsonRunOutput
	if err := json.Unmarshal(raw.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Failures) != 1 || team.RenderFailureText(&output.Failures[0]) != canonical {
		t.Fatalf("JSON output did not preserve canonical failure payload: %#v", output.Failures)
	}
	if len(output.UnresolvedTasks) != 1 || !strings.Contains(output.UnresolvedTasks[0].Error, "class=environment") || strings.Contains(output.UnresolvedTasks[0].Error, "unresolved-secret") {
		t.Fatalf("JSON unresolved task error = %#v", output.UnresolvedTasks)
	}
}

func TestFinalUnresolvedCLIErrorUsesStructuredFailureDisplay(t *testing.T) {
	item := &team.TodoItem{
		ID: "12", Agent: "worker", Status: team.TaskError,
		Detail: "api_token=cli-secret",
		FailureEvent: &team.FailureEventPayload{
			TaskID: "12", Phase: "verification", FailureClass: team.FailureEnvironment,
			RetryDisposition: team.ReplanRequired, Summary: "verification failed",
		},
	}
	err := formatUnresolvedTaskError("demo", item)
	if strings.Contains(err.Error(), "cli-secret") || !strings.Contains(err.Error(), "class=environment") || !strings.Contains(err.Error(), "disposition=replan_required") {
		t.Fatalf("final unresolved CLI error = %q", err)
	}
}
