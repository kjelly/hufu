package team

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

func TestProviderRejectsMediaBeforeTransport(t *testing.T) {
	file := fantasy.FilePart{Data: []byte("opaque-image"), MediaType: "image/png"}
	tool := fantasy.ToolResultPart{ToolCallID: "tool", Output: fantasy.ToolResultOutputContentMedia{MediaType: "image/png", Data: "opaque"}}
	for _, part := range []fantasy.MessagePart{file, &file, tool, &tool, fantasy.FilePart{MediaType: "text/plain", Data: []byte("opaque")}} {
		for _, shape := range []string{"messages", "call", "object"} {
			t.Run(string(part.GetType())+"/"+shape, func(t *testing.T) {
				messages := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{part}}}
				request := agent.ProviderRequest{ModelID: "test"}
				switch shape {
				case "messages":
					request.Messages = messages
				case "call":
					request.Call = &fantasy.Call{Prompt: messages}
				case "object":
					request.ObjectCall = &fantasy.ObjectCall{Prompt: messages}
				}
				err := (providerRequestAdmission{}).AdmitProviderRequest(t.Context(), request)
				if e, ok := errors.AsType[*media.Error](err); !ok || e.Code != media.Unsupported {
					t.Fatalf("admission: %v", err)
				}
				transport := &mediaCountingLanguageModel{}
				model := agent.NewAdmittedLanguageModel("test", transport, providerRequestAdmission{})
				for _, call := range []func() error{
					func() error { _, err := model.Generate(t.Context(), fantasy.Call{Prompt: messages}); return err },
					func() error { _, err := model.Stream(t.Context(), fantasy.Call{Prompt: messages}); return err },
					func() error {
						_, err := model.GenerateObject(t.Context(), fantasy.ObjectCall{Prompt: messages})
						return err
					},
					func() error {
						_, err := model.StreamObject(t.Context(), fantasy.ObjectCall{Prompt: messages})
						return err
					},
				} {
					err := call()
					if e, ok := errors.AsType[*media.Error](err); !ok || e.Code != media.Unsupported || transport.calls != 0 {
						t.Fatalf("calls=%d error=%v", transport.calls, err)
					}
				}
			})
		}
	}
}

type mediaCountingLanguageModel struct{ costAdmissionLanguageModel }

func (m *mediaCountingLanguageModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls++
	return nil, nil
}
func (m *mediaCountingLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.calls++
	return &fantasy.ObjectResponse{}, nil
}
func (m *mediaCountingLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	m.calls++
	return nil, nil
}
