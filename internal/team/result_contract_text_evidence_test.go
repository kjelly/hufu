package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func textEvidenceTestSchema(t *testing.T, targetFromOutput bool) string {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(evidenceTestSchema), &schema); err != nil {
		t.Fatal(err)
	}
	spec := schema["x-hufu-tool-evidence"].(map[string]any)
	delete(spec, "tool")
	delete(spec, "output_pointer")
	spec["tools"] = []string{"source__read_document", "counter__read_document"}
	spec["output_format"] = "text"
	spec["text_pattern"] = `(?ms)^### Content\n(.*?)\n### End(?:\n|$)`
	if targetFromOutput {
		delete(spec, "input_pointer")
		spec["target_pattern"] = `(?m)^Document: (\S+)$`
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func textEvidenceTestContract(t *testing.T, targetFromOutput bool) *CompiledResultContract {
	t.Helper()
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "text.json", textEvidenceTestSchema(t, targetFromOutput))
	contract, err := compileResultContractSchema(dir, "text.json")
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func textEvidenceTestRecords() []taskTranscriptRecord {
	return []taskTranscriptRecord{
		{Event: "tool_call", Tool: "source__read_document", ToolCallID: "observed", Input: `{"key":"document-a"}`},
		{Event: "tool_result", Tool: "source__read_document", ToolCallID: "observed", Output: "Document: document-a\n### Content\nthe exact words in a document\n### End\n"},
	}
}

func TestResultContractTextEvidence(t *testing.T) {
	for _, targetFromOutput := range []bool{false, true} {
		compiled := textEvidenceTestContract(t, targetFromOutput)
		for _, name := range []string{"matched", "second_tool", "failed", "empty", "truncated", "metadata_only", "wrong_quote", "wrong_target", "wrong_tool", "cross_tool_id", "unpaired", "ambiguous_text", "ambiguous_target", "argument_target_mismatch", "duplicate_call", "duplicate_result"} {
			t.Run(name+map[bool]string{true: "_output_target", false: "_input_target"}[targetFromOutput], func(t *testing.T) {
				records := textEvidenceTestRecords()
				valid := name == "matched" || name == "second_tool"
				switch name {
				case "duplicate_call":
					records = append(records, records[0])
				case "duplicate_result":
					records = append(records, records[1])
				case "second_tool":
					records[0].Tool = "counter__read_document"
					records[1].Tool = records[0].Tool
				case "failed":
					records[1].Error = true
				case "empty":
					records[1].Output = ""
				case "truncated":
					records[1].Output = "Document: document-a\n### Content\nthe exact words"
				case "metadata_only":
					records[1].Output = "Document: document-a\nexact words\n### Content\nother words\n### End\n"
				case "wrong_quote":
					records[1].Output = strings.ReplaceAll(records[1].Output, "exact words", "other words")
				case "wrong_target":
					records[0].Input = `{"key":"document-b"}`
					records[1].Output = strings.ReplaceAll(records[1].Output, "document-a", "document-b")
				case "wrong_tool":
					records[0].Tool = "search"
					records[1].Tool = "search"
				case "cross_tool_id":
					records[1].Tool = "counter__read_document"
				case "unpaired":
					records = records[1:]
				case "ambiguous_text":
					records[1].Output += "### Content\nexact words\n### End\n"
				case "ambiguous_target":
					records[1].Output += "Document: document-a\n"
					valid = !targetFromOutput
				case "argument_target_mismatch":
					records[0].Input = `{"key":"document-b"}`
					valid = targetFromOutput
				}
				payload, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(evidenceTestPayload), records)
				if err != nil {
					t.Fatal(err)
				}
				value, err := decodeResultContractJSON(payload.Value)
				if err != nil {
					t.Fatal(err)
				}
				if (payload.EvidenceDowngrades == 0) != valid {
					t.Fatalf("valid=%v payload=%s", valid, payload.Value)
				}
				if valid && evidenceString(value, "/findings/0/sources/0/call_id") != "observed" {
					t.Fatal("model call ID was not replaced")
				}
			})
		}
	}
}

func TestResultContractTextEvidenceConfiguration(t *testing.T) {
	for _, name := range []string{"invalid_format", "json_output_pointer", "invalid_pattern", "no_capture", "two_captures", "empty_tools", "duplicate_tools", "two_tools_fields", "ambiguous_target_fields"} {
		t.Run(name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal([]byte(textEvidenceTestSchema(t, true)), &root); err != nil {
				t.Fatal(err)
			}
			spec := root["x-hufu-tool-evidence"].(map[string]any)
			switch name {
			case "invalid_format":
				spec["output_format"] = "html"
			case "json_output_pointer":
				spec["output_pointer"] = "/text"
			case "invalid_pattern":
				spec["text_pattern"] = "("
			case "no_capture":
				spec["target_pattern"] = "Document"
			case "two_captures":
				spec["text_pattern"] = "(a)(b)"
			case "empty_tools":
				spec["tools"] = []string{}
			case "duplicate_tools":
				spec["tools"] = []string{"source__read_document", "source__read_document"}
			case "two_tools_fields":
				spec["tool"] = "source__read_document"
			case "ambiguous_target_fields":
				spec["input_pointer"] = "/key"
			}
			if _, _, err := compileResultContractExtensions(root); err == nil {
				t.Fatal("invalid evidence configuration accepted")
			}
		})
	}
}

func TestResultContractTextEvidenceWithoutSectionPattern(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(textEvidenceTestSchema(t, false)), &root); err != nil {
		t.Fatal(err)
	}
	delete(root["x-hufu-tool-evidence"].(map[string]any), "text_pattern")
	spec, _, err := compileResultContractExtensions(root)
	if err != nil {
		t.Fatal(err)
	}
	records := textEvidenceTestRecords()
	records[1].Output = "Raw exact words from a document"
	observations := matchingToolObservations(spec, records)
	if len(observations) != 1 || observations[0].output != records[1].Output {
		t.Fatalf("raw text observations=%#v", observations)
	}
}
