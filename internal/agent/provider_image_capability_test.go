package agent

import "testing"

func TestNativeImageAdapterCapabilityRequiresInitializedProvider(t *testing.T) {
	var missing *OpenAICompatibleProvider
	if missing.SupportsNativeImageInput() || new(OpenAICompatibleProvider).SupportsNativeImageInput() {
		t.Fatal("unavailable adapter claimed native image support")
	}
	provider, err := NewOpenAICompatibleProvider("http://127.0.0.1:11434/v1", "", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !provider.SupportsNativeImageInput() {
		t.Fatal("initialized pinned OpenAI-compatible encoder lost its image capability")
	}
}
