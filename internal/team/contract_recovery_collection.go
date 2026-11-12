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
	Original         *ResultCollectionCheckpoint `json:"original"`
	OriginalContract ResultContractRef           `json:"original_contract"`
	AcceptedContract ResultContractRef           `json:"accepted_contract"`
	AcceptedPolicy   string                      `json:"accepted_policy"`
	SourceTaskSHA256 string                      `json:"source_task_sha256"`
}

func validateCollectionContractUpgrade(cp *ResultCollectionCheckpoint) error {
	p := cp.ContractUpgrade
	if p == nil {
		return nil
	}
	o := p.Original
	if o == nil || o.ContractUpgrade != nil || validateResultCollectionCheckpoint(o) != nil || o.State == ResultCollectionCommitted ||
		!validCollectionDigest(p.SourceTaskSHA256) || p.OriginalContract.ID == "" || p.AcceptedContract.ID == "" || !validCollectionDigest(p.OriginalContract.SchemaSHA256) || !validCollectionDigest(p.AcceptedContract.SchemaSHA256) ||
		p.OriginalContract == p.AcceptedContract || p.AcceptedPolicy != cp.PolicySHA256 || p.AcceptedPolicy == o.PolicySHA256 ||
		cp.BranchID == o.BranchID || cp.Work != o.Work || cp.SourceRunID != o.SourceRunID || cp.TaskID != o.TaskID || cp.Agent != o.Agent ||
		cp.Attempt != o.Attempt || cp.ModelExecutionID != o.ModelExecutionID || cp.ExecutionTarget != o.ExecutionTarget || cp.FanoutLeaf ||
		cp.WorkOccurrenceRevision != o.WorkOccurrenceRevision || cp.WorkDispatchID != o.WorkDispatchID ||
		cp.InputsID != o.InputsID || cp.InputsSHA256 != o.InputsSHA256 || cp.WorkspaceIdentitySHA256 != o.WorkspaceIdentitySHA256 ||
		cp.TurnsUsed < o.TurnsUsed || cp.TurnLimit < o.TurnLimit {
		return fmt.Errorf("invalid collection contract-upgrade proof")
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
	if err := validateCollectionRecoveryTarget(item); err != nil {
		return err
	}
	if item.ResultContract == nil || currentResultCollection(item).LegacySubmission != nil || currentResultCollection(item).ContractUpgrade != nil || currentResultCollection(item).State == ResultCollectionCommitted {
		return fmt.Errorf("contract result repair requires an original uncommitted structured collection")
	}
	if _, active := c.activeTaskResultOccurrence(id); active {
		return errTaskResultStale
	}
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	restore := c.installResumeValidationSource(source, store)
	defer restore()
	cp := currentResultCollection(item)
	if cp.PolicySHA256 != plan.SourcePolicy || cp.BranchID != source.BranchID {
		return errResultCollectionStale
	}
	snapshot := contractRecoveryInputSnapshot(item, source.Session)
	if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil || snapshot.Team != c.session.Config.Name ||
		snapshot.SnapshotHash != cp.InputsSHA256 || snapshot.ID != cp.InputsID {
		return fmt.Errorf("contract result repair requires verified frozen inputs")
	}
	work, err := c.loadCollectionWorkAgainstPolicy(ctx, item, cp, source.Session.ExecutionPolicySnapshot, source.Session)
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
	if _, err := collectionContextManifest(item, cp, &work.Receipt); err != nil {
		return err
	}
	if err := c.validateSelectedRepairContext(ctx, item, work.Receipt.ContextManifest); err != nil {
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
	for i := range plan.Tasks {
		patch := &plan.Tasks[i]
		if patch.ID != id {
			continue
		}
		if patch.ResultContract == nil || *patch.ResultContract == *item.ResultContract {
			return fmt.Errorf("selected collection result contract is unchanged")
		}
		patch.Collection = &ResultCollectionContractUpgrade{Original: cp, OriginalContract: *item.ResultContract,
			AcceptedContract: *patch.ResultContract, AcceptedPolicy: plan.Policy.ConfigurationHash, SourceTaskSHA256: patch.SourceSHA256}
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
	if cp.FailureReason == "" {
		cp.FailureReason = RepairFailureInvalidSchema
	}
	// Publication does not refund reservations, grant turns or activate a lease.
	// The child requires ordinary resume --repair-result and its normal checks.
	r := cloneExecutionReceipt(item.ExecutionReceipt)
	r.ResultCollection = cp
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
	if p == nil || p.Original == nil || p.Original.ContractUpgrade != nil || validateCollectionRecoveryTarget(item) != nil ||
		item.ResultContract == nil || patch.ResultContract == nil || p.OriginalContract != *item.ResultContract ||
		p.AcceptedContract != *patch.ResultContract || p.OriginalContract == p.AcceptedContract ||
		p.SourceTaskSHA256 != patch.SourceSHA256 || p.AcceptedPolicy != plan.Policy.ConfigurationHash ||
		p.Original.PolicySHA256 != plan.SourcePolicy || p.Original.BranchID != plan.SourceBranch ||
		p.Original.InputsID != item.RunInputSnapshotID || p.Original.InputsSHA256 != item.RunInputSnapshotHash ||
		!sameCanonicalEvidence(p.Original, currentResultCollection(item)) {
		return fmt.Errorf("invalid original collection contract-recovery source")
	}
	digest, err := decisionTaskInputDigest(item)
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
