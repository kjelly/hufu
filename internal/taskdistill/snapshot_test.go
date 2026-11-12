package taskdistill

import (
	"strings"
	"testing"
	"time"
)

func TestSnapshotCodecPreservesRuntimeDurationWireFormat(t *testing.T) {
	type record struct {
		Duration time.Duration `json:"duration"`
	}
	original := record{Duration: time.Second}
	raw, err := MarshalSnapshot(original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"duration":1000000000`) {
		t.Fatalf("changed runtime wire format: %s", raw)
	}
	var restored record
	if err = UnmarshalSnapshot(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Duration != original.Duration || Digest(restored) != Digest(original) {
		t.Fatal("duration snapshot identity did not survive persistence")
	}
}
