package belief

import (
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func syntheticPolicy() Policy {
	return Policy{SchemaVersion: 1, Version: "synthetic-v1", Algorithm: PolicyAlgorithm, Mode: "advisory", Choices: []PolicyChoice{{HypothesisID: "h1", CandidateID: "check-a"}, {HypothesisID: "h2", CandidateID: "check-b"}}}
}

func TestPolicySyntheticSelectionAndStableTie(t *testing.T) {
	model := syntheticModel(t)
	state, err := Project(model, syntheticEpoch(t, model), []Observation{exampleObservation()})
	if err != nil {
		t.Fatal(err)
	}
	policy := syntheticPolicy()
	advice, err := Suggest(policy, state)
	if err != nil || advice != (Advice{CandidateID: "check-b", HypothesisID: "h2"}) {
		t.Fatalf("advice=%+v error=%v", advice, err)
	}
	state.Distribution = new([]Mass{{HypothesisID: "h2", Units: Scale / 2}, {HypothesisID: "h1", Units: Scale / 2}})
	tie, err := Suggest(policy, state)
	if err != nil || tie.CandidateID != "check-a" {
		t.Fatalf("tie=%+v error=%v", tie, err)
	}
	first, err := PolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(policy.Choices)
	second, err := PolicyDigest(policy)
	if err != nil || first != second {
		t.Fatal("choice order changed policy identity")
	}
	for range 100 {
		next, err := Suggest(policy, state)
		if err != nil || !reflect.DeepEqual(tie, next) {
			t.Fatal("advisory replay changed result")
		}
	}
}

func TestPolicyUnavailableDistributionsAndInvalidMass(t *testing.T) {
	for _, status := range []Status{DependenceUnresolved, ModelInconsistent, ResolutionLimited} {
		advice, err := Suggest(syntheticPolicy(), State{Status: status})
		if err != nil || advice.Unavailable != "distribution_unavailable" || advice.CandidateID != "" {
			t.Fatalf("status=%s advice=%+v error=%v", status, advice, err)
		}
	}
	for _, distribution := range [][]Mass{
		{{HypothesisID: "h1", Units: Scale}},
		{{HypothesisID: "h1", Units: Scale / 2}, {HypothesisID: "h1", Units: Scale / 2}},
		{{HypothesisID: "h1", Units: Scale}, {HypothesisID: "h2", Units: 1}},
		{{HypothesisID: "h1", Units: 1}, {HypothesisID: "unknown", Units: Scale - 1}},
	} {
		_, err := Suggest(syntheticPolicy(), State{Status: Estimated, Distribution: &distribution})
		requireCode(t, err, InvalidPolicy)
	}
}

func TestPolicyStrictResourceAndAuthorityBoundary(t *testing.T) {
	encoded, err := json.Marshal(syntheticPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePolicy(encoded); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		strings.Replace(string(encoded), `"advisory"`, `"active"`, 1),
		strings.Replace(string(encoded), `"max_posterior_v1"`, `"unknown"`, 1),
		strings.Replace(string(encoded), `"h2"`, `"h1"`, 1),
		strings.TrimSuffix(string(encoded), "}") + `,"proposers":["worker"]}`,
		strings.TrimSuffix(string(encoded), "}") + `,"mode":"advisory"}`,
		`null`,
		strings.Repeat(" ", MaxPolicyBytes+1),
	} {
		if _, err := DecodePolicy([]byte(data)); err == nil {
			t.Fatalf("accepted invalid policy: %.100s", data)
		}
	}
}
