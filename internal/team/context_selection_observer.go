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

// The staged query transaction supersedes earlier primary observations for
// this invocation, including IDs displaced by supplemental ranking.
func observeEffectiveQuerySelection(ctx context.Context, input WorkerContextInput) {
	observer, ok := ctx.Value(contextSelectionObserverKey{}).(func(ContextSelectionObservation))
	if !ok || observer == nil {
		return
	}
	var ids []string
	if input.CanonicalMemory != nil {
		for _, item := range input.CanonicalMemory.SharedPersistent {
			if !item.MustKeep {
				ids = append(ids, item.ID)
			}
		}
	}
	if input.WorkerMemory != nil {
		for _, item := range input.WorkerMemory.Items {
			if !item.MustKeep {
				ids = append(ids, item.ID)
			}
		}
	}
	observer(ContextSelectionObservation{RequestID: input.Request.RequestID, ModelExecutionID: input.Request.ModelExecutionID, Source: "query_effective", SelectedIDs: ids})
}

// Candidate observations precede injection/dedup/token budgeting. Post-budget
// selection observations must not be labelled retrieval recall/precision.
func observeRetrievalCandidates(ctx context.Context, results []contextstore.SearchResult, replace bool) {
	observer, ok := ctx.Value(contextSelectionObserverKey{}).(func(ContextSelectionObservation))
	if !ok || observer == nil {
		return
	}
	identity, ok := ContextQueryIdentitySnapshot(ctx)
	if !ok {
		return
	}
	ids := make([]string, 0, len(results))
	for _, row := range results {
		if !row.Item.MustKeep {
			ids = append(ids, row.Item.ID)
		}
	}
	source := "primary_candidates"
	if replace {
		source = "query_candidates"
	}
	observer(ContextSelectionObservation{RequestID: identity.ParentRequestID, ModelExecutionID: identity.ModelExecutionID, Source: source, SelectedIDs: ids})
}

func observeWorkerRetrievalCandidates(ctx context.Context, rows []rankedMemory) {
	if observer, ok := ctx.Value(contextSelectionObserverKey{}).(func(ContextSelectionObservation)); !ok || observer == nil {
		return
	}
	if _, ok := ContextQueryIdentitySnapshot(ctx); !ok {
		return
	}
	candidates := make([]contextstore.SearchResult, len(rows))
	for index, row := range rows {
		candidates[index] = contextstore.SearchResult{Item: row.item, Score: row.score}
	}
	observeRetrievalCandidates(ctx, candidates, false)
}
