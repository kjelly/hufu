package belief

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func syntheticModel(t testing.TB) Model {
	t.Helper()
	data, err := os.ReadFile("testdata/synthetic-model.json")
	if err != nil {
		t.Fatal(err)
	}
	model, err := DecodeModel(data)
	if err != nil {
		t.Fatal(err)
	}
	return model
}
func syntheticEpoch(t testing.TB, model Model) Epoch {
	t.Helper()
	hash, err := ModelDigest(model)
	if err != nil {
		t.Fatal(err)
	}
	return Epoch{ID: "epoch-1", ModelDigest: hash, Scope: Scope{RunID: "run-1", BranchID: "main", TaskID: "task-1", Attempt: 1}, SubjectRevision: "subject-1", EnvironmentRevision: "environment-1"}
}
func exampleObservation() Observation {
	return Observation{ID: "o1", ExperimentID: "comparison", OutcomeID: "pass", SourceID: "receipt-1"}
}
func requireCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	v, ok := errors.AsType[*Error](err)
	if !ok || v.Code != code {
		t.Fatalf("error=%v want %s", err, code)
	}
}

func TestKnownPosteriorAndDuplicateReplay(t *testing.T) {
	model := syntheticModel(t)
	epoch := syntheticEpoch(t, model)
	observation := exampleObservation()
	state, err := Project(model, epoch, []Observation{observation})
	if err != nil {
		t.Fatal(err)
	}
	expected := []Mass{{HypothesisID: "h1", Units: 142857143}, {HypothesisID: "h2", Units: 857142857}}
	if state.Status != Estimated || state.Distribution == nil || !reflect.DeepEqual(*state.Distribution, expected) || !state.Synthetic {
		t.Fatalf("state=%+v", state)
	}
	repeated := make([]Observation, 100)
	for i := range repeated {
		repeated[i] = observation
	}
	duplicate, err := Project(model, epoch, repeated)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, duplicate) {
		t.Fatal("replay added evidence")
	}
	if len(state.Digest) != 64 || len(state.InputDigest) != 64 {
		t.Fatal("missing digest")
	}
}
func TestPriorAndRetractionByRecomputation(t *testing.T) {
	model := syntheticModel(t)
	epoch := syntheticEpoch(t, model)
	prior, err := Project(model, epoch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prior.Status != Prior || (*prior.Distribution)[0].Units != 400000000 {
		t.Fatalf("prior=%+v", prior)
	}
	if _, err := Project(model, epoch, []Observation{exampleObservation()}); err != nil {
		t.Fatal(err)
	}
	retracted, err := Project(model, epoch, []Observation{})
	if err != nil || !reflect.DeepEqual(prior, retracted) {
		t.Fatalf("retraction=%+v %v", retracted, err)
	}
}
func TestCorrelationAndSourceIdentityAreNotIndependence(t *testing.T) {
	model := syntheticModel(t)
	a := exampleObservation()
	b := a
	b.ID = "o2"
	b.SourceID = "receipt-2"
	state, err := Project(model, syntheticEpoch(t, model), []Observation{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != DependenceUnresolved || state.Distribution != nil {
		t.Fatal("same group multiplied")
	}
	experiment := model.Experiments[0]
	experiment.ID = "second"
	experiment.CorrelationGroup = "independent"
	model.Experiments = append(model.Experiments, experiment)
	b.ExperimentID = "second"
	b.SourceID = a.SourceID
	state, err = Project(model, syntheticEpoch(t, model), []Observation{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != DependenceUnresolved {
		t.Fatal("same source masqueraded as independent")
	}
	b.SourceID = "receipt-2"
	state, err = Project(model, syntheticEpoch(t, model), []Observation{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != Estimated || (*state.Distribution)[0].Units != 40000000 {
		t.Fatalf("independent result=%+v", state)
	}
	reverse, err := Project(model, syntheticEpoch(t, model), []Observation{b, a})
	if err != nil || reverse.Digest != state.Digest {
		t.Fatal("set order changed result")
	}
}
func TestModelInconsistencyAndZeroPrior(t *testing.T) {
	model := syntheticModel(t)
	model.Experiments[0].Outcomes[0].LikelihoodUnits = []uint64{0, 0}
	model.Experiments[0].Outcomes[1].LikelihoodUnits = []uint64{Scale, Scale}
	state, err := Project(model, syntheticEpoch(t, model), []Observation{exampleObservation()})
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ModelInconsistent || state.Distribution != nil {
		t.Fatal("zero support became a prior")
	}
	encoded, err := json.Marshal(state)
	if err != nil || !bytes.Contains(encoded, []byte(`"distribution":null`)) {
		t.Fatalf("null distribution=%s %v", encoded, err)
	}
	model = syntheticModel(t)
	model.Hypotheses[0].PriorUnits = 0
	model.Hypotheses[1].PriorUnits = Scale
	state, err = Project(model, syntheticEpoch(t, model), []Observation{exampleObservation()})
	if err != nil {
		t.Fatal(err)
	}
	if (*state.Distribution)[0].Units != 0 || (*state.Distribution)[1].Units != Scale {
		t.Fatal("zero prior revived")
	}
}
func TestLargestRemainderTieAndResolution(t *testing.T) {
	model := syntheticModel(t)
	model.Hypotheses = []Hypothesis{{ID: "z", Meaning: "z", PriorUnits: 333333334}, {ID: "a", Meaning: "a", PriorUnits: 333333333}, {ID: "b", Meaning: "b", PriorUnits: 333333333}}
	model.Experiments[0].Outcomes[0].LikelihoodUnits = []uint64{333333333, 333333334, 333333334}
	model.Experiments[0].Outcomes[1].LikelihoodUnits = []uint64{666666667, 666666666, 666666666}
	state, err := Project(model, syntheticEpoch(t, model), []Observation{exampleObservation()})
	if err != nil {
		t.Fatal(err)
	}
	if (*state.Distribution)[0].HypothesisID != "a" || (*state.Distribution)[0].Units != 333333334 {
		t.Fatalf("tie=%+v", *state.Distribution)
	}
	model = syntheticModel(t)
	model.Hypotheses[0].PriorUnits = 1
	model.Hypotheses[1].PriorUnits = Scale - 1
	model.Experiments[0].Outcomes[0].LikelihoodUnits = []uint64{1, Scale}
	model.Experiments[0].Outcomes[1].LikelihoodUnits = []uint64{Scale - 1, 0}
	state, err = Project(model, syntheticEpoch(t, model), []Observation{exampleObservation()})
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != ResolutionLimited || (*state.Distribution)[0].Units != 0 {
		t.Fatal("positive exact mass quantized to zero without diagnostic")
	}
}
func TestInvalidBoundaries(t *testing.T) {
	model := syntheticModel(t)
	epoch := syntheticEpoch(t, model)
	a := exampleObservation()
	b := a
	b.OutcomeID = "fail"
	_, err := Project(model, epoch, []Observation{a, b})
	requireCode(t, err, IdentityConflict)
	bad := a
	bad.OutcomeID = "unknown"
	_, err = Project(model, epoch, []Observation{bad})
	requireCode(t, err, InvalidObservation)
	epoch.ModelDigest = "different"
	_, err = Project(model, epoch, nil)
	requireCode(t, err, InvalidEpoch)
	model.ReducerVersion = "future"
	_, err = ModelDigest(model)
	requireCode(t, err, UnsupportedVersion)
	_, err = DecodeModel(bytes.Repeat([]byte("x"), MaxModelBytes+1))
	requireCode(t, err, ResourceLimit)
	model = syntheticModel(t)
	model.Experiments = make([]Experiment, MaxExperiments+1)
	err = ValidateModel(model)
	requireCode(t, err, ResourceLimit)
	model = syntheticModel(t)
	many := make([]Observation, 4097)
	_, err = Project(model, syntheticEpoch(t, model), many)
	requireCode(t, err, ResourceLimit)
}
func TestStrictModelWireSchema(t *testing.T) {
	data, err := os.ReadFile("testdata/synthetic-model.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, old, new string }{
		{"unknown field", `"id": "synthetic-two-state"`, `"unknown": true, "id": "synthetic-two-state"`},
		{"missing prior", `"prior_units": 400000000`, `"not_prior": 400000000`},
		{"null prior", `"prior_units": 400000000`, `"prior_units": null`},
		{"negative", `"prior_units": 400000000`, `"prior_units": -1`},
		{"fractional", `"prior_units": 400000000`, `"prior_units": 400000000.5`},
		{"overflow", `"prior_units": 400000000`, `"prior_units": 18446744073709551616`},
		{"null likelihood", `[200000000, 800000000]`, `[null, 800000000]`},
		{"null synthetic", `"synthetic": true`, `"synthetic": null`},
		{"missing synthetic", `"synthetic": true,`, ``},
		{"duplicate key", `"synthetic": true`, `"synthetic": true, "synthetic": false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(string(data), tc.old, tc.new, 1)
			if mutated == string(data) {
				t.Fatal("fixture mutation missing")
			}
			if _, err := DecodeModel([]byte(mutated)); err == nil {
				t.Fatal("accepted invalid model")
			}
		})
	}
}
func TestInputImmutabilityAndSourceDigest(t *testing.T) {
	model := syntheticModel(t)
	before, _ := json.Marshal(model)
	epoch := syntheticEpoch(t, model)
	a := exampleObservation()
	state, err := Project(model, epoch, []Observation{a})
	if err != nil {
		t.Fatal(err)
	}
	a.SourceID = "another-source"
	other, err := Project(model, epoch, []Observation{a})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(state.Distribution, other.Distribution) == false || state.Digest == other.Digest || state.InputDigest == other.InputDigest {
		t.Fatal("digest failed to bind provenance")
	}
	after, _ := json.Marshal(model)
	if !bytes.Equal(before, after) {
		t.Fatal("mutated model")
	}
}
func FuzzProjection(f *testing.F) {
	f.Add(uint32(400000000), uint32(200000000), uint32(800000000))
	model := syntheticModel(f)
	f.Fuzz(func(t *testing.T, p, a, b uint32) {
		candidate := model
		candidate.Hypotheses = slices.Clone(model.Hypotheses)
		candidate.Experiments = slices.Clone(model.Experiments)
		candidate.Experiments[0].Outcomes = slices.Clone(model.Experiments[0].Outcomes)
		prior := uint64(p) % (Scale + 1)
		left := uint64(a) % (Scale + 1)
		right := uint64(b) % (Scale + 1)
		candidate.Hypotheses[0].PriorUnits = prior
		candidate.Hypotheses[1].PriorUnits = Scale - prior
		candidate.Experiments[0].Outcomes[0].LikelihoodUnits = []uint64{left, right}
		candidate.Experiments[0].Outcomes[1].LikelihoodUnits = []uint64{Scale - left, Scale - right}
		state, err := Project(candidate, syntheticEpoch(t, candidate), []Observation{exampleObservation()})
		if err != nil {
			t.Fatal(err)
		}
		if state.Distribution != nil {
			var sum uint64
			for _, mass := range *state.Distribution {
				sum += mass.Units
			}
			if sum != Scale {
				t.Fatalf("sum=%d", sum)
			}
		}
	})
}
func BenchmarkProjection(b *testing.B) {
	model := syntheticModel(b)
	epoch := syntheticEpoch(b, model)
	observations := []Observation{exampleObservation()}
	for b.Loop() {
		if _, err := Project(model, epoch, observations); err != nil {
			b.Fatal(err)
		}
	}
}

func TestModelCrossFieldValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Model)
		code   ErrorCode
	}{
		{"duplicate hypothesis", func(m *Model) { m.Hypotheses[1].ID = m.Hypotheses[0].ID }, InvalidModel},
		{"prior total", func(m *Model) { m.Hypotheses[0].PriorUnits++ }, InvalidModel},
		{"prior out of range", func(m *Model) { m.Hypotheses[0].PriorUnits = Scale + 1 }, InvalidModel},
		{"incomplete outcome", func(m *Model) { m.Experiments[0].Outcomes[0].LikelihoodUnits = []uint64{Scale} }, InvalidModel},
		{"likelihood total", func(m *Model) { m.Experiments[0].Outcomes[0].LikelihoodUnits[0]++ }, InvalidModel},
		{"likelihood bounds", func(m *Model) { m.Experiments[0].Outcomes[0].LikelihoodUnits[0] = Scale + 1 }, InvalidModel},
		{"duplicate outcome", func(m *Model) { m.Experiments[0].Outcomes[1].ID = m.Experiments[0].Outcomes[0].ID }, InvalidModel},
		{"duplicate experiment", func(m *Model) { m.Experiments = append(m.Experiments, m.Experiments[0]) }, InvalidModel},
		{"no dependence declaration", func(m *Model) { m.Experiments[0].CorrelationGroup = "" }, InvalidModel},
		{"unsupported source", func(m *Model) { m.Experiments[0].Source = "worker_claim" }, InvalidModel},
		{"missing applicability", func(m *Model) { m.Applicability = "" }, InvalidModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { m := syntheticModel(t); tc.mutate(&m); requireCode(t, ValidateModel(m), tc.code) })
	}
}

func TestUniqueObservationLimit(t *testing.T) {
	model := syntheticModel(t)
	epoch := syntheticEpoch(t, model)
	observations := make([]Observation, MaxObservations+1)
	for i := range observations {
		observations[i] = Observation{ID: fmt.Sprintf("o-%d", i), ExperimentID: "comparison", OutcomeID: "pass", SourceID: fmt.Sprintf("source-%d", i)}
	}
	_, err := Project(model, epoch, observations)
	requireCode(t, err, ResourceLimit)
}
