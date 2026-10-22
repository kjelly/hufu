package improver

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"slices"

	"github.com/kjelly/hufu/internal/improve/paired"
)

type stageTask struct {
	role     string
	slot     string
	input    string
	verifier paired.Verifier
}

func (e *ProbeExecutor) prepareStageTask(ctx context.Context, request StageRequest) (stageTask, *paired.ArtifactRef, error) {
	if request.Spec.Kind == "executor" {
		task, err := e.readStageTask(ctx, StageEvidence{Request: request})
		return task, nil, err
	}
	if e.roleInputBuilder == nil {
		return stageTask{}, nil, errors.New("refinement requires a committed evidence view")
	}
	input, err := e.roleInputBuilder(ctx, request)
	if err != nil {
		return stageTask{}, nil, err
	}
	_, _, _, err = e.renderRoleInput(ctx, input)
	if err != nil {
		return stageTask{}, nil, err
	}
	ref, err := e.objects.PutJSON("role_input", input)
	if err != nil {
		return stageTask{}, nil, err
	}
	task, err := e.readStageTask(ctx, StageEvidence{Request: request, RoleInput: &ref})
	return task, &ref, err
}

func (e *ProbeExecutor) readStageTask(ctx context.Context, stage StageEvidence) (stageTask, error) {
	request := stage.Request
	if err := e.validateRequest(request); err != nil {
		return stageTask{}, err
	}
	role, err := roleForStage(request.Spec.Kind)
	if err != nil {
		return stageTask{}, err
	}
	if role == "executor" {
		if stage.RoleInput != nil {
			return stageTask{}, errors.New("executor cannot consume auxiliary role input")
		}
		item, input, err := e.checkRequest(request)
		return stageTask{role, request.Spec.Slot.Variant, input, item.Verify}, err
	}
	input, err := e.readRoleInput(stage)
	if err != nil {
		return stageTask{}, err
	}
	_, _, text, err := e.renderRoleInput(ctx, input)
	return stageTask{role, "baseline", text, roleVerifier()}, err
}

func (e *ProbeExecutor) readRoleInput(stage StageEvidence) (roleInput, error) {
	if stage.RoleInput == nil || stage.RoleInput.Validate("role_input") != nil {
		return roleInput{}, errors.New("role evidence has no immutable input")
	}
	var input roleInput
	if err := readObjectJSON(e.objects, *stage.RoleInput, &input); err != nil {
		return roleInput{}, err
	}
	if input.Kind != stage.Request.Spec.Kind {
		return roleInput{}, errors.New("role input belongs to a different stage")
	}
	return input, nil
}

type roleResult struct {
	Proposal    *paired.ChangeProposal
	Composition *paired.Composition
	Reflection  *paired.Reflection
	Variant     *Variant
}

func (e *ProbeExecutor) classifyRole(ctx context.Context, stage StageEvidence, observation childObservation, outcome string) (string, error) {
	if outcome != "passed" {
		return "invalid_proposal", nil
	}
	payload := observation.session.Tasks[0].TypedResult.StructuredPayload.Value
	_, err := e.validateRoleResult(ctx, stage, payload)
	if err != nil {
		// Model-authored scope and schema failures are bounded proposal
		// failures, not infrastructure errors or permission to bind a variant.
		return "invalid_proposal", nil
	}
	return "passed", nil
}

func (e *ProbeExecutor) validateRoleResult(ctx context.Context, stage StageEvidence, raw jsontext.Value) (roleResult, error) {
	input, err := e.readRoleInput(stage)
	if err != nil {
		return roleResult{}, err
	}
	prepared, view, _, err := e.renderRoleInput(ctx, input)
	if err != nil {
		return roleResult{}, err
	}
	switch input.Kind {
	case "procedure", "domain":
		var proposal paired.ChangeProposal
		if err := json.Unmarshal(raw, &proposal, json.RejectUnknownMembers(true)); err != nil {
			return roleResult{}, err
		}
		variant, err := validateRoleProposal(prepared, view, proposal)
		return roleResult{Proposal: &proposal, Variant: &variant}, err
	case "compose":
		var composition paired.Composition
		if err := json.Unmarshal(raw, &composition, json.RejectUnknownMembers(true)); err != nil {
			return roleResult{}, err
		}
		if err := composition.Validate(); err != nil {
			return roleResult{}, err
		}
		result := roleResult{Composition: &composition}
		switch composition.Action {
		case "use_candidate":
			variant, exists := prepared.Variants[composition.CandidateSlot]
			if !exists || !slices.ContainsFunc(view.UntrustedData.Candidates, func(candidate roleCandidate) bool {
				return candidate.Slot == composition.CandidateSlot
			}) {
				return roleResult{}, errors.New("composition selected an unbound candidate")
			}
			result.Variant = &variant
		case "compose":
			variant, err := validateRoleProposal(prepared, view, *composition.Proposal)
			result.Variant = &variant
			return result, err
		}
		return result, nil
	case "reflect":
		var reflection paired.Reflection
		if err := json.Unmarshal(raw, &reflection, json.RejectUnknownMembers(true)); err != nil {
			return roleResult{}, err
		}
		if err := validateReflection(reflection, view); err != nil {
			return roleResult{}, err
		}
		return roleResult{Reflection: &reflection}, nil
	default:
		return roleResult{}, errors.New("unexpected role result")
	}
}

func validateRoleProposal(prepared Prepared, view roleView, proposal paired.ChangeProposal) (Variant, error) {
	if err := validateFailureReferences(proposal, view); err != nil {
		return Variant{}, err
	}
	return ApplyProposal(prepared, proposal)
}

func validateReflection(reflection paired.Reflection, view roleView) error {
	if reflection.SchemaVersion != 1 {
		return errors.New("invalid reflection version")
	}
	if reflection.Action == "no_entry" && reflection.Lesson == nil {
		return nil
	}
	if reflection.Action != "lesson" || reflection.Lesson == nil {
		return errors.New("invalid reflection action or lesson")
	}
	lesson := reflection.Lesson
	if len(lesson.BaselineObservationRefs) == 0 || len(lesson.CandidateObservationRefs) == 0 || len(lesson.VerificationRefs) == 0 {
		return errors.New("lesson requires baseline, candidate and verification evidence")
	}
	groups := [][]string{lesson.BaselineObservationRefs, lesson.CandidateObservationRefs, lesson.VerificationRefs, lesson.CounterevidenceRefs}
	for group, refs := range groups {
		for _, ref := range refs {
			if !slices.ContainsFunc(view.UntrustedData.Observations, func(item roleObservation) bool {
				return item.StageID == ref && (item.Outcome == "passed" || item.Outcome == "failed") &&
					(group != 0 || item.Slot.Variant == "baseline") &&
					(group != 1 || item.Slot.Variant == view.SelectedVariant && view.SelectedVariant != "baseline")
			}) {
				return errors.New("lesson references missing or mismatched evidence")
			}
		}
	}
	for _, candidate := range view.UntrustedData.Candidates {
		if candidate.Slot == view.SelectedVariant && lesson.ChangeRef == "proposal_"+candidate.Slot {
			return nil
		}
	}
	return errors.New("lesson change reference is not a bound proposal")
}

func (e *ProbeExecutor) readRoleResult(ctx context.Context, request StageRequest, result Settlement) (roleResult, error) {
	actual, err := e.ReadSettlement(ctx, request, result.Evidence)
	if err != nil || actual != result || actual.Outcome != "passed" {
		return roleResult{}, errors.New("role result lacks a verified completed settlement")
	}
	var stage StageEvidence
	if err := readObjectJSON(e.objects, result.Evidence, &stage); err != nil {
		return roleResult{}, err
	}
	observation, err := inspectChildEvidence(ctx, e.objects, stage.Child)
	if err != nil {
		return roleResult{}, err
	}
	return e.validateRoleResult(ctx, stage, observation.session.Tasks[0].TypedResult.StructuredPayload.Value)
}

func validateRoleProvenance(objects Objects, state LedgerState, stage StageEvidence) error {
	if stage.RoleInput == nil {
		return errors.New("missing role provenance")
	}
	var input roleInput
	if err := readObjectJSON(objects, *stage.RoleInput, &input); err != nil {
		return err
	}
	for _, binding := range input.Variants {
		if bound, exists := state.Variants[binding.Slot]; !exists || bound != binding {
			return errors.New("role consumed an uncommitted variant")
		}
	}
	for _, source := range input.Sources {
		committed := state.Stages[source.StageID]
		if committed == nil || len(source.Attempts) != len(committed.Attempts) {
			return errors.New("role consumed uncommitted evidence")
		}
		for i, ref := range source.Attempts {
			attempt := committed.Attempts[i]
			if attempt.Settlement == nil || attempt.Settlement.Evidence != ref {
				return errors.New("role consumed evidence not bound to its parent journal")
			}
		}
	}
	return nil
}
