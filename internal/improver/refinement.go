package improver

import (
	"context"
	"errors"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
)

func (e *Experiment) attachRoleInputs(executor *ProbeExecutor) {
	executor.roleInputBuilder = func(ctx context.Context, request StageRequest) (roleInput, error) {
		state, err := e.controller.Read(ctx)
		if err != nil {
			return roleInput{}, err
		}
		return buildRoleInput(e.Plan, state, request.Spec.Kind)
	}
}

func (e *Experiment) advanceRefinement(ctx context.Context, state LedgerState, spec StageSpec, executor *ProbeExecutor,
	settings RuntimeSettings, progress func(LedgerState)) (*ProbeExecutor, error) {
	stage := state.Stages[spec.ID]
	if stage.State != "planned" && stage.State != "completed" {
		return executor, nil
	}
	if _, exists := state.Variants[spec.Group]; exists {
		return executor, nil
	}
	if stage.State == "planned" {
		if state.StopReason != "" || state.ResourceOver || state.Budget.UnknownStages != 0 || state.Budget.ActiveStages != 0 {
			return executor, errors.New("refinement admission blocked by unresolved evidence or limits")
		}
		if _, reserved := state.Envelopes[spec.Group]; !reserved {
			if !canReserveRefinement(state, e.Plan.Limits, spec.Group) {
				return executor, e.skipRefinementGroup(ctx, spec.Group, "skipped_budget", "optional_budget")
			}
			if err := e.controller.Reserve(ctx, spec.Group); err != nil {
				return executor, err
			}
		}
	}
	if stage.State == "planned" || spec.Kind != "reflect" && retryable(&state, stage) &&
		int64(len(stage.Attempts)) <= e.Plan.Limits.StageRetries && availableTokens(&state) >= e.Plan.Limits.PerStageTokens {
		if err := e.executeRefinementAttempts(ctx, spec, executor, progress); err != nil {
			return executor, err
		}
		var err error
		state, err = e.controller.Read(ctx)
		if err != nil {
			return executor, err
		}
		stage = state.Stages[spec.ID]
		if stage == nil {
			return executor, errors.New("refinement stage disappeared")
		}
	}
	if stage.State != "completed" || len(stage.Attempts) == 0 {
		return executor, errors.New("refinement stage requires reconciliation")
	}
	attempt := stage.Attempts[len(stage.Attempts)-1]
	if attempt.Settlement == nil || attempt.Settlement.Outcome != "passed" {
		return executor, e.skipRefinementGroup(ctx, spec.Group, "failed", "proposal_unavailable")
	}
	result, err := executor.readRoleResult(ctx, requestFromAttempt(e.Plan, state, spec, attempt), *attempt.Settlement)
	if err != nil {
		return executor, err
	}
	if spec.Kind == "reflect" {
		return executor, nil
	}
	if result.Variant == nil {
		return executor, e.skipRefinementGroup(ctx, spec.Group, "skipped", "keep_baseline")
	}
	if spec.Kind == "compose" {
		for _, slot := range []string{"procedure", "domain"} {
			if bound, exists := state.Variants[slot]; exists && bound.Digest == result.Variant.VariantDigest &&
				variantFullyObserved(state, slot) {
				return executor, e.skipRefinementGroup(ctx, spec.Group, "skipped", "candidate_reused")
			}
		}
	}
	snapshot, err := freezeTree(e.objects, "team_snapshot", result.Variant.Tree)
	if err != nil {
		return executor, err
	}
	origin := "validated_proposal"
	if spec.Kind == "compose" {
		origin = "validated_composition"
	}
	if err := e.controller.BindVariant(ctx, VariantBinding{Slot: spec.Group, Snapshot: snapshot,
		Digest: result.Variant.VariantDigest, Origin: origin}); err != nil {
		return executor, err
	}
	return e.reloadExecutor(ctx, executor.children, settings)
}

func canReserveRefinement(state LedgerState, limits paired.Limits, group string) bool {
	tokens := availableTokens(&state)
	probes := limits.MaxProbeRuns - state.ProbeRuns - reservedProbes(&state)
	seconds := int64(time.Until(state.Deadline) / time.Second)
	found := false
	for _, stage := range state.Stages {
		if stage.Spec.Group != group || stage.State != "planned" {
			continue
		}
		found = true
		if tokens < limits.PerStageTokens || seconds < limits.PerStageSeconds {
			return false
		}
		tokens -= limits.PerStageTokens
		seconds -= limits.PerStageSeconds
		if stage.Spec.Kind == "executor" {
			probes--
		}
	}
	return state.StopReason == "" && !state.ResourceOver && state.Budget.UnknownStages == 0 &&
		state.Budget.ActiveStages == 0 && found && probes >= 0
}

func (e *Experiment) executeRefinementAttempts(ctx context.Context, spec StageSpec, executor *ProbeExecutor, progress func(LedgerState)) error {
	for {
		observed := progressStageExecutor{StageExecutor: executor, admitted: func() {
			if progress != nil {
				if state, err := e.controller.Read(ctx); err == nil {
					progress(state)
				}
			}
		}}
		if err := e.controller.Execute(ctx, spec.ID, observed); err != nil {
			return err
		}
		state, err := e.controller.Read(ctx)
		if err != nil {
			return err
		}
		stage := state.Stages[spec.ID]
		if spec.Kind == "reflect" || int64(len(stage.Attempts)) > e.Plan.Limits.StageRetries ||
			!retryable(&state, stage) || availableTokens(&state) < e.Plan.Limits.PerStageTokens ||
			state.StopReason != "" || state.ResourceOver || !time.Now().Before(state.Deadline) {
			return nil
		}
	}
}

func (e *Experiment) skipRefinementGroup(ctx context.Context, group, status, reason string) error {
	state, err := e.controller.Read(ctx)
	if err != nil {
		return err
	}
	specs, _, err := plannedStages(e.Plan)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if spec.Group == group && state.Stages[spec.ID].State == "planned" {
			if err := e.controller.Transition(ctx, spec.ID, 0, status, reason); err != nil {
				return err
			}
		}
	}
	return nil
}

func variantFullyObserved(state LedgerState, slot string) bool {
	found := false
	for _, stage := range state.Stages {
		if stage.Spec.Kind != "executor" || stage.Spec.Group != slot {
			continue
		}
		found = true
		if stage.State != "completed" || len(stage.Attempts) == 0 {
			return false
		}
		last := stage.Attempts[len(stage.Attempts)-1].Settlement
		if last == nil || !last.UsageKnown || last.Outcome != "passed" && last.Outcome != "failed" {
			return false
		}
	}
	return found
}

func (e *Experiment) reloadExecutor(ctx context.Context, children string, settings RuntimeSettings) (*ProbeExecutor, error) {
	state, err := e.controller.Read(ctx)
	if err != nil {
		return nil, err
	}
	var bindings []VariantBinding
	for _, binding := range state.Variants {
		bindings = append(bindings, binding)
	}
	prepared, err := LoadPrepared(e.objects, e.Plan, bindings)
	if err != nil {
		return nil, err
	}
	executor, err := NewProbeExecutor(prepared, e.Plan, e.objects, children, settings)
	if err != nil {
		return nil, err
	}
	e.prepared = prepared
	e.attachRoleInputs(executor)
	return executor, nil
}
