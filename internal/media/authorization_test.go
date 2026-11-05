package media

import (
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestMediaPolicyIntersection(t *testing.T) {
	operator := &Policy{Enabled: true, AllowedTargets: []Target{{Backend: "vision", ProviderKey: "provider", Model: "model", ImageTokenUpperBound: 8192}}}
	for _, tt := range []struct {
		name           string
		operator, team *Policy
		enabled        bool
		count          int
	}{
		{"default-disabled", nil, nil, false, 0},
		{"team-cannot-enable", nil, &Policy{Enabled: true}, false, 0},
		{"inherit", operator, nil, true, 1},
		{"team-disable", operator, &Policy{}, false, 0},
		{"deny-all-targets", operator, &Policy{Enabled: true, AllowedTargets: []Target{}}, true, 0},
		{"ungranted-target", operator, &Policy{Enabled: true, AllowedTargets: []Target{{Backend: "other", ProviderKey: "provider", Model: "model"}}}, true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := Intersect(tt.operator, tt.team)
			if err != nil {
				t.Fatal(err)
			}
			if (policy != nil && policy.Enabled) != tt.enabled {
				t.Fatalf("enabled policy: %+v", policy)
			}
			if policy != nil && len(policy.AllowedTargets) != tt.count {
				t.Fatalf("target count: %+v", policy)
			}
		})
	}
	restriction := &Policy{Enabled: true, Image: Limits{MaxCount: 2, MaxBranchBytes: 1024}, AllowedTargets: slices.Clone(operator.AllowedTargets)}
	restriction.AllowedTargets[0].ImageTokenUpperBound = 1024
	result, err := Intersect(operator, restriction)
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.MaxCount != 2 || result.Image.MaxBranchBytes != 1024 || result.AllowedTargets[0].ImageTokenUpperBound != 8192 {
		t.Fatalf("widened restriction or underestimated tokens: %+v", result)
	}
	result.AllowedTargets[0].Model = "changed"
	if operator.AllowedTargets[0].Model != "model" {
		t.Fatal("intersection mutated the operator grant")
	}
	if _, err := operator.Allow("vision", "provider", "model"); err != nil {
		t.Fatal(err)
	}
	if _, err := operator.Allow("vision", "other", "model"); err == nil {
		t.Fatal("provider identity mismatch accepted")
	}
}

func TestMediaPolicyYAML(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		valid       bool
	}{
		{"valid", "enabled: true\nimage: {max-count: 2}\nallowed-targets: [{backend: vision, provider-key: provider, model: model, image-token-upper-bound: 8192}]", true},
		{"unknown-limit", "enabled: true\nimage: {max-byte-per-image: 1}", false},
		{"raised-limit", "enabled: true\nimage: {max-count: 9}", false},
		{"fractional-count", "enabled: true\nimage: {max-count: 0.5}", false},
		{"fractional-bytes", "enabled: true\nimage: {max-bytes-per-image: 0.5}", false},
		{"fractional-alias", "enabled: true\nimage: {max-count: &fraction 0.5, max-side: *fraction}", false},
		{"fractional-estimate", "enabled: true\nallowed-targets: [{backend: vision, model: model, image-token-upper-bound: 0.5}]", false},
		{"non-finite", "enabled: true\nimage: {max-count: .nan}", false},
		{"wildcard", "enabled: true\nallowed-targets: [{backend: '*', provider-key: provider, model: model}]", false},
		{"negative-estimate", "enabled: true\nallowed-targets: [{backend: vision, provider-key: provider, model: model, image-token-upper-bound: -1}]", false},
		{"identity-override", "enabled: true\nallowed-targets: [{backend: vision, provider-key: provider, model: model, identity-hash: forged}]", false},
		{"duplicate", "enabled: true\nallowed-targets: [{backend: vision, provider-key: provider, model: model}, {backend: vision, provider-key: provider, model: model}]", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var p Policy
			err := yaml.Unmarshal([]byte(tt.input), &p)
			if (err == nil) != tt.valid {
				t.Fatalf("decode: %v", err)
			}
		})
	}
}
