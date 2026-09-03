// Package modelprofile contains the canonical model metadata representation.
//
// Provider adapters and catalogs can contribute independent evidence to a
// profile. The resolver in this package chooses effective values without
// making callers depend on the order in which that evidence arrived.
package modelprofile

import "strings"

// NormalizeCapabilityName maps provider vocabulary to canonical profile
// names. Unknown names are returned unchanged with ok=false so adapters can
// retain forward-compatible raw evidence without treating it as known.
func NormalizeCapabilityName(name string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	normalized = strings.ReplaceAll(normalized, " ", "_")
	switch normalized {
	case "tool", "tools", "tool_call", "tool_calls", "function_call", "function_calling", "functions", "supports_tools":
		return "tools", true
	case "attachment", "attachments", "vision", "image", "images", "multimodal", "multimodal_input", "supports_attachments", "supports_vision":
		return "attachments", true
	case "reasoning", "reason", "thinking", "chain_of_thought", "supports_reasoning":
		return "reasoning", true
	case "temperature", "sampling_temperature", "supports_temperature":
		return "temperature", true
	default:
		return normalized, false
	}
}

// CapabilityState is a tri-state capability value. Unknown is intentionally
// different from No: the absence of evidence must not be treated as a denial.
type CapabilityState string

const (
	CapabilityUnknown CapabilityState = "unknown"
	CapabilityYes     CapabilityState = "yes"
	CapabilityNo      CapabilityState = "no"
)

// MetadataSource identifies where a metadata value came from.
type MetadataSource string

const (
	SourceOperator         MetadataSource = "operator"
	SourceProviderRuntime  MetadataSource = "provider_runtime"
	SourceProviderMetadata MetadataSource = "provider_metadata"
	SourceModelConfig      MetadataSource = "model_config"
	SourceCatalog          MetadataSource = "catalog"
	SourceObserved         MetadataSource = "provider_observed"
	SourceFallback         MetadataSource = "fallback"
)

// ResolvedValue keeps a value together with its authority and confidence.
// A zero value represents an unavailable value.
type ResolvedValue[T any] struct {
	Value      T              `json:"value,omitzero"`
	Source     MetadataSource `json:"source,omitzero"`
	Confidence string         `json:"confidence,omitempty"`
}

// ContextResolutionInput contains independent context evidence. A non-
// positive value means that the corresponding source has no usable evidence.
// ModelInfoContext represents Ollama /api/show model_info or parameters
// configuration and is intentionally distinct from ConfiguredContext.
type ContextResolutionInput struct {
	Provider                string
	OperatorContext         int
	RuntimeContext          int
	ConfiguredContext       int
	ModelInfoContext        int
	ProviderMetadataContext int
	ObservedContext         int
	CatalogContext          int
	FallbackContext         int
}

// ContextResolution contains every context candidate and the selected
// effective value. Keeping the candidates makes provenance auditable and
// prevents a lower-authority update from erasing higher-authority evidence.
type ContextResolution struct {
	Operator         ResolvedValue[int]
	Runtime          ResolvedValue[int]
	Configured       ResolvedValue[int]
	ModelInfo        ResolvedValue[int]
	ProviderMetadata ResolvedValue[int]
	Observed         ResolvedValue[int]
	Catalog          ResolvedValue[int]
	Fallback         ResolvedValue[int]
	ModelMax         ResolvedValue[int]
	Effective        ResolvedValue[int]
}

// CapabilityEvidence contains capability values from the supported authority
// levels. An explicit unknown is retained at its source while an absent value
// allows resolution to continue to lower-authority evidence.
type CapabilityEvidence struct {
	Operator         CapabilityState
	Runtime          CapabilityState
	ProviderMetadata CapabilityState
	Catalog          CapabilityState
	Fallback         CapabilityState
}

// CapabilityResolutionInput groups evidence for each canonical capability.
type CapabilityResolutionInput struct {
	Tools       CapabilityEvidence
	Attachments CapabilityEvidence
	Reasoning   CapabilityEvidence
	Temperature CapabilityEvidence
}

// CapabilitySources records the provenance of resolved capabilities.
type CapabilitySources struct {
	Tools       ResolvedValue[CapabilityState]
	Attachments ResolvedValue[CapabilityState]
	Reasoning   ResolvedValue[CapabilityState]
	Temperature ResolvedValue[CapabilityState]
}

// ModelProfile is the canonical model metadata profile. Context values are
// kept separate by role; EffectiveContext is the only value intended for
// runtime admission.
type ModelProfile struct {
	ModelID  string `json:"model_id"`
	Provider string `json:"provider"`

	Family string `json:"family,omitempty"`

	// Catalog/theoretical and provider/runtime values are deliberately
	// separate. ModelMaxContext may be a conservative fallback estimate.
	ModelMaxContext int `json:"model_max_context,omitzero"`
	MaxOutputTokens int `json:"max_output_tokens,omitzero"`

	ProviderContext   int `json:"provider_context,omitzero"`
	ConfiguredContext int `json:"configured_context,omitzero"`
	RuntimeContext    int `json:"runtime_context,omitzero"`
	EffectiveContext  int `json:"effective_context,omitzero"`

	SupportsTools       CapabilityState `json:"supports_tools,omitzero"`
	SupportsAttachments CapabilityState `json:"supports_attachments,omitzero"`
	SupportsReasoning   CapabilityState `json:"supports_reasoning,omitzero"`
	SupportsTemperature CapabilityState `json:"supports_temperature,omitzero"`

	Sources ModelProfileSources `json:"sources,omitzero"`
}

// ModelProfileSources preserves all context provenance, including candidates
// which lost resolution to a higher-authority source.
type ModelProfileSources struct {
	OperatorContext         ResolvedValue[int] `json:"operator_context,omitzero"`
	RuntimeContext          ResolvedValue[int] `json:"runtime_context,omitzero"`
	ConfiguredContext       ResolvedValue[int] `json:"configured_context,omitzero"`
	ModelInfoContext        ResolvedValue[int] `json:"model_info_context,omitzero"`
	ProviderMetadataContext ResolvedValue[int] `json:"provider_metadata_context,omitzero"`
	ObservedContext         ResolvedValue[int] `json:"observed_context,omitzero"`
	CatalogContext          ResolvedValue[int] `json:"catalog_context,omitzero"`
	FallbackContext         ResolvedValue[int] `json:"fallback_context,omitzero"`
	ModelMaxContext         ResolvedValue[int] `json:"model_max_context,omitzero"`
	EffectiveContext        ResolvedValue[int] `json:"effective_context,omitzero"`
	MaxOutputTokens         ResolvedValue[int] `json:"max_output_tokens,omitzero"`
	Capabilities            CapabilitySources  `json:"capabilities,omitzero"`
}

// ModelProfileInput supplies identity, capability evidence, context evidence,
// and output-token metadata to ResolveModelProfile.
type ModelProfileInput struct {
	ModelID         string
	Provider        string
	Family          string
	MaxOutputTokens ResolvedValue[int]
	Context         ContextResolutionInput
	Capabilities    CapabilityResolutionInput
}
