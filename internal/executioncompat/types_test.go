package executioncompat

import "testing"

func TestCompatibilityVocabularyIsStable(t *testing.T) {
	if FeatureLocalAlias != "local_alias" || FeatureLegacyPolicyRoute != "legacy_policy_route" {
		t.Fatalf("feature vocabulary changed: %q %q", FeatureLocalAlias, FeatureLegacyPolicyRoute)
	}
	if ClassificationCanonical != "canonical" || ClassificationNotApplicable != "not_applicable" {
		t.Fatalf("classification vocabulary changed: %q %q", ClassificationCanonical, ClassificationNotApplicable)
	}
}

func TestCanonicalBackendRecognizesOnlyTheHistoricalLocalAlias(t *testing.T) {
	tests := []struct {
		backend string
		want    string
		legacy  bool
	}{
		{backend: "local", want: "ollama", legacy: true},
		{backend: " LOCAL ", want: "ollama", legacy: true},
		{backend: "ollama", want: "ollama"},
		{backend: "openai", want: "openai"},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			got, legacy := CanonicalBackend(test.backend)
			if got != test.want || legacy != test.legacy {
				t.Fatalf("CanonicalBackend(%q) = (%q, %v), want (%q, %v)", test.backend, got, legacy, test.want, test.legacy)
			}
		})
	}
}
