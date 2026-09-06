package team

import (
	"reflect"
	"strings"
	"testing"
)

func TestCheckAlternatives(t *testing.T) {
	execute := DecisionOption{ID: "a", Kind: OptionExecute}
	negotiate := DecisionOption{ID: "b", Kind: OptionNegotiate}
	defer_ := DecisionOption{ID: "defer", Kind: OptionDefer}
	abandon := DecisionOption{ID: "abandon", Kind: OptionAbandon}
	info := DecisionOption{ID: "info", Kind: OptionRequestInfo}

	tests := []struct {
		name    string
		policy  AlternativesPolicy
		options []DecisionOption
		want    string
	}{
		{
			name:    "no-go required and absent",
			policy:  AlternativesPolicy{RequireNoActionOption: true},
			options: []DecisionOption{execute, negotiate},
			want:    ReasonDecisionNoNoGoOption,
		},
		{
			name:    "defer satisfies the no-go requirement",
			policy:  AlternativesPolicy{RequireNoActionOption: true},
			options: []DecisionOption{execute, negotiate, defer_},
		},
		{
			name:    "abandon also satisfies it",
			policy:  AlternativesPolicy{RequireNoActionOption: true},
			options: []DecisionOption{execute, abandon},
		},
		{
			name:    "information option required and absent",
			policy:  AlternativesPolicy{RequireInfoOption: true},
			options: []DecisionOption{execute, defer_},
			want:    ReasonDecisionMissingAlternative,
		},
		{
			name:    "information option present",
			policy:  AlternativesPolicy{RequireInfoOption: true},
			options: []DecisionOption{execute, info},
		},
		{
			name:    "too few options",
			policy:  AlternativesPolicy{MinOptions: 3},
			options: []DecisionOption{execute, defer_},
			want:    ReasonDecisionMissingAlternative,
		},
		{
			name:    "enough options",
			policy:  AlternativesPolicy{MinOptions: 3},
			options: []DecisionOption{execute, defer_, info},
		},
		{
			name:    "unconfigured policy allows anything",
			options: []DecisionOption{execute},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := CheckAlternatives(tt.policy, tt.options)
			if tt.want == "" {
				if gate != nil {
					t.Fatalf("CheckAlternatives = %v, want nil", gate)
				}
				return
			}
			if gate == nil || gate.Reason != tt.want {
				t.Fatalf("CheckAlternatives = %v, want reason %s", gate, tt.want)
			}
		})
	}
}

func usableBaseRate() BaseRateEvidence {
	return BaseRateEvidence{
		ReferenceClass: "pipeline migrations",
		Metric:         "on-time completion",
		SampleSize:     30,
		Distribution:   DistributionSummary{Mean: 0.55, Median: 0.5, P10: 0.2, P90: 0.9},
		Source:         ArtifactRef{SHA256: "abc"},
	}
}

func TestCheckOutsideView(t *testing.T) {
	tests := []struct {
		name   string
		policy OutsideViewPolicy
		rates  []BaseRateEvidence
		want   string
	}{
		{name: "not required", policy: OutsideViewPolicy{}},
		{
			name:   "required and missing",
			policy: OutsideViewPolicy{Required: true},
			want:   ReasonDecisionOutsideViewMissing,
		},
		{
			name:   "required and usable",
			policy: OutsideViewPolicy{Required: true},
			rates:  []BaseRateEvidence{usableBaseRate()},
		},
		{
			name:   "sample of zero is not a sample",
			policy: OutsideViewPolicy{Required: true},
			rates: []BaseRateEvidence{func() BaseRateEvidence {
				r := usableBaseRate()
				r.SampleSize = 0
				return r
			}()},
			want: ReasonDecisionOutsideViewMissing,
		},
		{
			name:   "source must be a persisted artifact",
			policy: OutsideViewPolicy{Required: true},
			rates: []BaseRateEvidence{func() BaseRateEvidence {
				r := usableBaseRate()
				r.Source = ArtifactRef{}
				return r
			}()},
			want: ReasonDecisionOutsideViewMissing,
		},
		{
			name:   "reference class must be named",
			policy: OutsideViewPolicy{Required: true},
			rates: []BaseRateEvidence{func() BaseRateEvidence {
				r := usableBaseRate()
				r.ReferenceClass = "  "
				return r
			}()},
			want: ReasonDecisionOutsideViewMissing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := CheckOutsideView(tt.policy, tt.rates)
			if tt.want == "" {
				if gate != nil {
					t.Fatalf("CheckOutsideView = %v, want nil", gate)
				}
				return
			}
			if gate == nil || gate.Reason != tt.want {
				t.Fatalf("CheckOutsideView = %v, want %s", gate, tt.want)
			}
		})
	}
}

func TestCheckPremortem(t *testing.T) {
	good := &PremortemResult{FailureModes: []FailureMode{{ID: "F1", Description: "stalls", Likelihood: 0.2, Impact: 0.9}}}

	if gate := CheckPremortem(PremortemPolicy{Enabled: true}, nil); gate != nil {
		t.Fatalf("premortem not required but blocked: %v", gate)
	}
	if gate := CheckPremortem(PremortemPolicy{Enabled: true, RequiredBeforeCommit: true}, nil); gate == nil ||
		gate.Reason != ReasonDecisionPremortemRequired {
		t.Fatalf("missing required premortem = %v, want %s", gate, ReasonDecisionPremortemRequired)
	}
	if gate := CheckPremortem(PremortemPolicy{Enabled: true, RequiredBeforeCommit: true}, &PremortemResult{}); gate == nil {
		t.Fatal("premortem with no failure modes accepted")
	}
	if gate := CheckPremortem(PremortemPolicy{Enabled: true, RequiredBeforeCommit: true}, good); gate != nil {
		t.Fatalf("usable premortem blocked: %v", gate)
	}
	bad := &PremortemResult{FailureModes: []FailureMode{{ID: "F1", Description: "stalls", Likelihood: 5}}}
	if gate := CheckPremortem(PremortemPolicy{Enabled: true, RequiredBeforeCommit: true}, bad); gate == nil {
		t.Fatal("out-of-range likelihood accepted")
	}
}

func TestCheckForecast(t *testing.T) {
	record := DecisionRecord{Probability: 0.7, FalsificationConditions: []string{"ingest lag exceeds 5m"}}

	if gate := CheckForecast(ForecastPolicy{}, DecisionRecord{}); gate != nil {
		t.Fatalf("forecast not required but blocked: %v", gate)
	}
	if gate := CheckForecast(ForecastPolicy{Required: true}, record); gate != nil {
		t.Fatalf("usable forecast blocked: %v", gate)
	}
	if gate := CheckForecast(ForecastPolicy{Required: true}, DecisionRecord{Probability: 0}); gate == nil {
		t.Fatal("missing probability accepted")
	}
	if gate := CheckForecast(ForecastPolicy{Required: true}, DecisionRecord{Probability: 1.5}); gate == nil {
		t.Fatal("out-of-range probability accepted")
	}
	if gate := CheckForecast(ForecastPolicy{Required: true}, DecisionRecord{Probability: 0.7}); gate == nil {
		t.Fatal("forecast without a falsification condition accepted")
	}
}

func TestCheckRequestContract(t *testing.T) {
	good := &RequestContract{Objective: "cut ingest lag", SuccessCriteria: []SuccessCriterion{{ID: "S1", Statement: "p99 under 1m"}}}

	if gate := CheckRequestContract(good); gate != nil {
		t.Fatalf("usable contract blocked: %v", gate)
	}
	for name, contract := range map[string]*RequestContract{
		"nil":            nil,
		"no objective":   {SuccessCriteria: []SuccessCriterion{{Statement: "x"}}},
		"no criteria":    {Objective: "cut ingest lag"},
		"empty criteria": {Objective: "cut ingest lag", SuccessCriteria: []SuccessCriterion{{ID: "S1"}}},
	} {
		if gate := CheckRequestContract(contract); gate == nil || gate.Reason != ReasonDecisionMissingObjective {
			t.Errorf("%s: CheckRequestContract = %v, want %s", name, gate, ReasonDecisionMissingObjective)
		}
	}
}

// Three mirrors of one origin are one confirmation, not three (spec §28.1,
// test matrix I).
func TestGroupEvidenceMergesSharedOrigins(t *testing.T) {
	policy := EvidenceIndependencePolicy{WarnSharedOrigin: true}

	sameContent := GroupEvidence([]EvidenceProvenance{
		{SourceID: "a", ContentHash: "h1"},
		{SourceID: "b", ContentHash: "h1"},
		{SourceID: "c", ContentHash: "h1"},
	}, policy)
	if sameContent.SourceCount != 3 || sameContent.IndependenceGroupCount != 1 {
		t.Fatalf("identical content grouped as %d groups from %d sources",
			sameContent.IndependenceGroupCount, sameContent.SourceCount)
	}
	if len(sameContent.SharedOriginWarnings) != 1 {
		t.Fatalf("shared-origin warnings = %v, want one", sameContent.SharedOriginWarnings)
	}

	sameDomain := GroupEvidence([]EvidenceProvenance{
		{SourceID: "https://news.example.com/a", SourceType: EvidenceSourceURL},
		{SourceID: "https://blog.example.com/b", SourceType: EvidenceSourceURL},
		{SourceID: "https://other.test/c", SourceType: EvidenceSourceURL},
	}, policy)
	if sameDomain.IndependenceGroupCount != 2 {
		t.Fatalf("same registrable domain grouped as %d groups, want 2", sameDomain.IndependenceGroupCount)
	}

	citation := GroupEvidence([]EvidenceProvenance{
		{SourceID: "a"},
		{SourceID: "b", ParentSourceIDs: []string{"a"}},
		{SourceID: "c", ParentSourceIDs: []string{"a"}},
	}, policy)
	if citation.IndependenceGroupCount != 1 {
		t.Fatalf("derived parent links grouped as %d groups, want 1", citation.IndependenceGroupCount)
	}
}

// Model-declared parentage is recorded but never trusted for grouping: the
// same spec that asks for independence says self-description is not evidence
// (spec §28.1).
func TestGroupEvidenceIgnoresDeclaredParents(t *testing.T) {
	independent := GroupEvidence([]EvidenceProvenance{
		{SourceID: "a"},
		{SourceID: "b", DeclaredParentSourceIDs: []string{"a"}},
		{SourceID: "c", DeclaredParentSourceIDs: []string{"a"}},
	}, EvidenceIndependencePolicy{WarnSharedOrigin: true})
	if independent.IndependenceGroupCount != 3 {
		t.Fatalf("declared parentage changed grouping: %d groups, want 3", independent.IndependenceGroupCount)
	}
}

// Grouping must not depend on the order sources arrive in.
func TestGroupEvidenceIsOrderIndependent(t *testing.T) {
	sources := []EvidenceProvenance{
		{SourceID: "a", ContentHash: "h1"},
		{SourceID: "b", ContentHash: "h2"},
		{SourceID: "c", ContentHash: "h1"},
		{SourceID: "d", ParentSourceIDs: []string{"b"}},
	}
	policy := EvidenceIndependencePolicy{WarnSharedOrigin: true}
	forward := GroupEvidence(sources, policy)
	reversed := GroupEvidence([]EvidenceProvenance{sources[3], sources[2], sources[1], sources[0]}, policy)

	if forward.IndependenceGroupCount != reversed.IndependenceGroupCount {
		t.Fatalf("group counts differ: %d vs %d", forward.IndependenceGroupCount, reversed.IndependenceGroupCount)
	}
	for id, group := range forward.Groups {
		if reversed.Groups[id] != group {
			t.Fatalf("source %q grouped as %q then %q", id, group, reversed.Groups[id])
		}
	}
	if strings.Join(forward.SharedOriginWarnings, "|") != strings.Join(reversed.SharedOriginWarnings, "|") {
		t.Fatalf("warnings differ:\n%v\n%v", forward.SharedOriginWarnings, reversed.SharedOriginWarnings)
	}
}

func TestProvenanceFromArtifacts(t *testing.T) {
	sources := ProvenanceFromArtifacts([]ArtifactRef{
		{ID: "art-1", SHA256: "h1"},
		{Path: "reports/a.json", SHA256: "h1"},
		{SHA256: "h2"},
		{},
	})
	if len(sources) != 1 {
		t.Fatalf("derived %d sources, want only the runtime-identified artifact", len(sources))
	}
	grouped := GroupEvidence(sources, EvidenceIndependencePolicy{})
	if grouped.IndependenceGroupCount != 1 {
		t.Fatalf("runtime-identified artifact formed %d groups, want one", grouped.IndependenceGroupCount)
	}
}

func TestTrustedProvenanceUsesOnlyResolvedRuntimeMetadata(t *testing.T) {
	metadata := []RuntimeEvidenceMetadata{
		{Artifact: ArtifactRef{ID: "artifact-a", SHA256: "same"}, Origin: ArtifactOriginMetadata{RetrievalURL: "https://news.example.com/a", ParentSourceIDs: []string{"root"}}},
		{Artifact: ArtifactRef{ID: "artifact-b", SHA256: "same"}, Origin: ArtifactOriginMetadata{RetrievalURL: "https://blog.example.com/b"}},
		// A digest without a runtime artifact identity is not trusted input.
		{Artifact: ArtifactRef{SHA256: "forged"}, Origin: ArtifactOriginMetadata{RetrievalURL: "https://evil.invalid"}},
	}
	forward := TrustedProvenanceFromRuntimeMetadata(metadata)
	reversed := TrustedProvenanceFromRuntimeMetadata([]RuntimeEvidenceMetadata{metadata[2], metadata[1], metadata[0]})
	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("trusted provenance is order-dependent:\n%#v\n%#v", forward, reversed)
	}
	if len(forward) != 2 || forward[0].SourceID != "https://blog.example.com/b" || forward[1].SourceID != "https://news.example.com/a" {
		t.Fatalf("trusted provenance = %#v", forward)
	}
	grouped := GroupEvidence(forward, EvidenceIndependencePolicy{WarnSharedOrigin: true})
	if grouped.SourceCount != 2 || grouped.IndependenceGroupCount != 1 {
		t.Fatalf("runtime content/domain grouping = %#v", grouped)
	}
	// Model declarations are deliberately advisory even when they claim the
	// same parent relation the runtime did not record.
	declared := append([]EvidenceProvenance(nil), forward...)
	declared[1].DeclaredParentSourceIDs = []string{declared[0].SourceID}
	if !reflect.DeepEqual(GroupEvidence(forward, EvidenceIndependencePolicy{}), GroupEvidence(declared, EvidenceIndependencePolicy{})) {
		t.Fatal("declared parent changed trusted grouping")
	}
}
