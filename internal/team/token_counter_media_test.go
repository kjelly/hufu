package team

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

func TestNativeImageCounterIncludesRepeatedImagesAndCompleteText(t *testing.T) {
	for _, shape := range []string{"call", "object", "messages"} {
		t.Run(shape, func(t *testing.T) {
			c, ctx, _, request := nativeMediaProviderFixture(t)
			ctx = withNativeProviderCounter(ctx, c)
			messages := request.Call.Prompt
			if shape == "object" {
				request.ObjectCall, request.Call = &fantasy.ObjectCall{Prompt: messages}, nil
			} else if shape == "messages" {
				request.Messages, request.Call = messages, nil
			}
			before, err := agent.EncodeMediaHistory(messages, "counter-fixture")
			if err != nil {
				t.Fatal(err)
			}
			count, err := defaultCounter.CountProviderRequest(ctx, request.ModelID, request)
			if err != nil {
				t.Fatal(err)
			}
			text := nativeRequestTextProjection(request)
			if agent.ProviderRequestHasMedia(text) {
				t.Fatal("text projection retained native image references")
			}
			textCount, err := defaultCounter.CountProviderRequest(ctx, request.ModelID, text)
			if err != nil || count-textCount != 2*8192 {
				t.Fatal("counter deduplicated images or tokenized reference claims as text", err)
			}
			after, err := agent.EncodeMediaHistory(messages, "counter-fixture")
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("counter changed canonical history")
			}
			bareCount, err := defaultCounter.CountProviderRequest(ctx, "vision", request)
			if err != nil || bareCount != count {
				t.Fatal("counter lost the provider-bound target when SDK supplied its bare model name", err)
			}
		})
	}
}

func TestNativeImageCounterKeepsSystemAndToolSchemaCosts(t *testing.T) {
	c, ctx, _, request := nativeMediaProviderFixture(t)
	ctx = withNativeProviderCounter(ctx, c)
	baseline, err := defaultCounter.CountProviderRequest(ctx, request.ModelID, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Call.Prompt = append([]fantasy.Message{fantasy.NewSystemMessage(strings.Repeat("system instruction ", 100))}, request.Call.Prompt...)
	request.Call.Tools = []fantasy.Tool{fantasy.FunctionTool{Name: "inspect", Description: strings.Repeat("tool schema instruction ", 100), InputSchema: map[string]any{"type": "object"}}}
	complete, err := defaultCounter.CountProviderRequest(ctx, request.ModelID, request)
	if err != nil || complete <= baseline+100 {
		t.Fatal("native image counting discarded non-image request content", err)
	}
}

type unserializableCounterMedia struct{}

func (unserializableCounterMedia) GetType() fantasy.ContentType     { return fantasy.ContentTypeFile }
func (unserializableCounterMedia) Options() fantasy.ProviderOptions { return nil }
func (unserializableCounterMedia) MarshalJSON() ([]byte, error) {
	panic("native counter must reject raw media before JSON/Base64 serialization")
}

func TestNativeImageCounterRejectsUnadmittedMediaBeforeSerialization(t *testing.T) {
	c, ctx, _, request := nativeMediaProviderFixture(t)
	_, err := defaultCounter.CountProviderRequest(ctx, request.ModelID, request)
	requireMediaCode(t, err, media.Unsupported)
	ctx = withNativeProviderCounter(ctx, c)
	_, err = defaultCounter.CountProviderRequest(ctx, "another-model", request)
	requireMediaCode(t, err, media.EgressDenied)
	request.Call.Prompt[0].Content[1] = unserializableCounterMedia{}
	_, err = defaultCounter.CountProviderRequest(ctx, request.ModelID, request)
	if err == nil {
		t.Fatal("raw media reached the native reference counter")
	}
}

func TestNativeImageCounterRejectsUnavailableOrOverflowingBounds(t *testing.T) {
	_, ctx, _, request := nativeMediaProviderFixture(t)
	for _, bound := range []int64{0, -1, math.MaxInt64} {
		if _, err := defaultCounter.countAdmittedNativeRequest(ctx, request, bound); err == nil {
			t.Fatal("invalid native image estimate was accepted")
		}
	}
}
