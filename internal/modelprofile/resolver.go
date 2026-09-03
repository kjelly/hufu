package modelprofile

import (
	"cmp"
	"strings"
)

const (
	confidenceHigh      = "high"
	confidenceMedium    = "medium"
	confidenceEstimated = "estimated"
)

func resolvedContext(value int, source MetadataSource) ResolvedValue[int] {
	if value <= 0 {
		return ResolvedValue[int]{}
	}
	return ResolvedValue[int]{Value: value, Source: source, Confidence: confidenceForSource(source)}
}

func resolvedCapability(value CapabilityState, source MetadataSource) ResolvedValue[CapabilityState] {
	value = normalizeCapability(value)
	if value == "" {
		return ResolvedValue[CapabilityState]{}
	}
	return ResolvedValue[CapabilityState]{Value: value, Source: source, Confidence: confidenceForSource(source)}
}

func confidenceForSource(source MetadataSource) string {
	switch source {
	case SourceFallback:
		return confidenceEstimated
	case SourceOperator, SourceProviderRuntime, SourceModelConfig, SourceProviderMetadata:
		return confidenceHigh
	default:
		return confidenceMedium
	}
}

func normalizeCapability(value CapabilityState) CapabilityState {
	switch value {
	case CapabilityYes, CapabilityNo, CapabilityUnknown:
		return value
	default:
		return CapabilityUnknown
	}
}

func isOllama(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "ollama")
}

// ResolveContext applies the explicit authority ordering for context
// metadata. Ollama has an additional model-info/configuration authority between
// configured context and generic provider metadata; generic providers do not.
func ResolveContext(input ContextResolutionInput) ContextResolution {
	result := ContextResolution{
		Operator:         resolvedContext(input.OperatorContext, SourceOperator),
		Runtime:          resolvedContext(input.RuntimeContext, SourceProviderRuntime),
		Configured:       resolvedContext(input.ConfiguredContext, SourceModelConfig),
		ModelInfo:        resolvedContext(input.ModelInfoContext, SourceModelConfig),
		ProviderMetadata: resolvedContext(input.ProviderMetadataContext, SourceProviderMetadata),
		Observed:         resolvedContext(input.ObservedContext, SourceObserved),
		Catalog:          resolvedContext(input.CatalogContext, SourceCatalog),
		Fallback:         resolvedContext(input.FallbackContext, SourceFallback),
	}

	// ModelMax is the theoretical/catalog value, with the conservative static
	// fallback retained when no catalog value is available.
	result.ModelMax = firstContext(result.Catalog, result.Fallback)

	if isOllama(input.Provider) {
		result.Effective = firstContext(
			result.Operator,
			result.Runtime,
			result.Configured,
			result.ModelInfo,
			result.ProviderMetadata,
			result.Observed,
			result.Catalog,
			result.Fallback,
		)
	} else {
		result.Effective = firstContext(
			result.Operator,
			result.Runtime,
			result.ProviderMetadata,
			result.Observed,
			result.Catalog,
			result.Fallback,
		)
	}
	return result
}

func firstContext(values ...ResolvedValue[int]) ResolvedValue[int] {
	for _, value := range values {
		if value.Value > 0 {
			return value
		}
	}
	return ResolvedValue[int]{}
}

// ResolveCapability merges tri-state evidence by authority. An explicit
// unknown is evidence at its source and therefore overrides lower authorities;
// only absent evidence falls through.
func ResolveCapability(input CapabilityEvidence) ResolvedValue[CapabilityState] {
	for _, candidate := range []struct {
		value  CapabilityState
		source MetadataSource
	}{
		{input.Operator, SourceOperator},
		{input.Runtime, SourceProviderRuntime},
		{input.ProviderMetadata, SourceProviderMetadata},
		{input.Catalog, SourceCatalog},
		{input.Fallback, SourceFallback},
	} {
		if candidate.value == "" {
			continue
		}
		return resolvedCapability(candidate.value, candidate.source)
	}
	return ResolvedValue[CapabilityState]{Value: CapabilityUnknown}
}

// ResolveModelProfile builds the canonical profile and retains all resolved
// candidate provenance. It is deterministic for the same input regardless of
// evidence arrival order.
func ResolveModelProfile(input ModelProfileInput) ModelProfile {
	provider := cmp.Or(input.Provider, input.Context.Provider)
	context := input.Context
	context.Provider = cmp.Or(context.Provider, provider)
	resolvedContext := ResolveContext(context)

	tools := ResolveCapability(input.Capabilities.Tools)
	attachments := ResolveCapability(input.Capabilities.Attachments)
	reasoning := ResolveCapability(input.Capabilities.Reasoning)
	temperature := ResolveCapability(input.Capabilities.Temperature)

	maxOutput := input.MaxOutputTokens
	if maxOutput.Value > 0 {
		if maxOutput.Source == "" {
			maxOutput.Source = SourceFallback
		}
		if maxOutput.Confidence == "" {
			maxOutput.Confidence = confidenceForSource(maxOutput.Source)
		}
	}

	return ModelProfile{
		ModelID:             input.ModelID,
		Provider:            provider,
		Family:              input.Family,
		ModelMaxContext:     resolvedContext.ModelMax.Value,
		MaxOutputTokens:     maxOutput.Value,
		ProviderContext:     resolvedContext.ProviderMetadata.Value,
		ConfiguredContext:   resolvedContext.Configured.Value,
		RuntimeContext:      resolvedContext.Runtime.Value,
		EffectiveContext:    resolvedContext.Effective.Value,
		SupportsTools:       tools.Value,
		SupportsAttachments: attachments.Value,
		SupportsReasoning:   reasoning.Value,
		SupportsTemperature: temperature.Value,
		Sources: ModelProfileSources{
			OperatorContext:         resolvedContext.Operator,
			RuntimeContext:          resolvedContext.Runtime,
			ConfiguredContext:       resolvedContext.Configured,
			ModelInfoContext:        resolvedContext.ModelInfo,
			ProviderMetadataContext: resolvedContext.ProviderMetadata,
			ObservedContext:         resolvedContext.Observed,
			CatalogContext:          resolvedContext.Catalog,
			FallbackContext:         resolvedContext.Fallback,
			ModelMaxContext:         resolvedContext.ModelMax,
			EffectiveContext:        resolvedContext.Effective,
			MaxOutputTokens:         maxOutput,
			Capabilities: CapabilitySources{
				Tools:       tools,
				Attachments: attachments,
				Reasoning:   reasoning,
				Temperature: temperature,
			},
		},
	}
}
