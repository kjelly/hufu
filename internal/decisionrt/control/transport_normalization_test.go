package control

import (
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt/settings"
)

func TestNormalizeTransportAliases(t *testing.T) {
	tests := []struct {
		name           string
		canonical      settings.Config
		legacy         Config
		backend, model string
		timeout        time.Duration
		conflict       bool
	}{
		{name: "empty"},
		{name: "policy only", legacy: Config{Mode: ModeActive}},
		{name: "canonical", canonical: settings.Config{Backend: BackendSystemOne, SystemOne: settings.SystemOne{Model: "canonical"}}, backend: BackendSystemOne, model: "canonical"},
		{name: "implicit legacy backend", legacy: Config{Model: "legacy"}, backend: BackendSystemOne, model: "legacy"},
		{name: "legacy sidecar", legacy: Config{Backend: BackendSidecar, Model: "inactive", Endpoint: "inactive", APIKeyEnv: "INACTIVE", Timeout: time.Second}, backend: BackendSidecar, timeout: time.Second},
		{name: "same values", canonical: settings.Config{Backend: BackendSystemOne, SystemOne: settings.SystemOne{Model: "same"}}, legacy: Config{Model: "same"}, backend: BackendSystemOne, model: "same"},
		{name: "model conflict", canonical: settings.Config{SystemOne: settings.SystemOne{Model: "one"}}, legacy: Config{Model: "two"}, conflict: true},
		{name: "implicit backend conflict", canonical: settings.Config{Backend: BackendSidecar}, legacy: Config{Model: "legacy"}, conflict: true},
		{name: "explicit backend conflict", canonical: settings.Config{Backend: BackendSidecar}, legacy: Config{Backend: BackendSystemOne}, conflict: true},
		{name: "endpoint conflict", canonical: settings.Config{SystemOne: settings.SystemOne{Endpoint: "http://one"}}, legacy: Config{Endpoint: "http://two"}, conflict: true},
		{name: "credential name conflict", canonical: settings.Config{SystemOne: settings.SystemOne{APIKeyEnv: "FIRST_KEY"}}, legacy: Config{APIKeyEnv: "SECOND_KEY"}, conflict: true},
		{name: "systemone timeout conflict", canonical: settings.Config{SystemOne: settings.SystemOne{Timeout: time.Second}}, legacy: Config{Timeout: time.Minute}, conflict: true},
		{name: "sidecar timeout conflict", canonical: settings.Config{Backend: BackendSidecar, Sidecar: settings.Sidecar{Timeout: time.Second}}, legacy: Config{Backend: BackendSidecar, Timeout: time.Minute}, conflict: true},
		{name: "invalid backend", legacy: Config{Backend: "invalid"}, conflict: true},
		{name: "invalid timeout", legacy: Config{Timeout: -time.Second}, conflict: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeTransport(tt.canonical.WithSource("fixture"), tt.legacy, "fixture")
			if (err != nil) != tt.conflict {
				t.Fatalf("got=%+v err=%v", got, err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "FIRST_KEY") || strings.Contains(err.Error(), "SECOND_KEY") {
					t.Fatal("conflict exposes configured value")
				}
				return
			}
			if got.Backend != tt.backend || got.SystemOne.Model != tt.model {
				t.Fatalf("got=%+v", got)
			}
			timeout := got.SystemOne.Timeout
			if got.Backend == BackendSidecar {
				timeout = got.Sidecar.Timeout
			}
			if timeout != tt.timeout {
				t.Fatalf("timeout=%v want=%v", timeout, tt.timeout)
			}
			if tt.legacy.HasTransport() && tt.canonical.Backend == "" && !strings.Contains(got.Sources["backend"], "control-decisions") {
				t.Fatalf("legacy provenance=%v", got.Sources)
			}
		})
	}
}

func TestNormalizedTransportLayersAndPolicy(t *testing.T) {
	global, err := NormalizeTransport(settings.Config{}.WithSource("global"), Config{Model: "global", Endpoint: "http://localhost:11434/v1/systemone", Timeout: time.Minute}, "global")
	if err != nil {
		t.Fatal(err)
	}
	local, err := NormalizeTransport(settings.Config{SystemOne: settings.SystemOne{Model: "team"}}.WithSource("team"), Config{Mode: ModeActive, MinConfidence: new(.99)}, "team")
	if err != nil {
		t.Fatal(err)
	}
	got, err := settings.Resolve(settings.Merge(global, local), settings.Transport{}, BackendSidecar)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "team" || got.Timeout != time.Minute || got.Sources["model"] != "team" || got.Sources["timeout"] != "global:control-decisions.timeout" {
		t.Fatalf("transport=%+v", got)
	}
	original := Config{Backend: BackendSidecar, Timeout: time.Second, Mode: ModeActive, Points: map[Point]PointConfig{GuardReviewer: {MinConfidence: new(.99)}}}
	policy := original.PolicyOnly()
	*policy.Points[GuardReviewer].MinConfidence = .5
	if policy.HasTransport() || policy.Mode != ModeActive || *original.Points[GuardReviewer].MinConfidence != .99 {
		t.Fatalf("policy=%+v original=%+v", policy, original)
	}
}
