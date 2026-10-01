package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/tools"
)

// errAgentSelectionAmbiguous is the agent matcher's fail-closed answer when
// no worker was chosen with enough confidence.
var errAgentSelectionAmbiguous = errors.New("structured agent selection was ambiguous; specify agent explicitly")

const (
	guardDeniedByDecisionModel = "the decision model judged that this call violates a guard rule"
	guardAbstainedReason       = "the decision model was not confident enough to approve this call"
)

// reviewGuardCall is the coordinator's guard reviewer (§59 guard-reviewer).
// An abstention denies the call.
func (c *Coordinator) reviewGuardCall(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
	if c.controlDecisions.Mode(control.GuardReviewer) == control.ModeOff {
		return c.reviewGuardCallLegacy(ctx, toolName, args, rules)
	}
	type verdict struct {
		approved bool
		reason   string
	}
	legacy := func() controlLeg[verdict] {
		approved, reason, err := c.reviewGuardCallLegacy(ctx, toolName, args, rules)
		return controlLeg[verdict]{value: verdict{approved, reason}, err: err, code: controlBoolCode(approved, err)}
	}
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	request, ok := control.GuardReviewerRequest(agentName, toolName, args, rules)
	if !ok {
		leg := legacy()
		return leg.value.approved, leg.value.reason, leg.err
	}
	result, err := runControlPlan(c, ctx, controlPlan[verdict]{
		point: control.GuardReviewer, request: request, legacy: legacy,
		primitive: func(outcome control.Outcome) (verdict, bool) {
			approved, ok := outcome.Bool()
			if !approved {
				return verdict{reason: guardDeniedByDecisionModel}, ok
			}
			return verdict{approved: true}, ok
		},
		safe: func() (verdict, error) { return verdict{reason: guardAbstainedReason}, nil },
	})
	return result.approved, result.reason, err
}

func (c *Coordinator) reviewGuardCallLegacy(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
	s := c.AgentPool().GuardSidecar()
	prof := c.ExecutionProfile()
	if s == nil {
		if err := c.recordAuxiliaryFallback(ctx, "guard_reviewer", "no_model_fallback"); err != nil {
			return false, "", err
		}
		if prof.PolicyFailureMode == PolicyFailClosed || prof.StrictPolicy {
			return false, "guard reviewer unavailable under PolicyFailClosed policy", fmt.Errorf("guard reviewer unavailable")
		}
		return true, "", nil
	}
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	result, err := s.ReviewToolCall(sidecar.WithPurpose(ctx, "guard_reviewer"), agentName, toolName, args, rules)
	if err != nil {
		if prof.PolicyFailureMode == PolicyFailOpen {
			return true, "", nil
		}
		return false, "", err
	}
	return result.Approved, result.Reason, nil
}

// reviewPathAccess is the coordinator's bash path reviewer (§59
// path-reviewer). Only an accepted "not a file access" drops a path; an
// abstention keeps it, so the consent check still runs.
func (c *Coordinator) reviewPathAccess(ctx context.Context, command, path string) (bool, error) {
	if c.controlDecisions.Mode(control.PathReviewer) == control.ModeOff {
		return c.reviewPathAccessLegacy(ctx, command, path)
	}
	legacy := func() controlLeg[bool] {
		isFileAccess, err := c.reviewPathAccessLegacy(ctx, command, path)
		return controlLeg[bool]{value: isFileAccess, err: err, code: controlBoolCode(isFileAccess, err)}
	}
	request, ok := control.PathReviewerRequest(command, path)
	if !ok {
		leg := legacy()
		return leg.value, leg.err
	}
	return runControlPlan(c, ctx, controlPlan[bool]{
		point: control.PathReviewer, request: request, legacy: legacy,
		primitive: func(outcome control.Outcome) (bool, bool) { return outcome.Bool() },
		safe:      func() (bool, error) { return true, nil },
	})
}

func (c *Coordinator) reviewPathAccessLegacy(ctx context.Context, command, path string) (bool, error) {
	s := c.AgentPool().Sidecar()
	if s == nil {
		if err := c.recordAuxiliaryFallback(ctx, "path_reviewer", "no_model_fallback"); err != nil {
			return false, err
		}
		return true, nil
	}
	return s.ReviewPathAccess(sidecar.WithPurpose(ctx, "path_reviewer"), command, path)
}

// chooseAskUserResponse is the unattended ask_user selector (§59 ask-user).
// The decision model applies only to single_choice questions; an abstention
// returns tools.ErrAskUserAbstained instead of guessing an option.
func (c *Coordinator) chooseAskUserResponse(ctx context.Context, question, qtype string, opts []tools.AskUserTUIOption, allowAny bool) (tools.AskUserResponse, error) {
	legacyChoice := func() (tools.AskUserResponse, error) {
		s := c.AgentPool().Sidecar()
		if s == nil {
			return tools.AskUserResponse{}, fmt.Errorf("no sidecar configured")
		}
		return s.ChooseAskUserResponse(ctx, question, qtype, opts, allowAny)
	}
	if c.controlDecisions.Mode(control.AskUser) == control.ModeOff || qtype != "single_choice" {
		return legacyChoice()
	}
	options := make([]control.Option, 0, len(opts))
	for _, option := range opts {
		options = append(options, control.Option{Label: option.Label, Value: option.Value})
	}
	request, ok := control.AskUserRequest(question, options)
	if !ok {
		return legacyChoice()
	}
	return runControlPlan(c, ctx, controlPlan[tools.AskUserResponse]{
		point: control.AskUser, request: request, candidates: len(opts),
		legacy: func() controlLeg[tools.AskUserResponse] {
			response, err := legacyChoice()
			return controlLeg[tools.AskUserResponse]{value: response, err: err, code: askUserControlCode(response, err, opts)}
		},
		primitive: func(outcome control.Outcome) (tools.AskUserResponse, bool) {
			index, ok := outcome.Index()
			if !ok || index >= len(opts) {
				return tools.AskUserResponse{}, false
			}
			return tools.AskUserResponse{Answers: []string{askUserOptionValue(opts[index])}}, true
		},
		safe: func() (tools.AskUserResponse, error) { return tools.AskUserResponse{}, tools.ErrAskUserAbstained },
	})
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
	if c.controlDecisions.Mode(control.AgentMatcher) == control.ModeOff {
		return c.selectAgentForGoalLegacy(ctx, goal, workers)
	}
	// uniqueWorkerDefs follows map order; a stable order keeps the decision
	// request and its option indexes reproducible.
	workers = slices.Clone(workers)
	slices.SortFunc(workers, func(a, b *agent.AgentDef) int { return strings.Compare(a.Name, b.Name) })
	candidates := make([]control.Worker, 0, len(workers))
	for _, worker := range workers {
		candidates = append(candidates, control.Worker{Name: worker.Name, Description: worker.Description})
	}
	request, ok := control.AgentMatcherRequest(goal, candidates)
	if !ok {
		return c.selectAgentForGoalLegacy(ctx, goal, workers)
	}
	return runControlPlan(c, ctx, controlPlan[string]{
		point: control.AgentMatcher, request: request, candidates: len(workers),
		legacy: func() controlLeg[string] {
			name, err := c.selectAgentForGoalLegacy(ctx, goal, workers)
			return controlLeg[string]{value: name, err: err, code: agentMatcherControlCode(name, err, workers)}
		},
		primitive: func(outcome control.Outcome) (string, bool) {
			index, ok := outcome.Index()
			if !ok || index >= len(workers) {
				return "", false
			}
			return workers[index].Name, true
		},
		safe: func() (string, error) {
			_ = c.recordAuxiliaryFallback(ctx, "agent_matcher", "abstained")
			return "", errAgentSelectionAmbiguous
		},
	})
}

func (c *Coordinator) selectAgentForGoalLegacy(ctx context.Context, goal string, workers []*agent.AgentDef) (string, error) {
	s := c.AgentPool().Sidecar()
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

func controlBoolCode(value bool, err error) string {
	if err != nil {
		return controlLegacyError
	}
	return strconv.FormatBool(value)
}

func agentMatcherControlCode(name string, err error, workers []*agent.AgentDef) string {
	switch {
	case errors.Is(err, errAgentSelectionAmbiguous):
		return controlLegacyAbstain
	case err != nil:
		return controlLegacyError
	}
	for index, worker := range workers {
		if worker.Name == name {
			return strconv.Itoa(index)
		}
	}
	return controlLegacyInvalid
}

func askUserOptionValue(option tools.AskUserTUIOption) string {
	if value := strings.TrimSpace(option.Value); value != "" {
		return value
	}
	return strings.TrimSpace(option.Label)
}

func askUserControlCode(response tools.AskUserResponse, err error, opts []tools.AskUserTUIOption) string {
	if err != nil {
		return controlLegacyError
	}
	if len(response.Answers) == 0 {
		return controlLegacyInvalid
	}
	answer := strings.TrimSpace(response.Answers[0])
	for index, option := range opts {
		if answer == askUserOptionValue(option) || strings.EqualFold(answer, strings.TrimSpace(option.Label)) {
			return strconv.Itoa(index)
		}
	}
	return controlLegacyInvalid
}
