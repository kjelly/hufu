package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestFailureEventAttachmentClearsStaleSuccessDetail is the regression for a
// status file a real run left behind: status "working", detail "Task completed
// successfully. Both deliverables exist and are verified", and immediately below
// it a failure_event with failure_class execution and retry_disposition none.
// Nothing in the file said which half was current.
//
// Attaching a failure event fires the change hook, so the workspace status is
// projected right then — before the caller's own status/detail update lands. A
// run killed inside that window makes the transient contradiction durable, which
// is exactly what happened. Detail therefore has to move with the evidence.
func TestFailureEventAttachmentClearsStaleSuccessDetail(t *testing.T) {
	list := NewTaskTracker().TodoList()
	item := list.AddBatch([]TodoSpec{{Agent: "helper", Desc: "write and run the deploy drive script"}})[0]
	if err := list.TryUpdateStatusAndOutput(item.ID, TaskInProgress, "Task completed successfully. Both deliverables exist and are verified.", ""); err != nil {
		t.Fatal(err)
	}

	event := &FailureEventPayload{
		TaskID:           item.ID,
		Phase:            "execution",
		FailureClass:     FailureExecution,
		RetryDisposition: RetryNone,
		Command:          "submit_result",
		Summary:          "source=error | last_tool=bash | error=attempt content budget exceeded",
	}
	if err := list.SetFailureEventAndOutput(item.ID, event, "partial worker output"); err != nil {
		t.Fatal(err)
	}

	got := list.Items()[0]
	if got.FailureEvent == nil {
		t.Fatal("the failure event must be attached")
	}
	if strings.Contains(got.Detail, "completed successfully") {
		t.Errorf("a stale success detail must not survive beside a failure event: %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "attempt content budget exceeded") {
		t.Errorf("detail should carry the failure's own account: %q", got.Detail)
	}

	// And the workspace projection, which is what an operator reads, must agree
	// with itself even though the status update has not happened yet.
	workspace := t.TempDir()
	if err := ReconcileAgentStatuses(workspace, list.Items(), nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "helper.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded projectedStatusRecord
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("projected status is not valid YAML: %v; data=%q", err, data)
	}
	if decoded.FailureEvent == nil || strings.Contains(decoded.Detail, "completed successfully") {
		t.Errorf("projected status still pairs success prose with a failure event: %q", data)
	}
}

// TestFailureEventWithoutSummaryKeepsExistingDetail keeps the change narrow: an
// event that carries no account of its own must not blank out the detail that
// does.
func TestFailureEventWithoutSummaryKeepsExistingDetail(t *testing.T) {
	list := NewTaskTracker().TodoList()
	item := list.AddBatch([]TodoSpec{{Agent: "helper", Desc: "verify the deploy"}})[0]
	if err := list.TryUpdateStatusAndOutput(item.ID, TaskError, "verification failed", ""); err != nil {
		t.Fatal(err)
	}
	if err := list.SetFailureEvent(item.ID, &FailureEventPayload{TaskID: item.ID, Phase: "verification", FailureClass: FailureVerify}); err != nil {
		t.Fatal(err)
	}
	if got := list.Items()[0].Detail; got != "verification failed" {
		t.Errorf("detail = %q, want the existing failure detail preserved", got)
	}
}
