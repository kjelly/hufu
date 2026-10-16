package belief

import (
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func diagnosticTestInputs(t testing.TB) (Model, Epoch, DiagnosticPolicy) {
	t.Helper()
	model := syntheticModel(t)
	second := model.Experiments[0]
	second.ID, second.CorrelationGroup = "second", "second_group"
	model.Experiments = append(model.Experiments, second)
	policy := DiagnosticPolicy{SchemaVersion: 1, Version: "1", Algorithm: DiagnosticGiniAlgorithm, Threshold: 900_000_000,
		Limits:     DiagnosticLimits{MaxRounds: 2, MaxAttempts: 2, MaxCostUnits: 2, ActionTimeoutSeconds: 5, EpisodeTimeoutSeconds: 20, MaxEvents: 32, MaxObservationBytes: 16384, MaxFenceRetries: 3},
		Candidates: []DiagnosticCandidate{{ID: "z", ExperimentID: "second", CostUnits: 1}, {ID: "a", ExperimentID: "comparison", CostUnits: 1}}}
	return model, syntheticEpoch(t, model), policy
}

func TestDiagnosticExactRankingAndStableTie(t *testing.T) {
	model, epoch, policy := diagnosticTestInputs(t)
	choice, err := RankDiagnostic(model, epoch, nil, policy)
	if err != nil || choice.CandidateID != "a" || len(choice.Scores) != 2 || choice.Scores[0].Numerator == "0" || choice.Scores[0].Numerator != choice.Scores[1].Numerator || choice.Scores[0].Denominator != choice.Scores[1].Denominator {
		t.Fatalf("choice=%+v err=%v", choice, err)
	}
	policy.Candidates[1].CostUnits = 2
	choice, err = RankDiagnostic(model, epoch, nil, policy)
	if err != nil || choice.CandidateID != "z" {
		t.Fatalf("cost did not affect ranking: %+v %v", choice, err)
	}
	policy.Algorithm = DiagnosticFixedOrder
	policy.Candidates = []DiagnosticCandidate{policy.Candidates[1], policy.Candidates[0]}
	choice, err = RankDiagnostic(model, epoch, nil, policy)
	if err != nil || choice.CandidateID != "a" {
		t.Fatalf("baseline did not preserve declared order: %+v %v", choice, err)
	}
}

func TestDiagnosticDuplicateRetractionAndIndependence(t *testing.T) {
	model, epoch, policy := diagnosticTestInputs(t)
	observation := exampleObservation()
	one, err := RankDiagnostic(model, epoch, []Observation{observation}, policy)
	if err != nil || one.CandidateID != "z" {
		t.Fatalf("choice=%+v err=%v", one, err)
	}
	duplicate, err := RankDiagnostic(model, epoch, []Observation{observation, observation}, policy)
	if err != nil || !reflect.DeepEqual(one, duplicate) {
		t.Fatal("duplicate advanced selection")
	}
	prior, err := RankDiagnostic(model, epoch, nil, policy)
	if err != nil || prior.CandidateID != "a" {
		t.Fatal("retracted evidence did not restore original ranking")
	}
	alias := observation
	alias.ID, alias.ExperimentID = "alias", "second"
	unavailable, err := RankDiagnostic(model, epoch, []Observation{observation, alias}, policy)
	if err != nil || unavailable.CandidateID != "" || unavailable.StopReason != "distribution_unavailable" {
		t.Fatal("source alias was treated as independent")
	}
	conflict := observation
	conflict.OutcomeID = "fail"
	if _, err := RankDiagnostic(model, epoch, []Observation{observation, conflict}, policy); err == nil {
		t.Fatal("accepted conflicting observation identity")
	}
}

func TestDiagnosticStopRulesAndExactProjectionCompatibility(t *testing.T) {
	model, epoch, policy := diagnosticTestInputs(t)
	policy.Limits.MaxRounds = 1
	choice, err := RankDiagnostic(model, epoch, []Observation{exampleObservation()}, policy)
	if err != nil || choice.StopReason != "round_limit" {
		t.Fatalf("round limit=%+v %v", choice, err)
	}
	policy.Threshold = 800_000_000
	choice, err = RankDiagnostic(model, epoch, []Observation{exampleObservation()}, policy)
	if err != nil || choice.StopReason != "threshold_reached" {
		t.Fatal("threshold should take precedence over the round limit")
	}
	prior, err := RankDiagnostic(model, epoch, nil, policy)
	if err != nil || prior.StopReason != "" {
		t.Fatal("prior alone stopped without a new observation")
	}
	for i := range model.Experiments {
		model.Experiments[i].Outcomes = []Outcome{{ID: "pass", LikelihoodUnits: []uint64{500_000_000, 500_000_000}}, {ID: "fail", LikelihoodUnits: []uint64{500_000_000, 500_000_000}}}
	}
	epoch = syntheticEpoch(t, model)
	choice, err = RankDiagnostic(model, epoch, nil, policy)
	if err != nil || choice.StopReason != "no_information_gain" || choice.CandidateID != "" {
		t.Fatal("zero-information probe selected")
	}
	policy.Algorithm = DiagnosticFixedOrder
	choice, err = RankDiagnostic(model, epoch, nil, policy)
	if err != nil || choice.StopReason != "no_information_gain" {
		t.Fatal("baseline used different no-information stop rule")
	}
}

func TestDiagnosticDoesNotScoreCorrelatedFutureMeasurementsAsIndependent(t *testing.T) {
	model, _, policy := diagnosticTestInputs(t)
	model.Experiments[1].CorrelationGroup = model.Experiments[0].CorrelationGroup
	epoch := syntheticEpoch(t, model)
	choice, err := RankDiagnostic(model, epoch, []Observation{exampleObservation()}, policy)
	if err != nil || choice.StopReason != "no_candidate" || choice.CandidateID != "" {
		t.Fatalf("correlated probe selected: %+v %v", choice, err)
	}
	if choice.Scores[1].Excluded != "correlation_group_observed" {
		t.Fatal("missing exclusion reason")
	}
}

func TestDiagnosticThresholdDoesNotRoundUpToSuccess(t *testing.T) {
	model, _, policy := diagnosticTestInputs(t)
	model.Hypotheses[0].PriorUnits, model.Hypotheses[1].PriorUnits = Scale/2, Scale/2
	model.Experiments[0].Outcomes = []Outcome{{ID: "pass", LikelihoodUnits: []uint64{899_999_999, 100_000_000}}, {ID: "fail", LikelihoodUnits: []uint64{100_000_001, 900_000_000}}}
	epoch := syntheticEpoch(t, model)
	observations := []Observation{exampleObservation()}
	state, err := Project(model, epoch, observations)
	if err != nil || (*state.Distribution)[0].Units != 900_000_000 {
		t.Fatalf("fixture did not round to the display threshold: %+v %v", state, err)
	}
	// Exact mass 899999999/999999999 remains strictly below 9/10.
	choice, err := RankDiagnostic(model, epoch, observations, policy)
	if err != nil || choice.StopReason != "" || choice.CandidateID != "z" {
		t.Fatalf("rounded display caused an early stop: %+v %v", choice, err)
	}
}

func TestDiagnosticPolicyStrictBoundary(t *testing.T) {
	model, _, policy := diagnosticTestInputs(t)
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range [][2]string{
		{`"schema_version":1`, `"schema_version":2`},
		{`"threshold_units":900000000`, `"threshold_units":null`},
		{`"max_rounds":2`, `"max_rounds":0`},
		{`"max_rounds":2`, `"max_rounds":null`},
		{`"cost_units":1`, `"cost_units":0`},
		{`"cost_units":1`, `"cost_units":1000001`},
		{`"algorithm":"expected_gini_gain_per_cost_v1"`, `"algorithm":"ACTIVE"`},
		{`"version":"1"`, `"version":"1","extra":true`},
	} {
		bad := strings.ReplaceAll(string(data), replacement[0], replacement[1])
		if bad == string(data) {
			t.Fatalf("bad test replacement %q", replacement[0])
		}
		if _, err := DecodeDiagnosticPolicy([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err := DecodeDiagnosticPolicy(append(data, []byte(` {}`)...)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	if _, err := DecodeDiagnosticPolicy([]byte(strings.Repeat(" ", MaxPolicyBytes+1))); err == nil {
		t.Fatal("accepted oversized policy")
	}
	for _, mutate := range []func(*DiagnosticPolicy){
		func(p *DiagnosticPolicy) { p.Candidates[1].ID = p.Candidates[0].ID },
		func(p *DiagnosticPolicy) { p.Candidates[1].ExperimentID = p.Candidates[0].ExperimentID },
		func(p *DiagnosticPolicy) { p.Candidates[1].ExperimentID = "unknown" },
	} {
		bad := policy
		bad.Candidates = slices.Clone(policy.Candidates)
		mutate(&bad)
		if err := ValidateDiagnosticPolicy(model, bad); err == nil {
			t.Fatal("accepted invalid candidate mapping")
		}
	}
}

func TestDiagnosticLimitsComposeIndependentCaps(t *testing.T) {
	model, _, policy := diagnosticTestInputs(t)
	policy.Limits.MaxAttempts = 1
	policy.Limits.EpisodeTimeoutSeconds = 1
	policy.Limits.MaxEvents = 8
	if err := ValidateDiagnosticPolicy(model, policy); err != nil {
		t.Fatalf("stricter attempt, deadline and event caps must remain valid: %v", err)
	}
}

func diagnosticTestEvidence() DiagnosticEvidence {
	d := strings.Repeat("a", 64)
	return DiagnosticEvidence{SchemaVersion: 1, Source: DiagnosticSource{EpisodeID: "epoch-1", DecisionID: "decision", ExecutionID: "execution", ChildTaskID: "child", Occurrence: 1, InvocationID: "invocation", ReceiptKey: "receipt", EventID: "event", EventHash: d, Cursor: 1, Adapter: "diagnostic_measurement_v1", ExperimentID: "comparison"}, Revisions: DiagnosticRevisions{Subject: d, Source: d, Environment: d}, Outcome: "pass", Measurement: d}
}

func TestDiagnosticEvidenceStrictBoundaryAndRevisionBinding(t *testing.T) {
	evidence := diagnosticTestEvidence()
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := DecodeDiagnosticEvidence(data); err != nil || decoded != evidence {
		t.Fatalf("decode=%+v %v", decoded, err)
	}
	for _, outcome := range []string{"timeout", "cancelled", "unknown", "invalid", "PASS", ""} {
		bad := strings.ReplaceAll(string(data), `"outcome":"pass"`, `"outcome":"`+outcome+`"`)
		if _, err := DecodeDiagnosticEvidence([]byte(bad)); err == nil {
			t.Fatalf("accepted outcome %s", outcome)
		}
	}
	for _, bad := range []string{
		strings.ReplaceAll(string(data), `"child_occurrence":1`, `"child_occurrence":null`),
		strings.ReplaceAll(string(data), `"source_cursor":1`, `"source_cursor":0`),
		strings.ReplaceAll(string(data), `"schema_version":1`, `"schema_version":2`),
		strings.ReplaceAll(string(data), `"source":{`, `"unknown":true,"source":{`),
		strings.Repeat(" ", MaxDiagnosticEvidenceBytes+1),
	} {
		if _, err := DecodeDiagnosticEvidence([]byte(bad)); err == nil {
			t.Fatal("accepted invalid evidence")
		}
	}
	model := syntheticModel(t)
	epoch := syntheticEpoch(t, model)
	epoch.SubjectRevision, epoch.EnvironmentRevision = evidence.Revisions.Subject, evidence.Revisions.Environment
	observation, err := CheckDiagnosticEvidence(epoch, evidence.Source, evidence.Revisions, evidence)
	if err != nil || observation.SourceID != evidence.Measurement {
		t.Fatalf("observation=%+v %v", observation, err)
	}
	for _, mutate := range []func(*DiagnosticEvidence){
		func(e *DiagnosticEvidence) { e.Source.ExecutionID = "wrong" },
		func(e *DiagnosticEvidence) { e.Source.ChildTaskID = "wrong" },
		func(e *DiagnosticEvidence) { e.Source.Occurrence++ },
		func(e *DiagnosticEvidence) { e.Source.InvocationID = "wrong" },
		func(e *DiagnosticEvidence) { e.Source.EventHash = strings.Repeat("b", 64) },
		func(e *DiagnosticEvidence) { e.Source.ReceiptKey = "wrong" },
		func(e *DiagnosticEvidence) { e.Source.Cursor++ },
		func(e *DiagnosticEvidence) { e.Source.Adapter = "wrong" },
		func(e *DiagnosticEvidence) { e.Revisions.Subject = strings.Repeat("b", 64) },
		func(e *DiagnosticEvidence) { e.Revisions.Source = strings.Repeat("b", 64) },
		func(e *DiagnosticEvidence) { e.Revisions.Environment = strings.Repeat("b", 64) },
	} {
		bad := evidence
		mutate(&bad)
		if _, err := CheckDiagnosticEvidence(epoch, evidence.Source, evidence.Revisions, bad); err == nil {
			t.Fatal("accepted mismatched mapping or revisions")
		}
	}
	otherBranch := epoch
	otherBranch.Scope.BranchID = "sibling"
	other, err := CheckDiagnosticEvidence(otherBranch, evidence.Source, evidence.Revisions, evidence)
	if err != nil || other.ID == observation.ID {
		t.Fatal("observation identity was not branch scoped")
	}
}
