package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

const primaryDecisionValidationVersion = "primary-decision-process@v1"

type primaryDecisionService struct {
	coordinator *Coordinator
	question    string
	bundle      agent.DecisionProfileBundleV2
	teamID      string
	teamDigest  string
}

// NewPrimaryDecisionPreparer constructs the production adapter from the
// universal primary contract to the existing DecisionEngine. Construction is
// provider-free and validates every immutable profile/team input up front.
func NewPrimaryDecisionPreparer(coordinator *Coordinator, question, profileRef string) (DecisionTerminalPreparer, error) {
	if coordinator == nil || coordinator.session == nil {
		return nil, fmt.Errorf("primary decision coordinator is unavailable")
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("primary decision question is empty")
	}
	bundle, err := agent.ResolveBuiltInDecisionProfileBundle(profileRef)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(coordinator.session.Config.Name)
	if name == "" {
		name = "default"
	}
	teamID, err := DecisionTeamID(name)
	if err != nil {
		return nil, err
	}
	teamDigest, err := primaryDecisionTeamDigest(coordinator.session, name)
	if err != nil {
		return nil, err
	}
	return &primaryDecisionService{coordinator: coordinator, question: question, bundle: bundle, teamID: teamID, teamDigest: teamDigest}, nil
}

func primaryDecisionTeamDigest(session *TeamSession, name string) (string, error) {
	if session != nil && strings.TrimSpace(session.Dir) != "" {
		if digest, err := StrictTeamDefinitionRevision(session.Dir); err == nil {
			return digest, nil
		}
	}
	return DecisionContractDigest("hufu/team-definition-output/v1", struct {
		Name string `json:"name"`
	}{Name: name})
}

func (service *primaryDecisionService) PrepareDecisionForTerminal(ctx context.Context, request DecisionTerminalPreparationRequest) (DecisionTerminalPreparationResult, error) {
	if service == nil || service.coordinator == nil {
		return DecisionTerminalPreparationResult{}, fmt.Errorf("primary decision service is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	coordinator := service.coordinator
	store := coordinator.decisionArtifactStore()
	journal, err := coordinator.decisionJournalFor()
	if err != nil || store == nil || journal == nil {
		return DecisionTerminalPreparationResult{}, fmt.Errorf("primary decision durable services are unavailable: %w", err)
	}

	taskID, err := PrimaryDecisionTaskID(request.BranchID, request.LogicalRunID)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	decisionID, err := PrimaryDecisionID(request.BranchID, request.LogicalRunID, request.Generation)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	requirementDigest, err := DecisionContractDigest("hufu/primary-requirement/v1", struct {
		Question string `json:"question"`
	}{Question: service.question})
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}

	priorEvents, err := journal.ReadEvents(ctx)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	if existing, replayErr := ReplayLogicalDecisionRun(priorEvents, request.LogicalRunID, request.BranchID); replayErr == nil {
		if existing.ActivePrimary != nil {
			eventID := primaryBindingEventID(priorEvents, *existing.ActivePrimary)
			if eventID == "" {
				return DecisionTerminalPreparationResult{Action: TerminalPreparationRecoverOnly, ReasonCodes: []string{ReasonDecisionTerminalRecoveryRequired}}, nil
			}
			return DecisionTerminalPreparationResult{Action: TerminalPreparationCommitTerminal, PrimaryBinding: existing.ActivePrimary, PrimaryBindingEventID: &eventID, SupportRevisionDigest: &existing.ActivePrimary.SupportRevisionDigest}, nil
		}
		return DecisionTerminalPreparationResult{}, fmt.Errorf("logical decision %s is already active without a bound primary", request.LogicalRunID)
	} else if !strings.Contains(replayErr.Error(), "not found") {
		return DecisionTerminalPreparationResult{Action: TerminalPreparationRecoverOnly, ReasonCodes: []string{ReasonDecisionTerminalRecoveryRequired}}, replayErr
	}

	requirementRef, profileRef, authorityRef, redactionRef, limitsRef, err := service.persistOpeningArtifacts(ctx, store, request, requirementDigest)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	opened, err := appendPrimaryCorrectnessEvent(ctx, journal, EventDecisionRunOpened, request, decisionOpenedKey(request.LogicalRunID), DecisionRunOpenedPayload{
		decisionEventCommon: decisionEventCommonFor(request), TeamID: service.teamID, TeamDefinitionDigest: service.teamDigest,
		RequirementRef: requirementRef, RequirementDigest: requirementDigest, ProfileBundleRef: profileRef, AuthoritySnapshotRef: authorityRef,
		EffectiveLimits: DecisionEffectiveLimits{
			TotalDecisionTokens: service.bundle.Limits.TotalDecisionTokens, ActiveDecisionDurationMS: service.bundle.Limits.ActiveDecisionDurationMS,
			MaxGenerations: service.bundle.Limits.MaxGenerations, MaxStageInvocationAttempts: service.bundle.Limits.MaxStageInvocationAttempts,
			CleanupTimeoutMS: service.bundle.Limits.CleanupTimeoutMS,
		},
	})
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}

	supportCursor, supportDigest, err := primarySupportSnapshot(priorEvents, opened)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	candidates, err := service.primaryEvidenceCandidates(ctx, store, request, priorEvents, requirementRef, opened.ID)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	evidence, err := CollectPrimaryDecisionEvidence(ctx, PrimaryEvidenceCollectionRequest{
		LogicalRunID: request.LogicalRunID, BranchID: request.BranchID, Generation: request.Generation,
		RequirementDigest: requirementDigest, SupportCursor: supportCursor, SupportRevisionDigest: supportDigest,
		RedactionPolicyRef: redactionRef, LimitsRef: limitsRef, ExecutionRunIDs: []string{request.ExecutionRunID}, Candidates: candidates,
		Limits: primaryEvidenceLimitsFromBundle(service.bundle), Store: store,
	})
	if err != nil {
		_ = service.appendBlocked(ctx, journal, request, taskID, supportDigest, err)
		return DecisionTerminalPreparationResult{}, err
	}
	evidenceBytes, err := CanonicalDecisionJSON(evidence)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	evidenceRef, err := putPrimaryEvidenceArtifact(ctx, store, "primary-evidence", "application/json", evidenceBytes)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	rolePlan, rolePlanRef, err := service.prepareRolePlan(ctx, store, request)
	if err != nil {
		_ = service.appendBlocked(ctx, journal, request, taskID, supportDigest, err)
		return DecisionTerminalPreparationResult{}, err
	}
	preparationDigest, err := DecisionContractDigest("hufu/primary-preparation/v1", struct {
		Evidence DecisionArtifactRef `json:"evidence"`
		Plan     DecisionArtifactRef `json:"plan"`
	}{Evidence: evidenceRef, Plan: rolePlanRef})
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	preparedPayload := PrimaryDecisionPreparedPayload{
		decisionGenerationEventCommon: generationEventCommonFor(request), TaskID: taskID, DecisionID: decisionID,
		SupportCursor: supportCursor, SupportRevisionDigest: supportDigest, BaseEvidenceRef: evidenceRef, RolePlanRef: rolePlanRef, PreparationDigest: preparationDigest,
	}
	if _, err := appendPrimaryCorrectnessEvent(ctx, journal, EventPrimaryDecisionPrepared, request, decisionPreparedKey(request.LogicalRunID, request.Generation, preparationDigest), preparedPayload); err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	occurrence := RuntimeOccurrenceV1{
		SchemaVersion: 1, Kind: "runtime_task_occurrence", TaskID: taskID, TaskKind: TaskKindOutcome, OccurrenceRevision: request.Generation, Status: TaskPending,
		Runtime: RuntimeOccurrenceMetaV1{SchemaVersion: 1, Origin: "runtime", Purpose: "primary_decision", ExecutionOwner: "decision_engine", LogicalRunID: request.LogicalRunID, BranchID: request.BranchID, Generation: request.Generation, DecisionID: decisionID},
	}
	occurrenceBytes, _ := CanonicalDecisionJSON(occurrence)
	occurrenceRef, err := putPrimaryEvidenceArtifact(ctx, store, "primary-occurrence", "application/json", occurrenceBytes)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	admissionBytes, _ := CanonicalDecisionJSON(map[string]any{"logical_run_id": request.LogicalRunID, "generation": request.Generation, "preparation_digest": preparationDigest, "role_plan_digest": rolePlan.PolicyBundleDigest})
	admissionRef, err := putPrimaryEvidenceArtifact(ctx, store, "primary-admission", "application/json", admissionBytes)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	admitted, err := appendPrimaryCorrectnessEvent(ctx, journal, EventPrimaryDecisionAdmitted, request, decisionAdmittedKey(request.LogicalRunID, request.Generation), PrimaryDecisionAdmittedPayload{
		decisionGenerationEventCommon: generationEventCommonFor(request), TaskID: taskID, DecisionID: decisionID,
		OccurrenceRef: occurrenceRef, AdmissionRef: admissionRef, PreparationDigest: preparationDigest,
	})
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	primaryItem, err := NewPrimaryDecisionOccurrence(admitted, occurrence)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	coordinator.taskTracker.TodoList().AddReserved([]*TodoItem{primaryItem})
	coordinator.taskTracker.TodoList().UpdateStatus(taskID, TaskInProgress, "forming primary decision")

	record, recordRef, err := service.runDecisionEngine(ctx, request, taskID, decisionID, evidence, rolePlan)
	if err != nil {
		coordinator.taskTracker.TodoList().UpdateStatus(taskID, TaskBlocked, err.Error())
		_ = service.appendBlocked(ctx, journal, request, taskID, supportDigest, err)
		return DecisionTerminalPreparationResult{}, err
	}
	binding := PrimaryBindingV1{
		SchemaVersion: 1, LogicalRunID: request.LogicalRunID, BranchID: request.BranchID, TaskID: taskID, Generation: request.Generation,
		DecisionID: decisionID, RequirementDigest: requirementDigest, SupportRevisionDigest: supportDigest,
		AdmissionRef: admissionRef, BaseEvidenceRef: evidenceRef, RolePlanRef: rolePlanRef, RecordRef: recordRef, SealedEvidenceHash: record.EvidenceHash,
	}
	receiptBytes, _ := CanonicalDecisionJSON(map[string]any{"decision_id": decisionID, "record_ref": recordRef, "record_schema_version": record.SchemaVersion, "validation": "passed"})
	receiptRef, err := putPrimaryEvidenceArtifact(ctx, store, "primary-record-validation", "application/json", receiptBytes)
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	boundEvent, err := appendPrimaryCorrectnessEvent(ctx, journal, EventPrimaryDecisionBound, request, decisionBoundKey(request.LogicalRunID, request.Generation), PrimaryDecisionBoundPayload{
		decisionGenerationEventCommon: generationEventCommonFor(request), Binding: binding, RecordValidationReceiptRef: receiptRef,
	})
	if err != nil {
		return DecisionTerminalPreparationResult{}, err
	}
	primaryItem.PrimaryManifestProof = &PrimaryManifestProofV1{
		SchemaVersion: 1, Kind: "primary_decision_valid", State: "satisfied", LogicalRunID: request.LogicalRunID, BranchID: request.BranchID,
		RequirementDigest: requirementDigest, PrimaryTaskID: taskID, Generation: request.Generation, DecisionID: decisionID,
		AdmissionRef: admissionRef, RecordRef: recordRef, BaseEvidenceRef: evidenceRef, SealedEvidenceHash: record.EvidenceHash,
		RolePlanRef: rolePlanRef, BindingEventID: boundEvent.ID, SupportRevisionDigest: supportDigest, ValidationVersion: primaryDecisionValidationVersion,
	}
	coordinator.taskTracker.TodoList().UpdateStatusAndOutput(taskID, TaskDone, "primary decision bound", record.FinalOption)
	if manifest, manifestErr := coordinator.buildEvidenceManifest(ctx, false); manifestErr == nil && request.Candidate != nil {
		request.Candidate.EvidenceManifest = manifest
	}
	return DecisionTerminalPreparationResult{
		Action: TerminalPreparationCommitTerminal, PrimaryBinding: &binding, PrimaryBindingEventID: &boundEvent.ID, SupportRevisionDigest: &supportDigest,
	}, nil
}

func (service *primaryDecisionService) persistOpeningArtifacts(ctx context.Context, store ArtifactStore, request DecisionTerminalPreparationRequest, requirementDigest string) (DecisionArtifactRef, DecisionArtifactRef, DecisionArtifactRef, DecisionArtifactRef, DecisionArtifactRef, error) {
	put := func(role string, value any) (DecisionArtifactRef, error) {
		data, err := CanonicalDecisionJSON(value)
		if err != nil {
			return DecisionArtifactRef{}, err
		}
		return putPrimaryEvidenceArtifact(ctx, store, role, "application/json", data)
	}
	requirement, err := put("decision-requirement", map[string]any{"question": service.question, "requirement_digest": requirementDigest})
	if err != nil {
		return DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, err
	}
	profile, err := putPrimaryEvidenceArtifact(ctx, store, "decision-profile-bundle", "application/json", service.bundle.RawJSON)
	if err != nil {
		return DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, err
	}
	authority, err := put("decision-authority", map[string]any{"team_id": service.teamID, "team_name": service.coordinator.session.Config.Name, "team_definition_digest": service.teamDigest, "branch_id": request.BranchID})
	if err != nil {
		return DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, err
	}
	redaction, err := put("decision-redaction-policy", map[string]any{"version": DecisionOutputRedactionPolicyV1})
	if err != nil {
		return DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, DecisionArtifactRef{}, err
	}
	limits, err := put("decision-evidence-limits", service.bundle.Evidence)
	return requirement, profile, authority, redaction, limits, err
}

func (service *primaryDecisionService) primaryEvidenceCandidates(ctx context.Context, store ArtifactStore, request DecisionTerminalPreparationRequest, events []RunEvent, requirementRef DecisionArtifactRef, openedEventID string) ([]PrimaryEvidenceCandidate, error) {
	requirementArtifact, err := resolveDecisionArtifactRef(ctx, store, requirementRef, "decision requirement")
	if err != nil {
		return nil, err
	}
	records := []PrimaryEvidenceSourceRecord{{
		SourceKind: "fact", SourceArtifact: requirementArtifact, SourceEventID: openedEventID, ProvenanceAuthority: "operator_declared",
		Verification: PrimaryEvidenceVerificationV1{State: "not_checked", Scope: "none", AssertionRefs: []DecisionArtifactRef{}}, MandatoryReasons: []string{"request_input"}, EpistemicStatus: "reported", SourceEventOrder: 1,
	}}
	var manifest *EvidenceManifest
	if request.Candidate != nil {
		manifest = request.Candidate.EvidenceManifest
	}
	if manifest == nil {
		service.coordinator.lastEvidenceManifestMu.Lock()
		manifest = service.coordinator.lastEvidenceManifest
		service.coordinator.lastEvidenceManifestMu.Unlock()
	}
	if manifest != nil {
		for index, ref := range manifest.ArtifactRefs {
			if strings.TrimSpace(ref.TaskID) == "" {
				continue
			}
			eventID, order := sourceEventForArtifact(events, ref)
			if eventID == "" {
				continue
			}
			kind := "artifact_text"
			baseRate := strings.Contains(strings.ToLower(ref.Role+" "+ref.Kind+" "+ref.Description), "base rate") || strings.Contains(strings.ToLower(ref.Role+" "+ref.Kind), "base_rate")
			if baseRate {
				kind = "base_rate"
			}
			state := "passed"
			mandatory := []string(nil)
			if artifactEvidenceFailed(*manifest, ref.ID) {
				state = "failed"
				mandatory = []string{"failed_assertion"}
			}
			taskSource := &PrimaryEvidenceTaskSourceV1{TaskID: ref.TaskID, OccurrenceRevision: 1, Attempt: uint32(max(ref.Attempt, 1)), ProducerExecutionRunID: request.ExecutionRunID, ResultEventID: eventID}
			records = append(records, PrimaryEvidenceSourceRecord{
				SourceKind: kind, Task: taskSource, SourceArtifact: ref, SourceEventID: eventID, ProvenanceAuthority: "runtime_observed",
				Verification: PrimaryEvidenceVerificationV1{State: state, Scope: "integrity_only", AssertionRefs: []DecisionArtifactRef{}}, MandatoryReasons: mandatory,
				EpistemicStatus: "observed", SourceEventOrder: uint64(order + index + 2), BaseRate: baseRate,
			})
		}
	}
	return PrimaryEvidenceCandidatesFromSources(ctx, store, records)
}

func (service *primaryDecisionService) prepareRolePlan(ctx context.Context, store ArtifactStore, request DecisionTerminalPreparationRequest) (*DecisionRoleBindingPlanV1, DecisionArtifactRef, error) {
	model := strings.TrimSpace(service.coordinator.judgeModel)
	if model == "" {
		model = strings.TrimSpace(service.coordinator.sidecarModel)
	}
	if model == "" {
		return nil, DecisionArtifactRef{}, fmt.Errorf("%w: primary decision has no reviewer model", ErrDecisionBackendUnverified)
	}
	put := func(role string, value any) (DecisionArtifactRef, error) {
		data, err := CanonicalDecisionJSON(value)
		if err != nil {
			return DecisionArtifactRef{}, err
		}
		return putPrimaryEvidenceArtifact(ctx, store, role, "application/json", data)
	}
	target, err := put("decision-execution-target", map[string]any{"model": model, "credential_reference": "configured-provider"})
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	authority, err := put("decision-role-authority", map[string]any{"authorized": true, "team_id": service.teamID})
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	instructions := make(map[string]DecisionArtifactRef)
	for _, role := range []string{"proposal", "reference", "judge", "challenge", "premortem"} {
		instructions[role], err = put("decision-role-instruction", map[string]any{"role": role, "context_policy": "sealed-primary-evidence"})
		if err != nil {
			return nil, DecisionArtifactRef{}, err
		}
	}
	policyDigest, err := DecisionContractDigest("hufu/decision-invocation-policy/v1", map[string]any{"tools": []string{}, "session_isolation": true})
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	provider := "configured"
	candidate := DecisionRoleCandidate{
		ExecutionMode: "runtime_reviewer", ExecutionTargetRef: target, ModelIdentity: &model, ProviderIdentity: &provider,
		InvocationPolicyDigest: policyDigest, AuthorizationSnapshotRef: authority, RoleInstructionRefs: instructions,
		Authorized: true, RuntimeFallback: true, BackendCapability: DecisionBackendCapabilityV1{SchemaVersion: 1, ToolIsolation: "none_enforced", SessionIsolation: true, DeclarationSource: "adapter"},
	}
	constraints := service.coordinator.session.Config.Decision.RoleConstraints
	if constraints.SchemaVersion == 0 {
		constraints = agent.DefaultDecisionRoleConstraintsV1()
	}
	plan, err := ResolvePrimaryDecisionRoles(DecisionRoleResolutionRequest{
		LogicalRunID: request.LogicalRunID, Generation: request.Generation, Bundle: service.bundle,
		Constraints: constraints, Candidates: []DecisionRoleCandidate{candidate},
	})
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	ref, err := PersistDecisionRoleBindingPlan(ctx, store, plan)
	return plan, ref, err
}

func (service *primaryDecisionService) runDecisionEngine(ctx context.Context, request DecisionTerminalPreparationRequest, taskID, decisionID string, evidence *PrimaryDecisionEvidenceV1, _ *DecisionRoleBindingPlanV1) (*DecisionRecord, DecisionArtifactRef, error) {
	engine, err := service.coordinator.newDecisionEngine(taskID)
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	contractConfig := agent.RequestContractConfig{Enabled: true, Objective: service.question, SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "decision-produced", Statement: "A validated primary decision record is produced."}}}
	envelope, data, err := BuildRequestContract(service.question, service.question, contractConfig, 1, time.Now().UTC())
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	contractArtifact, err := PersistRequestContract(ctx, service.coordinator.decisionArtifactStore(), envelope, data)
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	contract := envelope.RequestContract()
	artifacts := make([]ArtifactRef, 0, len(evidence.Items))
	baseRates := make([]BaseRateEvidence, 0)
	for _, item := range evidence.Items {
		artifact, resolveErr := resolveDecisionArtifactRef(ctx, service.coordinator.decisionArtifactStore(), item.Source.SourceArtifact, "primary evidence source")
		if resolveErr == nil {
			artifacts = append(artifacts, artifact)
		}
		if item.Source.SourceKind == "base_rate" {
			var rate BaseRateEvidence
			if json.Unmarshal([]byte(item.Content), &rate) == nil {
				baseRates = append(baseRates, rate)
			}
		}
	}
	policyDigest, err := agent.DecisionPolicyDigest(service.bundle.Policy)
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	record, err := engine.Run(ctx, DecisionRequest{
		RunID: request.ExecutionRunID, TaskID: taskID, Attempt: 1, Profile: service.bundle.Ref, Policy: service.bundle.Policy,
		ProfileOrigin: agent.DecisionProfileOriginBuiltin, ProfileVersion: "v2", ProfileRef: service.bundle.Ref, PolicyDigest: policyDigest,
		Question: service.question, Artifacts: artifacts, BaseRates: baseRates, Role: "You are an isolated reviewer for the primary decision.",
		ProjectContext: service.coordinator.decisionProjectContext(), Contract: &contract, RequireRequestContract: true,
		RequestContractRef: contractArtifact.ID, RequestContractRevision: envelope.Revision, RequestContractArtifact: contractArtifact,
		DecisionID: decisionID,
	})
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	index, err := service.coordinator.decisionIndex()
	if err != nil {
		return nil, DecisionArtifactRef{}, err
	}
	entry, ok, err := index.Get(record.ID)
	if err != nil || !ok {
		return nil, DecisionArtifactRef{}, errors.Join(err, fmt.Errorf("primary decision record is absent from index"))
	}
	ref, err := decisionArtifactRefFromArtifact(entry.EffectiveRecordRef())
	return record, ref, err
}

func (service *primaryDecisionService) appendBlocked(ctx context.Context, journal EventJournal, request DecisionTerminalPreparationRequest, taskID, supportDigest string, cause error) error {
	reason := "decision_primary_blocked"
	if errors.Is(cause, ErrDecisionEvidenceInsufficient) {
		reason = "decision_evidence_insufficient"
	} else if errors.Is(cause, ErrDecisionBackendUnverified) || errors.Is(cause, ErrDecisionProfileUnsatisfied) {
		reason = "decision_role_admission_failed"
	}
	_, err := appendPrimaryCorrectnessEvent(ctx, journal, EventPrimaryDecisionBlocked, request, decisionBlockedKey(request.LogicalRunID, request.Generation, supportDigest, reason), PrimaryDecisionBlockedPayload{
		decisionGenerationEventCommon: generationEventCommonFor(request), TaskID: &taskID, ReasonCode: reason,
		SupportRevisionDigest: supportDigest, Repairable: errors.Is(cause, ErrDecisionEvidenceInsufficient), MissingRequirements: []DecisionMissingRequirement{},
	})
	return err
}

func appendPrimaryCorrectnessEvent(ctx context.Context, journal EventJournal, eventType EventType, request DecisionTerminalPreparationRequest, key string, payload any) (RunEvent, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return RunEvent{}, err
	}
	return journal.Append(ctx, RunEvent{Type: string(eventType), Actor: "decision-runtime", RunID: request.ExecutionRunID, BranchID: request.BranchID, IdempotencyKey: key, Payload: data})
}

func decisionEventCommonFor(request DecisionTerminalPreparationRequest) decisionEventCommon {
	return decisionEventCommon{SchemaVersion: 1, LogicalRunID: request.LogicalRunID, BranchID: request.BranchID, ExecutionRunID: request.ExecutionRunID, OwnerEpoch: 1}
}

func generationEventCommonFor(request DecisionTerminalPreparationRequest) decisionGenerationEventCommon {
	return decisionGenerationEventCommon{decisionEventCommon: decisionEventCommonFor(request), Generation: request.Generation}
}

func primarySupportSnapshot(events []RunEvent, fallback RunEvent) (DecisionSupportCursor, string, error) {
	selected := fallback
	if len(events) > 0 {
		selected = events[len(events)-1]
	}
	lineage := make([]struct {
		ID   string `json:"id"`
		Hash string `json:"hash"`
	}, 0, len(events))
	for _, event := range events {
		lineage = append(lineage, struct {
			ID   string `json:"id"`
			Hash string `json:"hash"`
		}{ID: event.ID, Hash: event.Hash})
	}
	lineageDigest, err := DecisionContractDigest("hufu/support-lineage/v1", lineage)
	if err != nil {
		return DecisionSupportCursor{}, "", err
	}
	supportDigest, err := DecisionContractDigest("hufu/support-revision/v1", struct {
		EventID string `json:"event_id"`
		Hash    string `json:"hash"`
	}{EventID: selected.ID, Hash: selected.Hash})
	return DecisionSupportCursor{EventID: selected.ID, EventHash: selected.Hash, LineageDigest: lineageDigest}, supportDigest, err
}

func primaryEvidenceLimitsFromBundle(bundle agent.DecisionProfileBundleV2) PrimaryEvidenceLimits {
	baseRateMinimum := uint32(0)
	if bundle.Evidence.RequireArtifactBackedBaseRate {
		baseRateMinimum = 1
	}
	return PrimaryEvidenceLimits{
		MaxCandidates: bundle.Evidence.MaxCandidates, MaxAggregateSourceBytes: bundle.Evidence.MaxScanBytes,
		MaxItems: bundle.Evidence.MaxSelectedItems, MaxViewBytes: bundle.Evidence.MaxTotalViewBytes, MaxItemViewBytes: bundle.Evidence.MaxItemViewBytes,
		MaxContextInputTokens: bundle.Evidence.MaxEvidenceInputTokens, ReservedFramingTokens: bundle.Evidence.FramingReserveTokens,
		KnownIndependenceMinimum: bundle.Evidence.MinKnownIndependentGroups, BaseRateMinimum: baseRateMinimum,
	}
}

func sourceEventForArtifact(events []RunEvent, ref ArtifactRef) (string, int) {
	for index := len(events) - 1; index >= 0; index-- {
		if ref.TaskID != "" && events[index].TaskID == ref.TaskID {
			return events[index].ID, index
		}
	}
	return "", 0
}

func artifactEvidenceFailed(manifest EvidenceManifest, artifactID string) bool {
	for _, result := range manifest.EvidenceResults {
		for _, ref := range result.ArtifactRefs {
			if ref.ID == artifactID {
				return strings.EqualFold(result.Status, "failed") || strings.EqualFold(result.Status, "error")
			}
		}
	}
	return false
}

func primaryBindingEventID(events []RunEvent, binding PrimaryBindingV1) string {
	for index := len(events) - 1; index >= 0; index-- {
		if EventType(events[index].Type) != EventPrimaryDecisionBound {
			continue
		}
		var payload PrimaryDecisionBoundPayload
		if json.Unmarshal(events[index].Payload, &payload) == nil && payload.Binding == binding {
			return events[index].ID
		}
	}
	return ""
}
