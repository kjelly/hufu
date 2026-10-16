package qualitative

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func testScope() Scope {
	return Scope{BranchID: "main", SubjectID: "test-1", SubjectRevision: "source-v1", EnvironmentRevision: "env-v1"}
}
func record(id string, kind Kind) Record {
	return Record{ID: id, Kind: kind, Scope: testScope(), Source: Source{ID: "source-" + id, Revision: "v1", Reference: "artifact:" + id, Digest: strings.Repeat("a", 64)}}
}
func fixture() Input {
	h := record("h", HypothesisRecord)
	h.Hypothesis = &Hypothesis{Claim: "The failure is caused by configuration."}
	f := record("f", EvidenceRecord)
	f.Evidence = &Evidence{Kind: Fact, Knowledge: Known, Statement: "The recorded configuration differs from the reference."}
	i := record("i", EvidenceRecord)
	i.Evidence = &Evidence{Kind: Interpretation, Knowledge: Known, Statement: "A reviewer suspects the test itself."}
	v := record("v", EvidenceRecord)
	v.Evidence = &Evidence{Kind: ObjectiveVerification, Knowledge: Known, Statement: "The isolated check passed.", Verification: &Verification{ReceiptID: "receipt-1", VerifierRevision: "verifier-v1", Outcome: Pass}}
	s := record("s", RelationRecord)
	s.Relation = &Relation{HypothesisID: "h", EvidenceID: "f", Stance: Support, Rationale: "A configuration mismatch could explain this failure."}
	c := record("c", RelationRecord)
	c.Relation = &Relation{HypothesisID: "h", EvidenceID: "i", Stance: Counterevidence, Rationale: "The reviewer offers a competing explanation."}
	g := record("g", GapRecord)
	g.Gap = &Gap{HypothesisID: "h", Question: "Does the mismatch affect the failing test?"}
	return Input{SchemaVersion: SchemaVersion, ReducerVersion: ReducerVersion, Scope: testScope(), Records: []Record{h, f, i, v, s, c, g}}
}
func retract(id, target string) Record {
	r := record(id, RetractionRecord)
	r.Retraction = &Retraction{TargetID: target, Reason: "The source withdrew this claim."}
	return r
}
func requireError(t testing.TB, err error, want ErrorCode) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != want {
		t.Fatalf("error=%v; want %s", err, want)
	}
}
func project(t testing.TB, input Input) State {
	t.Helper()
	state, err := Project(input)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestProjectionPreservesEvidenceKindsAndProvenance(t *testing.T) {
	input := fixture()
	state := project(t, input)
	want := HypothesisView{ID: "h", Status: Contested, SupportIDs: []string{"s"}, CounterevidenceIDs: []string{"c"}, PendingRelationIDs: []string{}, GapIDs: []string{"g"}}
	if !reflect.DeepEqual(state.Hypotheses, []HypothesisView{want}) {
		t.Fatalf("views=%+v", state.Hypotheses)
	}
	for _, original := range input.Records {
		index := slices.IndexFunc(state.Records, func(r Record) bool { return r.ID == original.ID })
		if index < 0 || !reflect.DeepEqual(original, state.Records[index]) {
			t.Fatalf("lost provenance for %s", original.ID)
		}
	}
	// A passing verifier is not automatically evidence against any hypothesis.
	if len(state.Hypotheses[0].CounterevidenceIDs) != 1 {
		t.Fatal("verifier outcome inferred a stance")
	}
	if len(state.Digest) != 64 || len(state.InputDigest) != 64 {
		t.Fatal("missing digest")
	}
	other := fixture()
	other.Records[0].Source.Reference = "artifact:another-reference"
	changed := project(t, other)
	if changed.Digest == state.Digest || changed.InputDigest == state.InputDigest {
		t.Fatal("provenance omitted from digest")
	}
}

func TestOrderAndDuplicateDeliveryDoNotChangeReplay(t *testing.T) {
	input := fixture()
	want := project(t, input)
	for offset := range len(input.Records) {
		permuted := input
		permuted.Records = append(slices.Clone(input.Records[offset:]), input.Records[:offset]...)
		permuted.Records = append(permuted.Records, input.Records...)
		slices.Reverse(permuted.Records)
		if got := project(t, permuted); !reflect.DeepEqual(got, want) {
			t.Fatal("set order or duplicate delivery changed replay")
		}
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := project(t, decoded); !reflect.DeepEqual(got, want) {
		t.Fatal("JSON restart changed replay")
	}
}

func TestRetractionsNeverResurrectRecords(t *testing.T) {
	for _, tc := range []struct {
		target        string
		status        Status
		pending, gaps []string
	}{
		{"c", Supported, []string{}, []string{"g"}},
		{"f", Countered, []string{"s"}, []string{"g"}},
		{"g", Contested, []string{}, []string{}},
		{"h", Withdrawn, []string{}, []string{}},
	} {
		t.Run(tc.target, func(t *testing.T) {
			input := fixture()
			original := project(t, input)
			input.Records = append([]Record{retract("r", tc.target)}, input.Records...)
			input.Records = append(input.Records, fixture().Records...)
			state := project(t, input)
			view := state.Hypotheses[0]
			if view.Status != tc.status || !slices.Equal(view.PendingRelationIDs, tc.pending) || !slices.Equal(view.GapIDs, tc.gaps) || !slices.Equal(state.RetractedIDs, []string{tc.target}) {
				t.Fatalf("state=%+v", state)
			}
			slices.Reverse(input.Records)
			if !reflect.DeepEqual(state, project(t, input)) {
				t.Fatal("late delivery revived a tombstone")
			}
			if original.Hypotheses[0].Status != Contested {
				t.Fatal("reprojection mutated prior state")
			}
		})
	}
}

func TestUnknownEvidenceIsPendingNotSupport(t *testing.T) {
	for _, kind := range []EvidenceKind{Fact, Interpretation, ObjectiveVerification} {
		t.Run(string(kind), func(t *testing.T) {
			input := fixture()
			input.Records[1].Evidence = &Evidence{Kind: kind, Knowledge: Unknown}
			if kind == ObjectiveVerification {
				input.Records[1].Evidence.Verification = &Verification{ReceiptID: "unknown-receipt", VerifierRevision: "v1", Outcome: OutcomeUnknown}
			}
			input.Records = append(input.Records, retract("r", "c"))
			view := project(t, input).Hypotheses[0]
			if view.Status != Unresolved || len(view.SupportIDs) != 0 || !slices.Equal(view.PendingRelationIDs, []string{"s"}) || !slices.Equal(view.GapIDs, []string{"g"}) {
				t.Fatalf("unknown became evidence: %+v", view)
			}
		})
	}
}

func TestContradictionIsExplicitAndNotMajorityVote(t *testing.T) {
	input := fixture()
	for n := range 20 {
		copyLink := cloneRecord(input.Records[4])
		copyLink.ID = fmt.Sprintf("support-%d", n)
		input.Records = append(input.Records, copyLink)
	}
	if project(t, input).Hypotheses[0].Status != Contested {
		t.Fatal("repeated support outvoted counterevidence")
	}
	// Sources can supply multiple explicit claims, without becoming independent samples.
	input.Records[2].Source = input.Records[1].Source
	if project(t, input).Hypotheses[0].Status != Contested {
		t.Fatal("shared source silently discarded a stance")
	}
}

func TestIdentityAndReferenceFailuresReturnNoPartialState(t *testing.T) {
	cases := []struct {
		name   string
		code   ErrorCode
		mutate func(*Input)
	}{
		{"record_conflict", IdentityConflict, func(i *Input) {
			r := cloneRecord(i.Records[1])
			r.Evidence.Statement = "Conflicting content."
			i.Records = append(i.Records, r)
		}},
		{"cross_kind_id", IdentityConflict, func(i *Input) { i.Records[1].ID = "h" }},
		{"source_revision", IdentityConflict, func(i *Input) { i.Records[2].Source = i.Records[1].Source; i.Records[2].Source.Revision = "v2" }},
		{"source_digest", IdentityConflict, func(i *Input) {
			i.Records[2].Source = i.Records[1].Source
			i.Records[2].Source.Digest = strings.Repeat("b", 64)
		}},
		{"source_reference", IdentityConflict, func(i *Input) { i.Records[2].Source = i.Records[1].Source; i.Records[2].Source.Reference = "different" }},
		{"branch", ScopeMismatch, func(i *Input) { i.Records[0].Scope.BranchID = "sibling" }},
		{"subject", ScopeMismatch, func(i *Input) { i.Records[0].Scope.SubjectRevision = "v2" }},
		{"environment", ScopeMismatch, func(i *Input) { i.Records[0].Scope.EnvironmentRevision = "v2" }},
		{"missing_evidence", InvalidReference, func(i *Input) { i.Records[4].Relation.EvidenceID = "missing" }},
		{"wrong_evidence_kind", InvalidReference, func(i *Input) { i.Records[4].Relation.EvidenceID = "h" }},
		{"missing_hypothesis", InvalidReference, func(i *Input) { i.Records[4].Relation.HypothesisID = "missing" }},
		{"gap_reference", InvalidReference, func(i *Input) { i.Records[6].Gap.HypothesisID = "f" }},
		{"unknown_retraction", InvalidReference, func(i *Input) { i.Records = append(i.Records, retract("r", "missing")) }},
		{"retract_retraction", InvalidReference, func(i *Input) { i.Records = append(i.Records, retract("r", "f"), retract("rr", "r")) }},
		{"self_retraction", InvalidReference, func(i *Input) { i.Records = append(i.Records, retract("r", "r")) }},
		{"conflict_after_retraction", IdentityConflict, func(i *Input) {
			r := cloneRecord(i.Records[1])
			r.Evidence.Statement = "Changed after withdrawal."
			i.Records = append(i.Records, retract("r", "f"), r)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := fixture()
			tc.mutate(&input)
			state, err := Project(input)
			requireError(t, err, tc.code)
			if !reflect.DeepEqual(state, State{}) {
				t.Fatal("invalid input exposed partial state")
			}
		})
	}
}

func TestProjectionOwnsItsValuesAndSupportsConcurrentReaders(t *testing.T) {
	input := fixture()
	want := project(t, input)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				if got, err := Project(input); err != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("concurrent projection changed: %v", err)
				}
			}
		})
	}
	wg.Wait()
	state := project(t, input)
	for i := range state.Records {
		r := &state.Records[i]
		if r.Evidence != nil {
			r.Evidence.Statement = "edited"
			if r.Evidence.Verification != nil {
				r.Evidence.Verification.Outcome = Fail
			}
		}
		if r.Hypothesis != nil {
			r.Hypothesis.Claim = "edited"
		}
		if r.Relation != nil {
			r.Relation.Stance = Support
		}
		if r.Gap != nil {
			r.Gap.Question = "edited"
		}
	}
	if !reflect.DeepEqual(project(t, input), want) {
		t.Fatal("state aliased input")
	}
	input.Records[0].Hypothesis.Claim = "input edited later"
	if want.Records[slices.IndexFunc(want.Records, func(r Record) bool { return r.ID == "h" })].Hypothesis.Claim == "input edited later" {
		t.Fatal("input aliased state")
	}
}

func TestEmptyAndRevisionScopedInputs(t *testing.T) {
	input := fixture()
	input.Records = nil
	state := project(t, input)
	if len(state.Records) != 0 || len(state.Hypotheses) != 0 || state.Records == nil || state.RetractedIDs == nil {
		t.Fatal("empty state not explicit")
	}
	input.Scope.SubjectRevision = "new-revision"
	if project(t, input).Digest == state.Digest {
		t.Fatal("revision absent from digest")
	}
}
