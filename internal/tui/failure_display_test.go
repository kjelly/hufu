package tui

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestItemLinesUseStructuredRedactedFailureDisplay(t *testing.T) {
	item := &team.TodoItem{
		ID: "12", Agent: "worker", Status: team.TaskProtocolIncomplete,
		Desc: "failed task", Detail: "api_token=raw-card-secret",
		FailureEvent: &team.FailureEventPayload{
			Diagnosis: &team.ActionFailureDiagnosis{Version: 1, Code: "missing_executable", Summary: "Tool is missing", CauseStatus: "confirmed", Cause: "Required executable unavailable"},
			TaskID:    "12", Phase: "verification", FailureClass: team.FailureEnvironment,
			RetryDisposition: team.ReplanRequired, Summary: "verification failed",
		},
	}
	view := stripViewANSI(strings.Join((&Model{}).itemLines(item, false, false, 120), "\n"))
	if strings.Contains(view, "raw-card-secret") || !strings.Contains(view, "Tool is missing") || !strings.Contains(view, "Required executable unavailable") || !strings.Contains(view, "class=environment") || !strings.Contains(view, "disposition=replan_required") {
		t.Fatalf("TUI item lines = %q", view)
	}
}
