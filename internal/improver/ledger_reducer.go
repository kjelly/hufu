package improver

import (
	"bytes"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

// ReplayLedger is pure: no filesystem access, dispatch, repair, or clock reads.
// The reader verifies the journal chain and artifact refs before using it.
func ReplayLedger(events []team.RunEvent, scope LedgerScope, plan paired.ExperimentPlan) (LedgerState, error) {
	specs, initial, err := plannedStages(plan)
	if err != nil {
		return LedgerState{}, err
	}
	if scope.ExperimentID != plan.ExperimentID || scope.BranchID != plan.ParentBranchID ||
		scope.WorkspaceID != plan.ParentWorkspaceID || !opaqueID(scope.RunID) || !opaqueID(scope.SessionID) {
		return LedgerState{}, errors.New("ledger: scope differs from frozen plan")
	}
	state := LedgerState{
		Scope: scope, Stages: map[string]*StageState{}, Variants: map[string]VariantBinding{},
		Envelopes: map[string]int64{}, Budget: paired.BudgetSummary{LimitTokens: plan.Limits.PipelineTokens},
	}
	for _, spec := range specs {
		state.Stages[spec.ID] = &StageState{Spec: spec, State: "planned"}
	}
	type committedKey struct {
		kind    string
		payload []byte
	}
	keys := map[string]committedKey{}
	for _, event := range events {
		state.Head = event.ID // CAS fences the global chain, including other runs.
		if event.BranchID != scope.BranchID || !strings.HasPrefix(event.Type, "improvement_") {
			continue
		}
		var meta eventMeta
		if err := json.Unmarshal(event.Payload, &meta); err != nil {
			return LedgerState{}, fmt.Errorf("ledger: invalid event metadata: %w", err)
		}
		if meta.ExperimentID != scope.ExperimentID {
			continue
		}
		if event.RunID != scope.RunID || event.SessionID != scope.SessionID || event.Actor != "improver" ||
			meta.SchemaVersion != 1 || meta.ObservedAt.IsZero() || event.IdempotencyKey == "" {
			return LedgerState{}, errors.New("ledger: conflicting event scope or version")
		}
		canonical, err := paired.CanonicalJSON(event.Payload)
		if err != nil {
			return LedgerState{}, err
		}
		if old, exists := keys[event.IdempotencyKey]; exists {
			if old.kind != event.Type || !bytes.Equal(old.payload, canonical) {
				return LedgerState{}, errors.New("ledger: conflicting idempotency key")
			}
			continue
		}
		keys[event.IdempotencyKey] = committedKey{event.Type, canonical}
		if meta.ObservedAt.Before(state.ObservedAt) {
			return LedgerState{}, errors.New("ledger: clock observation moved backwards")
		}
		if err := applyLedgerEvent(&state, plan, initial, event); err != nil {
			return LedgerState{}, fmt.Errorf("ledger %s: %w", event.Type, err)
		}
		state.ObservedAt = meta.ObservedAt
		if err := summarizeBudget(&state, plan.Limits); err != nil {
			return LedgerState{}, err
		}
	}
	return state, nil
}

func decodeEvent[T any](event team.RunEvent) (T, error) {
	var value T
	err := json.Unmarshal(event.Payload, &value, json.RejectUnknownMembers(true))
	return value, err
}

func applyLedgerEvent(s *LedgerState, p paired.ExperimentPlan, initial []string, e team.RunEvent) error {
	if e.Type == eventCreated {
		return createLedger(s, p, initial, e)
	}
	if !s.Created || s.Report != nil && e.Type != eventTransition {
		return errors.New("run absent or finished")
	}
	switch e.Type {
	case eventReserved:
		value, err := decodeEvent[envelopeReserved](e)
		if err != nil {
			return err
		}
		if err := canAdmit(s, value.ObservedAt); err != nil {
			return err
		}
		return reserveGroup(s, p.Limits, value.Group)
	case eventVariant:
		value, err := decodeEvent[variantBound](e)
		if err != nil {
			return err
		}
		return bindVariant(s, value.VariantBinding)
	case eventAdmitted:
		value, err := decodeEvent[stageAdmitted](e)
		if err != nil {
			return err
		}
		return admitStage(s, p.Limits, value)
	case eventBound:
		value, err := decodeEvent[stageBound](e)
		if err != nil {
			return err
		}
		return bindStage(s, value)
	case eventStarted:
		value, err := decodeEvent[stageStarted](e)
		if err != nil {
			return err
		}
		stage, attempt, err := currentAttempt(s, value.Lease)
		if err != nil {
			return err
		}
		if attempt.State != "bound" || s.StopReason != "" || !value.ObservedAt.Before(s.Deadline) ||
			!value.ObservedAt.Before(attempt.AdmittedAt.Add(time.Duration(p.Limits.PerStageSeconds)*time.Second)) {
			return errors.New("dispatch requires live binding and deadline")
		}
		stage.State, attempt.State = "running", "running"
		return nil
	case eventCompleted:
		value, err := decodeEvent[stageCompleted](e)
		if err != nil {
			return err
		}
		return completeStage(s, value)
	case eventTransition:
		value, err := decodeEvent[stageTransition](e)
		if err != nil {
			return err
		}
		if s.Report != nil && value.Reason != "completion_conflict" {
			return errors.New("finished run only accepts a completion conflict")
		}
		if err := transitionStage(s, value, p.Limits); err != nil {
			return err
		}
		if s.Report != nil {
			// Keep the historical report ref for audit, but never present its
			// prior decision as current after contradictory evidence.
			s.Decision = "blocked"
		}
		return nil
	case eventStopped:
		value, err := decodeEvent[admissionStopped](e)
		if err != nil {
			return err
		}
		if !slices.Contains([]string{"cancel_requested", "deadline_exceeded", "clock_rollback"}, value.Reason) {
			return errors.New("invalid stop reason")
		}
		if s.StopReason != "" && s.StopReason != value.Reason {
			return errors.New("conflicting stop reason")
		}
		s.StopReason = value.Reason
		return nil
	case eventFinished:
		return finishLedger(s, p, e)
	default:
		return errors.New("unsupported improvement event")
	}
}

func createLedger(s *LedgerState, p paired.ExperimentPlan, initial []string, event team.RunEvent) error {
	value, err := decodeEvent[runCreated](event)
	if err != nil {
		return err
	}
	if s.Created || value.Plan.Validate("experiment_plan") != nil ||
		!value.Deadline.Equal(value.ObservedAt.Add(time.Duration(p.Limits.PipelineSeconds)*time.Second)) {
		return errors.New("invalid creation or deadline")
	}
	s.Created, s.Plan = true, value.Plan
	s.StartedAt, s.Deadline = value.ObservedAt, value.Deadline
	for _, group := range initial {
		if err := reserveGroup(s, p.Limits, group); err != nil {
			return err
		}
	}
	return nil
}

func finishLedger(s *LedgerState, p paired.ExperimentPlan, event team.RunEvent) error {
	value, err := decodeEvent[runFinished](event)
	if err != nil {
		return err
	}
	if value.Report.Validate("experiment_report") != nil || s.Budget.ActiveStages > 0 ||
		s.Budget.UnknownStages > 0 || s.Budget.ReservedTokens > 0 {
		return errors.New("cannot finish an unsettled run")
	}
	if !slices.Contains([]string{"eligible_for_review", "no_change", "rejected", "simulation_only", "inconclusive", "cancelled"}, value.Decision) ||
		p.ExecutionMode == "fixture" && slices.Contains([]string{"eligible_for_review", "no_change", "rejected"}, value.Decision) {
		return errors.New("invalid finished decision")
	}
	s.Report, s.Decision = &value.Report, value.Decision
	return nil
}

func canAdmit(s *LedgerState, now time.Time) error {
	if s.StopReason != "" || s.ResourceOver || s.Budget.ActiveStages != 0 || s.Budget.UnknownStages != 0 {
		return errors.New("admission blocked by unresolved work or limits")
	}
	if !now.Before(s.Deadline) {
		return errors.New("pipeline deadline exceeded")
	}
	return nil
}

func reserveGroup(s *LedgerState, limits paired.Limits, group string) error {
	if _, exists := s.Envelopes[group]; exists {
		return errors.New("envelope already reserved")
	}
	count := int64(0)
	probes := int64(0)
	for _, stage := range s.Stages {
		if stage.Spec.Group == group {
			if stage.State != "planned" {
				return errors.New("cannot reserve a group after a stage transition")
			}
			count++
			if stage.Spec.Kind == "executor" {
				probes++
			}
		}
	}
	if count == 0 || count > math.MaxInt64/limits.PerStageTokens {
		return errors.New("invalid envelope")
	}
	cost := count * limits.PerStageTokens
	if err := summarizeBudget(s, limits); err != nil {
		return err
	}
	if cost > availableTokens(s) || probes > limits.MaxProbeRuns-s.ProbeRuns-reservedProbes(s) {
		return errors.New("insufficient complete validation envelope")
	}
	s.Envelopes[group] = cost
	return nil
}

func availableTokens(s *LedgerState) int64 {
	if s.Budget.SpentTokens >= s.Budget.LimitTokens {
		return 0
	}
	return max(0, s.Budget.LimitTokens-s.Budget.SpentTokens-s.Budget.ReservedTokens)
}

func reservedProbes(s *LedgerState) int64 {
	var n int64
	for _, stage := range s.Stages {
		_, reserved := s.Envelopes[stage.Spec.Group]
		if reserved && stage.State == "planned" && stage.Spec.Kind == "executor" {
			n++
		}
	}
	return n
}

func bindVariant(s *LedgerState, v VariantBinding) error {
	exists := false
	for _, stage := range s.Stages {
		exists = exists || stage.Spec.Kind == "executor" && stage.Spec.Group == v.Slot
	}
	if !exists || v.Snapshot.Validate("team_snapshot") != nil ||
		(paired.ArtifactRef{Kind: "variant", Digest: v.Digest}).Validate("variant") != nil ||
		!slices.Contains([]string{"frozen_input", "validated_proposal", "validated_composition"}, v.Origin) {
		return errors.New("invalid variant binding")
	}
	if old, ok := s.Variants[v.Slot]; ok && old != v {
		return errors.New("variant binding is immutable")
	}
	s.Variants[v.Slot] = v
	return nil
}

func admitStage(s *LedgerState, limits paired.Limits, v stageAdmitted) error {
	if err := canAdmit(s, v.ObservedAt); err != nil {
		return err
	}
	stage := s.Stages[v.StageID]
	if stage == nil || v.Generation != int64(len(stage.Attempts))+1 ||
		v.Generation > limits.StageRetries+1 || !opaqueID(v.Owner) || !opaqueID(v.ChildWorkspaceID) ||
		v.ReservedTokens != limits.PerStageTokens {
		return errors.New("invalid stage generation or reservation")
	}
	for _, existing := range s.Stages {
		for _, attempt := range existing.Attempts {
			if attempt.ChildWorkspaceID == v.ChildWorkspaceID {
				return errors.New("child workspace identity reused")
			}
		}
	}
	if stage.Spec.Kind == "executor" {
		if _, ok := s.Variants[stage.Spec.Group]; !ok {
			return errors.New("executor variant is not bound")
		}
		if s.ProbeRuns >= limits.MaxProbeRuns {
			return errors.New("probe attempt limit reached")
		}
	}
	if v.Generation == 1 {
		if stage.State != "planned" || s.Envelopes[stage.Spec.Group] < limits.PerStageTokens {
			return errors.New("stage has no reserved envelope")
		}
		s.Envelopes[stage.Spec.Group] -= limits.PerStageTokens
	} else {
		if !retryable(s, stage) || availableTokens(s) < limits.PerStageTokens ||
			stage.Spec.Kind == "executor" && limits.MaxProbeRuns-s.ProbeRuns <= reservedProbes(s) {
			return errors.New("retry not permitted or budget unavailable")
		}
	}
	stage.State, stage.Reason = "admitted", ""
	stage.Attempts = append(stage.Attempts, AttemptState{Lease: v.Lease, State: "admitted", Reserved: v.ReservedTokens, AdmittedAt: v.ObservedAt})
	if stage.Spec.Kind == "executor" {
		s.ProbeRuns++
	}
	return nil
}

func retryable(s *LedgerState, stage *StageState) bool {
	if len(stage.Attempts) == 0 || stage.State != "completed" {
		return false
	}
	last := stage.Attempts[len(stage.Attempts)-1].Settlement
	if last == nil || !last.Stopped || !last.UsageKnown {
		return false
	}
	_, bound := s.Variants[stage.Spec.Group]
	return last.Outcome == "transient_transport" ||
		last.Outcome == "invalid_proposal" && stage.Spec.Kind != "executor" && !bound
}

func currentAttempt(s *LedgerState, lease Lease) (*StageState, *AttemptState, error) {
	stage := s.Stages[lease.StageID]
	if stage == nil || len(stage.Attempts) == 0 {
		return nil, nil, errors.New("stage has no admission")
	}
	attempt := &stage.Attempts[len(stage.Attempts)-1]
	if attempt.Lease != lease {
		return nil, nil, errors.New("stale generation or conflicting ownership")
	}
	return stage, attempt, nil
}

func bindStage(s *LedgerState, value stageBound) error {
	stage, attempt, err := currentAttempt(s, value.Lease)
	if err != nil {
		return err
	}
	if attempt.State != "admitted" || !validRuntimeIdentity(value.Identity) || s.StopReason != "" {
		return errors.New("invalid child binding")
	}
	for _, other := range s.Stages {
		for _, old := range other.Attempts {
			if old.Identity != nil && (old.Identity.InvocationID == value.Identity.InvocationID || old.Identity.RunID == value.Identity.RunID) {
				return errors.New("runtime child identity reused")
			}
		}
	}
	attempt.Identity = &value.Identity
	attempt.State, stage.State = "bound", "bound"
	return nil
}

func validRuntimeIdentity(id paired.RuntimeIdentity) bool {
	return opaqueID(id.InvocationID) && opaqueID(id.RunID) && opaqueID(id.BranchID)
}

func completeStage(s *LedgerState, value stageCompleted) error {
	stage, attempt, err := currentAttempt(s, value.Lease)
	if err != nil {
		return err
	}
	result := value.Settlement
	if result.Evidence.Validate("stage_evidence") != nil || !result.Stopped || result.Tokens < 0 ||
		!slices.Contains([]string{"passed", "failed", "timed_out", "transient_transport", "invalid_proposal", "cancelled", "unknown", "not_dispatched"}, result.Outcome) {
		return errors.New("invalid or undrained settlement")
	}
	if attempt.Identity == nil {
		if result.Identity != (paired.RuntimeIdentity{}) || result.Outcome != "not_dispatched" || !result.UsageKnown || result.Tokens != 0 {
			return errors.New("unbound settlement requires proof of no dispatch")
		}
	} else if result.Identity != *attempt.Identity {
		return errors.New("settlement identity mismatch")
	}
	if previous := attempt.Settlement; previous != nil {
		if reflect.DeepEqual(*previous, result) {
			return nil
		}
		if previous.UsageKnown || !result.UsageKnown || result.Tokens < previous.Tokens || previous.Outcome != result.Outcome {
			return errors.New("conflicting completion")
		}
	}
	attempt.Settlement = &result
	if !result.UsageKnown {
		attempt.State, stage.State = "blocked", "blocked"
		attempt.Reason, stage.Reason = "usage_unknown", "usage_unknown"
	} else {
		attempt.Reserved = 0
		attempt.State, stage.State = "completed", "completed"
		attempt.Reason, stage.Reason = "", ""
	}
	return nil
}

func transitionStage(s *LedgerState, value stageTransition, limits paired.Limits) error {
	stage := s.Stages[value.StageID]
	if stage == nil || !paired.ValidID(value.Reason) {
		return errors.New("invalid transition target or reason")
	}
	if value.Generation == 0 {
		if stage.State != "planned" || !slices.Contains([]string{"skipped_budget", "cancelled", "failed"}, value.State) {
			return errors.New("invalid planned transition")
		}
		if _, exists := s.Envelopes[stage.Spec.Group]; exists {
			if s.Envelopes[stage.Spec.Group] < limits.PerStageTokens {
				return errors.New("envelope underflow")
			}
			s.Envelopes[stage.Spec.Group] -= limits.PerStageTokens
		}
		stage.State, stage.Reason = value.State, value.Reason
		return nil
	}
	if len(stage.Attempts) == 0 || stage.Attempts[len(stage.Attempts)-1].Generation != value.Generation || value.State != "blocked" {
		return errors.New("unsettled attempt can only be blocked")
	}
	attempt := &stage.Attempts[len(stage.Attempts)-1]
	if attempt.State == "completed" && value.Reason != "completion_conflict" {
		return errors.New("cannot change a settled attempt")
	}
	attempt.State, stage.State = value.State, value.State
	attempt.Reason, stage.Reason = value.Reason, value.Reason
	return nil
}

func summarizeBudget(s *LedgerState, limits paired.Limits) error {
	budget := paired.BudgetSummary{LimitTokens: limits.PipelineTokens}
	add := func(total *int64, value int64) error {
		if value < 0 || value > math.MaxInt64-*total {
			return errors.New("ledger: budget overflow")
		}
		*total += value
		return nil
	}
	for _, remaining := range s.Envelopes {
		if err := add(&budget.ReservedTokens, remaining); err != nil {
			return err
		}
	}
	for _, stage := range s.Stages {
		for _, attempt := range stage.Attempts {
			if err := add(&budget.ReservedTokens, attempt.Reserved); err != nil {
				return err
			}
			if attempt.Settlement == nil {
				budget.ActiveStages++
				budget.UnknownStages++
			} else {
				if err := add(&budget.SpentTokens, attempt.Settlement.Tokens); err != nil {
					return err
				}
				if !attempt.Settlement.UsageKnown {
					budget.UnknownStages++
				}
				if attempt.Settlement.Tokens > limits.PerStageTokens {
					s.ResourceOver = true
				}
			}
			if attempt.Reason == "completion_conflict" {
				budget.UnknownStages++
			}
		}
	}
	s.Budget = budget
	s.ResourceOver = s.ResourceOver || budget.SpentTokens > limits.PipelineTokens
	return nil
}
