package agent

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"image"
	"image/png"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/media"
)

func historyReferenceParts(t *testing.T) []fantasy.MessagePart {
	t.Helper()
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	ref, err := media.Validate(t.Context(), imageBytes.Bytes(), media.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := media.Digest("fixture media policy")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := media.InputSnapshot{RunID: "run", BranchID: "main", InvocationID: "invocation", TurnID: "turn", PolicyHash: policy,
		Input: media.TurnInput{Parts: []media.Part{{Type: "text", Text: "compare"}, {Type: "image", MediaID: ref.ID}, {Type: "image", MediaID: ref.ID}}}, Refs: []media.Ref{ref}}
	for _, index := range []int{1, 2} {
		snapshot.Occurrences = append(snapshot.Occurrences, media.Occurrence{RunID: snapshot.RunID, BranchID: snapshot.BranchID, InvocationID: snapshot.InvocationID, TurnID: snapshot.TurnID, PartIndex: index, MediaID: ref.ID, Origin: "user_file"})
	}
	snapshot, err = media.SealSnapshot(snapshot, media.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	parts := []fantasy.MessagePart{fantasy.TextPart{Text: "compare"}}
	for _, index := range []int{1, 2} {
		parts = append(parts, MediaReferencePart{Reference: media.HistoryReference{Version: media.Version, Source: media.Selection{SnapshotID: snapshot.ID, SnapshotHash: snapshot.Hash, TurnID: snapshot.TurnID, PartIndex: index}, Ref: ref, PolicyHash: policy, BindingHash: snapshot.Hash}})
	}
	return parts
}

func TestMediaHistoryCodecPreservesReferencesAndToolParts(t *testing.T) {
	messages := []fantasy.Message{
		{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ReasoningPart{Text: "reason"}, fantasy.ToolCallPart{ToolCallID: "call", ToolName: "lookup", Input: `{}`}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call", Output: fantasy.ToolResultOutputContentText{Text: "result"}}}},
	}
	encoded, err := EncodeMediaHistory(messages, "fixture-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"filename"`)) || bytes.Contains(encoded, []byte("data:image/")) || !bytes.Contains(encoded, []byte(`"media_ref"`)) {
		t.Fatal("history did not use the inert reference representation")
	}
	restored, checkpointID, err := DecodeMediaHistory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointID != "fixture-checkpoint" {
		t.Fatal("projection lost its canonical checkpoint identity")
	}
	if !reflect.DeepEqual(messages, restored) {
		t.Fatal("history round trip lost ordered refs or SDK tool parts")
	}
	first := restored[0].Content[1].(MediaReferencePart)
	second := restored[0].Content[2].(MediaReferencePart)
	if first.Reference.Ref != second.Reference.Ref || first.Reference.Source.PartIndex == second.Reference.Source.PartIndex {
		t.Fatal("repeated media occurrence was coalesced")
	}
}

func TestPersistentMessagesRejectRawMediaBeforeEncodingAndDecoding(t *testing.T) {
	for _, part := range []fantasy.MessagePart{
		fantasy.FilePart{Filename: "private.png", Data: []byte("raw sentinel"), MediaType: "image/png"},
		&fantasy.FilePart{Data: []byte("raw sentinel"), MediaType: "text/plain"},
		fantasy.ToolResultPart{ToolCallID: "call", Output: fantasy.ToolResultOutputContentMedia{Data: "raw sentinel", MediaType: "image/png"}},
	} {
		_, err := ValidatePersistentMessages([]fantasy.Message{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{part}}})
		if e, ok := errors.AsType[*media.Error](err); !ok || e.Code != media.Unsupported {
			t.Fatalf("raw part: %v", err)
		}
	}
	for _, part := range []string{
		`{"type":"file","data":{"filename":"private.png","data":"INVALID BASE64!","media_type":"image/png"}}`,
		`{"type":"tool-result","data":{"tool_call_id":"call","output":{"type":"media","data":{"data":"INVALID BASE64!","media_type":"image/png"}}}}`,
	} {
		_, err := DecodePersistentMessages([]byte(`[{"role":"user","content":[` + part + `],"provider_options":null}]`))
		if e, ok := errors.AsType[*media.Error](err); !ok || e.Code != media.Unsupported {
			t.Fatalf("raw media reached SDK decoding: %v", err)
		}
	}
}

func TestMediaHistoryCodecRejectsMalformedReferences(t *testing.T) {
	part := historyReferenceParts(t)[1].(MediaReferencePart)
	encoded, err := json.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		strings.Replace(string(encoded), `"version":1`, `"version":99`, 1),
		strings.Replace(string(encoded), `"data":{`, `"data":{"data_uri":"raw sentinel",`, 1),
		strings.Replace(string(encoded), `"type":"media_ref"`, `"type":"media_ref","type":"media_ref"`, 1),
	} {
		if _, err := DecodeMediaReferencePart([]byte(data)); err == nil {
			t.Fatal("invalid reference envelope accepted")
		}
	}
}

func TestProviderSerializationRejectsUnresolvedMediaReferences(t *testing.T) {
	messages := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}}
	for _, request := range []ProviderRequest{
		{Messages: messages}, {Call: &fantasy.Call{Prompt: messages}}, {ObjectCall: &fantasy.ObjectCall{Prompt: messages}},
	} {
		if !ProviderRequestHasMedia(request) {
			t.Fatal("media reference bypassed request detection")
		}
		if _, err := SerializeProviderRequest(request); err == nil {
			t.Fatal("unresolved image was silently dropped by SDK serialization")
		}
	}
}
