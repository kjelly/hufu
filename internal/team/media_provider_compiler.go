package team

import (
	"context"
	"slices"
	"sync"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

var _ agent.ProviderRequestCompiler = providerRequestAdmission{}

// CompileProviderRequest is called only after provider request admission and
// slot acquisition. Each read rechecks the canonical worker and provider grant;
// this hook alone grants no transport.
func (a providerRequestAdmission) CompileProviderRequest(ctx context.Context, request agent.ProviderRequest) (agent.ProviderRequest, func(), error) {
	if !agent.ProviderRequestHasMedia(request) {
		return request, nil, nil
	}
	if request.Call == nil && request.ObjectCall == nil {
		return agent.ProviderRequest{}, nil, media.Reject(media.Unsupported, "native compiler requires a concrete provider call")
	}
	admission, err := a.c.resolveNativeProviderMedia(ctx, request)
	if err != nil {
		return agent.ProviderRequest{}, nil, err
	}
	frames := make([]int64, len(admission.Binding.Refs))
	for index, ref := range admission.Binding.Refs {
		frames[index] = ref.ByteSize
	}
	reservation, err := media.ReserveTransportFrames(ctx, frames...)
	if err != nil {
		return agent.ProviderRequest{}, nil, err
	}
	buffers := make(map[string][]byte)
	release := sync.OnceFunc(func() {
		for _, data := range buffers {
			clear(data)
		}
		clear(buffers)
		reservation.Release()
	})
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	// Resource waiting is cancelable and can outlive an occurrence revision.
	// Resolve the whole metadata proof again before any private byte access.
	if err := a.c.recheckNativeProviderMedia(ctx, request, admission); err != nil {
		return agent.ProviderRequest{}, nil, err
	}
	store, err := OpenFileArtifactStoreReadOnly(a.c.artifactStoreRootPath(), a.c.projectDir)
	if err != nil {
		return agent.ProviderRequest{}, nil, err
	}
	for _, ref := range admission.Binding.Refs {
		if _, loaded := buffers[ref.ID]; loaded {
			continue
		}
		if err := a.c.validateMediaAttempt(ctx, TaskDef{MediaBinding: admission.Binding, MediaInputDigest: admission.Binding.InputDigest}, admission.Identity.TaskID); err != nil {
			return agent.ProviderRequest{}, nil, err
		}
		data, err := store.readReservedMedia(ctx, ref, admission.Limits)
		if err != nil {
			return agent.ProviderRequest{}, nil, err
		}
		buffers[ref.ID] = data
	}
	if err := a.c.recheckNativeProviderMedia(ctx, request, admission); err != nil {
		return agent.ProviderRequest{}, nil, err
	}
	wire := compileNativeProviderMessages(request, admission.Locations, buffers)
	handedOff = true
	return wire, release, nil
}

func (c *Coordinator) recheckNativeProviderMedia(ctx context.Context, request agent.ProviderRequest, original nativeProviderMedia) error {
	current, err := c.resolveNativeProviderMedia(ctx, request)
	if err != nil {
		return err
	}
	if current.Identity != original.Identity || current.Target != original.Target || current.Grant != original.Grant || current.Binding.Hash != original.Binding.Hash {
		return media.Reject(media.BindingConflict, "native provider admission changed during compilation")
	}
	return nil
}

func compileNativeProviderMessages(request agent.ProviderRequest, locations []historyReferenceLocation, buffers map[string][]byte) agent.ProviderRequest {
	compile := func(messages []fantasy.Message) []fantasy.Message {
		messages = slices.Clone(messages)
		for index := range messages {
			messages[index].Content = slices.Clone(messages[index].Content)
		}
		for _, location := range locations {
			ref := location.Reference.Ref
			messages[location.Message].Content[location.Part] = fantasy.FilePart{Data: buffers[ref.ID], MediaType: ref.MediaType}
		}
		return messages
	}
	if request.Call != nil {
		call := *request.Call
		call.Prompt = compile(call.Prompt)
		request.Call = &call
	} else {
		call := *request.ObjectCall
		call.Prompt = compile(call.Prompt)
		request.ObjectCall = &call
	}
	return request
}
