package modelprofile

import "testing"

func TestResolveContextOllamaAuthorityAndProvenance(t *testing.T) {
	profile := ResolveModelProfile(ModelProfileInput{
		ModelID:  "qwen3:8b",
		Provider: "ollama",
		Context: ContextResolutionInput{
			Provider:                "ollama",
			OperatorContext:         65_536,
			RuntimeContext:          32_768,
			ConfiguredContext:       16_384,
			ModelInfoContext:        8_192,
			ProviderMetadataContext: 4_096,
			ObservedContext:         2_048,
			CatalogContext:          1_024,
			FallbackContext:         512,
		},
	})

	if profile.EffectiveContext != 65_536 || profile.Sources.EffectiveContext.Source != SourceOperator {
		t.Fatalf("effective context = %#v, want operator value", profile.Sources.EffectiveContext)
	}
	if profile.Sources.RuntimeContext.Value != 32_768 || profile.Sources.ProviderMetadataContext.Value != 4_096 {
		t.Fatalf("lower-authority context evidence was not retained: %#v", profile.Sources)
	}
}

func TestResolveContextGenericAuthorityExcludesOllamaModelConfig(t *testing.T) {
	profile := ResolveModelProfile(ModelProfileInput{
		Provider: "openai",
		Context: ContextResolutionInput{
			Provider:                "openai",
			ConfiguredContext:       65_536,
			ModelInfoContext:        32_768,
			ProviderMetadataContext: 16_384,
			ObservedContext:         8_192,
			CatalogContext:          4_096,
			FallbackContext:         2_048,
		},
	})

	if profile.EffectiveContext != 16_384 || profile.Sources.EffectiveContext.Source != SourceProviderMetadata {
		t.Fatalf("effective context = %#v, want provider metadata", profile.Sources.EffectiveContext)
	}
	if profile.ConfiguredContext != 65_536 || profile.Sources.ConfiguredContext.Source != SourceModelConfig {
		t.Fatalf("configured context was not retained: %#v", profile.Sources.ConfiguredContext)
	}
}

func TestResolveContextPrecedenceByProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		want     int
		source   MetadataSource
		input    ContextResolutionInput
	}{
		{
			name:     "ollama operator",
			provider: "ollama",
			want:     8,
			source:   SourceOperator,
			input:    ContextResolutionInput{OperatorContext: 8, RuntimeContext: 7, ConfiguredContext: 6, ModelInfoContext: 5, ProviderMetadataContext: 4, ObservedContext: 3, CatalogContext: 2, FallbackContext: 1},
		},
		{
			name:     "ollama runtime",
			provider: "ollama",
			want:     7,
			source:   SourceProviderRuntime,
			input:    ContextResolutionInput{RuntimeContext: 7, ConfiguredContext: 6, ModelInfoContext: 5, ProviderMetadataContext: 4, ObservedContext: 3, CatalogContext: 2, FallbackContext: 1},
		},
		{
			name:     "ollama configured",
			provider: "ollama",
			want:     6,
			source:   SourceModelConfig,
			input:    ContextResolutionInput{ConfiguredContext: 6, ModelInfoContext: 5, ProviderMetadataContext: 4, ObservedContext: 3, CatalogContext: 2, FallbackContext: 1},
		},
		{
			name:     "ollama model info",
			provider: "ollama",
			want:     5,
			source:   SourceModelConfig,
			input:    ContextResolutionInput{ModelInfoContext: 5, ProviderMetadataContext: 4, ObservedContext: 3, CatalogContext: 2, FallbackContext: 1},
		},
		{
			name:     "generic provider metadata",
			provider: "openai",
			want:     4,
			source:   SourceProviderMetadata,
			input:    ContextResolutionInput{ConfiguredContext: 6, ModelInfoContext: 5, ProviderMetadataContext: 4, ObservedContext: 3, CatalogContext: 2, FallbackContext: 1},
		},
		{
			name:     "observed",
			provider: "openai",
			want:     3,
			source:   SourceObserved,
			input:    ContextResolutionInput{ObservedContext: 3, CatalogContext: 2, FallbackContext: 1},
		},
		{
			name:     "catalog",
			provider: "openai",
			want:     2,
			source:   SourceCatalog,
			input:    ContextResolutionInput{CatalogContext: 2, FallbackContext: 1},
		},
		{
			name:     "fallback",
			provider: "openai",
			want:     1,
			source:   SourceFallback,
			input:    ContextResolutionInput{FallbackContext: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.input.Provider = tt.provider
			got := ResolveContext(tt.input).Effective
			if got.Value != tt.want || got.Source != tt.source {
				t.Fatalf("effective context = %#v, want value=%d source=%q", got, tt.want, tt.source)
			}
		})
	}
}

func TestResolveCapabilityTriStateMerge(t *testing.T) {
	tests := []struct {
		name   string
		e      CapabilityEvidence
		want   CapabilityState
		source MetadataSource
	}{
		{name: "unknown does not override catalog yes", e: CapabilityEvidence{Runtime: CapabilityUnknown, Catalog: CapabilityYes}, want: CapabilityYes, source: SourceCatalog},
		{name: "provider no overrides catalog yes", e: CapabilityEvidence{ProviderMetadata: CapabilityNo, Catalog: CapabilityYes}, want: CapabilityNo, source: SourceProviderMetadata},
		{name: "fallback unknown is preserved", e: CapabilityEvidence{Fallback: CapabilityUnknown}, want: CapabilityUnknown, source: SourceFallback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveCapability(tt.e)
			if got.Value != tt.want || got.Source != tt.source {
				t.Fatalf("ResolveCapability() = %#v, want value=%q source=%q", got, tt.want, tt.source)
			}
		})
	}
}

func TestResolveCapabilityWithoutEvidenceIsUnknown(t *testing.T) {
	got := ResolveCapability(CapabilityEvidence{})
	if got.Value != CapabilityUnknown || got.Source != "" || got.Confidence != "" {
		t.Fatalf("ResolveCapability() = %#v, want unknown with no source or confidence", got)
	}
}

func TestResolveModelProfileNormalizesMaxOutputConfidence(t *testing.T) {
	profile := ResolveModelProfile(ModelProfileInput{
		ModelID: "catalog-model",
		MaxOutputTokens: ResolvedValue[int]{
			Value:  4_096,
			Source: SourceCatalog,
		},
	})

	if got := profile.Sources.MaxOutputTokens; got.Confidence != confidenceMedium {
		t.Fatalf("max-output provenance = %#v, want confidence %q", got, confidenceMedium)
	}
}

func TestResolveModelProfileFallbackIsEstimated(t *testing.T) {
	profile := ResolveModelProfile(ModelProfileInput{
		ModelID:         "unknown-model",
		MaxOutputTokens: ResolvedValue[int]{Value: 4_096},
		Context:         ContextResolutionInput{FallbackContext: 1_024},
	})

	if profile.EffectiveContext != 1_024 || profile.Sources.EffectiveContext.Source != SourceFallback {
		t.Fatalf("fallback effective context = %#v", profile.Sources.EffectiveContext)
	}
	if profile.Sources.EffectiveContext.Confidence != confidenceEstimated || profile.Sources.MaxOutputTokens.Source != SourceFallback {
		t.Fatalf("fallback provenance = %#v", profile.Sources)
	}
}
