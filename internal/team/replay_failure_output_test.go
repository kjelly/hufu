package team

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/utils"
)

func TestTaskFailureOutputSurvivesCheckpointAndReplay(t *testing.T) {
	t.Parallel()
	for _, status := range []TaskStatus{TaskBlocked, TaskError, TaskProtocolIncomplete} {
		t.Run(string(status), func(t *testing.T) {
			c := newFailureOutputCoordinator(t)
			items, err := c.CommitTaskCreation(t.Context(), []TodoSpec{{Agent: "worker", Desc: "retain complete output"}})
			if err != nil {
				t.Fatal(err)
			}
			output := strings.Repeat("完整 work evidence\n", 500)
			if err := c.CommitTaskTransition(t.Context(), items[0].ID, TaskPending, status, "protocol failure", output, map[string]any{"failure_event": &FailureEventPayload{TaskID: items[0].ID, FailureClass: FailureProtocol, Summary: "protocol failure"}}); err != nil {
				t.Fatal(err)
			}
			live := c.todoItemByID(items[0].ID)
			events := mustReadEvents(t, c.eventStore)
			replayed, err := ReplayTodoList(events)
			if err != nil || len(replayed) != 1 || replayed[0].Output != output || compareSingleTaskProjection(live, replayed[0]) != nil {
				t.Fatalf("complete failure output lost in replay: %v", err)
			}
		})
	}
}

func TestTaskReplayRestoresOnlyRedundantLegacyFailureOutput(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"redundant", "different_prefix", "changed_detail", "changed_receipt", "new_attempt", "complete_short", "without_source"} {
		t.Run(scenario, func(t *testing.T) {
			c := newFailureOutputCoordinator(t)
			items, err := c.CommitTaskCreation(t.Context(), []TodoSpec{{Agent: "worker", Desc: "work"}})
			if err != nil {
				t.Fatal(err)
			}
			output := strings.Repeat("sealed complete work\n", 500)
			if err := c.CommitTaskTransition(t.Context(), items[0].ID, TaskPending, TaskBlocked, "protocol failure", output, map[string]any{"failure_event": &FailureEventPayload{TaskID: items[0].ID, FailureClass: FailureProtocol, Summary: "protocol failure"}}); err != nil {
				t.Fatal(err)
			}
			events := mustReadEvents(t, c.eventStore)
			prior, err := ReplayTodoList(events)
			if err != nil {
				t.Fatal(err)
			}
			short := utils.TruncateString(output, 2000)
			next := cloneTodoItem(prior[0])
			switch scenario {
			case "different_prefix":
				short = "different " + short
			case "changed_detail":
				next.Detail = "new failure"
			case "changed_receipt":
				next.ExecutionReceipt = &ExecutionReceipt{RunID: "another-run", TaskID: next.ID, Attempt: 1}
			case "new_attempt":
				next.Retries++
			case "without_source":
				events = nil
			}
			payload := taskTransitionPayload(next)
			delete(payload, "output_complete") // Historical writer.
			payload["output"] = short
			if scenario == "complete_short" {
				payload["output_complete"] = true
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, RunEvent{Type: "task_blocked", TaskID: next.ID, Payload: raw})
			replayed, err := ReplayTodoList(events)
			if err != nil || len(replayed) != 1 {
				t.Fatalf("replay: %v", err)
			}
			want := short
			if scenario == "redundant" {
				want = output
			}
			if replayed[0].Output != want {
				t.Fatal("legacy recovery borrowed unrelated output or lost a redundant canonical source")
			}
		})
	}
}

// Exercise event/checkpoint persistence without claiming an admitted worker
// execution, matching the preflight persistence fixture in task_transition_test.
func newFailureOutputCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	c := newTestCoordinator(t)
	c.executionRunID = ""
	store, err := NewEventStore(c.session.Workspace, "output-replay", "output-replay")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	return c
}

func TestTaskReplayRejectsLegacyOutputWithChangedRedaction(t *testing.T) {
	t.Parallel()
	output := strings.Repeat("canonical evidence ", 500) + " password=replay-redaction-fixture-value-87654321"
	redacted := utils.RedactSecrets(output)
	if redacted == output {
		t.Fatal("fixture must contain a recognized synthetic secret")
	}
	if legacyFailureOutputSource("task_blocked", &TodoItem{Output: output}, utils.TruncateString(redacted, 2000), false) != nil {
		t.Fatal("legacy recovery reintroduced bytes removed by current redaction")
	}
}
