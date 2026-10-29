package team

import (
	"context"
	"errors"
	"sync"
)

type dependencyViewCallKey struct{}

type dependencyViewCallState struct {
	mu          sync.Mutex
	coordinator *Coordinator
	manifest    *ContextInjectionManifest
}

func primaryDependencyViewRequest(manifest ContextInjectionManifest) bool {
	return manifest.TaskID != "" &&
		((manifest.Trigger == ContextTriggerTaskDispatch && manifest.Purpose == contextPurposeForTrigger(ContextTriggerTaskDispatch)) ||
			(manifest.Trigger == ContextTriggerRetry && manifest.Purpose == contextPurposeForTrigger(ContextTriggerRetry)))
}

func (c *Coordinator) bindDependencyViewCall(ctx context.Context, manifest *ContextInjectionManifest) context.Context {
	if manifest.SchemaVersion != DependencyResultViewManifestSchemaVersion || !primaryDependencyViewRequest(*manifest) {
		if ctx.Value(dependencyViewCallKey{}) != nil {
			return context.WithValue(ctx, dependencyViewCallKey{}, (*dependencyViewCallState)(nil))
		}
		return ctx
	}
	return context.WithValue(ctx, dependencyViewCallKey{}, &dependencyViewCallState{coordinator: c, manifest: manifest})
}

// markDependencyViewModelCall runs at the actual agent/provider invocation,
// after context and execution admission. A prepared or cancelled request is
// never recorded as delivery. ModelCalled denotes runtime dispatch, not an
// assertion that a remote server acknowledged the request.
func markDependencyViewModelCall(ctx context.Context) (context.Context, error) {
	state, _ := ctx.Value(dependencyViewCallKey{}).(*dependencyViewCallState)
	if state == nil {
		return ctx, nil
	}
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if metadata, ok := invocationMetadataFromContext(ctx); ok && metadata.ParentRequestID != state.manifest.RequestID {
		// A nested auxiliary call can inherit its parent's Go context, but
		// must retain its own invocation identity and cannot mark its parent.
		return ctx, nil
	}
	if !state.manifest.ModelCalled {
		before := *cloneContextInjectionManifest(state.manifest)
		state.manifest.ModelCalled = true
		state.manifest.Outcome = "model_call"
		state.manifest.Fingerprint = dependencyResultViewManifestFingerprint(*state.manifest)
		if err := state.coordinator.persistContextManifest(state.manifest, ctx); err != nil {
			*state.manifest = before
			// Persistence updates the Todo projection before its durable sinks.
			// Restore that projection too; a failed dispatch record cannot be
			// left behind as delivery proof when the worker was never invoked.
			rollbackErr := state.coordinator.persistContextManifest(&before, ctx)
			return ctx, errors.Join(err, rollbackErr)
		}
	}
	return withInvocationMetadata(ctx, invocationMetadataFromManifest(state.manifest)), nil
}
