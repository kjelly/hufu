package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestReportCompletionMatchesFinalAudit(t *testing.T) {
	current := gateFixture(t)
	view, err := projectedFixture(current, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := deriveReportAction(view, &out); err != nil {
		t.Fatal(err)
	}
	var action struct {
		Outputs map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(out.Bytes(), &action); err != nil {
		t.Fatal(err)
	}
	var derived reportCompletion
	if err := json.Unmarshal(action.Outputs["review_completion"], &derived); err != nil {
		t.Fatal(err)
	}
	want := reportCounts{Sources: 2, Critiques: 2, Findings: 3, Gaps: 1, Decisions: 3, Unverified: 1}
	if derived.Status != "completed_with_gaps" || derived.Counts != want {
		t.Fatalf("unexpected completion: %+v", derived)
	}
	native := fixtureTask(t, "10", "derive-review-report", "success", payload{}, record{}, nil)
	native.SnapshotID = current.SnapshotID
	native.Result.RuntimeOutputs = action.Outputs
	sealFixtureOutputs(t, &native)
	current.Tasks = append(current.Tasks, native)
	report := &current.Tasks[4]
	report.Result.Summary = derived.Summary
	replacePayload(t, report, func(p *payload) { p.Completion = &derived; p.CanonicalFindings = action.Outputs["canonical_findings"] })
	if _, _, err := fixtureVerify(current, "run-1"); err != nil {
		t.Fatal(err)
	}
	replacePayload(t, report, func(p *payload) { p.Completion.Counts.Decisions++ })
	if _, _, err := fixtureVerify(current, "run-1"); err == nil || !strings.Contains(err.Error(), "completion or summary differs") {
		t.Fatalf("audit accepted altered counts: %v", err)
	}
}
