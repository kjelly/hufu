package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

type imageProfileIntrospector struct {
	info providerintrospection.RuntimeModelInfo
}

func (i imageProfileIntrospector) InspectModel(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, error) {
	return i.info, nil
}

func TestImageAdmissionSnapshotBindsCapabilityAndTransport(t *testing.T) {
	for _, test := range []struct {
		name  string
		state providerintrospection.CapabilityState
		want  providerintrospection.CapabilityState
	}{
		{name: "yes", state: providerintrospection.CapabilityYes, want: providerintrospection.CapabilityYes},
		{name: "no", state: providerintrospection.CapabilityNo, want: providerintrospection.CapabilityNo},
		{name: "unknown", state: providerintrospection.CapabilityUnknown, want: providerintrospection.CapabilityUnknown},
		{name: "attachments only", want: providerintrospection.CapabilityUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			runtime := &ModelProfileRuntime{manager: manager, resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return imageProfileIntrospector{info: providerintrospection.RuntimeModelInfo{ConfiguredContext: 32768, ImageInput: test.state, Capabilities: []string{"attachments"}}}
			}, modelprofile.ProfileCacheOptions{})}
			resolved := runtime.ResolveAdmission(t.Context(), "local/fixture", 32768, 512, 64)
			identity, err := manager.ResolveProviderExecutionIdentity("local/fixture")
			if err != nil {
				t.Fatal(err)
			}
			bound := resolved.Admission
			if !bound.IsBound() || bound.ProviderIdentity != identity.ProviderKey || bound.ProviderConfigurationHash == "" || bound.ProviderConfigurationHash != identity.ConfigurationHash {
				t.Fatal("image evidence lost the exact resolved provider transport identity")
			}
			if bound.ImageInputCapability != test.want || bound.ImageInputSource != string(resolved.Profile.Sources.Capabilities.ImageInput.Source) {
				t.Fatal("bound image capability differs from its immutable profile evidence")
			}
			if (bound.ImageInputSource == "") != (test.state == "") {
				t.Fatal("bound profile invented or discarded image capability provenance")
			}
		})
	}
}

func TestImageAdmissionSnapshotDetectsTransportConfigurationChanges(t *testing.T) {
	var hashes []string
	for _, endpoint := range []string{"http://127.0.0.1:11434/v1", "http://127.0.0.1:11435/v1"} {
		manager, err := agent.NewProviderManager(endpoint, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		runtime := &ModelProfileRuntime{manager: manager, resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return imageProfileIntrospector{info: providerintrospection.RuntimeModelInfo{ConfiguredContext: 32768, ImageInput: providerintrospection.CapabilityYes}}
		}, modelprofile.ProfileCacheOptions{})}
		bound := runtime.ResolveAdmission(t.Context(), "local/fixture", 32768, 512, 64).Admission
		if bound.ImageInputCapability != providerintrospection.CapabilityYes || bound.ProviderConfigurationHash == "" {
			t.Fatal("fixture did not bind image evidence to a transport")
		}
		hashes = append(hashes, bound.ProviderConfigurationHash)
	}
	if hashes[0] == hashes[1] {
		t.Fatal("different destinations shared a bound image transport identity")
	}
}
