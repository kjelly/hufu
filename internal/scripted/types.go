// Package scripted contains transport-neutral scripted model responses.
// It has no dependency on the runtime or evaluation harness.
package scripted

type Step struct {
	Match     *Match    `json:"match,omitzero"`
	Content   string    `json:"content,omitempty"`
	ToolCall  *ToolCall `json:"tool_call,omitzero"`
	Forbidden bool      `json:"forbidden,omitzero"`
	// Nil means usage is unknown. Legacy scripts can omit this field; paired
	// experiments require its explicit presence, including explicit null.
	Usage *Usage `json:"usage"`
}

type Match struct {
	Contains string `json:"contains,omitempty"`
}

type ToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}
