package agent

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type metadataCompilationAdmission struct {
	*compilationAdmission
	allowed bool
	err     error
}

func (a *metadataCompilationAdmission) AdmitProviderReferenceMetadata(context.Context, ProviderRequest) (bool, error) {
	return a.allowed, a.err
}

func TestProviderReferenceMetadataPreservesCanonicalRequests(t *testing.T) {
	for _, mode := range []string{"generate", "stream", "object", "object_stream"} {
		t.Run(mode, func(t *testing.T) {
			messages := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}}
			original := slices.Clone(messages)
			original[0].Content = slices.Clone(messages[0].Content)
			limiter := newCapacityLimiter(1)
			recording := &recordingAdmission{limiter: limiter}
			admission := &metadataCompilationAdmission{
				compilationAdmission: &compilationAdmission{recordingAdmission: recording, compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
					wire, err := ProjectProviderReferenceMetadata(request)
					return wire, nil, err
				}}, allowed: true,
			}
			inner := &compilationLanguageModel{}
			if err := invokeCompilationMode(t, mode, NewAdmittedLanguageModel("text-model", inner, admission), messages); err != nil {
				t.Fatal(err)
			}
			if inner.calls != 1 || len(recording.commits) != 1 || limiter.active.Load() != 0 {
				t.Fatal("metadata projection did not complete exactly one admitted invocation")
			}
			if !reflect.DeepEqual(messages, original) || !providerRequestHasReferences(recording.requests[0]) || !providerRequestHasReferences(recording.commits[0]) {
				t.Fatal("metadata projection replaced canonical references")
			}
			wire := inner.prompts[0]
			if len(wire) != 1 || len(wire[0].Content) != 3 || !reflect.DeepEqual(wire[0].Content[0], messages[0].Content[0]) {
				t.Fatal("metadata projection changed message shape or original text")
			}
			for index, part := range wire[0].Content[1:] {
				ref := messages[0].Content[index+1].(MediaReferencePart).Reference
				text, err := ReferenceMetadataText(ref)
				if err != nil || !reflect.DeepEqual(part, fantasy.TextPart{Text: text}) || strings.Contains(text, ref.Ref.ID) || strings.Contains(text, ref.Ref.SHA256) {
					t.Fatal("wire did not contain only safe, ordered metadata")
				}
			}
		})
	}
}

func TestProviderReferenceMetadataRejectsUnadmittedOrChangedProjections(t *testing.T) {
	for _, test := range []struct {
		name    string
		allowed bool
		absent  bool
		err     error
		mutate  func(*ProviderRequest)
	}{
		{name: "no metadata authority", absent: true},
		{name: "denied metadata authority"},
		{name: "authority failure", err: errors.New("metadata authority unavailable")},
		{name: "metadata text", allowed: true, mutate: func(r *ProviderRequest) {
			r.Call.Prompt[0].Content[1] = fantasy.TextPart{Text: "invented image contents"}
		}},
		{name: "nil metadata", allowed: true, mutate: func(r *ProviderRequest) { r.Call.Prompt[0].Content[1] = (*fantasy.TextPart)(nil) }},
		{name: "native image", allowed: true, mutate: func(r *ProviderRequest) {
			r.Call.Prompt[0].Content[1] = fantasy.FilePart{MediaType: "image/png", Data: []byte("unadmitted")}
		}},
		{name: "original text", allowed: true, mutate: func(r *ProviderRequest) { r.Call.Prompt[0].Content[0] = fantasy.TextPart{Text: "changed goal"} }},
		{name: "role", allowed: true, mutate: func(r *ProviderRequest) { r.Call.Prompt[0].Role = fantasy.MessageRoleAssistant }},
		{name: "target", allowed: true, mutate: func(r *ProviderRequest) { r.ModelID = "another-target" }},
		{name: "call options", allowed: true, mutate: func(r *ProviderRequest) { r.Call.MaxOutputTokens = new(int64(999)) }},
		{name: "missing occurrence", allowed: true, mutate: func(r *ProviderRequest) { r.Call.Prompt[0].Content = r.Call.Prompt[0].Content[:2] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			limiter := newCapacityLimiter(1)
			recording := &recordingAdmission{limiter: limiter}
			var releases int
			compiler := &compilationAdmission{recordingAdmission: recording, compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
				wire, err := ProjectProviderReferenceMetadata(request)
				if test.mutate != nil {
					test.mutate(&wire)
				}
				return wire, func() { releases++ }, err
			}}
			var admission RequestAdmission = &metadataCompilationAdmission{compilationAdmission: compiler, allowed: test.allowed, err: test.err}
			if test.absent {
				admission = compiler
			}
			inner := &countingLanguageModel{}
			_, err := NewAdmittedLanguageModel("text-model", inner, admission).Generate(t.Context(), fantasy.Call{Prompt: []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}}})
			wantReleases := 1
			if test.err != nil {
				wantReleases = 0
			}
			if err == nil || test.err != nil && !errors.Is(err, test.err) || inner.calls != 0 || len(recording.commits) != 0 || limiter.active.Load() != 0 || releases != wantReleases {
				t.Fatalf("invalid metadata projection escaped its boundary: err=%v calls=%d commits=%d releases=%d", err, inner.calls, len(recording.commits), releases)
			}
		})
	}
}
