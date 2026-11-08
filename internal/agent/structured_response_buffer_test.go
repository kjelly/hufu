package agent

import (
	"errors"
	"reflect"
	"testing"

	"charm.land/fantasy"
)

func TestStructuredResponseStreamBuffersOnlyUncommittedData(t *testing.T) {
	partial := fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, Delta: "partial reasoning"}
	transient := fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{TransientError: true}}
	permanent := fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New("invalid request")}
	finish := fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop}
	for _, tc := range []struct {
		name  string
		parts []fantasy.StreamPart
		limit int
		want  []fantasy.StreamPart
		err   error
	}{
		{name: "complete response", parts: []fantasy.StreamPart{partial, finish}, want: []fantasy.StreamPart{partial, finish}},
		{name: "discard transient partial", parts: []fantasy.StreamPart{partial, transient}, want: []fantasy.StreamPart{transient}},
		{name: "preserve permanent rejection", parts: []fantasy.StreamPart{partial, permanent}, want: []fantasy.StreamPart{partial, permanent}},
		{name: "provider effect", parts: []fantasy.StreamPart{partial, {Type: fantasy.StreamPartTypeToolCall, ToolCallName: StructuredResponseFunctionName, ProviderExecuted: true}, transient}, err: ErrStructuredResponseTool},
		{name: "foreign call", parts: []fantasy.StreamPart{partial, {Type: fantasy.StreamPartTypeToolInputStart, ToolCallName: "write"}, transient}, err: ErrStructuredResponseTool},
		{name: "tool result", parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolResult}, transient}, err: ErrStructuredResponseTool},
		{name: "bounded framing", parts: []fantasy.StreamPart{partial, partial, finish}, limit: 1024, err: errStructuredResponseBuffer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ended := false
			consumed := 0
			stream := func(yield func(fantasy.StreamPart) bool) {
				defer func() { ended = true }()
				for _, part := range tc.parts {
					consumed++
					if !yield(part) {
						return
					}
				}
			}
			limit := tc.limit
			if limit == 0 {
				limit = structuredResponseBufferLimit
			}
			call := fantasy.Call{Tools: []fantasy.Tool{fantasy.FunctionTool{Name: StructuredResponseFunctionName}}, ToolChoice: new(fantasy.SpecificToolChoice(StructuredResponseFunctionName))}
			var got []fantasy.StreamPart
			for part := range bufferStructuredResponseStream(call, stream, limit) {
				if part.Type != fantasy.StreamPartTypeError && !ended {
					t.Fatal("incomplete data response escaped the buffer")
				}
				got = append(got, part)
			}
			if !ended {
				t.Fatal("upstream iterator was not closed")
			}
			if tc.err != nil {
				if len(got) != 1 || !errors.Is(got[0].Error, tc.err) || consumed >= len(tc.parts) {
					t.Fatalf("got=%+v consumed=%d", got, consumed)
				}
			} else if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%+v want=%+v", got, tc.want)
			}
		})
	}
}
