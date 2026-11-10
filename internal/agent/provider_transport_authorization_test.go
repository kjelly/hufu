package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

type transportAuthorizationAdmission struct {
	*compilationAdmission
	authorize func(context.Context, ProviderRequest) error
}

func (a *transportAuthorizationAdmission) AuthorizeProviderTransport(ctx context.Context, request ProviderRequest) error {
	return a.authorize(ctx, request)
}

func invokeAuthorizedSDK(t *testing.T, mode string, model fantasy.LanguageModel, messages []fantasy.Message) error {
	t.Helper()
	object := fantasy.ObjectCall{Prompt: messages, Schema: fantasy.Schema{Type: "object"}, SchemaName: "answer"}
	switch mode {
	case "generate":
		_, err := model.Generate(t.Context(), fantasy.Call{Prompt: messages})
		return err
	case "object":
		_, err := model.GenerateObject(t.Context(), object)
		return err
	case "stream":
		stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: messages})
		if err == nil {
			for part := range stream {
				if part.Error != nil {
					return part.Error
				}
			}
		}
		return err
	case "object_stream":
		stream, err := model.StreamObject(t.Context(), object)
		if err == nil {
			for part := range stream {
				if part.Error != nil {
					return part.Error
				}
			}
		}
		return err
	}
	t.Fatal("unknown SDK invocation mode")
	return nil
}

func TestProviderCompilationAuthorizesCommittedReferencesAtHTTPBoundary(t *testing.T) {
	for _, mode := range []string{"generate", "stream", "object", "object_stream"} {
		for _, scenario := range []string{"native allowed", "native revoked", "text compatibility"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				var received, checks, releases atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					received.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if mode == "stream" || mode == "object_stream" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-vision\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{}\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-vision\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if mode == "object" {
						_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","created":1,"model":"fixture-vision","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"answer-fixture","type":"function","function":{"name":"answer","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","created":1,"model":"fixture-vision","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}]}`)
				}))
				t.Cleanup(server.Close)
				denied := errors.New("fixture lease revoked after compilation")
				limiter := newCapacityLimiter(1)
				recording := &recordingAdmission{limiter: limiter}
				admission := &transportAuthorizationAdmission{compilationAdmission: &compilationAdmission{recordingAdmission: recording,
					compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
						wire, cleanup, err := compileFixtureImages(t, request)
						return wire, func() { cleanup(); releases.Add(1) }, err
					}}, authorize: func(_ context.Context, request ProviderRequest) error {
					checks.Add(1)
					if len(recording.commits) != 1 || request.InvocationID == "" || request.InvocationID != recording.commits[0].InvocationID || !providerRequestHasReferences(request) {
						return errors.New("transport did not receive committed canonical references")
					}
					if scenario == "native revoked" {
						return denied
					}
					return nil
				}}
				messages := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}}
				if scenario == "text compatibility" {
					messages = []fantasy.Message{fantasy.NewUserMessage("answer")}
				}
				model := NewAdmittedLanguageModel("fixture-vision", nativeImageModel(t, server), admission)
				err := invokeAuthorizedSDK(t, mode, model, messages)
				if scenario == "native revoked" {
					if !errors.Is(err, denied) || received.Load() != 0 {
						t.Fatal("revoked permission reached HTTP or lost its rejection", err, received.Load())
					}
				} else if err != nil || received.Load() != 1 {
					t.Fatal("authorized SDK request did not complete", err, received.Load())
				}
				wantChecks := int32(1)
				if scenario == "text compatibility" {
					wantChecks = 0
				}
				if checks.Load() != wantChecks || releases.Load() != 1 || limiter.active.Load() != 0 {
					t.Fatal("transport authorization or compiler resource ownership was bypassed", checks.Load(), releases.Load(), limiter.active.Load())
				}
			})
		}
	}
}

type rejectedProviderBody struct {
	closed atomic.Int32
}

func (*rejectedProviderBody) Read([]byte) (int, error) { panic("rejected body was read") }
func (b *rejectedProviderBody) Close() error           { b.closed.Add(1); return nil }

func TestProviderCompilationClosesRejectedHTTPBodyWithoutReadingIt(t *testing.T) {
	for _, scenario := range []string{"closed", "cancelled", "denied", "cancelled by authorizer"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			scope := newProviderCompilation(true, nil)
			defer func() { scope.innerReturned(); scope.release() }()
			ctx = scope.bind(ctx)
			body := new(rejectedProviderBody)
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/chat/completions", body)
			if err != nil {
				t.Fatal(err)
			}
			denied := errors.New("fixture denied transport")
			switch scenario {
			case "closed":
				scope.closeBodies()
			case "cancelled":
				cancel()
			case "denied":
				scope.authorize = func(context.Context) error { return denied }
			case "cancelled by authorizer":
				scope.authorize = func(context.Context) error { cancel(); return nil }
			}
			var sent atomic.Int32
			_, err = compiledProviderRoundTrip(request, roundTripFunc(func(*http.Request) (*http.Response, error) {
				sent.Add(1)
				return &http.Response{Body: io.NopCloser(strings.NewReader("unexpected"))}, nil
			}))
			want := error(context.Canceled)
			if scenario == "denied" {
				want = denied
			}
			if !errors.Is(err, want) || sent.Load() != 0 || body.closed.Load() != 1 {
				t.Fatal("rejected request was sent, left open or lost its cause", err, sent.Load(), body.closed.Load())
			}
		})
	}
}
