package team

import (
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebResearchAdmitsHistoricalResearchButRequiresIndependentVerifier(t *testing.T) {
	dir := filepath.Join(bundledTeamsDir(t), "web-research")
	research, err := compileResultContractSchema(dir, "schemas/research-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := compileResultContractSchema(dir, "schemas/report-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	const body = "- paragraph: exact words"
	for _, tool := range research.toolEvidence.Tools {
		t.Run(tool, func(t *testing.T) {
			read := &ObservationReadPayload{ReadEventID: "current-read-event", ObservationID: "original-event", SourceRunID: "prior", SourceTaskID: "1", SourceCallID: "original-snapshot", ObservedAt: "2026-10-18T00:00:00Z", SHA256: strings.Repeat("a", 64), ContentSHA256: observationContentHash(body), ContentFormat: "evidence_text", Tool: tool, ReadCallID: "current-read", Bytes: len(body), TotalBytes: len(body)}
			callInput, err := json.Marshal(recallObservationArgs{Action: "read", ObservationID: read.ObservationID})
			if err != nil {
				t.Fatal(err)
			}
			resultOutput, err := json.Marshal(map[string]any{"historical": true, "content": body, "target": "https://example.com/record", "provenance": read})
			if err != nil {
				t.Fatal(err)
			}
			records := []taskTranscriptRecord{
				{Event: "tool_call", Tool: recallObservationToolName, ToolCallID: read.ReadCallID, Input: string(callInput)},
				{Event: "historical_observation", Tool: tool, ToolCallID: read.ReadCallID, Output: body, Historical: read, HistoricalTarget: "https://example.com/record"},
				{Event: "tool_result", Tool: recallObservationToolName, ToolCallID: read.ReadCallID, Output: string(resultOutput)},
			}
			source := map[string]any{"title": "Original record", "publisher": "Example", "publication_date": "unknown", "url": "https://example.com/record", "source_type": "primary", "page_status": "fetched", "quote": "exact words", "relation": "Supports this predicate", "independence_group": "original"}
			raw, err := json.Marshal(map[string]any{"lens": "primary_search", "question": "Follow-up question", "findings": []any{map[string]any{"claim": "One atomic predicate", "assessment": "supported", "reasoning": "Earlier record supports this predicate", "sources": []any{source}, "open_questions": "No current-development claim"}}, "disconfirmation_effort": "Checked relevant historical text", "limitations": "Source observation time is historical"})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := validateStructuredResultPayload(research, research.ref(true), raw, records)
			if err != nil || payload.EvidenceDowngrades != 0 {
				t.Fatalf("research schema rejected authorized historical evidence: %v", err)
			}
			value, err := decodeResultContractJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			if evidenceString(value, "/findings/0/sources/0/historical_observation/source_call_id") != "original-snapshot" || evidenceString(value, "/findings/0/sources/0/tool_call_id") != "current-read" {
				t.Fatalf("real schema lost provenance: %s", payload.Value)
			}
			if verifier.toolEvidence.AllowHistorical || len(matchingToolObservations(verifier.toolEvidence, records)) != 0 {
				t.Fatal("verifier credited another researcher's historical read")
			}
		})
	}
}
