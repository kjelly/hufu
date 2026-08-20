package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/jsonrepair"
)

// errToolCallNotRepairable signals that neither the concatenated-JSON split
// nor the jsonrepair fallback could recover a tool call. Fantasy treats any
// non-nil error the same way (repair failed, keep the original validation
// error), so the message only matters for anyone reading logs.
var errToolCallNotRepairable = errors.New("tool call input is not a repairable JSON payload")

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
// This recovers the first complete top-level JSON value (the arguments that
// actually belong to the declared ToolName) and discards the orphaned
// remainder, logging what was dropped so the loss stays observable instead
// of silent. When the input isn't shaped like that specific corruption, it
// falls back to fantasy's own jsonrepair so we don't regress whatever that
// default fallback used to fix before a custom repair function was wired in.
func RepairConcatenatedToolCall(_ context.Context, opts fantasy.ToolCallRepairOptions) (*fantasy.ToolCallContent, error) {
	original := opts.OriginalToolCall

	if head, trailing, ok := splitLeadingJSONValue(original.Input); ok {
		log.Printf("warning: tool call %q (%s) arguments had a second tool call's JSON concatenated onto them; recovered the first %d bytes and dropped %d trailing bytes: %.200q",
			original.ToolCallID, original.ToolName, len(head), len(trailing), trailing)
		repaired := original
		repaired.Input = head
		repaired.Invalid = false
		repaired.ValidationError = nil
		return &repaired, nil
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
// raw substring (byte-for-byte, so no re-encoding risk) and the trailing
// substring that was discarded, which may itself contain further
// concatenated values.
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
