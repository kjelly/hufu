package team

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"charm.land/fantasy"
)

// RepairGenerationDiagnostic preserves provider termination and usage without
// persisting generated reasoning or duplicating source text. A zero value means
// no completed generation was observed, including legacy receipts.
type RepairGenerationDiagnostic struct {
	Steps           int    `json:"steps"`
	FinishReason    string `json:"finish_reason"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	TextBytes       int    `json:"text_bytes"`
	ReasoningBytes  int    `json:"reasoning_bytes"`
	ToolCalls       int    `json:"tool_calls"`
}

// UnmarshalJSON accepts the legacy redaction marker as unavailable telemetry.
// Earlier writers redacted reasoning_tokens even though it is a numeric count.
// Other strings remain invalid; this field is never authorization evidence.
func (d *RepairGenerationDiagnostic) UnmarshalJSON(data []byte) error {
	type diagnostic RepairGenerationDiagnostic
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["reasoning_tokens"]; ok {
		var marker string
		if err := json.Unmarshal(raw, &marker); err == nil && marker == "[REDACTED]" {
			fields["reasoning_tokens"] = jsontext.Value("0")
		}
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var decoded diagnostic
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		return err
	}
	*d = RepairGenerationDiagnostic(decoded)
	return nil
}

func protocolRepairGenerationDiagnostic(steps []fantasy.StepResult) RepairGenerationDiagnostic {
	var diagnostic RepairGenerationDiagnostic
	for _, step := range steps {
		diagnostic.Steps++
		diagnostic.FinishReason = string(step.FinishReason)
		if diagnostic.FinishReason == "" {
			diagnostic.FinishReason = string(fantasy.FinishReasonUnknown)
		}
		diagnostic.InputTokens += step.Usage.InputTokens
		diagnostic.OutputTokens += step.Usage.OutputTokens
		diagnostic.ReasoningTokens += step.Usage.ReasoningTokens
		diagnostic.ToolCalls += len(step.Content.ToolCalls())
		for _, part := range step.Content {
			switch content := part.(type) {
			case fantasy.TextContent:
				diagnostic.TextBytes += len(content.Text)
			case fantasy.ReasoningContent:
				diagnostic.ReasoningBytes += len(content.Text)
			}
		}
	}
	return diagnostic
}
