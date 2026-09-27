package improve

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMemoryRetrievalFusionValidationAndHashStability(t *testing.T) {
	baseline := DefaultMemoryPolicySnapshot("memory-policy-v1")
	data, err := json.Marshal(baseline.Retrieval)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fusion") {
		t.Fatalf("legacy retrieval policy serializes fusion, which would change existing revision hashes: %s", data)
	}
	for _, tc := range []struct {
		fusion string
		ok     bool
	}{{"", true}, {"legacy", true}, {"rrf_normalized", true}, {"rrf", false}} {
		candidate := baseline
		candidate.Retrieval.Fusion = tc.fusion
		err := validateMemoryRetrievalPolicy(candidate.Retrieval)
		if (err == nil) != tc.ok {
			t.Fatalf("fusion %q err = %v, want ok=%v", tc.fusion, err, tc.ok)
		}
	}
	candidate := baseline
	candidate.Retrieval.Fusion = "rrf_normalized"
	created, err := CreateMemoryPolicyCandidate("memory-policy-fusion", baseline, candidate)
	if err != nil || created.ChangedCategory != "retrieval" || created.Retrieval.Fusion != "rrf_normalized" {
		t.Fatalf("fusion candidate = %+v err=%v", created, err)
	}
}

func TestMemoryRetrievalCarriedWeightValidation(t *testing.T) {
	for _, tc := range []struct {
		weight float64
		ok     bool
	}{{0, true}, {0.8, true}, {1, true}, {-0.1, false}, {1.5, false}} {
		policy := DefaultMemoryPolicySnapshot("memory-policy-v1").Retrieval
		policy.Fusion, policy.FusionCarriedWeight = "score_normalized", tc.weight
		if err := validateMemoryRetrievalPolicy(policy); (err == nil) != tc.ok {
			t.Fatalf("weight %v err = %v, want ok=%v", tc.weight, err, tc.ok)
		}
	}
}
