package agent

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestProviderRecoveryHandlesSDKDeferredHTTPStreamRejection(t *testing.T) {
	for _, object := range []bool{false, true} {
		name := "stream"
		if object {
			name = "object stream"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				response := &http.Response{Header: make(http.Header)}
				if calls == 1 {
					response.StatusCode = 429
					response.Header.Set("Content-Type", "application/json")
					response.Header.Set("Retry-After", "600")
					response.Body = io.NopCloser(strings.NewReader(`{"error":{"message":"quota exhausted","type":"rate_limit_exceeded"}}`))
				} else {
					response.StatusCode = 200
					response.Header.Set("Content-Type", "text/event-stream")
					response.Body = io.NopCloser(strings.NewReader("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"{}\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
				}
				if calls > 1 && object {
					response.Body = io.NopCloser(strings.NewReader(sseToolChunk(sseToolCall(0, "obj", "response", `{}`)) + "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" + sseDone))
				}

				return response, nil
			})}
			provider, err := newOpenAICompatProvider("https://ollama.com/v1", "test-key", "local", client)
			if err != nil {
				t.Fatal(err)
			}
			inner, err := provider.LanguageModel(t.Context(), "test")
			if err != nil {
				t.Fatal(err)
			}
			limiter := newCapacityLimiter(1)
			admission := &recoveryAdmission{settlingAdmission: &settlingAdmission{recordingAdmission: &recordingAdmission{limiter: limiter}}, limiter: limiter, t: t}
			model := NewAdmittedLanguageModel("test", inner, admission)
			if object {
				stream, err := model.StreamObject(t.Context(), fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("answer")}, Schema: fantasy.Schema{Type: "object"}})
				if err != nil {
					t.Fatal(err)
				}
				for part := range stream {
					if part.Error != nil {
						t.Fatal(part.Error)
					}
				}
			} else {
				stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("answer")}})
				if err != nil {
					t.Fatal(err)
				}
				for part := range stream {
					if part.Error != nil {
						t.Fatal(part.Error)
					}
				}
			}
			if calls != 2 || admission.recoveries != 1 || len(admission.snapshotSettlements()) != 2 || limiter.active.Load() != 0 {
				t.Fatalf("HTTP calls=%d recoveries=%d settlements=%d", calls, admission.recoveries, len(admission.snapshotSettlements()))
			}
		})
	}
}
