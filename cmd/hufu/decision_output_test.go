package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestWriteDecisionOutputJSONLUsesOneResultEnvelope(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts.eventFormat = "jsonl"
	decisionJSONLSequence.Store(0)

	var output bytes.Buffer
	value := team.NewDecisionOutputV1("decision_run_preview")
	value.Outcome = "preview"
	value.ExitCode = 0
	if err := writeDecisionOutput(&output, value, false); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) != 0 {
		t.Fatalf("JSONL result contains more than one line: %q", output.String())
	}
	var envelope struct {
		SchemaVersion int                   `json:"schema_version"`
		Type          string                `json:"type"`
		Sequence      uint64                `json:"sequence"`
		Data          team.DecisionOutputV1 `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || envelope.Type != "result" || envelope.Sequence != 1 || envelope.Data.Kind != "decision_run_preview" {
		t.Fatalf("result envelope = %#v", envelope)
	}
}

func TestWriteDecisionOutputJSONRemainsBareDTO(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts.eventFormat = "text"

	var output bytes.Buffer
	value := team.NewDecisionOutputV1("decision_run_preview")
	value.Outcome = "preview"
	value.ExitCode = 0
	if err := writeDecisionOutput(&output, value, true); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["kind"] != "decision_run_preview" {
		t.Fatalf("bare DTO = %#v", decoded)
	}
	if _, wrapped := decoded["data"]; wrapped {
		t.Fatalf("JSON output was unexpectedly wrapped: %#v", decoded)
	}
}
