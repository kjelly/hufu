package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/tools"
)

// errAgentSelectionAmbiguous is the agent matcher's fail-closed answer when
// no worker was chosen with enough confidence.
var errAgentSelectionAmbiguous = errors.New("structured agent selection was ambiguous; specify agent explicitly")

// reviewGuardCall is the coordinator's guard reviewer (§59 guard-reviewer).
// An abstention denies the call.
func (c *Coordinator) reviewGuardCall(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
	return c.reviewGuardCallLegacy(ctx, toolName, args, rules)
}

func (c *Coordinator) reviewGuardCallLegacy(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
	s := c.semanticSidecar(c.AgentPool().GuardSidecar())
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	result, err := s.ReviewToolCall(sidecar.WithPurpose(ctx, "guard_reviewer"), agentName, toolName, args, rules)
	if err != nil {
		return false, "", err
	}
	return result.Approved, result.Reason, nil
}

// reviewPathAccess is the coordinator's bash path reviewer (§59
// path-reviewer). Only an accepted "not a file access" drops a path; an
// abstention keeps it, so the consent check still runs.
func (c *Coordinator) reviewPathAccess(ctx context.Context, command, path string) (bool, error) {
	return c.reviewPathAccessLegacy(ctx, command, path)
}

func (c *Coordinator) reviewPathAccessLegacy(ctx context.Context, command, path string) (bool, error) {
	return c.semanticSidecar(c.AgentPool().Sidecar()).ReviewPathAccess(sidecar.WithPurpose(ctx, "path_reviewer"), command, path)
}

// chooseAskUserResponse is the unattended ask_user selector (§59 ask-user).
// The decision model applies only to single_choice questions; an abstention
// returns tools.ErrAskUserAbstained instead of guessing an option.
func (c *Coordinator) chooseAskUserResponse(ctx context.Context, question, qtype string, opts []tools.AskUserTUIOption, allowAny bool) (tools.AskUserResponse, error) {
	return c.semanticSidecar(c.AgentPool().Sidecar()).ChooseAskUserResponse(ctx, question, qtype, opts, allowAny)
}

// selectAgentForGoal picks the worker for request_agent when none is named
// (§59 agent-matcher). An abstention fails closed.
func (c *Coordinator) selectAgentForGoal(ctx context.Context, goal string) (string, error) {
	workers := c.uniqueWorkerDefs()
	if len(workers) == 0 {
		return "", fmt.Errorf("no workers available")
	}
	if len(workers) == 1 {
		return workers[0].Name, nil
	}
	slices.SortFunc(workers, func(a, b *agent.AgentDef) int { return strings.Compare(a.Name, b.Name) })
	return c.selectAgentForGoalLegacy(ctx, goal, workers)
}

func (c *Coordinator) selectAgentForGoalLegacy(ctx context.Context, goal string, workers []*agent.AgentDef) (string, error) {
	s := c.semanticSidecar(c.AgentPool().Sidecar())
	if s == nil {
		_ = c.recordAuxiliaryFallback(ctx, "agent_matcher", "fail_closed")
		return "", fmt.Errorf("cannot select among multiple workers without a structured agent resolver; specify agent explicitly")
	}
	candidates := make([]sidecar.TeamSummary, 0, len(workers))
	for _, w := range workers {
		candidates = append(candidates, sidecar.TeamSummary{Name: w.Name, Description: w.Description})
	}
	selection, err := s.SelectAgent(sidecar.WithPurpose(ctx, "agent_matcher"), goal, candidates)
	if err != nil {
		_ = c.recordAuxiliaryFallback(ctx, "agent_matcher", "fail_closed")
		return "", fmt.Errorf("structured agent selection failed: %w", err)
	}
	if selection.Agent == "" {
		_ = c.recordAuxiliaryFallback(ctx, "agent_matcher", "abstained")
		return "", errAgentSelectionAmbiguous
	}
	return selection.Agent, nil
}
