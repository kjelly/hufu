package modelprofile

import "strings"

// TelemetryValue is a bounded, secret-free value with its resolution
// provenance. It is intended for status, durable events, and reports; it does
// not carry provider transport details.
type TelemetryValue[T any] struct {
	Value      T              `json:"value,omitzero"`
	Source     MetadataSource `json:"source,omitzero"`
	Provenance string         `json:"provenance,omitempty"`
	Confidence string         `json:"confidence,omitempty"`
}

// TelemetryCapability is the public projection of one capability decision.
// Unknown is retained as a state rather than being converted to false.
type TelemetryCapability struct {
	State      CapabilityState `json:"state"`
	Source     MetadataSource  `json:"source,omitzero"`
	Provenance string          `json:"provenance,omitempty"`
	Confidence string          `json:"confidence,omitempty"`
}

// TelemetryProjection is the canonical model-profile telemetry DTO. Only
// normalized identity and metadata evidence is included. Provider URLs,
// credentials, headers, and raw provider responses are intentionally absent.
type TelemetryProjection struct {
	SchemaVersion int                    `json:"schema_version"`
	ModelID       string                 `json:"model_id"`
	Provider      string                 `json:"provider"`
	Family        string                 `json:"family,omitempty"`
	Estimator     TelemetryValue[string] `json:"estimator,omitzero"`

	Operator         TelemetryValue[int] `json:"operator_context,omitzero"`
	Catalog          TelemetryValue[int] `json:"catalog_context,omitzero"`
	Configured       TelemetryValue[int] `json:"configured_context,omitzero"`
	Runtime          TelemetryValue[int] `json:"runtime_context,omitzero"`
	ModelInfo        TelemetryValue[int] `json:"model_info_context,omitzero"`
	ProviderMetadata TelemetryValue[int] `json:"provider_metadata_context,omitzero"`
	Observed         TelemetryValue[int] `json:"observed_context,omitzero"`
	Fallback         TelemetryValue[int] `json:"fallback_context,omitzero"`
	ModelMax         TelemetryValue[int] `json:"model_max_context,omitzero"`
	Effective        TelemetryValue[int] `json:"effective_context,omitzero"`
	MaxOutput        TelemetryValue[int] `json:"max_output_tokens,omitzero"`

	Capabilities map[string]TelemetryCapability `json:"capabilities,omitzero"`
}

const telemetrySchemaVersion = 1

// TelemetryFromProfile creates the only supported status/durable projection
// of a ModelProfile. Provenance is restricted to short metadata labels so an
// adapter cannot accidentally turn a free-form provider response into an
// observable payload.
func TelemetryFromProfile(profile ModelProfile) TelemetryProjection {
	return TelemetryProjection{
		SchemaVersion:    telemetrySchemaVersion,
		ModelID:          safeTelemetryIdentity(profile.ModelID),
		Provider:         safeTelemetryLabel(profile.Provider),
		Family:           safeTelemetryLabel(profile.Family),
		Estimator:        telemetryString(profile.Sources.Estimator, profile.EstimatorProvenance),
		Operator:         telemetryContext(profile.Sources.OperatorContext),
		Catalog:          telemetryContext(profile.Sources.CatalogContext),
		Configured:       telemetryContext(profile.Sources.ConfiguredContext),
		Runtime:          telemetryContext(profile.Sources.RuntimeContext),
		ModelInfo:        telemetryContext(profile.Sources.ModelInfoContext),
		ProviderMetadata: telemetryContext(profile.Sources.ProviderMetadataContext),
		Observed:         telemetryContext(profile.Sources.ObservedContext),
		Fallback:         telemetryContext(profile.Sources.FallbackContext),
		ModelMax:         telemetryContext(profile.Sources.ModelMaxContext),
		Effective:        telemetryContext(profile.Sources.EffectiveContext),
		MaxOutput:        telemetryContext(profile.Sources.MaxOutputTokens),
		Capabilities: map[string]TelemetryCapability{
			"tools":       telemetryCapability(profile.Sources.Capabilities.Tools),
			"attachments": telemetryCapability(profile.Sources.Capabilities.Attachments),
			"reasoning":   telemetryCapability(profile.Sources.Capabilities.Reasoning),
			"temperature": telemetryCapability(profile.Sources.Capabilities.Temperature),
		},
	}
}

func telemetryContext(value ResolvedValue[int]) TelemetryValue[int] {
	return TelemetryValue[int]{Value: value.Value, Source: value.Source, Provenance: safeTelemetryLabel(string(value.Source)), Confidence: value.Confidence}
}

func telemetryString(value ResolvedValue[string], provenance string) TelemetryValue[string] {
	return TelemetryValue[string]{Value: safeTelemetryLabel(value.Value), Source: value.Source, Provenance: safeTelemetryLabel(provenance), Confidence: value.Confidence}
}

func telemetryCapability(value ResolvedValue[CapabilityState]) TelemetryCapability {
	return TelemetryCapability{State: value.Value, Source: value.Source, Provenance: safeTelemetryLabel(string(value.Source)), Confidence: value.Confidence}
}

func safeTelemetryLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 80 || strings.ContainsAny(value, ":/\\\n\r\t") {
		return ""
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._-", r) {
			return ""
		}
	}
	return value
}

func safeTelemetryIdentity(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 160 || strings.ContainsAny(value, "\\\n\r\t") || strings.Contains(value, "://") {
		return ""
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._-/:@+", r) {
			return ""
		}
	}
	return value
}
