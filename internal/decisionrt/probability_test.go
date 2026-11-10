package decisionrt_test

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
)

func TestRuntimeProbabilitySumTolerance(t *testing.T) {
	for _, test := range []struct {
		name          string
		probabilities []float64
		valid         bool
	}{
		{"exact", []float64{0.7, 0.3}, true},
		{"rounded_below", []float64{0.9857, 0.0046, 0.0046, 0.005}, true},
		{"rounded_above", []float64{0.7, 0.3001}, true},
		{"lower_boundary", []float64{0.7, 0.299}, true},
		{"upper_boundary", []float64{0.7, 0.301}, true},
		{"below_tolerance", []float64{0.7, 0.2989999}, false},
		{"above_tolerance", []float64{0.7, 0.3010001}, false},
		{"invalid_range", []float64{-0.1, 1.1}, false},
		{"nan", []float64{math.NaN(), 0.3}, false},
		{"infinite", []float64{math.Inf(1), 0.3}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := validChoiceRequest()
			request.Spec.Options = nil
			var candidates []decisionrt.Candidate
			for i, probability := range test.probabilities {
				id := fmt.Sprintf("option_%d", i)
				request.Spec.Options = append(request.Spec.Options, decisionrt.Option{ID: id})
				candidates = append(candidates, decisionrt.Candidate{Value: id, Probability: probability})
			}
			backendResult := decisionrt.BackendResult{
				Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: candidates[0].Value},
				Candidates: candidates, Confidence: test.probabilities[0], ConfidenceSemantics: decisionrt.ConfidenceRaw,
			}
			runtime := mustRuntime(t, decisionrt.RuntimeConfig{Primary: fixedBackend("primary", backendResult)})
			result, _, err := runtime.Decide(t.Context(), request)
			if !test.valid {
				assertErrorKind(t, err, decisionrt.ErrorInvalidBackendOutput)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.Candidates, candidates) || result.Confidence != test.probabilities[0] {
				t.Fatalf("runtime changed original probabilities: %+v", result)
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var restored decisionrt.Result
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if err := decisionrt.ValidateResult(request.Spec, restored); err != nil {
				t.Fatalf("persisted result validation disagreed with runtime: %v", err)
			}
			if !reflect.DeepEqual(result, restored) {
				t.Fatal("persistence changed the accepted result")
			}
		})
	}
}
