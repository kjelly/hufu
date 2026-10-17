package team

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// queryWorkerMemory answers a worker's memory_query with the recall the
// runtime uses to inject memory into that worker: the worker's agent and
// branch scope, the session lineage, and the agent's memory policy. The
// caller is the Todo's agent, never a model-supplied name. The
// coordinator-wide scope memory_query used before matched only shared
// records, so a worker could not recall its own private memories, and it
// ignored the agent's policy, so an agent with memory off could still query.
func (c *Coordinator) queryWorkerMemory(ctx context.Context, todoID, query string, limit int, kind contextstore.ContextKind, minConfidence *float64) fantasy.ToolResponse {
	item := c.todoItemByID(todoID)
	if item == nil {
		return fantasy.NewTextErrorResponse("memory_query requires the calling task")
	}
	if !workerMemoryPhaseAllowed(item.Phase) {
		return fantasy.NewTextErrorResponse(verifyMemoryQueryDenial)
	}
	def := c.agentDefByName(item.Agent)
	if def == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("memory_query requires a known worker; %q is not one", item.Agent))
	}
	if c.workerMemorySvc == nil || !shouldRecallWorkerMemory(def, c.ExecutionProfile()) {
		return fantasy.NewTextResponse("Memory recall is disabled for this agent.")
	}
	scope := resolveWorkerScope(c.contextScope(), def, c.activeBranchID())
	lineage, lineageOK := c.workerMemoryLineage(scope.BranchID)
	bundle, err := c.workerMemorySvc.Recall(ctx, WorkerMemoryRecallRequest{
		WorkerID:           def.MemoryID,
		BranchID:           scope.BranchID,
		Scope:              scope,
		SessionLineage:     lineage,
		LineageUnavailable: !lineageOK,
		Query:              query,
		Policy:             def.Memory,
		MaxItems:           limit,
		MaxTokens:          def.Memory.MaxTokens,
		Trace: SemanticTraceAttribution{
			RunID: c.executionRunID, TaskID: todoID, Trigger: "memory_query", Agent: item.Agent,
			Source: semanticSourceWorkerPrivate,
		},
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("worker memory query failed: %v", err))
	}
	c.reportWorkerMemoryDegraded(ctx, bundle.Trace)
	note := workerMemoryUnavailableNote(bundle.Trace)
	var b strings.Builder
	for _, recalled := range bundle.Items {
		if kind != "" && recalled.Kind != kind {
			continue
		}
		if minConfidence != nil && recalled.Confidence < *minConfidence {
			continue
		}
		fmt.Fprintf(&b, "- [%.2f] %s (id: %s)\n", recalled.FinalScore, recalled.Content, recalled.ID)
	}
	if b.Len() == 0 {
		if note != "" {
			return fantasy.NewTextResponse("No memories could be returned. " + note)
		}
		return fantasy.NewTextResponse("No relevant memories found.")
	}
	if note != "" {
		b.WriteString("\n" + note)
	}
	return fantasy.NewTextResponse(strings.TrimSpace(b.String()))
}

// unboundArtifactPolicyTool marks Hufu-owned tools that an unbound worker may
// call under the artifact policy. Such a tool takes no filesystem path and
// reads only state the runtime scopes to the calling task. The interface is
// sealed to this package: a tool's own workspace-scope description is not
// enough, because any tool can describe itself that way.
type unboundArtifactPolicyTool interface {
	unboundArtifactPolicyTool()
}

func (*canonicalMemoryQueryTool) unboundArtifactPolicyTool() {}

func isUnboundArtifactPolicyTool(tool fantasy.AgentTool) bool {
	_, ok := tool.(unboundArtifactPolicyTool)
	return ok
}

const verifyMemoryQueryDenial = "memory_query is unavailable during VERIFY; use the task's authorized evidence"

func workerMemoryPhaseAllowed(phase Phase) bool {
	return !strings.EqualFold(strings.TrimSpace(string(phase)), string(PhaseVerify))
}

func (c *Coordinator) workerMemoryToolDenial(ctx context.Context, name string) string {
	if name != "memory_query" {
		return ""
	}
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	if todoID == "" || todoID == CoordTodoID {
		return ""
	}
	if item := c.todoItemByID(todoID); item != nil && !workerMemoryPhaseAllowed(item.Phase) {
		return verifyMemoryQueryDenial
	}
	if metadata, ok := invocationMetadataFromContext(ctx); ok && !workerMemoryPhaseAllowed(metadata.Phase) {
		return verifyMemoryQueryDenial
	}
	return ""
}
