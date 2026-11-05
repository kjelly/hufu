package agent

import (
	"context"
	"reflect"
	"sync"

	"github.com/kjelly/hufu/internal/media"
)

// ProviderRequestCompiler creates a private transport projection after
// admission and slot acquisition. The original request remains authoritative
// for commits, accounting, retries and callbacks. Release must clear owned
// buffers; the wrapper invokes it only after inner encoding and HTTP consumers
// have drained. Implementations must not mutate the original request and must
// release partial allocations on error.
type ProviderRequestCompiler interface {
	CompileProviderRequest(context.Context, ProviderRequest) (ProviderRequest, func(), error)
}

// ProviderTransportAuthorizer rechecks a native request at each HTTP send,
// after durable invocation commit and SDK encoding. The request is the original
// canonical reference projection, never the SDK's private image buffers.
// Implementations must not issue provider calls or mutate the request.
type ProviderTransportAuthorizer interface {
	AuthorizeProviderTransport(context.Context, ProviderRequest) error
}

func (m *admittedLanguageModel) bindTransportAuthorization(ctx context.Context, request ProviderRequest) {
	scope := nativeProviderCompilation(ctx)
	if scope == nil {
		return
	}
	if authorizer, ok := m.admission.(ProviderTransportAuthorizer); ok {
		scope.mu.Lock()
		scope.authorize = func(ctx context.Context) error {
			return authorizer.AuthorizeProviderTransport(ctx, request)
		}
		scope.mu.Unlock()
	}
}

func (m *admittedLanguageModel) compileInvocation(ctx context.Context, original ProviderRequest) (ProviderRequest, *providerCompilation, error) {
	wire := original
	var release func()
	if compiler, ok := m.admission.(ProviderRequestCompiler); ok {
		var err error
		wire, release, err = compiler.CompileProviderRequest(ctx, original)
		if err != nil {
			if release != nil {
				release()
			}
			return ProviderRequest{}, nil, err
		}
		if !providerRequestHasReferences(original) && !reflect.DeepEqual(original, wire) {
			if release != nil {
				release()
			}
			return ProviderRequest{}, nil, media.Reject(media.BindingConflict, "request compiler changed content without canonical media references")
		}
	}
	if err := validateCompiledProviderRequest(original, wire); err != nil {
		if release != nil {
			release()
		}
		return ProviderRequest{}, nil, err
	}
	native := ProviderRequestHasMedia(original)
	if native && providerRequestHasReferences(original) && release == nil {
		return ProviderRequest{}, nil, media.Reject(media.BindingConflict, "native request compiler omitted buffer ownership")
	}
	if !native && release == nil {
		return wire, nil, nil
	}
	return wire, newProviderCompilation(native, release), nil
}

func validateCompiledProviderRequest(original, wire ProviderRequest) error {
	if wire.ModelID != original.ModelID || wire.InvocationID != original.InvocationID || wire.AdmissionContext != original.AdmissionContext ||
		(wire.Call == nil) != (original.Call == nil) || (wire.ObjectCall == nil) != (original.ObjectCall == nil) {
		return media.Reject(media.BindingConflict, "request compiler changed the admitted identity or call kind")
	}
	if providerRequestHasReferences(wire) {
		return media.Reject(media.Unsupported, "media references require an admitted request compiler")
	}
	if !ProviderRequestHasMedia(original) && ProviderRequestHasMedia(wire) {
		return media.Reject(media.BindingConflict, "request compiler introduced unadmitted media")
	}
	if providerRequestHasReferences(original) {
		return validateCompiledReferenceRequest(original, wire)
	}
	return nil
}

func joinProviderReleases(slot func(), compilation *providerCompilation) func() {
	if slot == nil && compilation == nil {
		return nil
	}
	return sync.OnceFunc(func() {
		compilation.release()
		if slot != nil {
			slot()
		}
	})
}
