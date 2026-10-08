package team

import (
	"encoding/json"
	"testing"
)

func diagnosticsTestContract(t *testing.T) *CompiledResultContract {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(evidenceTestSchema), &schema); err != nil {
		t.Fatal(err)
	}
	spec := schema["x-hufu-tool-evidence"].(map[string]any)
	spec["diagnostics"] = map[string]any{
		"fetch_pointer": "/fetch", "citation_pointer": "/citation",
		"fetch_values":    map[string]string{"succeeded": "fetched", "failed": "failed", "unusable": "unusable", "absent": "absent", "pending": "pending"},
		"citation_values": map[string]string{"matched": "matched", "mismatch": "mismatch", "empty": "empty", "unavailable": "unavailable"},
	}
	props := schema["properties"].(map[string]any)["findings"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["sources"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	props["fetch"] = map[string]any{"enum": []string{"fetched", "failed", "unusable", "absent", "pending"}}
	props["citation"] = map[string]any{"enum": []string{"matched", "mismatch", "empty", "unavailable"}}
	props["origin"] = map[string]any{"type": "string"}
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "diagnostics.json", string(data))
	contract, err := compileResultContractSchema(dir, "diagnostics.json")
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func TestResultEvidenceSeparatesFetchAndCitation(t *testing.T) {
	for _, test := range []struct{ name, fetch, citation string }{
		{"matched", "fetched", "matched"}, {"mismatch", "fetched", "mismatch"},
		{"punctuation", "fetched", "mismatch"}, {"empty_quote", "fetched", "empty"},
		{"failed", "failed", "unavailable"}, {"empty_content", "unusable", "unavailable"},
		{"malformed", "unusable", "unavailable"}, {"absent", "absent", "unavailable"},
		{"pending", "pending", "unavailable"}, {"retry", "fetched", "matched"},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract := diagnosticsTestContract(t)
			var value map[string]any
			if err := json.Unmarshal([]byte(evidenceTestPayload), &value); err != nil {
				t.Fatal(err)
			}
			source := value["findings"].([]any)[0].(map[string]any)["sources"].([]any)[0].(map[string]any)
			// Spoofed model diagnostics must never survive normalization.
			source["fetch"], source["citation"] = "failed", "unavailable"
			records := evidenceTestRecords()
			switch test.name {
			case "mismatch":
				source["quote"] = "words never observed"
			case "punctuation":
				source["quote"] = "exact，words"
				records[1].Output = "{\"text\":\"exact,words\"}"
			case "empty_quote":
				source["quote"] = ""
			case "failed":
				records[1].Error = true
			case "empty_content":
				records[1].Output = "{\"text\":\"\"}"
			case "malformed":
				records[1].Output = "{"
			case "absent":
				records = nil
			case "pending":
				records = records[:1]
			case "retry":
				failed := evidenceTestRecords()
				failed[0].ToolCallID, failed[1].ToolCallID = "earlier", "earlier"
				failed[1].Error = true
				records = append(failed, records...)
			}
			submittedQuote := source["quote"]
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := validateStructuredResultPayload(contract, contract.ref(true), data, records)
			if err != nil {
				t.Fatal(err)
			}
			normalized, err := decodeResultContractJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			for pointer, want := range map[string]string{"/findings/0/sources/0/fetch": test.fetch, "/findings/0/sources/0/citation": test.citation} {
				if got := evidenceString(normalized, pointer); got != want {
					t.Fatalf("%s=%s want %s", pointer, got, want)
				}
			}
			if evidenceString(normalized, "/findings/0/sources/0/quote") != submittedQuote {
				t.Fatal("submitted quote lost")
			}
			matched := test.citation == "matched"
			if (payload.EvidenceDowngrades == 0) != matched {
				t.Fatalf("incorrect verdict: %s", payload.Value)
			}
			call := evidenceString(normalized, "/findings/0/sources/0/call_id")
			if (call == "observed") != (test.fetch == "fetched") {
				t.Fatalf("incorrect fetch binding: %s", payload.Value)
			}
		})
	}
}

func TestResultEvidenceCorroborationRequiresDistinctOriginsAndTargets(t *testing.T) {
	for _, name := range []string{"distinct", "same_origin", "same_target", "one_source"} {
		t.Run(name, func(t *testing.T) {
			contract := diagnosticsTestContract(t)
			contract.toolEvidence.Corroboration = &resultEvidenceCorroboration{GroupPointer: "/verdict", GroupValue: "supported", OriginPointer: "/origin", Minimum: 2}
			records := evidenceTestRecords()
			value, err := decodeResultContractJSON([]byte(evidenceTestPayload))
			if err != nil {
				t.Fatal(err)
			}
			group := value.(map[string]any)["findings"].([]any)[0].(map[string]any)
			sources := group["sources"].([]any)
			sources[0].(map[string]any)["origin"] = "origin-a"
			if name != "one_source" {
				key := "document-b"
				if name == "same_target" {
					key = "document-a"
				}
				origin := "origin-b"
				if name == "same_origin" {
					origin = "origin-a"
				}
				sources = append(sources, map[string]any{"key": key, "origin": origin, "state": "checked", "quote": "exact words"})
				group["sources"] = sources
				records = append(records, taskTranscriptRecord{Event: "tool_call", Tool: "read_document", ToolCallID: "second", Input: "{\"key\":\"" + key + "\"}"}, taskTranscriptRecord{Event: "tool_result", Tool: "read_document", ToolCallID: "second", Output: "{\"text\":\"exact words\"}"})
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			_, err = validateStructuredResultPayload(contract, contract.ref(true), data, records)
			if (err == nil) != (name == "distinct") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
