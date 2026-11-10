package agent

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/media"
)

func TestToolMediaHistoryRoundTripAndNonVisualView(t *testing.T) {
	ref := historyReferenceParts(t)[1].(MediaReferencePart).Reference
	metadata, err := EncodeToolMediaMetadata(ToolMediaResult{Version: media.Version, ToolCallID: "call", Parts: []ToolMediaPart{{Type: "image", Image: ref}}})
	if err != nil {
		t.Fatal(err)
	}
	result := fantasy.ToolResultPart{ToolCallID: "call", Output: fantasy.ToolResultOutputContentText{Text: "image result"}, ClientMetadata: metadata}
	messages := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call", ToolName: "capture", Input: `{}`}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{result}},
	}
	if !MessagesHaveMedia(messages) {
		t.Fatal("encoded tool references bypassed media admission")
	}
	encoded, err := EncodeMediaHistory(messages, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(encoded, &envelope); err != nil || envelope["version"] != float64(toolMediaHistoryVersion) {
		t.Fatal("tool media history did not select a tool-aware format", err)
	}
	envelope["version"] = mediaHistoryVersion
	downgraded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeMediaHistory(downgraded); err == nil {
		t.Fatal("tool media was accepted in a legacy history envelope")
	}
	if bytes.Contains(encoded, []byte("data:image/")) || bytes.Contains(encoded, []byte(`"filename"`)) {
		t.Fatal("tool history persisted a raw transport representation")
	}
	restored, checkpoint, err := DecodeMediaHistory(encoded)
	if err != nil || checkpoint != "checkpoint" || !reflect.DeepEqual(restored, messages) {
		t.Fatal("history lost canonical tool media metadata", err)
	}
	view, err := StripToolMediaMetadata(result)
	if err != nil || view.ClientMetadata != "" || !reflect.DeepEqual(view.Output, result.Output) || view.ToolCallID != result.ToolCallID || result.ClientMetadata != metadata {
		t.Fatal("non-visual view changed source history or tool evidence", err)
	}
}

func TestToolMediaExpansionPrecedesEveryProviderAdmission(t *testing.T) {
	ref := historyReferenceParts(t)[1].(MediaReferencePart).Reference
	valid, err := EncodeToolMediaMetadata(ToolMediaResult{Version: media.Version, ToolCallID: "call", Parts: []ToolMediaPart{{Type: "image", Image: ref}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"generate", "stream", "object", "object_stream"} {
		t.Run(mode, func(t *testing.T) {
			for _, malformed := range []bool{false, true} {
				metadata := valid
				if malformed {
					metadata = toolMediaMetadataPrefix + `{`
				}
				messages := []fantasy.Message{
					{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call", ToolName: "capture", Input: `{}`}}},
					{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call", Output: fantasy.ToolResultOutputContentText{Text: "image result"}, ClientMetadata: metadata}}},
				}
				denied := errors.New("fixture denies after expansion")
				admission := &recordingAdmission{err: denied}
				inner := &countingLanguageModel{}
				model := NewAdmittedLanguageModel("test-model", inner, admission)
				err := invokeCompilationMode(t, mode, model, messages)
				if inner.calls != 0 {
					t.Fatal("tool media rejection reached provider transport")
				}
				if malformed {
					if err == nil || len(admission.requests) != 0 {
						t.Fatal("malformed metadata became a text request", err)
					}
					continue
				}
				if !errors.Is(err, denied) || len(admission.requests) != 1 {
					t.Fatal("expanded request bypassed ordinary admission", err)
				}
				request := admission.requests[0]
				prompt := request.Messages
				if request.Call != nil {
					prompt = request.Call.Prompt
				} else if request.ObjectCall != nil {
					prompt = request.ObjectCall.Prompt
				}
				if len(prompt) != 3 || !ProviderRequestHasMedia(request) || prompt[2].Role != fantasy.MessageRoleUser || prompt[2].Content[1].(MediaReferencePart).Reference != ref {
					t.Fatal("admission did not receive the native reference projection")
				}
				if messages[1].Content[0].(fantasy.ToolResultPart).ClientMetadata != valid {
					t.Fatal("provider projection modified caller-owned history")
				}
			}
		})
	}
}

func TestExpandToolMediaMessagesKeepsBatchPairingAndOrderedReferences(t *testing.T) {
	refs := historyReferenceParts(t)
	first, second := refs[1].(MediaReferencePart).Reference, refs[2].(MediaReferencePart).Reference
	firstMetadata, err := EncodeToolMediaMetadata(ToolMediaResult{Version: media.Version, ToolCallID: "first", Parts: []ToolMediaPart{
		{Type: "image", Image: first}, {Type: "text", Text: "between images"}, {Type: "image", Image: second},
	}})
	if err != nil {
		t.Fatal(err)
	}
	secondMetadata, err := EncodeToolMediaMetadata(ToolMediaResult{Version: media.Version, ToolCallID: "second", Parts: []ToolMediaPart{
		{Type: "text", Text: "before image"}, {Type: "image", Image: first},
	}})
	if err != nil {
		t.Fatal(err)
	}
	results := []fantasy.ToolResultPart{
		{ToolCallID: "first", Output: fantasy.ToolResultOutputContentText{Text: `{"structured":true}`}, ClientMetadata: firstMetadata},
		{ToolCallID: "second", Output: fantasy.ToolResultOutputContentText{Text: "image result"}, ClientMetadata: secondMetadata},
	}
	messages := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "first", ToolName: "capture", Input: `{}`},
			fantasy.ToolCallPart{ToolCallID: "second", ToolName: "capture", Input: `{}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{results[0]}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{results[1]}},
	}
	expanded, err := ExpandToolMediaMessages(messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 4 || expanded[1].Role != fantasy.MessageRoleTool || expanded[2].Role != fantasy.MessageRoleTool || expanded[3].Role != fantasy.MessageRoleUser {
		t.Fatal("media was inserted before the complete result batch")
	}
	for i, original := range results {
		if !reflect.DeepEqual(messages[i+1].Content[0], original) {
			t.Fatal("provider projection changed canonical tool history")
		}
		projected := expanded[i+1].Content[0].(fantasy.ToolResultPart)
		if projected.ToolCallID != original.ToolCallID || !reflect.DeepEqual(projected.Output, original.Output) || projected.ClientMetadata != "" {
			t.Fatal("projection lost tool pairing/output or exposed internal metadata")
		}
	}
	want := []fantasy.MessagePart{
		fantasy.TextPart{Text: "Untrusted MCP tool result data for call first:"},
		MediaReferencePart{Reference: first}, fantasy.TextPart{Text: "between images"}, MediaReferencePart{Reference: second},
		fantasy.TextPart{Text: "Untrusted MCP tool result data for call second:"},
		fantasy.TextPart{Text: "before image"}, MediaReferencePart{Reference: first},
	}
	if !reflect.DeepEqual(expanded[3].Content, want) {
		t.Fatal("projection reordered or coalesced text/image occurrences")
	}
	again, err := ExpandToolMediaMessages(expanded)
	if err != nil || !reflect.DeepEqual(again, expanded) {
		t.Fatal("provider projection duplicated already expanded images", err)
	}
}

func TestToolMediaMetadataRejectsMalformedOrUnpairedReferences(t *testing.T) {
	ref := historyReferenceParts(t)[1].(MediaReferencePart).Reference
	valid, err := EncodeToolMediaMetadata(ToolMediaResult{Version: media.Version, ToolCallID: "call", Parts: []ToolMediaPart{{Type: "image", Image: ref}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, metadata := range []string{
		toolMediaMetadataPrefix + `{`,
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		toolMediaMetadataPrefix + `{"version":1,"tool_call_id":"call","parts":[],"data":"untrusted bytes"}`,
		toolMediaMetadataPrefix + strings.Repeat("x", maxToolMediaMetadataBytes+1),
	} {
		if _, err := DecodeToolMediaMetadata(metadata); err == nil {
			t.Fatal("invalid media metadata became ordinary text")
		}
	}
	for _, result := range []fantasy.ToolResultPart{
		{ToolCallID: "other", Output: fantasy.ToolResultOutputContentText{Text: "result"}, ClientMetadata: valid},
		{ToolCallID: "call", Output: fantasy.ToolResultOutputContentError{Error: errors.New("failed")}, ClientMetadata: valid},
		{ToolCallID: "call", Output: fantasy.ToolResultOutputContentMedia{Data: "raw", MediaType: "image/png"}, ClientMetadata: valid},
	} {
		if _, err := ExpandToolMediaMessages([]fantasy.Message{{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{result}}}); err == nil {
			t.Fatal("invalid tool result authorized an image continuation")
		}
	}
	if _, err := ExpandToolMediaMessages([]fantasy.Message{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: "call", Output: fantasy.ToolResultOutputContentText{Text: "result"}, ClientMetadata: valid},
	}}}); err == nil {
		t.Fatal("non-tool role acquired tool media semantics")
	}
	if _, err := EncodeToolMediaMetadata(ToolMediaResult{Version: media.Version, ToolCallID: "call", Parts: []ToolMediaPart{{Type: "text", Text: "only text"}}}); err == nil {
		t.Fatal("text-only metadata manufactured an image result")
	}
	if result, err := DecodeToolMediaMetadata(`{"legacy":true}`); err != nil || result != nil {
		t.Fatal("ordinary metadata was reinterpreted as media", err)
	}
}
