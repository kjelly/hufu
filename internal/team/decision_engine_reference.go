package team

import (
	"context"
	"fmt"
	"reflect"

	"github.com/kjelly/hufu/internal/agent"
)

// The bounded reference-evidence stage.
//
// A profile that requires an outside view may have the runtime produce it,
// but the result is evidence and not a recommendation: identity, artifacts
// and provenance are assigned by the runtime after validation, never taken
// from the producer. Moved out of decision_engine.go unchanged.

func validateReferenceRecoveryState(state decisionState) error {
	if state.ReferenceFailure != nil || (state.ReferenceInvocation != nil && state.ReferenceResult == nil) {
		detail := "reference evidence invocation has no safely reusable completed result"
		if state.ReferenceFailure != nil {
			detail = state.ReferenceFailure.Reason
		}
		return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: detail}
	}
	return nil
}

func (e *decisionEngine) attachReferenceEvidence(ctx context.Context, req DecisionRequest, state decisionState) (DecisionRequest, error) {
	if len(req.BaseRates) == 0 && req.Policy.OutsideView.Required && req.Policy.OutsideView.ReferenceEvidence {
		rates, artifacts, provenance, resultRef, err := e.runReferenceEvidence(ctx, req, state)
		if err != nil {
			return req, err
		}
		req.BaseRates = rates
		req.Artifacts = append(req.Artifacts, artifacts...)
		req.Provenance = append(req.Provenance, provenance...)
		req.ReferenceEvidenceResultRef = resultRef
		return req, nil
	}
	if state.ReferenceResult != nil {
		req.BaseRates = cloneBaseRateEvidence(state.ReferenceResult.BaseRates)
		req.Artifacts = append(req.Artifacts, state.ReferenceResult.Artifacts...)
		req.Provenance = append(req.Provenance, state.ReferenceResult.Provenance...)
		if state.ReferenceResult.ResultArtifactRef != nil {
			req.ReferenceEvidenceResultRef = *state.ReferenceResult.ResultArtifactRef
		}
	}
	return req, nil
}

func (e *decisionEngine) runReferenceEvidence(ctx context.Context, req DecisionRequest, state decisionState) ([]BaseRateEvidence, []ArtifactRef, []EvidenceProvenance, ArtifactRef, error) {
	if state.ReferenceResult != nil {
		if state.ReferenceInvocation == nil || state.ReferenceResult.InvocationID != state.ReferenceInvocation.InvocationID || state.ReferenceResult.InputHash != state.ReferenceInvocation.InputHash {
			return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "reference evidence result identity is invalid"}
		}
		expected := ReferenceEvidenceRequest{
			SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: state.ReferenceInvocation.InvocationID,
			DecisionID: req.DecisionID, RunID: req.RunID, TaskID: req.TaskID, Question: req.Question,
			ContractRef: req.RequestContractRef, ContractRevision: req.RequestContractRevision,
		}
		expectedHash, err := expected.ComputeInputHash()
		if err != nil || expectedHash != state.ReferenceInvocation.InputHash {
			return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "reference evidence request changed after invocation started"}
		}
		if err := validateReferenceEvidenceResultArtifact(ctx, e.services.Store, *state.ReferenceResult); err != nil {
			return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: err.Error()}
		}
		return cloneBaseRateEvidence(state.ReferenceResult.BaseRates), append([]ArtifactRef(nil), state.ReferenceResult.Artifacts...), cloneEvidenceProvenance(state.ReferenceResult.Provenance), *state.ReferenceResult.ResultArtifactRef, nil
	}
	if state.ReferenceInvocation != nil || state.ReferenceFailure != nil {
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "reference evidence invocation is incomplete and cannot be replayed safely"}
	}
	if e.services.ReferenceEvidence == nil || e.services.Store == nil {
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "reference evidence requires a producer and artifact store"}
	}

	request := ReferenceEvidenceRequest{
		SchemaVersion:    ReferenceEvidenceSchemaVersion,
		InvocationID:     e.newID("reference"),
		DecisionID:       req.DecisionID,
		RunID:            req.RunID,
		TaskID:           req.TaskID,
		Question:         req.Question,
		ContractRef:      req.RequestContractRef,
		ContractRevision: req.RequestContractRevision,
	}
	inputHash, err := request.ComputeInputHash()
	if err != nil {
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: err.Error()}
	}
	request.InputHash = inputHash
	if err := request.Validate(); err != nil {
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: err.Error()}
	}
	invocation := ReferenceEvidenceInvocation{
		SchemaVersion: request.SchemaVersion, InvocationID: request.InvocationID,
		InputHash: request.InputHash, DecisionID: request.DecisionID, RunID: request.RunID,
		TaskID: request.TaskID, Question: request.Question, ContractRef: request.ContractRef,
		ContractRevision: request.ContractRevision, StartedAt: e.now(),
	}
	event := decisionEventFor(req, "reference_started", request.InputHash)
	event.ReferenceInvocation = &invocation
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceStarted, event); err != nil {
		return nil, nil, nil, ArtifactRef{}, err
	}

	draft, producerErr := e.services.ReferenceEvidence.RunReferenceEvidence(ctx, request)
	if producerErr != nil {
		failure := &ReferenceEvidenceFailure{
			SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID,
			InputHash: request.InputHash, Reason: "reference evidence producer failed", FailedAt: e.now(),
		}
		event := decisionEventFor(req, "reference_failed", request.InputHash)
		event.ReferenceFailure = failure
		if appendErr := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceFailed, event); appendErr != nil {
			return nil, nil, nil, ArtifactRef{}, appendErr
		}
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: producerErr.Error()}
	}
	if err := draft.Validate(); err != nil {
		failure := &ReferenceEvidenceFailure{
			SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID,
			InputHash: request.InputHash, Reason: "reference evidence draft failed validation", FailedAt: e.now(),
		}
		event := decisionEventFor(req, "reference_failed", request.InputHash)
		event.ReferenceFailure = failure
		if appendErr := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceFailed, event); appendErr != nil {
			return nil, nil, nil, ArtifactRef{}, appendErr
		}
		return nil, nil, nil, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: err.Error()}
	}

	rates := make([]BaseRateEvidence, 0, len(draft.Entries))
	artifacts := make([]ArtifactRef, 0, len(draft.Entries))
	provenance := make([]EvidenceProvenance, 0, len(draft.Entries)*2)
	for i, entry := range draft.Entries {
		artifact := ReferenceEvidenceArtifact{
			SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID,
			DecisionID: request.DecisionID, RunID: request.RunID, TaskID: request.TaskID,
			Entry: entry, PublishedAt: e.now(),
		}
		content, encodeErr := referenceEvidenceEntryBytes(artifact)
		if encodeErr != nil {
			return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, encodeErr)
		}
		put, putErr := e.services.Store.Put(ctx, PutArtifactRequest{
			Kind: "reference_evidence", Role: "decision_reference",
			Path:        fmt.Sprintf("decisions/reference-evidence/%s/%d.json", request.InvocationID, i),
			Description: fmt.Sprintf("validated reference evidence %s entry %d", request.DecisionID, i),
			MediaType:   ReferenceEvidenceMediaType, Content: content,
			RunID: request.RunID, TaskID: request.TaskID,
		})
		if putErr != nil {
			return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, putErr)
		}
		ref, resolveErr := e.services.Store.Resolve(ctx, put.ArtifactRef)
		if resolveErr != nil {
			return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, resolveErr)
		}
		declaredID, idErr := referenceDeclaredSourceID(entry.Source)
		if idErr != nil {
			return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, idErr)
		}
		rates = append(rates, BaseRateEvidence{
			ReferenceClass: entry.ReferenceClass, Metric: entry.Metric, SampleSize: entry.SampleSize,
			Distribution: entry.Distribution, Source: ref, Limitations: append([]string(nil), entry.Limitations...),
		})
		artifacts = append(artifacts, ref)
		provenance = append(provenance,
			EvidenceProvenance{SourceID: ref.ID, SourceType: EvidenceSourceArtifact, IndependenceGroup: ref.SHA256, ContentHash: ref.SHA256, RetrievedAt: e.now()},
			EvidenceProvenance{SourceID: declaredID, SourceType: EvidenceSourceDeclared, DeclaredParentSourceIDs: append([]string(nil), entry.Source.DeclaredParentSourceIDs...), RetrievedAt: e.now()},
		)
	}
	result := &ReferenceEvidenceResult{
		SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID, InputHash: request.InputHash,
		BaseRates: rates, Artifacts: artifacts, Provenance: provenance, CompletedAt: e.now(),
	}
	resultBytes, err := referenceEvidenceResultBytes(*result)
	if err != nil {
		return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, err)
	}
	put, err := e.services.Store.Put(ctx, PutArtifactRequest{
		Kind: "reference_evidence_result", Role: "decision_reference_result",
		Path:        fmt.Sprintf("decisions/reference-evidence/%s/result.json", request.InvocationID),
		Description: fmt.Sprintf("validated reference evidence result %s", req.DecisionID),
		MediaType:   ReferenceEvidenceResultMediaType, Content: resultBytes, RunID: request.RunID, TaskID: request.TaskID,
	})
	if err != nil {
		return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, err)
	}
	resultRef, err := e.services.Store.Resolve(ctx, put.ArtifactRef)
	if err != nil {
		return nil, nil, nil, ArtifactRef{}, e.referencePublicationFailure(ctx, req, request, err)
	}
	result.ResultArtifactRef = &resultRef
	event = decisionEventFor(req, "reference_completed", request.InputHash)
	event.ReferenceResult = result
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceCompleted, event); err != nil {
		return nil, nil, nil, ArtifactRef{}, err
	}
	return rates, artifacts, provenance, resultRef, nil
}

func validateReferenceEvidenceResultArtifact(ctx context.Context, store ArtifactStore, result ReferenceEvidenceResult) error {
	if err := validateReferenceEvidenceResult(result); err != nil {
		return err
	}
	if store == nil || result.ResultArtifactRef == nil || result.ResultArtifactRef.ID == "" {
		return fmt.Errorf("reference evidence result has no CAS envelope reference")
	}
	ref, err := store.Resolve(ctx, *result.ResultArtifactRef)
	if err != nil {
		return fmt.Errorf("resolve reference evidence result: %w", err)
	}
	if ref.MediaType != ReferenceEvidenceResultMediaType || ref.Kind != "reference_evidence_result" || ref.Role != "decision_reference_result" {
		return fmt.Errorf("reference evidence result CAS metadata is invalid")
	}
	reader, err := store.Open(ctx, ref.ID)
	if err != nil {
		return fmt.Errorf("open reference evidence result: %w", err)
	}
	defer func() { _ = reader.Close() }()
	stored, err := decodeReferenceEvidenceResult(reader)
	if err != nil {
		return err
	}
	stored.ResultArtifactRef = nil
	expected := result
	expected.ResultArtifactRef = nil
	if !reflect.DeepEqual(stored, expected) {
		return fmt.Errorf("reference evidence result CAS envelope does not match completion event")
	}
	for i, artifact := range result.Artifacts {
		if err := validateReferenceEvidenceEntryArtifact(ctx, store, artifact, result.InvocationID, result.BaseRates[i]); err != nil {
			return fmt.Errorf("reference evidence entry %d: %w", i, err)
		}
	}
	return nil
}

func validateReferenceEvidenceEntryArtifact(ctx context.Context, store ArtifactStore, ref ArtifactRef, invocationID string, rate BaseRateEvidence) error {
	if store == nil {
		return fmt.Errorf("artifact store is unavailable")
	}
	resolved, err := store.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolve artifact: %w", err)
	}
	if resolved.MediaType != ReferenceEvidenceMediaType || resolved.Kind != "reference_evidence" || resolved.Role != "decision_reference" {
		return fmt.Errorf("artifact metadata is invalid")
	}
	reader, err := store.Open(ctx, resolved.ID)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}
	defer func() { _ = reader.Close() }()
	artifact, err := decodeReferenceEvidenceEntry(reader)
	if err != nil {
		return err
	}
	if artifact.InvocationID != invocationID {
		return fmt.Errorf("artifact invocation identity does not match result")
	}
	if artifact.Entry.ReferenceClass != rate.ReferenceClass || artifact.Entry.Metric != rate.Metric ||
		artifact.Entry.SampleSize != rate.SampleSize || !reflect.DeepEqual(artifact.Entry.Distribution, rate.Distribution) ||
		!reflect.DeepEqual(artifact.Entry.Limitations, rate.Limitations) {
		return fmt.Errorf("artifact entry does not match result base rate")
	}
	return nil
}

func validateReferenceEvidenceResult(result ReferenceEvidenceResult) error {
	if result.SchemaVersion != ReferenceEvidenceSchemaVersion {
		return fmt.Errorf("unsupported reference evidence result schema version %d", result.SchemaVersion)
	}
	if result.InvocationID == "" || result.InputHash == "" {
		return fmt.Errorf("reference evidence result identity is incomplete")
	}
	if result.ResultArtifactRef == nil || result.ResultArtifactRef.ID == "" {
		return fmt.Errorf("reference evidence result has no CAS envelope reference")
	}
	if len(result.BaseRates) < ReferenceEvidenceMinEntries || len(result.BaseRates) > ReferenceEvidenceMaxEntries || len(result.BaseRates) != len(result.Artifacts) {
		return fmt.Errorf("reference evidence result has inconsistent entry and artifact counts")
	}
	for i, rate := range result.BaseRates {
		if err := validateBaseRate(rate); err != nil {
			return fmt.Errorf("reference evidence result base_rates[%d]: %w", i, err)
		}
		if rate.Source.ID == "" || result.Artifacts[i].ID != rate.Source.ID {
			return fmt.Errorf("reference evidence result entry %d is not bound to its artifact", i)
		}
	}
	return nil
}

func (e *decisionEngine) referencePublicationFailure(ctx context.Context, req DecisionRequest, request ReferenceEvidenceRequest, cause error) error {
	failure := &ReferenceEvidenceFailure{
		SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: request.InvocationID,
		InputHash: request.InputHash, Reason: "reference evidence publication failed", FailedAt: e.now(),
	}
	event := decisionEventFor(req, "reference_failed", request.InputHash)
	event.ReferenceFailure = failure
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionReferenceFailed, event); err != nil {
		return err
	}
	return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: cause.Error()}
}
