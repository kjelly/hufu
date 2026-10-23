package settings

import (
	"testing"
	"time"
)

func TestTransportResolution(t *testing.T) {
	global := Config{Backend: "systemone", SystemOne: SystemOne{Model: "global", Endpoint: "http://localhost:11434/v1/systemone", Timeout: time.Minute, APIKeyEnv: "DECISION_KEY"}}.WithSource("global")
	project := Config{SystemOne: SystemOne{Model: "project"}}.WithSource("project")
	team := Config{SystemOne: SystemOne{Timeout: 30 * time.Second}}.WithSource("team")
	merged := Merge(Merge(global, project), team)
	got, err := Resolve(merged, Transport{}, "sidecar")
	if err != nil {
		t.Fatal(err)
	}
	if got.Backend != "systemone" || got.Model != "project" || got.Timeout != 30*time.Second || got.APIKeyEnv != "DECISION_KEY" || got.Sources["model"] != "project" || got.Sources["timeout"] != "team" {
		t.Fatalf("resolution = %#v", got)
	}
	got, err = Resolve(merged, Transport{Model: "entry", Timeout: 5 * time.Second}, "sidecar")
	if err != nil || got.Model != "entry" || got.Timeout != 5*time.Second {
		t.Fatalf("entry override = %#v, %v", got, err)
	}
	got, err = Resolve(merged, Transport{Backend: "sidecar"}, "sidecar")
	if err != nil || got.Model != "" || got.Endpoint != "" || got.APIKeyEnv != "" || got.Timeout == 30*time.Second {
		t.Fatalf("backend leaked: %#v, %v", got, err)
	}
	got.Sources["model"] = "changed"
	if merged.Sources["systemone.model"] != "project" {
		t.Fatal("mutated configuration")
	}
}

func TestTransportMissingModelAndLegacyDefault(t *testing.T) {
	if _, err := Resolve(Config{Backend: "systemone"}, Transport{}, "sidecar"); err == nil {
		t.Fatal("missing model accepted")
	}
	got, err := Resolve(Config{}, Transport{}, "sidecar")
	if err != nil || got.Backend != "sidecar" {
		t.Fatalf("default = %#v, %v", got, err)
	}
	for _, cfg := range []Config{{Backend: "openai"}, {SystemOne: SystemOne{Timeout: -1}}, {SystemOne: SystemOne{APIKeyEnv: "bad-key"}}, {SystemOne: SystemOne{Endpoint: "http://secret:password@localhost/systemone"}}} {
		if cfg.Validate() == nil {
			t.Fatalf("accepted invalid configuration %#v", cfg)
		}
	}
}
