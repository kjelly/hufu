package team

import (
	"context"
	"fmt"
	"slices"
)

type contractCollectionRecoveryKey struct{}

// WithContractResultRepair selects one completed work attempt for a schema
// upgrade. Publication never invokes the worker or grants collector turns.
func WithContractResultRepair(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, contractCollectionRecoveryKey{}, taskID)
}

func contractResultRepairID(ctx context.Context) string {
	id, _ := ctx.Value(contractCollectionRecoveryKey{}).(string)
	return id
}

// ResultCollectionContractUpgrade authorizes a new submission binding while
// retaining the original sealed work, transcript, inputs and consumed budget.
// Only a validated contract_recovery_created event can establish this proof.
type ResultCollectionContractUpgrade struct {
	SourceOrigin     *ResultCollectionSourceOrigin `json:"source_origin,omitempty"`
	AcceptedResult   *AcceptedResultRepair         `json:"accepted_result,omitempty"`
	Original         *ResultCollectionCheckpoint   `json:"original"`
	OriginalContract ResultContractRef             `json:"original_contract"`
	AcceptedContract ResultContractRef             `json:"accepted_contract"`
	AcceptedPolicy   string                        `json:"accepted_policy"`
	SourceTaskSHA256 string                        `json:"source_task_sha256"`
}

func validateCollectionContractUpgrade(cp *ResultCollectionCheckpoint) error {
	p := cp.ContractUpgrade
	if p == nil {
		return nil
	}
	o := p.Original
	accepted := p.AcceptedResult != nil
	if o == nil || o.ContractUpgrade != nil || validateResultCollectionCheckpoint(o) != nil || (o.State == ResultCollectionCommitted) != accepted ||
		!validCollectionDigest(p.SourceTaskSHA256) || p.OriginalContract.ID == "" || p.AcceptedContract.ID == "" || !validCollectionDigest(p.OriginalContract.SchemaSHA256) || !validCollectionDigest(p.AcceptedContract.SchemaSHA256) ||
		(!accepted && (p.OriginalContract == p.AcceptedContract || p.AcceptedPolicy == o.PolicySHA256)) || p.AcceptedPolicy != cp.PolicySHA256 ||
		cp.BranchID == o.BranchID || cp.Work != o.Work || cp.SourceRunID != o.SourceRunID || cp.TaskID != o.TaskID || cp.Agent != o.Agent ||
		cp.Attempt != o.Attempt || cp.ModelExecutionID != o.ModelExecutionID || cp.ExecutionTarget != o.ExecutionTarget || cp.FanoutLeaf ||
		cp.WorkOccurrenceRevision != o.WorkOccurrenceRevision || cp.WorkDispatchID != o.WorkDispatchID ||
		cp.InputsID != o.InputsID || cp.InputsSHA256 != o.InputsSHA256 || cp.WorkspaceIdentitySHA256 != o.WorkspaceIdentitySHA256 ||
		cp.TurnsUsed < o.TurnsUsed || cp.TurnLimit < o.TurnLimit {
		return fmt.Errorf("invalid collection contract-upgrade proof")
	}
	if accepted && !validAcceptedResultRepair(p.AcceptedResult) {
		return fmt.Errorf("invalid accepted-result repair proof")
	}
	if p.SourceOrigin != nil && (!accepted || p.SourceOrigin.EventID == "" || !validCollectionDigest(p.SourceOrigin.EventHash) ||
		!validCollectionDigest(p.SourceOrigin.TaskSHA256) || !validCollectionDigest(p.SourceOrigin.ContractHash)) {
		return fmt.Errorf("invalid accepted collection source origin")
	}
	return nil
}

// A schema upgrade may retain browser observations, but never an uncertain
// operation or permission to replay a completed interaction. Other external
// side-effect classes keep their ordinary reconciliation requirement.
func collectionRecoveryBrowserTask(item *TodoItem, selected string) bool {
	if item == nil || item.SideEffect != SideEffectBrowserInteraction || item.CheckpointPause ||
		item.RecoveryState == RecoveryStateUnknown || item.RecoveryState == RecoveryStatePartial ||
		structuredStepsContainEffect(item.Execution.Steps, ExecutionEffectMutate) || structuredStepsContainEffect(item.Execution.Steps, ExecutionEffectProduce) {
		return false
	}
	if item.ID == selected && validateCollectionRecoveryTarget(item) == nil {
		return true // Sealed work and original sources are checked before planning.
	}
	if (item.Status == TaskPending || item.Status == TaskPlanned) && item.ExecutionReceipt == nil && item.StartedAt.IsZero() && len(item.ExecutionReceipts) == 0 {
		return true
	}
	r := item.ExecutionReceipt
	return item.Status == TaskDone && item.TypedResult != nil && taskResultStatusIsSuccessful(item.TypedResult.Status) &&
		r != nil && !r.FinishedAt.IsZero() && r.ExitCode != nil && *r.ExitCode == 0
}

func validateContractRecoverySessionSafety(ctx context.Context, session *SessionData) error {
	selected := contractResultRepairID(ctx)
	if selected == "" || session == nil {
		return validateDerivedForkSessionSafety(session)
	}
	projection := *session
	projection.Tasks = slices.DeleteFunc(slices.Clone(session.Tasks), func(item *TodoItem) bool {
		return collectionRecoveryBrowserTask(item, selected)
	})
	return validateDerivedForkSessionSafety(&projection)
}

func (c *Coordinator) assessContractResultRepair(ctx context.Context, source *ResumeSource, plan *ContractRecoveryPlan) error {
	id := contractResultRepairID(ctx)
	if id == "" {
		return nil
	}
	item := todoItemByID(source.Session.Tasks, id)
	accepted := item != nil && item.Status == TaskDone
	if legacySubmissionTarget(item) {
		proof, err := c.assessLegacySubmission(ctx, source, item, plan)
		if err != nil {
			return err
		}
		for i := range plan.Tasks {
			if plan.Tasks[i].ID == id {
				plan.Tasks[i].Reason = "legacy_submission"
				plan.Tasks[i].LegacySubmission = proof
				return nil
			}
		}
		return fmt.Errorf("legacy task missing from plan")
	}
	validateTarget := validateCollectionRecoveryTarget
	if accepted {
		validateTarget = validateAcceptedResultRepairTarget
	}
	if err := validateTarget(item); err != nil {
		return err
	}
	if item.ResultContract == nil || currentResultCollection(item).LegacySubmission != nil || currentResultCollection(item).ContractUpgrade != nil {
		return fmt.Errorf("contract result repair requires an original uncommitted structured collection")
	}
	if _, active := c.activeTaskResultOccurrence(id); active {
		return errTaskResultStale
	}
	originalSource, originalItem, origin, err := retainedCollectionOrigin(source, item)
	if err != nil {
		return fmt.Errorf("selected collection origin: %w", err)
	}
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	var currentRuntimeInputs map[string]any
	if origin != nil {
		err := func() error {
			restoreCurrent := c.installResumeValidationSource(source, store)
			defer restoreCurrent()
			if item.WorksetBinding != nil && item.WorksetBinding.SourceArtifact.ID != "" {
				if _, err := c.verifiedResultWorksetBinding(ctx, id); err != nil {
					return err
				}
			}
			for _, patch := range plan.Tasks {
				if patch.ID == id && patch.ResultContract != nil {
					compiled := c.session.ResultContracts[patch.ResultContract.ID]
					var err error
					currentRuntimeInputs, err = c.resultRuntimeInputValuesWithContext(ctx, compiled, id)
					return err
				}
			}
			return errResultCollectionStale
		}()
		if err != nil {
			return fmt.Errorf("retained collection current sources: %w", err)
		}
	}
	restore := c.installResumeValidationSource(originalSource, store)
	defer restore()
	previousOrigin := c.collectionOriginValidation
	if origin != nil {
		c.collectionOriginValidation = &collectionOriginValidation{source: originalSource, store: store, cp: currentResultCollection(originalItem)}
	}
	defer func() { c.collectionOriginValidation = previousOrigin }()
	cp := currentResultCollection(originalItem)
	if cp.PolicySHA256 != originalSource.Session.ExecutionPolicySnapshot.ConfigurationHash || cp.BranchID != originalSource.BranchID {
		return errResultCollectionStale
	}
	snapshot := contractRecoveryInputSnapshot(originalItem, originalSource.Session)
	if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil || snapshot.Team != c.session.Config.Name ||
		snapshot.SnapshotHash != cp.InputsSHA256 || snapshot.ID != cp.InputsID {
		return fmt.Errorf("contract result repair requires verified frozen inputs")
	}
	work, err := c.loadCollectionWorkAgainstPolicy(ctx, originalItem, cp, originalSource.Session.ExecutionPolicySnapshot, originalSource.Session)
	if err != nil {
		return err
	}
	if !collectionWorkCompleted(&work.Receipt, true) {
		return fmt.Errorf("contract result repair requires successful completed work")
	}
	if err := validateRecoveryTerminalContext(c.session.Workspace, cp); err != nil {
		return err
	}
	if _, err := c.restoreTranscriptEvidence(ctx, &work.Receipt, cp.SourceRunID); err != nil {
		return err
	}
	if _, err := collectionContextManifest(originalItem, cp, &work.Receipt); err != nil {
		return err
	}
	if err := c.validateSelectedRepairContext(ctx, originalItem, work.Receipt.ContextManifest); err != nil {
		return err
	}
	for _, blob := range []struct {
		kind string
		ref  *ResultCollectionRef
	}{{"request", cp.Request}, {"response", cp.Response}, {"claims", cp.Prepared}} {
		if blob.ref != nil {
			if _, err := c.readCollectionBlob(ctx, cp, blob.kind, blob.ref); err != nil {
				return err
			}
		}
	}
	return c.assessCollectionRepairBinding(ctx, plan, originalItem, work, origin, currentRuntimeInputs)
}

func (c *Coordinator) assessCollectionRepairBinding(ctx context.Context, plan *ContractRecoveryPlan, item *TodoItem, work *resultCollectionWork, origin *ResultCollectionSourceOrigin, currentRuntimeInputs map[string]any) error {
	cp, accepted := currentResultCollection(item), item.Status == TaskDone
	for i := range plan.Tasks {
		patch := &plan.Tasks[i]
		if patch.ID != item.ID {
			continue
		}
		if patch.ResultContract == nil || !accepted && *patch.ResultContract == *item.ResultContract {
			return fmt.Errorf("selected collection result contract is unchanged")
		}
		// New runtime-owned fields must have verified frozen sources before a
		// recovery child is published, even when old claims need format repair.
		compiled := c.session.ResultContracts[patch.ResultContract.ID]
		if compiled == nil {
			return fmt.Errorf("recovery target result schema is unavailable")
		}
		inputs, err := c.resultRuntimeInputValuesWithContext(ctx, compiled, item.ID)
		if err != nil {
			return fmt.Errorf("recovery target runtime inputs: %w", err)
		}
		if origin != nil && !sameCanonicalEvidence(inputs, currentRuntimeInputs) {
			return fmt.Errorf("retained collection runtime sources changed: %w", errResultCollectionStale)
		}
		patch.Collection = &ResultCollectionContractUpgrade{SourceOrigin: origin, Original: cp, OriginalContract: *item.ResultContract,
			AcceptedContract: *patch.ResultContract, AcceptedPolicy: plan.Policy.ConfigurationHash, SourceTaskSHA256: patch.SourceSHA256}
		if accepted {
			proof, err := c.assessAcceptedResultRepair(ctx, item, cp, work, compiled)
			if err != nil {
				return err
			}
			patch.Collection.AcceptedResult = proof
			patch.Reason = "accepted_reference_invalid"
		}
		return nil
	}
	return fmt.Errorf("selected collection has no static recovery task")
}

func projectContractResultRepair(item *TodoItem, patch ContractRecoveryTask, plan *ContractRecoveryPlan, branchID string) {
	cp := cloneResultCollectionCheckpoint(patch.Collection.Original)
	cp.ContractUpgrade = patch.Collection
	cp.BranchID, cp.PolicySHA256 = branchID, plan.Policy.ConfigurationHash
	cp.TaskContractSHA256, _ = decisionTaskInputDigest(item)
	cp.Revision++
	cp.Prepared, cp.Accepted = nil, nil
	cp.State = ResultCollectionBlocked
	if patch.Collection.AcceptedResult != nil {
		cp.Response = nil // Original responses remain immutable in the proof.
		cp.FailureReason = RepairFailureInvalidSchema
	}
	if cp.FailureReason == "" {
		cp.FailureReason = RepairFailureInvalidSchema
	}
	// Publication does not refund reservations, grant turns or activate a lease.
	// The child requires ordinary resume --repair-result and its normal checks.
	r := cloneExecutionReceipt(item.ExecutionReceipt)
	r.ResultCollection = cp
	if patch.Collection.AcceptedResult != nil {
		if r.RepairProvenance == nil {
			r.RepairProvenance = &RepairProvenance{}
		}
		// Retain spent reservations, prior grants and history. Only the child's
		// active submission is reopened; the original success remains durable.
		r.RepairProvenance.Attempted, r.RepairProvenance.Success = true, false
		r.RepairProvenance.RepairAttempts, r.RepairProvenance.FailureReason = cp.TurnsUsed, RepairFailureInvalidSchema
		r.RepairProvenance.SubmittedResult = nil
		r.RepairProvenance.PreparedCandidateSHA256 = ""
		item.FailureEvent = &FailureEventPayload{TaskID: item.ID, Phase: "protocol", FailureClass: FailureProtocol, FailureType: FailureTypeResultCollection, RetryDisposition: NeedsHuman}
	}
	item.ExecutionReceipt = &r
	item.ExecutionReceipts = appendExecutionReceipt(item.ExecutionReceipts, r)
	item.TypedResult, item.ResultCandidate, item.VerifyResult = nil, nil, nil
	item.Status = TaskBlocked
	item.Detail = "contract recovery: result-only repair; worker replay forbidden"
}

func contractRepairTaskID(plan *ContractRecoveryPlan) string {
	for _, patch := range plan.Tasks {
		if patch.Action == "repair_result" {
			return patch.ID
		}
	}
	return ""
}

func (c *Coordinator) validateOriginalContractCollectionEvidence(ctx context.Context, session *SessionData, item *TodoItem, cp *ResultCollectionCheckpoint) error {
	var sources []*TodoItem
	for _, id := range resultDependencyIDs(item) {
		source := todoItemByID(session.Tasks, id)
		if source == nil || source.Status != TaskDone {
			return fmt.Errorf("original collection source %s is unavailable", id)
		}
		if err := c.validateContractRecoverySource(ctx, source, session); err != nil {
			return err
		}
		sources = append(sources, source)
	}
	if cp.EvidenceRevision == 1 {
		sources = canonicalCollectionSources(sources)
	}
	digest, err := collectionDigest(sources)
	if err != nil || digest != cp.EvidenceSHA256 {
		return errResultCollectionStale
	}
	return nil
}

func collectionContractAdmissionMatches(item *TodoItem, admission DecisionAdmission) bool {
	cp := currentResultCollection(item)
	return legacyAdmissionMatches(item, admission) || cp != nil && cp.ContractUpgrade != nil && !admission.Enabled && admission.Profile == DecisionProfileOff &&
		validateCollectionContractUpgrade(cp) == nil && admission.TaskInputDigest == cp.ContractUpgrade.Original.TaskContractSHA256
}

func validateContractResultRepairSource(item *TodoItem, patch ContractRecoveryTask, plan *ContractRecoveryPlan) error {
	if patch.LegacySubmission != nil {
		return validateLegacySubmissionSource(item, patch.LegacySubmission, plan)
	}
	p := patch.Collection
	original, err := originalCollectionRepairItem(item, p)
	if err != nil {
		return err
	}
	validateTarget := validateCollectionRecoveryTarget
	if p != nil && p.AcceptedResult != nil {
		validateTarget = validateAcceptedResultRepairTarget
		if err := validateAcceptedResultRepairProof(original, p.AcceptedResult, patch.ResultContract); err != nil {
			return err
		}
	}
	if p == nil || p.Original == nil || p.Original.ContractUpgrade != nil || validateTarget(item) != nil ||
		original == nil || original.ResultContract == nil || patch.ResultContract == nil || p.OriginalContract != *original.ResultContract ||
		p.AcceptedContract != *patch.ResultContract || p.AcceptedResult == nil && p.OriginalContract == p.AcceptedContract ||
		p.SourceTaskSHA256 != patch.SourceSHA256 || p.AcceptedPolicy != plan.Policy.ConfigurationHash ||
		p.SourceOrigin == nil && (p.Original.PolicySHA256 != plan.SourcePolicy || p.Original.BranchID != plan.SourceBranch) ||
		p.Original.InputsID != item.RunInputSnapshotID || p.Original.InputsSHA256 != item.RunInputSnapshotHash ||
		!sameCanonicalEvidence(p.Original, currentResultCollection(item)) {
		return fmt.Errorf("invalid original collection contract-recovery source")
	}
	digest, err := decisionTaskInputDigest(original)
	if err != nil || digest != p.Original.TaskContractSHA256 {
		return errResultCollectionStale
	}
	return nil
}

// Rebind only metadata for sources retained byte-for-byte by this plan. Any
// source requiring new work invalidates result-only repair of its consumer.
func bindContractRepairEvidence(tasks map[string]*TodoItem, plan *ContractRecoveryPlan) error {
	for _, patch := range plan.Tasks {
		if patch.Action != "repair_result" {
			continue
		}
		item := tasks[patch.ID]
		var sources []*TodoItem
		for _, id := range resultDependencyIDs(item) {
			source := tasks[id]
			if source == nil || source.Status != TaskDone || source.TypedResult == nil {
				return fmt.Errorf("result-only contract recovery source %s requires new work", id)
			}
			sources = append(sources, source)
		}
		digest, err := collectionDigest(canonicalCollectionSources(sources))
		if err != nil {
			return err
		}
		item.ExecutionReceipt.ResultCollection.EvidenceSHA256 = digest
		item.ExecutionReceipt.ResultCollection.EvidenceRevision = 1
		if err := validateResultCollectionCheckpoint(item.ExecutionReceipt.ResultCollection); err != nil {
			return err
		}
		if limit, err := collectionTurnLimit(item.ExecutionReceipt); err != nil || limit != item.ExecutionReceipt.ResultCollection.TurnLimit {
			return errResultCollectionStale
		}
		item.ExecutionReceipts = appendExecutionReceipt(item.ExecutionReceipts, cloneExecutionReceipt(item.ExecutionReceipt))
	}
	return nil
}

func validateContractCollectionProjection(source *ResumeSource, plan *ContractRecoveryPlan) error {
	if plan.Repaired == 0 {
		return nil
	}
	projection := make(map[string]*TodoItem)
	for _, item := range source.Session.Tasks {
		if item != nil && !IsPrimaryOccurrence(item) {
			projection[item.ID] = cloneTodoItem(item)
		}
	}
	return applyContractRecoveryTasks(projection, plan, source.BranchID+"-result-repair-preview")
}
