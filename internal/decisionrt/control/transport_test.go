package control

import (
	"github.com/kjelly/hufu/internal/decisionrt/settings"
	"testing"
	"time"
)

func TestCommonRuntimeTransportAndLegacyOverrides(t *testing.T) {
	defaults := settings.Config{Backend: "systemone", SystemOne: settings.SystemOne{Model: "shared", Endpoint: "http://localhost:11434/v1/systemone", Timeout: time.Minute}}.WithSource("global")
	got, err := resolveTransport(Config{}, defaults)
	if err != nil || got.Model != "shared" || got.Timeout != time.Minute {
		t.Fatalf("shared = %#v, %v", got, err)
	}
	got, err = resolveTransport(Config{Model: "legacy", Timeout: time.Second}, defaults)
	if err != nil || got.Model != "legacy" || got.Timeout != time.Second {
		t.Fatalf("legacy = %#v, %v", got, err)
	}
	got, err = resolveTransport(Config{Backend: "sidecar", Model: "legacy-inactive", APIKeyEnv: "LEGACY_KEY"}, defaults)
	if err != nil || got.Model != "" || got.Endpoint != "" || got.APIKeyEnv != "" {
		t.Fatalf("isolated = %#v, %v", got, err)
	}
	if _, err := New(Config{Mode: ModeActive}, func(s string) string { return s }, WithRuntimeConfig(defaults)); err != nil {
		t.Fatal(err)
	}
}
