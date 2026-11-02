package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestReviewGateReportsAllInvalidSources(t *testing.T) {
	current := gateFixture(t)
	current.Tasks[0].Result.Status = "success"
	current.Tasks[3].Result.Status = "success"
	_, _, err := verify(current, "run-1")
	source, ok := errors.AsType[*sourceReviewError](err)
	if !ok {
		t.Fatalf("source error lost attribution: %v", err)
	}
	for _, want := range []string{"task 3 concealed", "critic 6 concealed"} {
		if !strings.Contains(source.Error(), want) {
			t.Fatalf("missing %s: %v", want, err)
		}
	}
	// Rewriting the report cannot fix immutable source status.
	replacePayload(t, &current.Tasks[4], func(p *payload) { p.Coverage = "complete" })
	_, _, err = verify(current, "run-1")
	if _, ok := errors.AsType[*sourceReviewError](err); !ok {
		t.Fatal("source failure routed to report repair")
	}
}

func TestReviewGateSourceFailureDoesNotTakeReportRepairEdge(t *testing.T) {
	workspace, _ := actionFixture(t)
	root, _, err := checkpointRoot(workspace, "run-1", "action-1")
	if err != nil {
		t.Fatal(err)
	}
	current := gateFixture(t)
	current.Tasks[3].Result.Status = "success"
	current.Tasks = append(current.Tasks, task{ID: "8", ContractID: "verify-review-report", Status: "in_progress", SnapshotID: current.SnapshotID})
	writeCheckpoint(t, root, raw(t, current))
	for key, value := range map[string]string{"HUFU_WORKSPACE": workspace, "HUFU_RUN_ID": "run-1", "HUFU_ACTION_INVOCATION_ID": "action-1", "HUFU_TASK_ID": "8"} {
		t.Setenv(key, value)
	}
	var output bytes.Buffer
	err = Run(t.Context(), bytes.NewReader(raw(t, team.Action{Type: "verify_review_report", Payload: `{"scope":{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}}`})), &output)
	if _, ok := errors.AsType[*sourceReviewError](err); !ok || output.Len() != 0 {
		t.Fatalf("source violation became report verification output: err=%v output=%s", err, output.String())
	}
}
