package improver

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
)

// ExperimentReport is shared by JSON, plain output and runtime UI adapters.
// It contains references and typed state, never prompt or response bodies.
type ExperimentReport struct {
	paired.Report
	Scope      LedgerScope          `json:"scope"`
	StartedAt  time.Time            `json:"started_at"`
	Deadline   time.Time            `json:"deadline"`
	Limits     paired.Limits        `json:"limits"`
	StopReason string               `json:"stop_reason,omitempty"`
	Stages     []StageState         `json:"stages"`
	Manifests  []paired.RunManifest `json:"manifests"`
}

// ProjectReport always validates the journal projection and imported canonical
// child evidence. publish writes immutable manifests; readers only verify.
func ProjectReport(ctx context.Context, objects Objects, plan paired.ExperimentPlan, state LedgerState, publish bool) (ExperimentReport, error) {
	bindings := make([]VariantBinding, 0, len(state.Variants))
	for _, binding := range state.Variants {
		bindings = append(bindings, binding)
	}
	prepared, err := LoadPrepared(objects, plan, bindings)
	if err != nil {
		return ExperimentReport{}, err
	}
	reader := &ProbeExecutor{
		factory: &RuntimeFactory{config: prepared.Config}, plan: plan, fixture: prepared.Fixture,
		program: prepared.Program, inputs: prepared.Inputs, objects: objects,
	}
	specs, _, err := plannedStages(plan)
	if err != nil {
		return ExperimentReport{}, err
	}
	report := ExperimentReport{
		Scope: state.Scope, StartedAt: state.StartedAt, Deadline: state.Deadline,
		Limits: plan.Limits, StopReason: state.StopReason,
	}
	evaluation := paired.Evaluation{
		ExecutionMode: plan.ExecutionMode, ComparisonContextDigest: plan.ComparisonContextDigest,
		Cases: prepared.Fixture.Cases, Replicates: prepared.Config.Replicates,
		Objective: plan.Objective, Budget: state.Budget, ProtectedSurfaceValid: true,
		ResumeConsistent: state.StopReason != "clock_rollback", ResourceOver: state.ResourceOver,
	}
	for _, slot := range plan.Slots {
		if slot.Variant != "baseline" && !slices.Contains(evaluation.Candidates, slot.Variant) {
			if _, bound := state.Variants[slot.Variant]; bound || plan.Mode == "manual" {
				evaluation.Candidates = append(evaluation.Candidates, slot.Variant)
			}
		}
	}
	var manifests []paired.ArtifactRef
	for _, spec := range specs {
		stage := state.Stages[spec.ID]
		if stage == nil {
			return ExperimentReport{}, errors.New("report stage missing from journal")
		}
		report.Stages = append(report.Stages, *stage)
		if stage.State == "blocked" {
			evaluation.ResumeConsistent = false
		}
		if spec.Kind != "executor" {
			continue
		}
		var last *paired.Observation
		var tokens int64
		for _, attempt := range stage.Attempts {
			if attempt.Settlement == nil {
				evaluation.ResumeConsistent = false
				continue
			}
			request := requestFromAttempt(plan, state, spec, attempt)
			actual, err := reader.ReadSettlement(ctx, request, attempt.Settlement.Evidence)
			if err != nil {
				return ExperimentReport{}, err
			}
			if actual != *attempt.Settlement {
				return ExperimentReport{}, errors.New("journal settlement differs from canonical child evidence")
			}
			if actual.Tokens > math.MaxInt64-tokens {
				return ExperimentReport{}, errors.New("slot usage overflow")
			}
			tokens += actual.Tokens
			outcome := actual.Outcome
			if outcome == "transient_transport" || outcome == "not_dispatched" {
				outcome = "unknown"
			}
			last = &paired.Observation{Slot: spec.Slot, ComparisonContextDigest: plan.ComparisonContextDigest,
				Outcome: outcome, UsageKnown: actual.UsageKnown, AttributableTokens: tokens, EvidenceValid: true}
			manifest, err := canonicalManifest(objects, plan, state, request, actual)
			if err != nil {
				return ExperimentReport{}, err
			}
			if len(report.Manifests) > 0 && attempt.Generation > 1 {
				previous, err := objectReference("run_manifest", report.Manifests[len(report.Manifests)-1])
				if err != nil {
					return ExperimentReport{}, err
				}
				manifest.RetryOf = &previous
			}
			report.Manifests = append(report.Manifests, manifest)
			ref, err := reportManifestReference(objects, manifest, publish, state.Report != nil)
			if err != nil {
				return ExperimentReport{}, err
			}
			if ref != nil {
				manifests = append(manifests, *ref)
			}
		}
		if last != nil {
			evaluation.Observations = append(evaluation.Observations, *last)
		}
	}
	report.Report, err = paired.Evaluate(evaluation)
	if err != nil {
		return ExperimentReport{}, err
	}
	report.PlanRef, report.ManifestRefs = &state.Plan, manifests
	if state.StopReason == "cancel_requested" && state.Budget.ActiveStages == 0 && state.Budget.UnknownStages == 0 {
		report.Decision = "cancelled"
	}
	if !publish && state.Report != nil && evaluation.ResumeConsistent {
		expected, err := paired.CanonicalJSON(report)
		if err != nil {
			return ExperimentReport{}, err
		}
		raw, err := objects.Read(*state.Report)
		if err != nil {
			return ExperimentReport{}, err
		}
		if !bytes.Equal(raw, expected) || state.Decision != report.Decision {
			return ExperimentReport{}, errors.New("published report differs from canonical projection")
		}
	}
	return report, nil
}

func reportManifestReference(objects Objects, manifest paired.RunManifest, publish, required bool) (*paired.ArtifactRef, error) {
	if publish {
		ref, err := objects.PutJSON("run_manifest", manifest)
		return &ref, err
	}
	if !required {
		return nil, nil
	}
	ref, err := objectReference("run_manifest", manifest)
	if err != nil {
		return nil, err
	}
	if _, err := objects.Read(ref); err != nil {
		return nil, err
	}
	return &ref, nil
}

func requestFromAttempt(plan paired.ExperimentPlan, state LedgerState, spec StageSpec, attempt AttemptState) StageRequest {
	deadline := attempt.AdmittedAt.Add(time.Duration(plan.Limits.PerStageSeconds) * time.Second)
	if state.Deadline.Before(deadline) {
		deadline = state.Deadline
	}
	return StageRequest{Lease: attempt.Lease, Spec: spec, Deadline: deadline, Limits: plan.Limits}
}

func canonicalManifest(objects Objects, plan paired.ExperimentPlan, state LedgerState, request StageRequest, result Settlement) (paired.RunManifest, error) {
	var stage StageEvidence
	if err := readObjectJSON(objects, result.Evidence, &stage); err != nil {
		return paired.RunManifest{}, err
	}
	variant, exists := state.Variants[request.Spec.Slot.Variant]
	if !exists {
		return paired.RunManifest{}, errors.New("executed variant was never bound")
	}
	key, err := paired.Digest("pair_key", map[string]any{
		"experiment": plan.ExperimentID, "context": plan.ComparisonContextDigest,
		"case": request.Spec.Slot.Case, "replicate": request.Spec.Slot.Replicate,
	})
	if err != nil {
		return paired.RunManifest{}, err
	}
	usage := "unknown"
	if result.UsageKnown {
		usage = "known"
	}
	models := map[string]paired.Model{}
	for role, model := range plan.Models {
		models[role] = paired.Model{Backend: model.Backend, Model: model.Model}
	}
	// The exact canonical event log contains the original policy, receipts,
	// verifier results and cost events. References point to those bytes, not
	// reconstructed receipt objects with parent-generated identities.
	log := stage.Child.EventLog
	return paired.RunManifest{
		SchemaVersion: 1, ExperimentID: plan.ExperimentID, ParentBranch: plan.ParentBranchID,
		SlotID: request.StageID, Slot: request.Spec.Slot, PairKey: key,
		Attempt: request.Generation, Generation: request.Generation, Identity: result.Identity,
		ExecutionMode: plan.ExecutionMode, Snapshot: variant.Snapshot, VariantDigest: variant.Digest,
		ComparisonContextDigest: plan.ComparisonContextDigest, EvidenceDigest: result.Evidence.Digest,
		ExecutionPolicy: log, ResolvedModels: models, Outcome: result.Outcome,
		Receipts: []paired.ArtifactRef{log}, Verifications: []paired.ArtifactRef{log},
		Usage: log, UsageCompleteness: usage,
	}, nil
}

func objectReference(kind string, value any) (paired.ArtifactRef, error) {
	raw, err := paired.CanonicalJSON(value)
	if err != nil {
		return paired.ArtifactRef{}, err
	}
	return paired.ArtifactRef{Kind: kind, Digest: paired.BytesDigest(raw), SizeBytes: int64(len(raw))}, nil
}
