package providerintrospection

// ImageInputEvidence accepts explicit image/vision claims only. Generic
// attachments and multimodal declarations do not identify their modalities.
// Conflicting claims at the same authority fail closed: no dominates unknown,
// which dominates yes. Empty is absence, distinct from explicit unknown.
func ImageInputEvidence(capabilities []string, fields map[string]any) CapabilityState {
	var evidence CapabilityState
	for _, name := range capabilities {
		if isImageInputName(name) {
			evidence = reduceCapabilityState(evidence, capabilityStateFromName(name))
		}
	}
	for name, value := range fields {
		if !isImageInputName(name) {
			continue
		}
		state, valid := explicitCapabilityState(value)
		if !valid {
			state = CapabilityUnknown
		}
		// A negative alias never establishes positive image support.
		if capabilityStateFromName(name) == CapabilityNo {
			state = CapabilityNo
		}
		evidence = reduceCapabilityState(evidence, state)
	}
	return evidence
}

func isImageInputName(name string) bool {
	switch normalizedCapabilityName(name) {
	case "vision", "image", "images", "image_input", "supports_vision", "supports_image_input":
		return true
	}
	return false
}
