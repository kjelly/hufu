package sidecar

import (
	"context"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
)

// DecisionInvocation is constructed by runtime code, never decoded from model
// output. Input is the complete accepted snapshot; candidates are ordered and
// fixed before inference. The owner supplies the question and call limit.
type DecisionInvocation struct {
	Name     string
	Question string
	Input    any
	Options  []catalog.Option
	MaxCalls int
}

type DecisionExecutor func(context.Context, DecisionInvocation) (decisionrt.Result, error)

func (s *Sidecar) SetDecisionExecutor(executor DecisionExecutor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisionExecutor = executor
}

// Decide never falls back to free-form generation or parses model prose.
func (s *Sidecar) Decide(ctx context.Context, invocation DecisionInvocation) (decisionrt.Result, error) {
	if s == nil {
		return decisionrt.Result{}, fmt.Errorf("decision runtime unavailable")
	}
	s.mu.Lock()
	executor := s.decisionExecutor
	s.mu.Unlock()
	if executor == nil {
		return decisionrt.Result{}, fmt.Errorf("durable decision executor unavailable")
	}
	result, err := executor(ctx, invocation)
	if err != nil {
		return decisionrt.Result{}, err
	}
	if result.Status == decisionrt.StatusDecided && !slices.ContainsFunc(invocation.Options, func(option catalog.Option) bool { return option.ID == result.Value.Choice }) {
		return decisionrt.Result{}, fmt.Errorf("decision returned unknown option")
	}
	if result.Status != decisionrt.StatusDecided && result.Status != decisionrt.StatusAbstained {
		return decisionrt.Result{}, fmt.Errorf("invalid decision status")
	}
	return result, nil
}

func (s *Sidecar) selectCandidate(ctx context.Context, name, question, prompt string, candidates []TeamSummary) (int, error) {
	if s == nil {
		return -1, fmt.Errorf("sidecar not initialized")
	}
	if len(candidates) == 0 {
		return -1, nil
	}
	options := []catalog.Option{{ID: "none"}}
	for i := range candidates {
		options = append(options, catalog.Option{ID: fmt.Sprintf("candidate_%d", i), Description: fmt.Sprintf("Candidate at index %d in the accepted snapshot", i)})
	}
	result, err := s.Decide(ctx, DecisionInvocation{Name: name, Question: question, Input: struct {
		Prompt     string
		Candidates []TeamSummary
	}{prompt, candidates}, Options: options, MaxCalls: 1000})
	if err != nil {
		return -1, err
	}
	if result.Status != decisionrt.StatusDecided {
		return -1, fmt.Errorf("decision abstained")
	}
	if result.Value.Choice == "none" {
		return -1, nil
	}
	for i := range candidates {
		if result.Value.Choice == options[i+1].ID {
			return i, nil
		}
	}
	return -1, fmt.Errorf("decision returned unknown candidate")
}
