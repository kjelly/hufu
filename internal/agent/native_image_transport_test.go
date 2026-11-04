package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

type nativeImageWirePart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type nativeImageWireMessage struct {
	Role       string         `json:"role"`
	Content    jsontext.Value `json:"content"`
	ToolCallID string         `json:"tool_call_id"`
}

type nativeImageWireRequest struct {
	Messages []nativeImageWireMessage `json:"messages"`
}

func nativeImageFixture(t *testing.T, red uint8) fantasy.FilePart {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.SetRGBA(0, 0, color.RGBA{R: red, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return fantasy.FilePart{Filename: "fixture.png", MediaType: "image/png", Data: data.Bytes()}
}

func nativeImageModel(t *testing.T, server *httptest.Server) fantasy.LanguageModel {
	t.Helper()
	provider, err := newOpenAICompatProvider(server.URL+"/v1", "fixture-key", "local", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.LanguageModel(t.Context(), "fixture-vision")
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func captureNativeImageRequest(w http.ResponseWriter, r *http.Request, requests chan<- nativeImageWireRequest) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "unexpected provider endpoint", http.StatusBadRequest)
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var request nativeImageWireRequest
	if err != nil || json.Unmarshal(raw, &request) != nil {
		http.Error(w, "invalid bounded request", http.StatusBadRequest)
		return false
	}
	select {
	case requests <- request:
		return true
	default:
		http.Error(w, "unexpected extra provider request", http.StatusBadRequest)
		return false
	}
}

func nativeImageWireParts(t *testing.T, message nativeImageWireMessage) []nativeImageWirePart {
	t.Helper()
	raw := bytes.TrimSpace(message.Content)
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			t.Fatal(err)
		}
		return []nativeImageWirePart{{Type: "text", Text: text}}
	}
	var parts []nativeImageWirePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatal(err)
	}
	return parts
}

func assertNativeImageWire(t *testing.T, part nativeImageWirePart, image fantasy.FilePart) {
	t.Helper()
	prefix := "data:" + image.MediaType + ";base64,"
	encoded, ok := strings.CutPrefix(part.ImageURL.URL, prefix)
	if part.Type != "image_url" || !ok {
		t.Fatal("native image part or MIME is missing")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(decoded) != sha256.Sum256(image.Data) {
		t.Fatal("wire image bytes differ from the source fixture")
	}
}

func TestNativeImageStreamInput(t *testing.T) {
	first, second := nativeImageFixture(t, 32), nativeImageFixture(t, 224)
	imageMessage := fantasy.NewUserMessage("Compare", first, second, first)
	for _, tc := range []struct {
		name         string
		call         fantasy.AgentStreamCall
		wantMessages int
		images       []fantasy.FilePart
		wantError    bool
	}{
		{name: "text-only prompt", call: fantasy.AgentStreamCall{Prompt: "Compare"}, wantMessages: 1},
		{name: "ordered and repeated images in messages", call: fantasy.AgentStreamCall{Messages: []fantasy.Message{imageMessage}}, wantMessages: 1, images: []fantasy.FilePart{first, second, first}},
		{name: "prompt appends after messages", call: fantasy.AgentStreamCall{Prompt: "Follow up", Messages: []fantasy.Message{imageMessage}}, wantMessages: 2, images: []fantasy.FilePart{first, second, first}},
		{name: "files require nonempty prompt", call: fantasy.AgentStreamCall{Messages: []fantasy.Message{imageMessage}, Files: []fantasy.FilePart{first}}, wantError: true},
		{name: "empty turn", call: fantasy.AgentStreamCall{}, wantError: true},
		{name: "empty prompt cannot end with assistant", call: fantasy.AgentStreamCall{Messages: []fantasy.Message{{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "previous reply"}}}}}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan nativeImageWireRequest, 2)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if !captureNativeImageRequest(w, r, requests) {
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-vision\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-vision\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(server.Close)
			ag := fantasy.NewAgent(nativeImageModel(t, server), fantasy.WithMaxRetries(0), fantasy.WithStopConditions(fantasy.StepCountIs(1)))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err := ag.Stream(ctx, tc.call)
			if (err != nil) != tc.wantError {
				t.Fatalf("stream error presence=%t, want %t: %v", err != nil, tc.wantError, err)
			}
			if tc.wantError {
				if calls.Load() != 0 {
					t.Fatal("invalid SDK input reached the provider")
				}
				return
			}
			if calls.Load() != 1 {
				t.Fatalf("provider calls=%d, want 1", calls.Load())
			}
			request := <-requests
			if len(request.Messages) != tc.wantMessages {
				t.Fatalf("messages=%d, want %d", len(request.Messages), tc.wantMessages)
			}
			for _, message := range request.Messages {
				if message.Role != "user" {
					t.Fatalf("unexpected role %q", message.Role)
				}
			}
			parts := nativeImageWireParts(t, request.Messages[0])
			if len(parts) != 1+len(tc.images) || parts[0].Type != "text" || parts[0].Text != "Compare" {
				t.Fatal("user part order or text changed")
			}
			for i, image := range tc.images {
				assertNativeImageWire(t, parts[i+1], image)
			}
			if tc.wantMessages == 2 {
				followup := nativeImageWireParts(t, request.Messages[1])
				if len(followup) != 1 || followup[0].Text != "Follow up" {
					t.Fatal("Prompt was not appended after Messages")
				}
			}
		})
	}
}

func TestNativeImageToolResultReachesNextProviderTurn(t *testing.T) {
	image := nativeImageFixture(t, 127)
	requests := make(chan nativeImageWireRequest, 3)
	var calls, effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !captureNativeImageRequest(w, r, requests) {
			return
		}
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","created":1,"model":"fixture-vision","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"capture-1","type":"function","function":{"name":"capture","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","created":1,"model":"fixture-vision","choices":[{"index":0,"message":{"role":"assistant","content":"received"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(server.Close)
	tool := fantasy.NewAgentTool("capture", "Return an image fixture", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		effects.Add(1)
		return fantasy.NewImageResponse(image.Data, image.MediaType), nil
	})
	ag := fantasy.NewAgent(nativeImageModel(t, server), fantasy.WithTools(tool), fantasy.WithMaxRetries(0), fantasy.WithStopConditions(fantasy.StepCountIs(2)))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := ag.Generate(ctx, fantasy.AgentCall{Prompt: "Capture and inspect"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || effects.Load() != 1 {
		t.Fatalf("provider=%d tool=%d, want 2 and 1", calls.Load(), effects.Load())
	}
	<-requests
	next := <-requests
	if len(next.Messages) != 4 {
		t.Fatalf("follow-up message count=%d, want 4", len(next.Messages))
	}
	for i, role := range []string{"user", "assistant", "tool", "user"} {
		if next.Messages[i].Role != role {
			t.Fatalf("message %d role=%q, want %q", i, next.Messages[i].Role, role)
		}
	}
	if next.Messages[2].ToolCallID != "capture-1" {
		t.Fatal("tool result lost its call identity")
	}
	parts := nativeImageWireParts(t, next.Messages[3])
	if len(parts) != 1 {
		t.Fatalf("follow-up media count=%d, want 1", len(parts))
	}
	assertNativeImageWire(t, parts[0], image)
}
