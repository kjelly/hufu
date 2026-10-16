package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/belief"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectDiagnosticTextShowsOfflineProvenanceScoresAndUnavailability(t *testing.T) {
	view := team.DiagnosticEpisodeState{Status: "stopped", Validation: "verified", ReplayDigest: "replay-digest", State: &belief.State{Status: belief.Estimated, Synthetic: true, Digest: "state-digest", Distribution: new([]belief.Mass{{HypothesisID: "first", Units: 900_000_000}})},
		Decisions: []team.DiagnosticShadowDecision{{Generation: 1, StateDigest: "prior-digest", Choice: belief.DiagnosticChoice{CandidateID: "check", Scores: []belief.DiagnosticScore{{CandidateID: "check", Numerator: "7009", Denominator: "31200", CostUnits: 1}}}}, {Generation: 2, Choice: belief.DiagnosticChoice{StopReason: "threshold_reached"}}}}
	view.Epoch = belief.Epoch{ID: "offline-episode", Scope: belief.Scope{TaskID: "owner"}}
	view.Mode, view.Source = "shadow", "offline_fixture"
	view.FixtureRef = team.ArtifactRef{ID: "frozen-fixture"}
	view.Revisions = belief.DiagnosticRevisions{Subject: "subject-revision", Source: "source-revision", Environment: "environment-revision"}
	data := inspectpkg.BeliefsData{Episodes: []team.DiagnosticEpisodeState{view}}
	var output bytes.Buffer
	if err := renderInspectBeliefsText(&output, inspectpkg.InspectQuery{}, data); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Episode offline-episode", "source=offline_fixture", "mode=shadow", "validation=verified", "gain_per_cost=7009/31200", "stop=threshold_reached", "replay=replay-digest", "0.900000000", "source=source-revision"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q: %s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "Beliefs: not recorded") || strings.Contains(output.String(), "\x1b") {
		t.Fatal("offline episode was hidden or emitted terminal controls")
	}
	data.Episodes[0].State = nil
	data.Episodes[0].Validation, data.Episodes[0].Unavailable = "unavailable", "fixture_unavailable"
	output.Reset()
	if err := renderInspectBeliefsText(&output, inspectpkg.InspectQuery{}, data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "decisions above are historical records") || !strings.Contains(output.String(), "Fixture: frozen-fixture") || strings.Contains(output.String(), "0.900000000") {
		t.Fatalf("unavailable text fabricated state or lost history: %s", output.String())
	}
}
