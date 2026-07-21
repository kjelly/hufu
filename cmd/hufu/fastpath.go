package main

import (
	"context"

	"github.com/anomalyco/hufu/internal/team"
)

// fastPathDispatch abstracts the two execution entry points so the fast-path
// escalation loop is unit-testable without a live LLM provider. In production
// the runDirect closure calls (*team.Coordinator).RunDirectAgent and the
// canEscalate closure delegates to ExecutionRouter.CanEscalateToTeam.
type fastPathDispatch struct {
	runDirect   func(ctx context.Context, agentName, task string) (*team.DirectAgentResult, error)
	canEscalate func(currentRoute RouteDecision, stepCount, errorCount int, requiresMultiAgent bool) (bool, string)
}

// fastPathOutcome is the result of a fast-path attempt loop.
type fastPathOutcome struct {
	output    string
	err       error
	attempted bool // true if at least one direct dispatch was performed
	escalated bool // true if the loop signalled escalation to the team path
}

// shouldUseFastPath reports whether the resolved route warrants a fast-path
// direct dispatch for the given team. It returns true only when the route is
// Fast and the team has exactly one worker agent (PrimaryWorkerName != "").
// Multi-worker teams fall through to the team path so the coordinator can pick
// the right specialist rather than guessing.
func shouldUseFastPath(route RouteDecision, coordinator *team.Coordinator) bool {
	if route.Route != RouteFast {
		return false
	}
	return coordinator != nil && coordinator.PrimaryWorkerName() != ""
}

// runFastPath executes the prompt via a single direct agent, retrying up to
// maxFastAttempts times. When canEscalate signals escalation (e.g. on repeated
// failure, errorCount >= 2, or a blown step budget), it returns escalated=true
// so the caller falls through to the team path (coordinator.Run). On success it
// returns the agent output with attempted=true and escalated=false.
func runFastPath(ctx context.Context, primaryWorker, content string, route RouteDecision, d fastPathDispatch) fastPathOutcome {
	if primaryWorker == "" {
		// No single worker to dispatch to; caller falls through to team path.
		return fastPathOutcome{}
	}
	const maxFastAttempts = 2
	errCount := 0
	for attempt := 1; attempt <= maxFastAttempts; attempt++ {
		res, derr := d.runDirect(ctx, primaryWorker, content)
		if derr == nil && res != nil && res.Error == nil {
			return fastPathOutcome{output: res.Output, attempted: true}
		}
		errCount++
		stepCount := 0
		if res != nil {
			stepCount = res.Steps
		}
		if escalate, _ := d.canEscalate(route, stepCount, errCount, false); escalate {
			stderrLog("\n%s Fast path escalating to team path after %d attempt(s) [%d error(s), %d step(s)].\n",
				boldStyle.Render("⇡"), attempt, errCount, stepCount)
			return fastPathOutcome{attempted: true, escalated: true}
		}
	}
	// Exhausted retries without an explicit escalation signal. Fall through to
	// the team path so the coordinator can retry/recover with full context.
	return fastPathOutcome{attempted: true, escalated: true}
}
