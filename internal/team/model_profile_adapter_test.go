package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/modelprofile"
)

func TestModelContextSpecFromProfile(t *testing.T) {
	profile := modelprofile.ResolveModelProfile(modelprofile.ModelProfileInput{
		ModelID:         "qwen3:8b",
		Family:          "qwen",
		MaxOutputTokens: modelprofile.ResolvedValue[int]{Value: 8_192, Source: modelprofile.SourceCatalog},
		Context:         modelprofile.ContextResolutionInput{FallbackContext: 128_000},
	})

	spec := ModelContextSpecFromProfile(profile)
	if spec.ContextWindow != 128_000 || spec.ContextWindowSource != "fallback" || !spec.IsEstimated {
		t.Fatalf("adapted fallback spec = %#v", spec)
	}
	if spec.MaxOutputTokens != 8_192 || spec.Estimator != "qwen" {
		t.Fatalf("adapted metadata = %#v", spec)
	}
}

func TestModelContextSpecFromProfilePreservesResolvedEstimator(t *testing.T) {
	profile := modelprofile.ResolveModelProfile(modelprofile.ModelProfileInput{
		ModelID: "catalog-model",
		Family:  "family",
		Estimator: modelprofile.EstimatorEvidence{
			Catalog: "catalog-estimator",
		},
		Context: modelprofile.ContextResolutionInput{
			CatalogContext:  32_768,
			FallbackContext: 4_096,
		},
	})

	spec := ModelContextSpecFromProfile(profile)
	if spec.Estimator != "catalog-estimator" {
		t.Fatalf("adapted estimator = %q, want catalog-estimator", spec.Estimator)
	}
}

func TestModelContextSpecFromProfileUsesEffectiveProviderValue(t *testing.T) {
	profile := modelprofile.ResolveModelProfile(modelprofile.ModelProfileInput{
		ModelID: "local-model",
		MaxOutputTokens: modelprofile.ResolvedValue[int]{
			Value:  2_048,
			Source: modelprofile.SourceProviderMetadata,
		},
		Context: modelprofile.ContextResolutionInput{
			Provider:                "openai",
			ProviderMetadataContext: 16_384,
			CatalogContext:          128_000,
			FallbackContext:         4_096,
		},
	})

	spec := ModelContextSpecFromProfile(profile)
	if spec.ContextWindow != 16_384 || spec.ContextWindowSource != "provider_metadata" || spec.IsEstimated {
		t.Fatalf("adapted provider spec = %#v", spec)
	}
}
