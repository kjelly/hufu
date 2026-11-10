package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestProviderRecoveryBoundsDeferredTransientErrors(t *testing.T) {
	for _, object := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			failures int
			partial  bool
			mixed    bool
			wantErr  bool
		}{
			{name: "one rejection", failures: 1},
			{name: "two rejections", failures: 2},
			{name: "persistent rejection", failures: 10, wantErr: true},
			{name: "shared opening and iterator limit", failures: 10, mixed: true, wantErr: true},
			{name: "observed content", failures: 1, partial: true, wantErr: true},
		} {
			t.Run(fmt.Sprintf("%s/object_%t", tc.name, object), func(t *testing.T) {
				var requests [][]byte
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					requests = append(requests, body)
					if tc.mixed && len(requests)%2 == 1 {
						return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture server rejection","type":"api_error"}}`))}, nil
					}
					data := sseToolChunk(sseToolCall(0, "result", "response", `{"ok":true}`))
					if !object {
						data = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n"
					}
					if len(requests) <= tc.failures {
						if !tc.partial {
							data = ""
						}
						data += "data: {\"error\":{\"message\":\"Internal Server Error\",\"type\":\"api_error\"}}\n\n"
					} else {
						data += "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(data + sseDone))}, nil
				})}
				provider, err := newOpenAICompatProvider("https://provider.invalid/v1", "", "fixture", client)
				if err != nil {
					t.Fatal(err)
				}
				inner, err := provider.LanguageModel(t.Context(), "test")
				if err != nil {
					t.Fatal(err)
				}
				limiter := newCapacityLimiter(1)
				base := &recordingAdmission{limiter: limiter}
				admission := &recoveryAdmission{settlingAdmission: &settlingAdmission{recordingAdmission: base}, limiter: limiter, t: t, allowTransient: true}
				model := NewAdmittedLanguageModel("test", inner, admission)
				invoke := func(ctx context.Context) error {
					if object {
						stream, err := model.StreamObject(ctx, fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("answer")}, Schema: fantasy.Schema{Type: "object"}})
						if err != nil {
							return err
						}
						for part := range stream {
							if part.Error != nil {
								return part.Error
							}
						}
						return nil
					}
					stream, err := model.Stream(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("answer")}})
					if err != nil {
						return err
					}
					for part := range stream {
						if part.Error != nil {
							return part.Error
						}
					}
					return nil
				}
				err = invoke(t.Context())
				if (err != nil) != tc.wantErr {
					t.Fatalf("error=%v", err)
				}
				if tc.wantErr {
					failure, ok := errors.AsType[*fantasy.ProviderError](err)
					if !ok || !IsTransientProviderError(err) || !tc.mixed && (!failure.TransientError || failure.StatusCode != 0) || tc.mixed && failure.StatusCode != 500 {
						t.Fatalf("lost SDK SSE metadata: %v", err)
					}
				}
				wantCalls := min(tc.failures+1, ProviderTransientRetryLimit+1)
				if tc.partial {
					wantCalls = 1
				}
				if len(requests) != wantCalls || len(base.requests) != wantCalls || len(base.commits) != wantCalls || len(admission.snapshotSettlements()) != wantCalls || limiter.active.Load() != 0 {
					t.Fatalf("wire=%d admissions=%d commits=%d settlements=%d", len(requests), len(base.requests), len(base.commits), len(admission.snapshotSettlements()))
				}
				for _, body := range requests[1:] {
					if !bytes.Equal(body, requests[0]) {
						t.Fatal("resend changed the model request")
					}
				}
			})
		}
	}
}

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
