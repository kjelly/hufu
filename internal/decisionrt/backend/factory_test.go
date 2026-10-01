package backend

import (
	"testing"
)

func TestFactorySharesAdaptersWithoutNetwork(t *testing.T) {
	for _, config := range []Config{{Name: "rule"}, {Name: "systemone", Endpoint: "http://localhost:11434/v1/systemone", Model: "nimble"}} {
		adapter, err := New(config)
		if err != nil || adapter.Name() != config.Name {
			t.Fatalf("adapter %q: %v", config.Name, err)
		}
	}
	for _, config := range []Config{{Name: "missing"}, {Name: "sidecar"}, {Name: "systemone"}} {
		if _, err := New(config); err == nil {
			t.Fatalf("invalid backend accepted: %s", config.Name)
		}
	}
}
