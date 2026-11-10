package modelprofile

import (
	"testing"

	"github.com/kjelly/hufu/internal/providerintrospection"
)

func TestImageInputProfileRetainsIndependentProviderEvidence(t *testing.T) {
	for _, test := range []struct {
		name    string
		show    providerintrospection.RuntimeModelInfo
		process providerintrospection.RuntimeModelInfo
		want    CapabilityState
		source  MetadataSource
	}{
		{name: "attachments are not vision", show: providerintrospection.RuntimeModelInfo{Capabilities: []string{"attachments", "multimodal"}}, want: CapabilityUnknown},
		{name: "vision metadata", show: providerintrospection.RuntimeModelInfo{Capabilities: []string{"vision"}}, want: CapabilityYes, source: SourceProviderMetadata},
		{name: "runtime no overrides metadata", show: providerintrospection.RuntimeModelInfo{ImageInput: providerintrospection.CapabilityYes}, process: providerintrospection.RuntimeModelInfo{ImageInput: providerintrospection.CapabilityNo}, want: CapabilityNo, source: SourceProviderRuntime},
		{name: "runtime unknown overrides metadata", show: providerintrospection.RuntimeModelInfo{ImageInput: providerintrospection.CapabilityYes}, process: providerintrospection.RuntimeModelInfo{ImageInput: providerintrospection.CapabilityUnknown}, want: CapabilityUnknown, source: SourceProviderRuntime},
		{name: "absent runtime preserves metadata", show: providerintrospection.RuntimeModelInfo{ImageInput: providerintrospection.CapabilityYes}, process: providerintrospection.RuntimeModelInfo{Capabilities: []string{"attachments"}}, want: CapabilityYes, source: SourceProviderMetadata},
		{name: "canonical evidence map", show: providerintrospection.RuntimeModelInfo{CapabilityEvidence: map[string]providerintrospection.CapabilityState{"image_input": providerintrospection.CapabilityYes}}, want: CapabilityYes, source: SourceProviderMetadata},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := ModelProfileInput{ModelID: "fixture", Provider: "ollama", Capabilities: CapabilityResolutionInput{Attachments: CapabilityEvidence{Catalog: CapabilityYes}}}
			applyRuntimeEvidence(&input, providerintrospection.ProviderRef{Type: "ollama"}, runtimeEvidence{show: test.show, showOK: true, process: test.process, processOK: true})
			profile := ResolveModelProfile(input)
			if profile.SupportsImageInput != test.want || profile.Sources.Capabilities.ImageInput.Source != test.source {
				t.Fatalf("image evidence=%+v want state=%s source=%s", profile.Sources.Capabilities.ImageInput, test.want, test.source)
			}
			if profile.SupportsAttachments != CapabilityYes {
				t.Fatal("independent image evidence changed legacy attachments")
			}
			telemetry, found := TelemetryFromProfile(profile).Capabilities["image_input"]
			if test.source == "" {
				if found {
					t.Fatal("missing image evidence changed legacy telemetry")
				}
			} else if !found || telemetry.State != test.want || telemetry.Source != test.source {
				t.Fatal("telemetry lost image evidence or its authority")
			}
		})
	}
}
