package settings

import (
	"bytes"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/utils"
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

func TestSidecarTimeoutIsBackendScoped(t *testing.T) {
	global := Config{Backend: "sidecar", Sidecar: Sidecar{Timeout: time.Minute}, SystemOne: SystemOne{Model: "inactive", APIKeyEnv: "UNUSED_DECISION_KEY", Timeout: 30 * time.Second}}.WithSource("global")
	local := Config{Sidecar: Sidecar{Timeout: time.Second}}.WithSource("team")
	merged := Merge(global, local)
	got, err := Resolve(merged, Transport{}, "sidecar")
	if err != nil || got.Timeout != time.Second || got.Sources["timeout"] != "team" || got.APIKeyEnv != "" || got.Model != "" {
		t.Fatalf("transport=%+v err=%v", got, err)
	}
	before, err := merged.Fingerprint("sidecar")
	if err != nil {
		t.Fatal(err)
	}
	merged.SystemOne.Model = "another-inactive-model"
	after, err := merged.Fingerprint("sidecar")
	if err != nil || before != after {
		t.Fatalf("inactive model changed fingerprint: %v", err)
	}
	merged.Sidecar.Timeout = 2 * time.Second
	after, err = merged.Fingerprint("sidecar")
	if err != nil || before == after {
		t.Fatalf("active timeout did not change fingerprint: %v", err)
	}
	merged.Sidecar.Timeout = -time.Second
	if merged.Validate() == nil {
		t.Fatal("negative sidecar timeout accepted")
	}
}

func TestTransportProvenanceSurvivesRedaction(t *testing.T) {
	cfg := Config{Backend: "systemone", SystemOne: SystemOne{Model: "fixture-model", APIKeyEnv: "DECISION_TEST_KEY"}}.WithSource("global")
	transport, err := Resolve(cfg, Transport{}, "sidecar")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(transport)
	if err != nil {
		t.Fatal(err)
	}
	redacted, err := utils.RedactJSON(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Transport
	if err := json.Unmarshal(redacted, &persisted); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, canonical) {
		t.Fatal("transport provenance changed during journal redaction")
	}
}
