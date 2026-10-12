package team

import (
	"context"
	"slices"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// ContextSelectionObservation is the authorized shared-persistent selection
// before compiler budgeting. It contains neither task prose nor memory text.
type ContextSelectionObservation struct {
	RequestID        string
	ModelExecutionID string
	Source           string
	SelectedIDs      []string
	Elapsed          time.Duration
}

type contextSelectionObserverKey struct{}
type contextQueryIdentityKey struct{}

func startContextStage(ctx context.Context, requestID, modelExecutionID, stage string) func() {
	observer, ok := ctx.Value(contextSelectionObserverKey{}).(func(ContextSelectionObservation))
	if !ok || observer == nil {
		return nil
	}
	started := time.Now()
	return func() {
		observer(ContextSelectionObservation{RequestID: requestID, ModelExecutionID: modelExecutionID, Source: stage, Elapsed: time.Since(started)})
	}
}

func WithContextSelectionObserver(ctx context.Context, observer func(ContextSelectionObservation)) context.Context {
	return context.WithValue(ctx, contextSelectionObserverKey{}, observer)
}

// ContextQueryIdentitySnapshot identifies retrieval work, independently of the
// admitted tool/model identity. It cannot supply authorization or cost scope.
func ContextQueryIdentitySnapshot(ctx context.Context) (InvocationMetadata, bool) {
	metadata, ok := ctx.Value(contextQueryIdentityKey{}).(InvocationMetadata)
	metadata.TouchedPaths = slices.Clone(metadata.TouchedPaths)
	return metadata, ok
}

func withContextQueryIdentity(ctx context.Context, request ContextRequest) context.Context {
	if !contextstore.QueryWorkObserverEnabled(ctx) {
		return ctx
	}
	return context.WithValue(ctx, contextQueryIdentityKey{}, InvocationMetadata{
		RunID: request.RunID, TaskID: request.TaskID, AgentName: request.AgentName, AgentRole: request.AgentRole,
		Attempt: request.Attempt, ModelExecutionID: request.ModelExecutionID, Purpose: request.Purpose,
		ParentRequestID: request.RequestID, Phase: request.Phase, Trigger: request.Trigger,
	})
}

func observeContextSelection(ctx context.Context, request ContextRequest, items []contextstore.ContextItem) {
	observer, ok := ctx.Value(contextSelectionObserverKey{}).(func(ContextSelectionObservation))
	if !ok || observer == nil {
		return
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if !item.MustKeep {
			ids = append(ids, item.ID)
		}
	}
	observer(ContextSelectionObservation{RequestID: request.RequestID, ModelExecutionID: request.ModelExecutionID, Source: "shared_persistent", SelectedIDs: ids})
}

func observeWorkerContextSelection(ctx context.Context, items []WorkerMemoryItem) {
	observer, ok := ctx.Value(contextSelectionObserverKey{}).(func(ContextSelectionObservation))
	if !ok || observer == nil {
		return
	}
	identity, ok := ContextQueryIdentitySnapshot(ctx)
	if !ok {
		return
	}
	var ids []string
	for _, item := range items {
		if !item.MustKeep {
			ids = append(ids, item.ID)
		}
	}
	observer(ContextSelectionObservation{RequestID: identity.ParentRequestID, ModelExecutionID: identity.ModelExecutionID, Source: "worker_memory", SelectedIDs: ids})
}
