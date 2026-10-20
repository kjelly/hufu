package inspect

import (
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/team"
)

func TestOverviewObservationsMatchEconomicsAtOneHead(t *testing.T) {
	fixture := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent {
		return economicsFixtureEvents(t, events)
	})
	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}
	before := snapshotInspectWorkspace(t, fixture.workspace)
	envelope, err := InspectOverview(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := envelope.Data.(OverviewData).Snapshot
	economics, err := InspectEconomics(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	data := economics.Data.(EconomicsData)
	summary := snapshot.Observations
	if summary == nil || summary.Status != "available" || summary.SourceHead != data.SourceHead ||
		summary.SourceHead.EventID != snapshot.Freshness.EventID || summary.SourceHead.EventHash != snapshot.Freshness.EventHash ||
		*summary.TaskCount != data.TaskCount || *summary.VerifiedTasks != data.VerifiedTaskCount ||
		*summary.UsageCostMicros != *data.UsageCostMicros || *summary.KnownCostCoverage != *data.KnownCostCoverage ||
		snapshot.Cost == nil || *snapshot.Cost.UsageDerivedMicros != *summary.UsageCostMicros ||
		snapshot.Outcome.RunOutcome != data.RunOutcome || len(summary.Routes) != len(data.Routes) {
		t.Fatalf("snapshot mixed inputs or recomputed economics: %+v", snapshot)
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, fixture.workspace)) {
		t.Fatal("overview observations mutated the workspace")
	}
}

func TestOverviewObservationsProjectionFailureIsUnavailable(t *testing.T) {
	head := cost.Freshness{EventID: "event", EventHash: "hash"}
	summary := overviewObservations(t.Context(), InspectQuery{RunID: "absent"}, Lineage{}, head)
	if summary.Status != "unavailable" || summary.SourceHead != head || summary.TaskCount != nil ||
		summary.UsageCostMicros != nil || summary.KnownCostCoverage != nil ||
		!reflect.DeepEqual(summary.ReasonCodes, []string{"evidence_unavailable"}) {
		t.Fatalf("projection failure invented values: %+v", summary)
	}
}
