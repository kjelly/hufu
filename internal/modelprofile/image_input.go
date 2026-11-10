package modelprofile

import "github.com/kjelly/hufu/internal/providerintrospection"

func applyImageInputEvidence(target *CapabilityEvidence, info providerintrospection.RuntimeModelInfo, runtime bool) {
	state := info.ImageInput
	if state == "" {
		state = providerintrospection.ImageInputEvidence(info.Capabilities, info.Raw)
	}
	if state == "" {
		return
	}
	value := CapabilityUnknown
	switch state {
	case providerintrospection.CapabilityYes:
		value = CapabilityYes
	case providerintrospection.CapabilityNo:
		value = CapabilityNo
	}
	setImageInputEvidence(target, value, runtime)
}

func setImageInputEvidence(target *CapabilityEvidence, value CapabilityState, runtime bool) {
	if runtime {
		target.Runtime = value
	} else {
		target.ProviderMetadata = value
	}
}
