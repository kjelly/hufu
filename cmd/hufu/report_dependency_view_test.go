package main

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestDependencyResultViewReportDistinguishesPreparedAndDispatched(t *testing.T) {
	for _, called := range []bool{false, true} {
		var b strings.Builder
		summary := team.ContextManifestSummary{Requests: 1, Prepared: 1, DependencyResultViews: []team.DependencyResultViewReport{{
			TaskID: "consumer", Agent: "reviewer", Attempt: 2, ModelCalled: called, ContentSHA256: "view-hash",
			Attribution: &team.DependencyResultViewAttribution{Policy: team.DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary"}}, PolicySHA256: "policy-hash",
				Sources: []team.DependencyResultViewSource{{BranchID: "source-branch", RunID: "source-run", TaskID: "source-task", Attempt: 1, OccurrenceRevision: 4, SourceEventID: "source-event", SourceEventSHA256: "event-hash", FullViewSHA256: "handoff-hash"}}},
		}}}
		writeDependencyResultViewReport(&b, summary)
		for _, required := range []string{"selected", "summary", "policy-hash", "view-hash", "source-branch", "source-run", "source-task", "source-event", "event-hash", "handoff-hash"} {
			if !strings.Contains(b.String(), required) {
				t.Fatalf("report omitted %s", required)
			}
		}
		if strings.Contains(b.String(), "attempt 2, dispatched") != called || strings.Contains(b.String(), "attempt 2, prepared") == called {
			t.Fatal("report confused preparation and dispatch")
		}
	}
}
