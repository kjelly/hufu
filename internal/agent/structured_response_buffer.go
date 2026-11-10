package agent

import (
	"encoding/json/v2"
	"errors"

	"charm.land/fantasy"
)

// Includes framing and reasoning as well as claims; the caller's smaller claims
// limit still applies. No incomplete attempt is visible to the response parser.
const structuredResponseBufferLimit = 32 << 20

var errStructuredResponseBuffer = errors.New("structured response stream exceeds its buffer limit")

func bufferStructuredResponseStream(call fantasy.Call, stream fantasy.StreamResponse, limit int) fantasy.StreamResponse {
	tool, ok := fantasy.FunctionTool{}, false
	if len(call.Tools) == 1 {
		tool, ok = call.Tools[0].(fantasy.FunctionTool)
	}
	if !ok || tool.Name != StructuredResponseFunctionName || call.ToolChoice == nil || *call.ToolChoice != fantasy.SpecificToolChoice(StructuredResponseFunctionName) {
		return stream
	}
	return func(yield func(fantasy.StreamPart) bool) {
		var parts []fantasy.StreamPart
		used := 0
		for part := range stream {
			// Never hide a provider side effect or an unexpected executable call
			// behind transient recovery, even when no claims have been published.
			if part.ProviderExecuted || part.Type == fantasy.StreamPartTypeToolResult ||
				(part.Type == fantasy.StreamPartTypeToolCall || part.Type == fantasy.StreamPartTypeToolInputStart) && part.ToolCallName != "" && part.ToolCallName != StructuredResponseFunctionName {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ErrStructuredResponseTool})
				return
			}
			if isRecoverableProviderRejection(part.Error) {
				// Discard only this uncommitted data response. The shared recovery
				// wrapper settles the invocation before admitting a bounded resend.
				yield(part)
				return
			}
			metadata, err := json.Marshal(part.ProviderMetadata)
			warnings, warningErr := json.Marshal(part.Warnings)
			used += 512 + len(part.ID) + len(part.Delta) + len(part.ToolCallName) + len(part.ToolCallInput) + len(part.URL) + len(part.Title) + len(metadata) + len(warnings)
			if err != nil || warningErr != nil || used > limit {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errStructuredResponseBuffer})
				return
			}
			parts = append(parts, part)
			if part.Type == fantasy.StreamPartTypeError {
				break
			}
		}
		for _, part := range parts {
			if !yield(part) {
				return
			}
		}
	}
}
