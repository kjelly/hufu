package agent

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"slices"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

type compilationAdmission struct {
	*recordingAdmission
	compile func(ProviderRequest) (ProviderRequest, func(), error)
}

func (a *compilationAdmission) CompileProviderRequest(_ context.Context, request ProviderRequest) (ProviderRequest, func(), error) {
	return a.compile(request)
}

type compilationLanguageModel struct{ promptCapturingModel }

func (m *compilationLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	_, err := m.promptCapturingModel.Stream(ctx, call)
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "done"})
	}, err
}

func (m *compilationLanguageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	_, err := m.promptCapturingModel.StreamObject(ctx, call)
	return func(yield func(fantasy.ObjectStreamPart) bool) { yield(fantasy.ObjectStreamPart{}) }, err
}

func compileFixtureImages(t *testing.T, request ProviderRequest) (ProviderRequest, func(), error) {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	imageBytes := data.Bytes()
	compile := func(messages []fantasy.Message) []fantasy.Message {
		result := slices.Clone(messages)
		for index := range result {
			result[index].Content = slices.Clone(result[index].Content)
			for partIndex, part := range result[index].Content {
				if reference, ok := part.(MediaReferencePart); ok {
					result[index].Content[partIndex] = fantasy.FilePart{Data: imageBytes, MediaType: reference.Reference.Ref.MediaType}
				}
			}
		}
		return result
	}
	wire := request
	wire.Messages = compile(request.Messages)
	if request.Call != nil {
		call := *request.Call
		call.Prompt = compile(call.Prompt)
		wire.Call = &call
	}
	if request.ObjectCall != nil {
		call := *request.ObjectCall
		call.Prompt = compile(call.Prompt)
		wire.ObjectCall = &call
	}
	return wire, func() { clear(imageBytes) }, nil
}

func invokeCompilationMode(t *testing.T, mode string, model fantasy.LanguageModel, messages []fantasy.Message) error {
	t.Helper()
	switch mode {
	case "generate":
		_, err := model.Generate(t.Context(), fantasy.Call{Prompt: messages})
		return err
	case "stream":
		stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: messages})
		if err == nil {
			for range stream {
			}
		}
		return err
	case "object":
		_, err := model.GenerateObject(t.Context(), fantasy.ObjectCall{Prompt: messages})
		return err
	case "object_stream":
		stream, err := model.StreamObject(t.Context(), fantasy.ObjectCall{Prompt: messages})
		if err == nil {
			for range stream {
			}
		}
		return err
	}
	t.Fatal("unknown fixture mode")
	return nil
}

func TestProviderCompilationKeepsCanonicalRequestsSeparateFromWire(t *testing.T) {
	for _, mode := range []string{"generate", "stream", "object", "object_stream"} {
		t.Run(mode, func(t *testing.T) {
			messages := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}}
			limiter := newCapacityLimiter(1)
			recording := &recordingAdmission{limiter: limiter}
			var releases atomic.Int32
			admission := &compilationAdmission{recordingAdmission: recording, compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
				if limiter.active.Load() != 1 {
					t.Fatal("compilation happened without its provider slot")
				}
				wire, release, err := compileFixtureImages(t, request)
				return wire, func() { release(); releases.Add(1) }, err
			}}
			inner := &compilationLanguageModel{}
			model := NewAdmittedLanguageModel("test-model", inner, admission)
			if err := invokeCompilationMode(t, mode, model, messages); err != nil {
				t.Fatal(err)
			}
			if inner.calls != 1 || releases.Load() != 1 || limiter.active.Load() != 0 || len(recording.commits) != 1 {
				t.Fatal("compiler/provider/cleanup lifecycle was not exactly once")
			}
			commit := recording.commits[0]
			if !providerRequestHasReferences(commit) || !providerRequestHasReferences(recording.requests[0]) {
				t.Fatal("transport bytes replaced the canonical admission or commit request")
			}
			if _, ok := messages[0].Content[1].(MediaReferencePart); !ok {
				t.Fatal("compiler mutated the caller's history")
			}
			if len(inner.prompts) != 1 || len(inner.prompts[0][0].Content) != 3 {
				t.Fatal("provider did not receive both image occurrences")
			}
			for _, part := range inner.prompts[0][0].Content[1:] {
				file, ok := part.(fantasy.FilePart)
				if !ok || !bytes.Equal(file.Data, make([]byte, len(file.Data))) {
					t.Fatal("private wire buffer was not released after consumption")
				}
			}
		})
	}
}

func TestProviderCompilationRejectsDriftBeforeCommitAndTransport(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ProviderRequest)
	}{
		{"target", func(request *ProviderRequest) { request.ModelID = "another-model" }},
		{"role", func(request *ProviderRequest) { request.Call.Prompt[0].Role = fantasy.MessageRoleAssistant }},
		{"dropped image", func(request *ProviderRequest) { request.Call.Prompt[0].Content = request.Call.Prompt[0].Content[:2] }},
		{"text", func(request *ProviderRequest) {
			request.Call.Prompt[0].Content[0] = fantasy.TextPart{Text: "changed goal"}
		}},
		{"bytes", func(request *ProviderRequest) { request.Call.Prompt[0].Content[1].(fantasy.FilePart).Data[0] ^= 1 }},
		{"nil file", func(request *ProviderRequest) { request.Call.Prompt[0].Content[1] = (*fantasy.FilePart)(nil) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var releases atomic.Int32
			recording := &recordingAdmission{limiter: newCapacityLimiter(1)}
			admission := &compilationAdmission{recordingAdmission: recording, compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
				wire, cleanup, err := compileFixtureImages(t, request)
				test.mutate(&wire)
				return wire, func() { cleanup(); releases.Add(1) }, err
			}}
			inner := &countingLanguageModel{}
			model := NewAdmittedLanguageModel("test-model", inner, admission)
			_, err := model.Generate(t.Context(), fantasy.Call{Prompt: []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}}})
			if err == nil || releases.Load() != 1 || inner.calls != 0 || len(recording.commits) != 0 {
				t.Fatal("invalid compilation reached commit/transport or leaked its buffer")
			}
		})
	}
}

func TestProviderCompilationReleasesAfterFailedCommit(t *testing.T) {
	for _, mode := range []string{"generate", "stream", "object", "object_stream"} {
		t.Run(mode, func(t *testing.T) {
			failure := errors.New("injected durable commit failure")
			limiter := newCapacityLimiter(1)
			var releases atomic.Int32
			admission := &compilationAdmission{recordingAdmission: &recordingAdmission{commitErr: failure, limiter: limiter}, compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
				wire, cleanup, err := compileFixtureImages(t, request)
				return wire, func() { cleanup(); releases.Add(1) }, err
			}}
			inner := &compilationLanguageModel{}
			model := NewAdmittedLanguageModel("test-model", inner, admission)
			err := invokeCompilationMode(t, mode, model, []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}})
			if !errors.Is(err, failure) || releases.Load() != 1 || inner.calls != 0 || limiter.active.Load() != 0 {
				t.Fatal("failed commit dispatched transport or leaked compiler/slot resources")
			}
		})
	}
}

func TestProviderCompilationRejectsTextMutationAndReleasesPartialFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "text mutation"},
		{name: "partial compile failure", err: errors.New("injected compiler failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			limiter := newCapacityLimiter(1)
			var releases int
			recording := &recordingAdmission{limiter: limiter}
			admission := &compilationAdmission{recordingAdmission: recording, compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
				call := *request.Call
				call.Prompt = []fantasy.Message{fantasy.NewUserMessage("changed input")}
				request.Call = &call
				return request, func() { releases++ }, test.err
			}}
			inner := &countingLanguageModel{}
			model := NewAdmittedLanguageModel("test-model", inner, admission)
			_, err := model.Generate(t.Context(), fantasy.Call{Prompt: []fantasy.Message{fantasy.NewUserMessage("original input")}})
			if err == nil || test.err != nil && !errors.Is(err, test.err) || releases != 1 || inner.calls != 0 || len(recording.commits) != 0 || limiter.active.Load() != 0 {
				t.Fatal("invalid/failed compilation escaped admission or leaked its resources")
			}
		})
	}
}

func TestProviderCompilationRequiresOwnedBuffers(t *testing.T) {
	limiter := newCapacityLimiter(1)
	admission := &compilationAdmission{recordingAdmission: &recordingAdmission{limiter: limiter}, compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
		wire, cleanup, err := compileFixtureImages(t, request)
		t.Cleanup(cleanup)
		return wire, nil, err
	}}
	inner := &countingLanguageModel{}
	_, err := NewAdmittedLanguageModel("test-model", inner, admission).Generate(t.Context(), fantasy.Call{Prompt: []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}}})
	if err == nil || inner.calls != 0 || limiter.active.Load() != 0 {
		t.Fatal("compiler without a buffer owner reached transport or leaked its slot")
	}
}
