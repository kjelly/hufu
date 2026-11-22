package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestReviewGateReportsAllInvalidSources(t *testing.T) {
	current := gateFixture(t)
	current.Tasks[0].Result.Status = "success"
	current.Tasks[3].Result.Status = "success"
	_, _, err := fixtureVerify(current, "run-1")
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
	_, _, err = fixtureVerify(current, "run-1")
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

func TestReviewGateAttributesInvalidAuditInputsToSources(t *testing.T) {
	for _, test := range []struct {
		name       string
		edit       func(*sourceSession)
		diagnostic string
	}{
		{"no_sources", func(view *sourceSession) { view.Tasks = nil }, "review workset results missing"},
		{"duplicate_source", func(view *sourceSession) { view.Tasks = append(view.Tasks, view.Tasks[0]) }, "duplicate task ID"},
		{"missing_critique", func(view *sourceSession) {
			view.Tasks = slices.DeleteFunc(view.Tasks, func(item sourceTask) bool { return item.ID == "6" })
		}, "unassigned required critique"},
		{"missing_test_evidence", func(view *sourceSession) {
			view.Tasks = slices.DeleteFunc(view.Tasks, func(item sourceTask) bool { return item.ContractID == "verify-targeted-go-tests" })
		}, "targeted test evidence"},
		{"missing_prepare_scope", func(view *sourceSession) {
			view.Tasks = slices.DeleteFunc(view.Tasks, func(item sourceTask) bool { return item.ContractID == "produce-workset" })
		}, "missing PREPARE evidence"},
		{"missing_inventory", func(view *sourceSession) {
			view.Tasks = slices.DeleteFunc(view.Tasks, func(item sourceTask) bool { return item.ContractID == "inventory-review-handoffs" })
		}, "review handoff inventory missing"},
		{"incomplete_supplement_round", func(view *sourceSession) {
			view.Tasks = append(view.Tasks, sourceTask{ContractID: "inventory-review-supplement-requests"})
		}, "bounded supplement request and prepare actions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			view, err := projectedFixture(gateFixture(t), "run-1")
			if err != nil {
				t.Fatal(err)
			}
			test.edit(&view)
			var output bytes.Buffer
			err = runGateAction(t.Context(), "verify_review_report", view, &output)
			if source, ok := errors.AsType[*sourceReviewError](err); !ok || !strings.Contains(source.Error(), test.diagnostic) || output.Len() != 0 {
				t.Fatalf("source problem became report verification output: err=%v output=%s", err, output.String())
			}
		})
	}
}

func TestReviewGateRoutesSnapshotFailureToItsOwner(t *testing.T) {
	for _, sourceFailure := range []bool{true, false} {
		name := "substituted_report_coverage"
		if sourceFailure {
			name = "invalid_producer_identity"
		}
		t.Run(name, func(t *testing.T) {
			fixture := gateFixture(t)
			view, err := projectedFixture(fixture, "run-1")
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if sourceFailure {
				scope := raw(t, map[string]any{"requested": map[string]string{"kind": "repository_snapshot"}, "satisfied": true})
				fixture.Tasks[5].Result.RuntimeOutputs["scope"] = scope
				sealFixtureOutputs(t, &fixture.Tasks[5])
				replacePayload(t, &fixture.Tasks[4], func(p *payload) { p.Scope = scope })
				view, err = projectedFixture(fixture, "run-1")
				if err != nil {
					t.Fatal(err)
				}
				err = runGateAction(t.Context(), "verify_review_report", view, &output)
				if _, ok := errors.AsType[*sourceReviewError](err); !ok || output.Len() != 0 || !strings.Contains(err.Error(), "invalid snapshot identity") {
					t.Fatalf("snapshot source failure became report verification output: err=%v output=%s", err, output.String())
				}
				return
			}
			coverage := snapshotCoverage{Mode: "repository_snapshot", Revision: "frozen-head"}
			for _, item := range view.Tasks {
				if item.ContractID == "inventory-review-handoffs" {
					item.Result.RuntimeOutputs["snapshot_coverage"] = raw(t, coverage)
				}
			}
			err = verifySnapshotCoverage(view, coverage, raw(t, map[string]string{"revision": "different"}))
			if _, ok := errors.AsType[*sourceReviewError](err); err == nil || ok || !strings.Contains(err.Error(), "report substituted") {
				t.Fatalf("report-owned snapshot failure has wrong attribution: %v", err)
			}
		})
	}
}
