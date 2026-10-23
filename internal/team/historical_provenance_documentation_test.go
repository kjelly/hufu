package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPublishedHistoricalProvenanceSchemaAcceptsRuntimeBinding(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(bundledTeamsDir(t), "..", "docs", "reference", "agent-format.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(doc), "<!-- historical-observation-provenance-schema -->")
	if !found {
		t.Fatal("published provenance schema is missing")
	}
	_, section, found = strings.Cut(section, "```json\n")
	if !found {
		t.Fatal("published provenance schema has no JSON block")
	}
	encoded, _, found := strings.Cut(section, "\n```")
	if !found {
		t.Fatal("published provenance schema is incomplete")
	}
	var schema any
	if err := json.Unmarshal([]byte(encoded), &schema); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("provenance.json", schema); err != nil {
		t.Fatal(err)
	}
	validator, err := compiler.Compile("provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.Validate(nil); err != nil {
		t.Fatalf("fresh observations cannot use null provenance: %v", err)
	}
	c, ctx, _ := observationFixture(t)
	event := captureObservation(t, c, ctx, "source__read_document", `{"service":"public"}`, textEvidenceTestRecords()[1].Output)
	item, ctx := nextObservationTask(t, c)
	compiled := historicalEvidenceContract(t)
	ref := compiled.ref(true)
	item.ResultContract = &ref
	c.session.ResultContracts = map[string]*CompiledResultContract{ref.ID: compiled}
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, c.executionRunID, 1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	ctx = context.WithValue(ctx, taskTranscriptKey{}, transcript)
	response, _ := recallResponse(t, c, ctx, "documented-read", fmt.Sprintf(`{"action":"read","observation_id":%q,"offset":4,"limit":11}`, event.ID))
	if response.IsError {
		t.Fatal(response.Content)
	}
	payload, err := validateStructuredResultPayload(compiled, ref, []byte(evidenceTestPayload), transcript.evidenceRecords())
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := resolveJSONPointer(value, "/findings/0/sources/0/history")
	if err != nil || provenance == nil {
		t.Fatalf("missing actual historical binding: %v", err)
	}
	if err := validator.Validate(provenance); err != nil {
		t.Fatalf("published schema rejects runtime provenance: %v", err)
	}
}
