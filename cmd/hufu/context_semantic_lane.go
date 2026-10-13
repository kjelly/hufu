package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/embedding"
	"github.com/kjelly/hufu/internal/semantic"
)

// contextQuerySemanticMode is --semantic-mode of `hufu context query`.
var contextQuerySemanticMode string

const contextQuerySemanticModeUsage = "Semantic retrieval mode for this query: off, shadow, active, or auto (default: hufu.yaml with the team's semantic-retrieval block)"

// contextQueryRetrievalOptions picks the semantic lane of one `hufu context
// query` the way a run picks it: off and shadow never change selection, so
// the query stays exact and lexical without contacting the encoder; auto uses
// the lane only when the query routes to it; active always does. A request
// that uses the lane fuses with the configured fusion even when the lane then
// falls back, exactly like a run.
func contextQueryRetrievalOptions(ctx context.Context, store *contextstore.SQLiteRepository, projectID, query string, settings semantic.Settings, mode semantic.Mode, warn io.Writer) contextstore.HybridRetrievalOptions {
	switch {
	case mode == semantic.ModeActive:
	case mode == semantic.ModeAuto && semantic.RoutesSemantic(query):
	default:
		return contextstore.HybridRetrievalOptions{Mode: contextstore.RetrievalOff}
	}
	options := contextstore.HybridRetrievalOptions{Mode: contextstore.RetrievalActive, Fusion: contextstore.FusionMode(settings.Fusion)}
	index, err := openContextSemanticIndex(ctx, store, projectID, settings, warn)
	if err != nil {
		_, _ = fmt.Fprintf(warn, "warning: semantic lane unavailable; using exact and lexical retrieval: %v\n", err)
		options.UnavailableReason = contextstore.SemanticFallbackReasonForError(err)
		return options
	}
	options.Vector = deadlineVectorSearcher{index: index, timeout: settings.QueryTimeout}
	return options
}

// openContextSemanticIndex loads the configured encoder, refreshes the
// project's semantic generation so only new or edited items are embedded, and
// returns an index over it. A failed refresh is reported to warn and the
// index is still returned: a request whose candidates the active generation
// already covers keeps the lane, any other request falls back.
func openContextSemanticIndex(ctx context.Context, store *contextstore.SQLiteRepository, projectID string, settings semantic.Settings, warn io.Writer) (*contextstore.SemanticIndex, error) {
	encoder, err := openSemanticEncoder(ctx, settings, true)
	if err != nil {
		return nil, err
	}
	if _, err = refreshContextSemantic(ctx, store, encoder, projectID, settings); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: %v; items the active generation does not cover fall back to exact and lexical retrieval\n", err)
	}
	index, err := contextstore.NewSemanticIndexWithOptions(store, store, encoder, contextstore.SemanticIndexOptions{
		InventoryPolicy: contextstore.InventoryPolicyRetrievable,
	})
	if err != nil {
		return nil, fmt.Errorf("open semantic index: %w", err)
	}
	return index, nil
}

// refreshContextSemantic embeds the project's new or edited items within the
// index timeout and activates a generation that covers every item.
func refreshContextSemantic(ctx context.Context, store *contextstore.SQLiteRepository, encoder embedding.Embedder, projectID string, settings semantic.Settings) (contextstore.SemanticRefreshResult, error) {
	builder, err := contextstore.NewSemanticBuilder(store, encoder, contextstore.SemanticBuilderOptions{
		InventoryPolicy: contextstore.InventoryPolicyRetrievable, BatchSize: settings.IndexBatchSize,
	})
	if err != nil {
		return contextstore.SemanticRefreshResult{}, fmt.Errorf("open semantic builder: %w", err)
	}
	buildCtx, cancel := context.WithTimeout(ctx, settings.IndexTimeout)
	defer cancel()
	result, err := builder.Refresh(buildCtx, projectID)
	if err != nil {
		return result, fmt.Errorf("refresh semantic generation: %w", err)
	}
	return result, nil
}

// deadlineVectorSearcher bounds each search by the query timeout, as a run's
// semantic lane does. Its own deadline becomes ErrSemanticTimeout so the
// request falls back; only the caller's cancellation is returned as is.
type deadlineVectorSearcher struct {
	index   *contextstore.SemanticIndex
	timeout time.Duration
}

func (d deadlineVectorSearcher) SearchVector(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	queryCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	results, err := d.index.SearchVector(queryCtx, req)
	if err == nil {
		return results, nil
	}
	if parentErr := ctx.Err(); parentErr != nil {
		return nil, parentErr
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return nil, contextstore.ErrSemanticTimeout
	}
	return nil, err
}
