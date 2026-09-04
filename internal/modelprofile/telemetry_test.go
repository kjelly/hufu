package modelprofile

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTelemetryProjectionIsCompleteAndSecretFree(t *testing.T) {
	profile := ModelProfile{
		ModelID: "ollama/qwen3:8b", Provider: "ollama", Family: "provider-family",
		Estimator: "qwen", EstimatorProvenance: "provider_family_derived",
		Sources: ModelProfileSources{
			OperatorContext:  ResolvedValue[int]{Value: 8_192, Source: SourceOperator},
			CatalogContext:   ResolvedValue[int]{Value: 131_072, Source: SourceCatalog},
			RuntimeContext:   ResolvedValue[int]{Value: 32_768, Source: SourceProviderRuntime},
			EffectiveContext: ResolvedValue[int]{Value: 32_768, Source: SourceProviderRuntime, Confidence: "observed"},
			MaxOutputTokens:  ResolvedValue[int]{Value: 2_048, Source: SourceCatalog},
			Capabilities: CapabilitySources{
				Tools: ResolvedValue[CapabilityState]{Value: CapabilityYes, Source: SourceCatalog},
			},
		},
	}
	projection := TelemetryFromProfile(profile)
	if projection.Operator.Value != 8_192 || projection.Effective.Value != 32_768 || projection.Capabilities["tools"].State != CapabilityYes {
		t.Fatalf("projection lost profile evidence: %#v", projection)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	for _, secret := range []string{"http://", "Bearer", "api_key", "raw"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("telemetry contains forbidden value %q: %s", secret, encoded)
		}
	}
	profile.Family = "https://provider.example/raw"
	profile.Estimator = "provider response"
	data, err = json.Marshal(TelemetryFromProfile(profile))
	if err != nil {
		t.Fatal(err)
	}
	encoded = string(data)
	if strings.Contains(encoded, "provider.example") || strings.Contains(encoded, "provider response") {
		t.Fatalf("untrusted metadata was projected: %s", encoded)
	}
}
