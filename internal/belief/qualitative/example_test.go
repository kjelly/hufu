package qualitative_test

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/belief/qualitative"
)

func ExampleProject() {
	// These are synthetic source references. The caller must authenticate real
	// artifacts/receipts before making decisions from this library's output.
	scope := qualitative.Scope{BranchID: "main", SubjectID: "test-failure", SubjectRevision: "source-v1", EnvironmentRevision: "env-v1"}
	source := qualitative.Source{ID: "fixture", Revision: "v1", Reference: "artifact:fixture", Digest: strings.Repeat("a", 64)}
	record := func(id string, kind qualitative.Kind) qualitative.Record {
		return qualitative.Record{ID: id, Kind: kind, Scope: scope, Source: source}
	}
	h := record("h", qualitative.HypothesisRecord)
	h.Hypothesis = &qualitative.Hypothesis{Claim: "Configuration explains the test failure."}
	e := record("e", qualitative.EvidenceRecord)
	e.Evidence = &qualitative.Evidence{Kind: qualitative.Fact, Knowledge: qualitative.Known, Statement: "The fixture configuration differs from its reference."}
	s := record("s", qualitative.RelationRecord)
	s.Relation = &qualitative.Relation{HypothesisID: "h", EvidenceID: "e", Stance: qualitative.Support, Rationale: "This mismatch could explain the failure; it does not prove the cause."}
	g := record("g", qualitative.GapRecord)
	g.Gap = &qualitative.Gap{HypothesisID: "h", Question: "Does the mismatch affect this test?"}
	input := qualitative.Input{SchemaVersion: qualitative.SchemaVersion, ReducerVersion: qualitative.ReducerVersion, Scope: scope, Records: []qualitative.Record{s, g, e, h}}
	state, err := qualitative.Project(input)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(state.Hypotheses[0].Status, state.Hypotheses[0].GapIDs)

	withdrawal := record("r", qualitative.RetractionRecord)
	withdrawal.Retraction = &qualitative.Retraction{TargetID: "e", Reason: "The source record was withdrawn."}
	input.Records = append(input.Records, withdrawal)
	state, err = qualitative.Project(input)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(state.Hypotheses[0].Status, state.Hypotheses[0].PendingRelationIDs, state.RetractedIDs)
	// Output:
	// supported [g]
	// unknown [s] [e]
}
