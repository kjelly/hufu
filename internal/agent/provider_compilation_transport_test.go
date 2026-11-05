package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

func TestProviderCompilationReachesSDKWireWithoutChangingHistory(t *testing.T) {
	for _, mode := range []string{"generate", "stream"} {
		t.Run(mode, func(t *testing.T) {
			parts := historyReferenceParts(t)
			messages := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: parts}}
			before, err := EncodeMediaHistory(messages, "fixture-checkpoint")
			if err != nil {
				t.Fatal(err)
			}
			requests := make(chan nativeImageWireRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !captureNativeImageRequest(w, r, requests) {
					return
				}
				if mode == "generate" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","created":1,"model":"fixture-vision","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-vision\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture-vision\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(server.Close)
			var releases atomic.Int32
			admission := &compilationAdmission{recordingAdmission: new(recordingAdmission), compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
				wire, cleanup, err := compileFixtureImages(t, request)
				return wire, func() { cleanup(); releases.Add(1) }, err
			}}
			model := NewAdmittedLanguageModel("fixture-vision", nativeImageModel(t, server), admission)
			ag := fantasy.NewAgent(model, fantasy.WithMaxRetries(0), fantasy.WithStopConditions(fantasy.StepCountIs(1)))
			if mode == "generate" {
				_, err = ag.Generate(t.Context(), fantasy.AgentCall{Messages: messages})
			} else {
				_, err = ag.Stream(t.Context(), fantasy.AgentStreamCall{Messages: messages})
			}
			if err != nil {
				t.Fatal(err)
			}
			if releases.Load() != 1 || len(admission.commits) != 1 || !providerRequestHasReferences(admission.commits[0]) {
				t.Fatal("SDK transport replaced canonical references or leaked its compiler buffers")
			}
			var request nativeImageWireRequest
			select {
			case request = <-requests:
			default:
				t.Fatal("compiled request did not reach the SDK HTTP transport")
			}
			if len(request.Messages) != 1 || request.Messages[0].Role != "user" {
				t.Fatal("SDK compiler duplicated or changed the current user turn")
			}
			wireParts := nativeImageWireParts(t, request.Messages[0])
			if len(wireParts) != len(parts) || wireParts[0].Type != "text" || wireParts[0].Text != "compare" {
				t.Fatal("SDK compiler changed the user text or ordered image count")
			}
			for index := 1; index < len(parts); index++ {
				reference := parts[index].(MediaReferencePart).Reference
				encoded, found := strings.CutPrefix(wireParts[index].ImageURL.URL, "data:"+reference.Ref.MediaType+";base64,")
				if !found || wireParts[index].Type != "image_url" {
					t.Fatal("SDK omitted a native image or its canonical MIME")
				}
				data, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(data)
				if hex.EncodeToString(digest[:]) != reference.Ref.SHA256 {
					t.Fatal("SDK image wire bytes differ from the admitted source digest")
				}
			}
			after, err := EncodeMediaHistory(messages, "fixture-checkpoint")
			if err != nil || string(before) != string(after) {
				t.Fatal("hydrated wire parts changed persistent caller history")
			}
		})
	}
}
