package team

import (
	"context"
	"errors"
	"log"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func isContextCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// workerMemoryErrorClass names a retrieval failure without its message, which
// may quote the query or repository paths.
func workerMemoryErrorClass(err error) string {
	if errors.Is(err, contextstore.ErrSemanticCanonicalRepo) {
		return "canonical_repository"
	}
	return "retrieval_error"
}

// reportWorkerMemoryDegraded records that recall ran without some sources, so
// a degraded recall is never indistinguishable from "no relevant memory".
func (c *Coordinator) reportWorkerMemoryDegraded(ctx context.Context, trace WorkerMemoryTrace) {
	if len(trace.Degraded) == 0 {
		return
	}
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	if err := c.emitEvent("worker_memory_recall_degraded", "coordinator", todoID, map[string]any{
		"worker_id": trace.WorkerID,
		"degraded":  trace.Degraded,
	}); err != nil {
		log.Printf("warning: worker memory degraded event failed: %v", err)
	}
}

// workerMemoryUnavailableNote tells a memory_query caller that an empty or
// partial answer reflects failed sources, not an absence of memory.
func workerMemoryUnavailableNote(trace WorkerMemoryTrace) string {
	if len(trace.Degraded) == 0 {
		return ""
	}
	return "Some memory sources were unavailable, so this recall may be incomplete."
}
