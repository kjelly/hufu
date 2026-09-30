package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/jsonrepair"
)

// errToolCallNotRepairable signals that the jsonrepair fallback could not
// recover a single malformed JSON value. Fantasy treats any non-nil error the
// same way (repair failed, keep the original validation error), so the message
// only matters for anyone reading logs.
var errToolCallNotRepairable = errors.New("tool call input is not a repairable JSON payload")

// errMultipleToolCallValues is deliberately terminal. A second top-level
// value may belong to another tool call, so selecting either value would
// silently change the model's request and could execute the wrong operation.
var errMultipleToolCallValues = errors.New("tool call input contains multiple JSON values")

// RepairConcatenatedToolCall is a fantasy.RepairToolCallFunction. Some
// OpenAI-compatible streaming backends key parallel tool-call argument deltas
// by a numeric index rather than the call ID; when two tool calls collide on
// that index, their argument fragments get appended into the same buffer
// with no separator, producing input like `{"a":1}{"b":2}`. Fantasy's own
// json.Unmarshal validation rejects that outright ("invalid character '{'
// after top-level value"), and its default jsonrepair fallback turns
// multiple top-level values into a JSON array, which still fails to
// unmarshal into the expected object — so neither recovers the call.
//
// Concatenated top-level values are rejected rather than repaired: the tool
// boundary cannot prove which object belongs to the declared ToolName. When
// the input is a single malformed JSON value, the function still falls back
// to fantasy's jsonrepair for its bounded syntax-only repairs.
func RepairConcatenatedToolCall(_ context.Context, opts fantasy.ToolCallRepairOptions) (*fantasy.ToolCallContent, error) {
	original := opts.OriginalToolCall

	if _, _, ok := splitLeadingJSONValue(original.Input); ok {
		return nil, errMultipleToolCallValues
	}

	if repaired, err := jsonrepair.RepairJSON(original.Input); err == nil && repaired != original.Input {
		repairedCall := original
		repairedCall.Input = repaired
		repairedCall.Invalid = false
		repairedCall.ValidationError = nil
		return &repairedCall, nil
	}

	return nil, errToolCallNotRepairable
}

// splitLeadingJSONValue reports whether input is one complete top-level JSON
// value immediately followed by at least one more, independently valid JSON
// value with no separator between them — the exact signature left behind
// when a streaming provider concatenates two (or more) parallel tool calls'
// argument deltas into one buffer. On success it returns the first value's
// raw substring and the trailing substring. Callers must reject the whole
// payload; the split is detection only and never authorizes either value.
func splitLeadingJSONValue(input string) (head string, trailing string, ok bool) {
	dec := json.NewDecoder(strings.NewReader(input))
	var first any
	if err := dec.Decode(&first); err != nil {
		return "", "", false
	}

	offset := dec.InputOffset()
	if offset <= 0 || int(offset) > len(input) {
		return "", "", false
	}

	rest := strings.TrimSpace(input[offset:])
	if rest == "" {
		return "", "", false
	}

	// Continue decoding on the same stream (rather than re-parsing rest in
	// isolation) so a third or later concatenated value doesn't defeat
	// detection of the second one.
	var second any
	if err := dec.Decode(&second); err != nil {
		return "", "", false
	}

	return input[:offset], rest, true
}
