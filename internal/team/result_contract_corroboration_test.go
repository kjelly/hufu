package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResultEvidenceCorroborationRequiresSafeFallback(t *testing.T) {
	for _, test := range []struct {
		name          string
		corroboration bool
		fallback      map[string]any
		wantError     bool
	}{
		{"missing", true, map[string]any{"/analysis": "missing evidence"}, true},
		{"unchanged", true, map[string]any{"/verdict": "supported"}, true},
		{"safe", true, map[string]any{"/verdict": "unverified"}, false},
		{"without_corroboration", false, map[string]any{"/analysis": "missing evidence"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var schema map[string]any
			if err := json.Unmarshal([]byte(evidenceTestSchema), &schema); err != nil {
				t.Fatal(err)
			}
			spec := schema["x-hufu-tool-evidence"].(map[string]any)
			spec["group_fallback"] = test.fallback
			if test.corroboration {
				spec["corroboration"] = resultEvidenceCorroboration{GroupPointer: "/verdict", GroupValue: "supported", OriginPointer: "/origin", Minimum: 2}
			}
			data, err := json.Marshal(schema)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			writeResultContractSchema(t, dir, "corroboration.json", string(data))
			_, err = compileResultContractSchema(dir, "corroboration.json")
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "corroboration") {
					t.Fatalf("unsafe corroboration fallback was not rejected: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResultEvidenceCorroborationCountsOnlyBoundSources(t *testing.T) {
	for _, binding := range []string{"legacy", "diagnostics"} {
		for _, fallback := range []string{"safe", "missing", "unchanged"} {
			for _, evidence := range []string{"verified", "partial", "absent", "mismatch"} {
				t.Run(binding+"/"+fallback+"/"+evidence, func(t *testing.T) {
					contract := diagnosticsTestContract(t)
					contract.toolEvidence.Corroboration = &resultEvidenceCorroboration{GroupPointer: "/verdict", GroupValue: "supported", OriginPointer: "/origin", Minimum: 2}
					if binding == "legacy" {
						contract.toolEvidence.Diagnostics = nil
					}
					// Bypass compilation to exercise the execution-time defense
					// independently of static rejection of unsafe fallbacks.
					switch fallback {
					case "missing":
						delete(contract.toolEvidence.GroupFallback, "/verdict")
					case "unchanged":
						contract.toolEvidence.GroupFallback["/verdict"] = "supported"
					}
					records := evidenceTestRecords()
					records = append(records,
						taskTranscriptRecord{Event: "tool_call", Tool: "read_document", ToolCallID: "second", Input: `{"key":"document-b"}`},
						taskTranscriptRecord{Event: "tool_result", Tool: "read_document", ToolCallID: "second", Output: `{"text":"exact words"}`},
					)
					value, err := decodeResultContractJSON([]byte(evidenceTestPayload))
					if err != nil {
						t.Fatal(err)
					}
					group := value.(map[string]any)["findings"].([]any)[0].(map[string]any)
					sources := group["sources"].([]any)
					sources[0].(map[string]any)["origin"] = "origin-a"
					second := map[string]any{"key": "document-b", "origin": "origin-b", "state": "checked", "quote": "exact words", "call_id": "forged"}
					group["sources"] = append(sources, second)
					switch evidence {
					case "partial":
						records = records[:2]
					case "absent":
						records = nil
					case "mismatch":
						second["quote"] = "words never observed"
					}
					data, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					payload, err := validateStructuredResultPayload(contract, contract.ref(true), data, records)
					if evidence != "verified" && fallback != "safe" {
						if err == nil || !strings.Contains(err.Error(), "corroboration") {
							t.Fatalf("unbound sources were not rejected by corroboration: %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					normalized, err := decodeResultContractJSON(payload.Value)
					if err != nil {
						t.Fatal(err)
					}
					wantVerdict, wantDowngrades := "supported", 0
					if evidence != "verified" {
						wantVerdict, wantDowngrades = "unverified", 1
					}
					if evidenceString(normalized, "/findings/0/verdict") != wantVerdict || payload.EvidenceDowngrades != wantDowngrades {
						t.Fatalf("unexpected corroboration verdict: %s (downgrades=%d)", payload.Value, payload.EvidenceDowngrades)
					}
				})
			}
		}
	}
}
